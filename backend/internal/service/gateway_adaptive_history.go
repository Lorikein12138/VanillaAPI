package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	At       time.Time
	Payload  string
	StateKey string
}

// Read returns a bounded snapshot in (after, through], newest first.
// Merge retains records newer than cutoff and may receive the same batch again.
type AdaptiveHistoryStore interface {
	Read(ctx context.Context, after, through time.Time) ([]AdaptiveHistoryRecord, error)
	Merge(ctx context.Context, cutoff time.Time, records []AdaptiveHistoryRecord) error
}

// AdaptiveScoreHistoryStore keeps scoring-only successes separate from the
// routing history's write queue, retention capacity, and health records.
type AdaptiveScoreHistoryStore interface {
	ReadScoreHistory(ctx context.Context, after, through time.Time) ([]AdaptiveHistoryRecord, error)
	MergeScoreHistory(ctx context.Context, cutoff time.Time, records []AdaptiveHistoryRecord) error
}

type adaptiveScoreHistoryAdapter struct{ AdaptiveScoreHistoryStore }

func (s adaptiveScoreHistoryAdapter) Read(ctx context.Context, after, through time.Time) ([]AdaptiveHistoryRecord, error) {
	return s.ReadScoreHistory(ctx, after, through)
}

func (s adaptiveScoreHistoryAdapter) Merge(ctx context.Context, cutoff time.Time, records []AdaptiveHistoryRecord) error {
	return s.MergeScoreHistory(ctx, cutoff, records)
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
	NonText         bool            `json:",omitempty"`
	ScoreOnly       bool            `json:",omitempty"`
	GroupID         int64           `json:",omitempty"`
	Config          string          `json:",omitempty"`
	Health          *adaptiveHealth `json:",omitempty"`
}

type adaptiveHistoryEntry struct {
	key    adaptiveSampleKey
	sample adaptiveSample
}

type adaptiveHistory struct {
	store     AdaptiveHistoryStore
	scoreOnly bool
	queue     chan adaptiveHistoryEntry
	mu        sync.Mutex
	stopped   bool
	stop      context.CancelFunc
	done      chan struct{}
	stopErr   error // published by closing done
	writerID  string
	sequence  atomic.Uint64

	// Only the worker accesses the pending batch. Retry the exact same ZSET
	// members when a write may have succeeded but its response was lost.
	pending    []AdaptiveHistoryRecord
	reconciled []AdaptiveHistoryRecord
}

func newAdaptiveTelemetry(store AdaptiveHistoryStore) *adaptiveTelemetry {
	t := &adaptiveTelemetry{}
	if store != nil {
		ctx := t.attachAdaptiveHistory(store)
		t.history.start(ctx, t)
	}
	if scoreStore, ok := store.(AdaptiveScoreHistoryStore); ok {
		ctx := t.attachAdaptiveScoreHistory(scoreStore)
		t.scoreHistory.start(ctx, t)
	}
	if streamStore, ok := store.(AdaptiveStreamHistoryStore); ok {
		ctx := t.streamHealth.attach(streamStore)
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			t.streamHealth.history.run(ctx, &t.streamHealth, ticker.C)
		}()
	}
	return t
}

func (t *adaptiveTelemetry) attachAdaptiveHistory(store AdaptiveHistoryStore) context.Context {
	ctx, history := makeAdaptiveHistory(store, false)
	t.history = history
	return ctx
}

func (t *adaptiveTelemetry) attachAdaptiveScoreHistory(store AdaptiveScoreHistoryStore) context.Context {
	ctx, history := makeAdaptiveHistory(adaptiveScoreHistoryAdapter{store}, true)
	t.scoreHistory = history
	return ctx
}

func makeAdaptiveHistory(store AdaptiveHistoryStore, scoreOnly bool) (context.Context, *adaptiveHistory) {
	ctx, stop := context.WithCancel(context.Background())
	return ctx, &adaptiveHistory{
		store: store, queue: make(chan adaptiveHistoryEntry, adaptiveHistoryQueue),
		scoreOnly: scoreOnly, stop: stop, done: make(chan struct{}), writerID: uuid.NewString() + "/",
	}
}

