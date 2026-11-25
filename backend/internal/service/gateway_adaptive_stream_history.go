package service

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// AdaptiveStreamHistoryStore is implemented alongside AdaptiveHistoryStore.
// The separate methods keep older stores and test doubles compatible.
type AdaptiveStreamHistoryStore interface {
	ReadStreamHealth(ctx context.Context, after, through time.Time) ([]AdaptiveHistoryRecord, error)
	MergeStreamHealth(ctx context.Context, cutoff time.Time, records []AdaptiveHistoryRecord) (int64, error)
}

type adaptiveStreamStoredSample struct {
	Version int
	Wire    string
	Work    adaptiveWorkload
	Sample  adaptiveStoredSample
}

type adaptiveStreamEntry struct {
	key    adaptiveStreamKey
	sample adaptiveSample
}

type adaptiveStreamHistory struct {
	store    AdaptiveStreamHistoryStore
	queue    chan adaptiveStreamEntry
	mu       sync.Mutex
	stopped  bool
	stop     context.CancelFunc
	done     chan struct{}
	stopErr  error
	writerID string
	sequence atomic.Uint64
	pending  []AdaptiveHistoryRecord
}

func (h *adaptiveStreamHealth) attach(store AdaptiveStreamHistoryStore) context.Context {
	ctx, stop := context.WithCancel(context.Background())
	h.history = &adaptiveStreamHistory{
		store: store, queue: make(chan adaptiveStreamEntry, adaptiveHistoryQueue),
		stop: stop, done: make(chan struct{}), writerID: uuid.NewString() + "/",
	}
	return ctx
}

func (h *adaptiveStreamHistory) nextID() string {
	return h.writerID + strconv.FormatUint(h.sequence.Add(1), 36)
}

func (h *adaptiveStreamHistory) enqueue(health *adaptiveStreamHealth, entry adaptiveStreamEntry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return
	}
	select {
	case h.queue <- entry:
		health.count(streamQueued)
	default:
		health.count(streamQueueFull)
	}
}

func (h *adaptiveStreamHistory) restore(ctx context.Context, health *adaptiveStreamHealth, now time.Time) error {
	records, err := h.store.ReadStreamHealth(ctx, now.Add(-adaptiveStreamWindow), now)
	if err != nil {
		return err
	}
	health.mu.Lock()
	defer health.mu.Unlock()
	for i := len(records) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		record := records[i]
		var data adaptiveStreamStoredSample
		if len(record.Payload) > AdaptiveHistoryMaxMember || json.Unmarshal([]byte(record.Payload), &data) != nil ||
			data.Version != 1 || data.Wire != APIProtocolChatCompletions ||
			data.Work&adaptiveWorkText == 0 || data.Work & ^adaptiveWorkKnown != 0 {
			continue
		}
		sample := data.Sample
		if sample.ID == "" || sample.AccountID <= 0 || sample.Model == "" || len(sample.Model) > 128 ||
			!adaptiveStreamOperation(sample.Operation) || sample.Transport != "sse" || sample.Effort == "unknown" ||
			adaptiveTier(sample.Tier) == "unknown" || !sample.Success || sample.Health != nil ||
			sample.Latency <= 0 || sample.Latency > adaptiveStreamWindow ||
			!record.At.After(now.Add(-adaptiveStreamWindow)) || record.At.After(now) {
			continue
		}
		key := adaptiveStreamKey{wire: data.Wire, work: data.Work, sample: adaptiveSampleKey{
			route: adaptiveRoute{operation: sample.Operation, transport: sample.Transport, inputSize: sample.InputSize,
				effort: sample.Effort, duration: sample.Duration, tier: sample.Tier, thinkingLevel: sample.ThinkingLevel,
				thinkingEnabled: sample.ThinkingEnabled},
			accountID: sample.AccountID, platform: sample.Platform, accountType: sample.AccountType, model: sample.Model,
			groupID: sample.GroupID, config: sample.Config,
		}}
		health.insert(key, adaptiveSample{at: record.At, latency: sample.Latency, success: true, id: sample.ID}, now)
		health.count(streamRestored)
	}
	health.ready.Store(true)
	return nil
}

func (h *adaptiveStreamHistory) fill(health *adaptiveStreamHealth) {
	for len(h.pending) < adaptiveHistoryBatch {
		select {
		case entry := <-h.queue:
			payload, err := json.Marshal(adaptiveStreamStoredSample{
				Version: 1, Wire: entry.key.wire, Work: entry.key.work,
				Sample: adaptiveStoredObservation(entry.key.sample, entry.sample),
			})
			if err != nil || len(payload) > AdaptiveHistoryMaxMember {
				health.count(streamCapacity)
				continue
			}
			h.pending = append(h.pending, AdaptiveHistoryRecord{At: entry.sample.at, Payload: string(payload)})
		default:
			return
		}
	}
}

func (h *adaptiveStreamHistory) flush(ctx context.Context, health *adaptiveStreamHealth, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(h.pending) == 0 {
		h.fill(health)
	}
	if len(h.pending) == 0 {
		return nil
	}
	trimmed, err := h.store.MergeStreamHealth(ctx, now.Add(-adaptiveStreamWindow), h.pending)
	if err != nil {
		health.count(streamFlushFailed)
		return err
	}
	if trimmed > 0 {
		health.counters[streamCapacity].Add(uint64(trimmed))
	}
	health.counters[streamFlushed].Add(uint64(len(h.pending)))
	clear(h.pending)
	h.pending = h.pending[:0]
	return nil
}

func (h *adaptiveStreamHistory) run(ctx context.Context, health *adaptiveStreamHealth, ticks <-chan time.Time) {
	defer close(h.done)
	for initial := true; ctx.Err() == nil; initial = false {
		now := time.Now()
		if !initial {
			writeCtx, cancel := context.WithTimeout(ctx, adaptiveHistoryTimeout)
			_ = h.flush(writeCtx, health, now)
			cancel()
		}
		if !health.ready.Load() && ctx.Err() == nil {
			readCtx, cancel := context.WithTimeout(ctx, adaptiveHistoryTimeout)
			if err := h.restore(readCtx, health, now); err != nil {
				health.count(streamRestoreFailed)
			}
			cancel()
		}
		if !initial {
			health.mu.Lock()
			for key, samples := range health.series {
				if !samples[len(samples)-1].at.After(now.Add(-adaptiveStreamWindow)) {
					delete(health.series, key)
				}
			}
			health.mu.Unlock()
			health.summarize()
		}
		select {
		case <-ctx.Done():
		case <-ticks:
		}
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), adaptiveHistoryTimeout)
	defer cancel()
	for len(h.queue) > 0 || len(h.pending) > 0 {
		if h.stopErr = h.flush(drainCtx, health, time.Now()); h.stopErr != nil {
			break
		}
	}
	health.summarize()
}

func (h *adaptiveStreamHistory) stopAccepting() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopped = true
	h.stop()
}

func (h *adaptiveStreamHistory) wait(ctx context.Context) error {
	select {
	case <-h.done:
		return h.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
