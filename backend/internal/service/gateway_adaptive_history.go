package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	adaptiveHistoryQueue     = 4096
	adaptiveHistoryBatch     = 1024
	AdaptiveHistoryMaxMember = 1024
	AdaptiveHistoryRetention = 24 * time.Hour
	adaptiveHistoryTimeout   = 2 * time.Second
)

type AdaptiveHistoryRecord struct {
	At      time.Time
	Payload string
}

// Read returns a bounded snapshot in (after, through], newest first.
// Merge retains records newer than cutoff and may receive the same batch again.
type AdaptiveHistoryStore interface {
	Read(ctx context.Context, after, through time.Time) ([]AdaptiveHistoryRecord, error)
	Merge(ctx context.Context, cutoff time.Time, records []AdaptiveHistoryRecord) error
}

// Only fields in the exact sample key and the outcome leave the process.
// The sorted set score carries the observation time in Unix microseconds.
type adaptiveStoredSample struct {
	ID              string
	AccountID       int64
	Platform        string
	AccountType     string
	Model           string
	Operation       string
	Transport       string
	InputSize       int
	Effort          string
	Duration        string
	Tier            string
	ThinkingLevel   string
	ThinkingEnabled bool
	Latency         time.Duration
	Success         bool
}

type adaptiveHistoryEntry struct {
	key    adaptiveSampleKey
	sample adaptiveSample
}

type adaptiveHistory struct {
	store    AdaptiveHistoryStore
	queue    chan adaptiveHistoryEntry
	mu       sync.Mutex
	stopped  bool
	stop     context.CancelFunc
	done     chan struct{}
	stopErr  error // published by closing done
	writerID string
	sequence atomic.Uint64

	// Only the worker accesses the pending batch. Retry the exact same ZSET
	// members when a write may have succeeded but its response was lost.
	pending []AdaptiveHistoryRecord
}

func newAdaptiveTelemetry(store AdaptiveHistoryStore) *adaptiveTelemetry {
	t := &adaptiveTelemetry{}
	if store != nil {
		ctx := t.attachAdaptiveHistory(store)
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			t.history.run(ctx, t, ticker.C)
		}()
	}
	return t
}

func (t *adaptiveTelemetry) attachAdaptiveHistory(store AdaptiveHistoryStore) context.Context {
	ctx, stop := context.WithCancel(context.Background())
	t.history = &adaptiveHistory{
		store: store, queue: make(chan adaptiveHistoryEntry, adaptiveHistoryQueue),
		stop: stop, done: make(chan struct{}), writerID: uuid.NewString() + "/",
	}
	return ctx
}

func (h *adaptiveHistory) nextID() string {
	return h.writerID + strconv.FormatUint(h.sequence.Add(1), 10)
}

func (h *adaptiveHistory) enqueue(key adaptiveSampleKey, sample adaptiveSample) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	select {
	case h.queue <- adaptiveHistoryEntry{key, sample}:
	default:
	}
}

func (h *adaptiveHistory) restore(ctx context.Context, t *adaptiveTelemetry, now time.Time) error {
	entries, err := h.store.Read(ctx, now.Add(-adaptiveWindow), now)
	if err != nil {
		return err
	}
	for i := len(entries) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := entries[i]
		if len(entry.Payload) > AdaptiveHistoryMaxMember {
			continue
		}
		var data adaptiveStoredSample
		// The worker may have persisted local samples after a failed read.
		if json.Unmarshal([]byte(entry.Payload), &data) != nil || data.ID == "" || strings.HasPrefix(data.ID, h.writerID) || len(data.Model) > 128 || data.Operation == "" ||
			(data.Success && (data.Latency <= 0 || data.Latency > adaptiveMaxSuccessLatency)) {
			continue
		}
		key := adaptiveSampleKey{
			route: adaptiveRoute{
				operation: data.Operation, transport: data.Transport, inputSize: data.InputSize,
				effort: data.Effort, duration: data.Duration, tier: data.Tier,
				thinkingLevel: data.ThinkingLevel, thinkingEnabled: data.ThinkingEnabled,
			},
			accountID: data.AccountID, platform: data.Platform, accountType: data.AccountType, model: data.Model,
		}
		t.addSample(key, adaptiveSample{at: entry.At, latency: data.Latency, success: data.Success, id: data.ID})
	}
	return nil
}

func (h *adaptiveHistory) fillBatch() {
	for range adaptiveHistoryBatch {
		var entry adaptiveHistoryEntry
		select {
		case entry = <-h.queue:
		default:
			return
		}
		key, sample := entry.key, entry.sample
		data, err := json.Marshal(adaptiveStoredSample{
			ID: sample.id, AccountID: key.accountID,
			Platform: key.platform, AccountType: key.accountType, Model: key.model,
			Operation: key.route.operation, Transport: key.route.transport, InputSize: key.route.inputSize,
			Effort: key.route.effort, Duration: key.route.duration, Tier: key.route.tier,
			ThinkingLevel: key.route.thinkingLevel, ThinkingEnabled: key.route.thinkingEnabled,
			Latency: sample.latency, Success: sample.success,
		})
		if err == nil && len(data) <= AdaptiveHistoryMaxMember {
			h.pending = append(h.pending, AdaptiveHistoryRecord{sample.at, string(data)})
		}
	}
}

func (h *adaptiveHistory) run(ctx context.Context, t *adaptiveTelemetry, ticks <-chan time.Time) {
	defer close(h.done)
	restored := false
	for initial := true; ctx.Err() == nil; initial = false {
		if !initial {
			writeCtx, cancel := context.WithTimeout(ctx, adaptiveHistoryTimeout)
			if err := h.flush(writeCtx); err != nil {
				slog.Debug("gateway.adaptive_history_flush_failed", "error", err)
			}
			cancel()
		}
		if !restored && ctx.Err() == nil {
			readCtx, cancel := context.WithTimeout(ctx, adaptiveHistoryTimeout)
			if err := h.restore(readCtx, t, time.Now()); err != nil {
				slog.Debug("gateway.adaptive_history_restore_failed", "error", err)
			} else {
				restored = true
			}
			cancel()
		}
		select {
		case <-ctx.Done():
		case <-ticks:
		}
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), adaptiveHistoryTimeout)
	defer cancel()
	for len(h.pending) > 0 || len(h.queue) > 0 {
		if err := h.flush(drainCtx); err != nil {
			h.stopErr = fmt.Errorf("flush gateway adaptive history: %w", err)
			return
		}
	}
}

func (h *adaptiveHistory) flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// An uncertain batch must keep the same member IDs until it is acknowledged.
	if len(h.pending) == 0 {
		h.fillBatch()
	}
	if len(h.pending) == 0 {
		return nil
	}
	if err := h.store.Merge(ctx, time.Now().Add(-adaptiveWindow), h.pending); err != nil {
		return err
	}
	clear(h.pending)
	h.pending = h.pending[:0]
	return nil
}

// StopGatewayAdaptiveHistory drains the bounded queue before Redis is closed.
func (s *SettingService) StopGatewayAdaptiveHistory(ctx context.Context) error {
	if s == nil || s.adaptiveTelemetry == nil || s.adaptiveTelemetry.history == nil {
		return nil
	}
	h := s.adaptiveTelemetry.history
	h.mu.Lock()
	h.stopped = true
	h.stop()
	h.mu.Unlock()
	select {
	case <-h.done:
		return h.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