func (h *adaptiveHistory) start(ctx context.Context, t *adaptiveTelemetry) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		h.run(ctx, t, ticker.C)
	}()
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
	states := make(map[adaptiveSampleKey]adaptiveHealth)
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
		if json.Unmarshal([]byte(entry.Payload), &data) != nil || (data.Health == nil && (data.ID == "" || strings.HasPrefix(data.ID, h.writerID))) || len(data.Model) > 128 || data.Operation == "" ||
			(data.Success && (data.Latency <= 0 || data.Latency > adaptiveMaxSuccessLatency)) {
			continue
		}
		if data.ScoreOnly != h.scoreOnly || (data.ScoreOnly && (data.Health != nil || !data.Success || data.NonText ||
			data.GroupID <= 0 || data.AccountID <= 0 || data.Model == "" || !adaptiveStreamOperation(data.Operation) || data.Transport != "sse")) {
			continue
		}
		key := adaptiveSampleKey{
			route: adaptiveRoute{
				operation: data.Operation, transport: data.Transport, inputSize: data.InputSize,
				effort: data.Effort, duration: data.Duration, tier: data.Tier,
				thinkingLevel: data.ThinkingLevel, thinkingEnabled: data.ThinkingEnabled, nonText: data.NonText,
			},
			accountID: data.AccountID, platform: data.Platform, accountType: data.AccountType, model: data.Model,
			groupID: data.GroupID, config: data.Config, scoreOnly: data.ScoreOnly,
		}
		if data.Health != nil {
			state := *data.Health
			if state.Last.IsZero() || state.Last.After(now) || now.Sub(state.Last) >= adaptiveHealthRetention ||
				state.Fault > adaptiveUpstreamDegraded || state.Passed > 2 || state.Backoff > 3 {
				continue
			}
			if state.Last.After(states[key].Last) {
				states[key] = state
			}
			continue
		}
		if age := now.Sub(entry.At); age >= 0 && age < adaptiveWindow {
			t.addSample(key, adaptiveSample{at: entry.At, latency: data.Latency, success: data.Success, id: data.ID})
		}
	}
	for key, state := range states {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.restoreHealth(t, key, state)
	}
	return nil
}

func (h *adaptiveHistory) restoreHealth(t *adaptiveTelemetry, key adaptiveSampleKey, state adaptiveHealth) {
	t.mu.Lock()
	defer t.mu.Unlock()
	series := t.seriesForObservation(key, state.Last)
	if series == nil {
		return
	}
	local := series.health
	series.health = state
	for offset := range adaptiveSamples {
		sample := series.samples[(series.next+offset)%adaptiveSamples]
		if sample.at.After(state.Last) && strings.HasPrefix(sample.id, h.writerID) {
			series.updateHealth(key, sample, sample.probe)
		}
	}
	// Keep newer degradation even when its triggering observations rolled out of the sample ring.
	if local.Fault > series.health.Fault && local.FaultAt.After(state.Last) &&
		local.FaultAt.After(series.health.RecoveredAt) && local.Last.Equal(series.health.Last) {
		series.health.Fault, series.health.FaultAt = local.Fault, local.FaultAt
		series.health.RetryAt, series.health.Backoff, series.health.Passed = local.RetryAt, local.Backoff, local.Passed
	}
}

func adaptiveStoredObservation(key adaptiveSampleKey, sample adaptiveSample) adaptiveStoredSample {
	return adaptiveStoredSample{
		ID: sample.id, AccountID: key.accountID,
		Platform: key.platform, AccountType: key.accountType, Model: key.model,
		Operation: key.route.operation, Transport: key.route.transport, InputSize: key.route.inputSize,
		Effort: key.route.effort, Duration: key.route.duration, Tier: key.route.tier,
		ThinkingLevel: key.route.thinkingLevel, ThinkingEnabled: key.route.thinkingEnabled,
		Latency: sample.latency, Success: sample.success, GroupID: key.groupID, Config: key.config, NonText: key.route.nonText, ScoreOnly: key.scoreOnly,
	}
}

func adaptiveHealthRecord(key adaptiveSampleKey, health adaptiveHealth) (AdaptiveHistoryRecord, bool) {
	record := adaptiveStoredObservation(key, adaptiveSample{})
	identity, err := json.Marshal(record)
	if err != nil {
		return AdaptiveHistoryRecord{}, false
	}
	digest := sha256.Sum256(identity)
	record.Health = &health
	data, err := json.Marshal(record)
	return AdaptiveHistoryRecord{At: health.Last, Payload: string(data), StateKey: hex.EncodeToString(digest[:])},
		err == nil && len(data) <= AdaptiveHistoryMaxMember
}

