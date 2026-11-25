package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type adaptiveStreamHistoryFake struct {
	mu          sync.Mutex
	rows        []AdaptiveHistoryRecord
	batches     [][]AdaptiveHistoryRecord
	readErr     error
	failNext    bool
	readStarted chan struct{}
	allowRead   chan struct{}
}

func (s *adaptiveStreamHistoryFake) ReadStreamHealth(ctx context.Context, _, _ time.Time) ([]AdaptiveHistoryRecord, error) {
	if s.allowRead != nil {
		close(s.readStarted)
		select {
		case <-s.allowRead:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AdaptiveHistoryRecord(nil), s.rows...), s.readErr
}

func (s *adaptiveStreamHistoryFake) MergeStreamHealth(_ context.Context, _ time.Time, rows []AdaptiveHistoryRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]AdaptiveHistoryRecord(nil), rows...))
	// Return all writes during restore to check reducer deduplication too.
	s.rows = append(s.rows, rows...)
	if s.failNext {
		s.failNext = false
		return 0, errors.New("lost acknowledgement")
	}
	return 0, nil
}

func TestGatewayAdaptiveStreamHealthHistoryRestart(t *testing.T) {
	for _, count := range []int{0, 2, 3} {
		name := []string{"slow", "", "recovering", "recovered"}[count]
		t.Run(name, func(t *testing.T) {
			now := time.Unix(1_800_000_000, 0)
			store := &adaptiveStreamHistoryFake{failNext: true}
			telemetry := &adaptiveTelemetry{}
			c, _, _ := streamHealthFixture(72)
			body := "{\"instructions\":\"guide\",\"prompt_cache_options\":{\"ttl\":\"5m\"}," + streamHealthBody[1:]
			ctx := streamHealthContext(context.Background(), telemetry, 72, body)
			key := streamHealthKey(t, ctx, &c)
			require.Equal(t, adaptiveWorkText|adaptiveWorkInstructions|adaptiveWorkCache|adaptiveWorkState, key.work)
			health := &telemetry.streamHealth
			health.attach(store)
			defer health.history.stopAccepting()
			for i, latency := range []time.Duration{39800, 48400, 32200} {
				health.observe(key, now.Add(time.Duration(i-10)*time.Second), latency*time.Millisecond)
			}
			for i := range count {
				health.observe(key, now.Add(time.Duration(i-5)*time.Second), 12*time.Second)
			}
			require.Error(t, health.history.flush(ctx, health, now))
			require.NoError(t, health.history.flush(ctx, health, now))
			require.Equal(t, store.batches[0], store.batches[1])
			var stored adaptiveStreamStoredSample
			require.NoError(t, json.Unmarshal([]byte(store.rows[0].Payload), &stored))
			require.Equal(t, 2, stored.Version)
			require.Equal(t, key.work, stored.Work)
			legacy := store.rows[0]
			legacy.Payload = strings.Replace(legacy.Payload, "\"Version\":2", "\"Version\":1", 1)
			store.rows = append(store.rows, legacy)
			restarted := &adaptiveStreamHealth{}
			restarted.attach(store)
			defer restarted.history.stopAccepting()
			require.NoError(t, restarted.history.restore(ctx, restarted, now))
			require.True(t, restarted.ready.Load())
			require.EqualValues(t, (count+3)*2, restarted.counters[streamRestored].Load())
			require.Len(t, restarted.series[key], count+3)
			require.Equal(t, adaptiveStreamStatus(health.series[key], now), adaptiveStreamStatus(restarted.series[key], now))
			require.Equal(t, adaptiveStreamState{}, adaptiveStreamStatus(restarted.series[key], now.Add(adaptiveStreamWindow)))
		})
	}
}

func TestGatewayAdaptiveStreamHealthHistoryConcurrentRestoreAndDrain(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond)
	store := &adaptiveStreamHistoryFake{}
	telemetry := &adaptiveTelemetry{}
	c, _, _ := streamHealthFixture(72)
	ctx := streamHealthContext(context.Background(), telemetry, 72, streamHealthBody)
	key := streamHealthKey(t, ctx, &c)
	health := &telemetry.streamHealth
	runCtx := health.attach(store)
	store.readErr = errors.New("Redis offline")
	require.Error(t, health.history.restore(ctx, health, now))
	require.False(t, health.ready.Load())
	health.observe(key, now.Add(-2*time.Second), 39*time.Second)
	require.NoError(t, health.history.flush(ctx, health, now))

	store.readErr = nil
	store.readStarted, store.allowRead = make(chan struct{}), make(chan struct{})
	readDone := make(chan error, 1)
	go func() { readDone <- health.history.restore(ctx, health, now) }()
	<-store.readStarted
	health.observe(key, now.Add(-time.Second), 48*time.Second)
	require.False(t, health.ready.Load())
	close(store.allowRead)
	require.NoError(t, <-readDone)
	require.True(t, health.ready.Load())
	require.Len(t, health.series[key], 2)
	require.True(t, adaptiveStreamStatus(health.series[key], now).slow)

	go health.history.run(runCtx, health, make(chan time.Time))
	health.history.stopAccepting()
	stopCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	require.NoError(t, health.history.wait(stopCtx))
	require.Len(t, store.batches, 2)
	require.Len(t, store.rows, 2)
}

func TestGatewayAdaptiveStreamHealthHistoryCapacity(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond)
	health := &adaptiveStreamHealth{}
	historyCtx := health.attach(&adaptiveStreamHistoryFake{})
	defer health.history.stopAccepting()
	key := adaptiveStreamKey{wire: APIProtocolChatCompletions, work: adaptiveWorkText}
	for i := range adaptiveHistoryQueue + 1 {
		key.sample.accountID = int64(i + 1)
		health.observe(key, now.Add(time.Duration(i-adaptiveHistoryQueue)*time.Microsecond), time.Second)
	}
	require.Len(t, health.series, adaptiveCapacity)
	require.Len(t, health.history.queue, adaptiveHistoryQueue)
	require.EqualValues(t, 1, health.counters[streamQueueFull].Load())
	require.EqualValues(t, 1, health.counters[streamCapacity].Load())
	require.NoError(t, health.history.flush(historyCtx, health, now))
	require.Len(t, health.history.queue, adaptiveHistoryQueue-adaptiveHistoryBatch)

	for i := range 11 {
		health.observe(key, now.Add(time.Duration(i)*time.Microsecond), 30*time.Second)
	}
	require.Len(t, health.series[key], 10)
}