func (h *adaptiveHistory) reconcilePendingHealth(t *adaptiveTelemetry, now time.Time) {
	t.mu.Lock()
	states := make(map[adaptiveSampleKey]adaptiveHealth)
	for key, series := range t.series {
		for _, sample := range series.samples {
			if sample.at.Equal(series.health.Last) && strings.HasPrefix(sample.id, h.writerID) && now.Sub(sample.at) < adaptiveHealthRetention {
				states[key] = series.health
				break
			}
		}
	}
	t.mu.Unlock()
	for key, health := range states {
		if record, ok := adaptiveHealthRecord(key, health); ok {
			h.reconciled = append(h.reconciled, record)
		}
	}
}

func (h *adaptiveHistory) fillBatch(t *adaptiveTelemetry, allowHealth bool) {
	if allowHealth {
		n := min(len(h.reconciled), adaptiveHistoryBatch)
		h.pending = append(h.pending, h.reconciled[:n]...)
		h.reconciled = h.reconciled[n:]
	}
	for range adaptiveHistoryBatch {
		var entry adaptiveHistoryEntry
		select {
		case entry = <-h.queue:
		default:
			return
		}
		key, sample := entry.key, entry.sample
		data, err := json.Marshal(adaptiveStoredObservation(key, sample))
		if err == nil && len(data) <= AdaptiveHistoryMaxMember {
			h.pending = append(h.pending, AdaptiveHistoryRecord{At: sample.at, Payload: string(data)})
		}
		if !allowHealth {
			continue
		}
		t.mu.Lock()
		var health adaptiveHealth
		if series := t.series[key]; series != nil {
			health = series.health
		}
		t.mu.Unlock()
		if health.Last.Equal(sample.at) {
			if record, ok := adaptiveHealthRecord(key, health); ok {
				h.pending = append(h.pending, record)
			}
		}
	}
}

func (h *adaptiveHistory) run(ctx context.Context, t *adaptiveTelemetry, ticks <-chan time.Time) {
	defer close(h.done)
	restored := false
	for initial := true; ctx.Err() == nil; initial = false {
		if !initial {
			writeCtx, cancel := context.WithTimeout(ctx, adaptiveHistoryTimeout)
			if err := h.flush(writeCtx, t, restored && !h.scoreOnly); err != nil {
				slog.Debug("gateway.adaptive_history_flush_failed", "error", err, "score_only", h.scoreOnly)
			}
			cancel()
		}
		if !restored && ctx.Err() == nil {
			readCtx, cancel := context.WithTimeout(ctx, adaptiveHistoryTimeout)
			if err := h.restore(readCtx, t, time.Now()); err != nil {
				slog.Debug("gateway.adaptive_history_restore_failed", "error", err, "score_only", h.scoreOnly)
			} else {
				if !h.scoreOnly {
					h.reconcilePendingHealth(t, time.Now())
				}
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
	for len(h.pending) > 0 || len(h.queue) > 0 || len(h.reconciled) > 0 {
		if err := h.flush(drainCtx, t, restored && !h.scoreOnly); err != nil {
			h.stopErr = fmt.Errorf("flush gateway adaptive history: %w", err)
			return
		}
	}
}

func (h *adaptiveHistory) flush(ctx context.Context, t *adaptiveTelemetry, allowHealth bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// An uncertain batch must keep the same member IDs until it is acknowledged.
	if len(h.pending) == 0 {
		h.fillBatch(t, allowHealth)
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
	if s == nil || s.adaptiveTelemetry == nil {
		return nil
	}
	h := s.adaptiveTelemetry.history
	score := s.adaptiveTelemetry.scoreHistory
	stream := s.adaptiveTelemetry.streamHealth.history
	if stream != nil {
		stream.stopAccepting()
	}
	h.stopAccepting()
	score.stopAccepting()
	err := errors.Join(h.wait(ctx), score.wait(ctx))
	if stream != nil {
		err = errors.Join(err, stream.wait(ctx))
	}
	return err
}

func (h *adaptiveHistory) stopAccepting() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.stopped = true
	h.stop()
	h.mu.Unlock()
}

func (h *adaptiveHistory) wait(ctx context.Context) error {
	if h == nil {
		return nil
	}
	select {
	case <-h.done:
		return h.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
