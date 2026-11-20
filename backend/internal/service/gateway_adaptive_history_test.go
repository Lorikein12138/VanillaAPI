package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type adaptiveHistoryTestStore struct {
	read  func(context.Context, time.Time, time.Time) ([]AdaptiveHistoryRecord, error)
	merge func(context.Context, time.Time, []AdaptiveHistoryRecord) error
}

func (s adaptiveHistoryTestStore) Read(ctx context.Context, from, to time.Time) ([]AdaptiveHistoryRecord, error) {
	return s.read(ctx, from, to)
}

func (s adaptiveHistoryTestStore) Merge(ctx context.Context, cutoff time.Time, batch []AdaptiveHistoryRecord) error {
	return s.merge(ctx, cutoff, batch)
}

func TestGatewayAdaptiveHistoryRetriesAndRestoresWithoutCountingLiveOrHistoricalIDsTwice(t *testing.T) {
	now := time.Now()
	account := adaptiveTestAccount(244, PlatformAnthropic, 1)
	route := adaptiveRoute{
		operation: "test_messages", transport: "sse", inputSize: 3, effort: "high/8",
		duration: "8", tier: "ultrafast", thinkingLevel: "high", thinkingEnabled: true,
	}
	key := adaptiveKey(route, &account, "mapped-model")
	makeRecord := func(id string) AdaptiveHistoryRecord {
		payload, err := json.Marshal(adaptiveStoredSample{
			ID: id, AccountID: account.ID, Platform: account.Platform, AccountType: account.Type, Model: key.model,
			Operation: route.operation, Transport: route.transport, InputSize: route.inputSize, Effort: route.effort,
			Duration: route.duration, Tier: route.tier, ThinkingLevel: route.thinkingLevel, ThinkingEnabled: route.thinkingEnabled,
			Latency: time.Second, Success: true,
		})
		require.NoError(t, err)
		return AdaptiveHistoryRecord{At: now.Add(-3 * time.Hour), Payload: string(payload)}
	}
	oldA, oldB, oldC := makeRecord("previous/1"), makeRecord("previous/2"), makeRecord("previous/3")
	var live []AdaptiveHistoryRecord
	var reads int
	readCalls := make(chan int, 4)
	writes := make(chan int, 2)
	store := adaptiveHistoryTestStore{
		read: func(_ context.Context, _, _ time.Time) ([]AdaptiveHistoryRecord, error) {
			reads++
			readCalls <- reads
			if reads < 3 {
				return nil, errors.New("unavailable")
			}
			return append(live, oldA, oldB, oldB, oldC), nil
		},
		merge: func(_ context.Context, _ time.Time, batch []AdaptiveHistoryRecord) error {
			live = append([]AdaptiveHistoryRecord(nil), batch...)
			writes <- len(live)
			return nil
		},
	}
	telemetry := &adaptiveTelemetry{}
	ctx := telemetry.attachAdaptiveHistory(store)
	ticks := make(chan time.Time, 2)
	go telemetry.history.run(ctx, telemetry, ticks)
	settings := &SettingService{adaptiveTelemetry: telemetry}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = settings.StopGatewayAdaptiveHistory(ctx)
	})

	require.Eventually(t, func() bool { return len(readCalls) == 1 }, time.Second, time.Millisecond)
	telemetry.observe(key, now, time.Second, true)
	ticks <- now
	require.Eventually(t, func() bool { return len(writes) == 1 }, time.Second, time.Millisecond)
	require.Equal(t, 1, <-writes) // Samples continue while health restoration is unavailable.
	ticks <- now
	require.Eventually(t, func() bool {
		telemetry.mu.Lock()
		defer telemetry.mu.Unlock()
		return telemetry.score(key, now).known
	}, time.Second, time.Millisecond)
	require.NoError(t, settings.StopGatewayAdaptiveHistory(stopCtx))
	require.Len(t, readCalls, 3)
	require.Equal(t, 4, telemetry.series[key].next)
	require.Equal(t, oldA.At.UnixMicro(), telemetry.series[key].samples[0].at.UnixMicro())
	for _, field := range []string{"operation", "transport", "tier", "model"} {
		isolated := key
		switch field {
		case "operation":
			isolated.route.operation = "test_other_protocol"
		case "transport":
			isolated.route.transport = "http"
		case "tier":
			isolated.route.tier = "priority"
		case "model":
			isolated.model = "different-model"
		}
		require.False(t, telemetry.score(isolated, now).known, field)
	}
}

func TestGatewayAdaptiveHistoryUnavailableStillRanksAndReportsFinalFlushError(t *testing.T) {
	unavailable := errors.New("store unavailable")
	store := adaptiveHistoryTestStore{
		read:  func(context.Context, time.Time, time.Time) ([]AdaptiveHistoryRecord, error) { return nil, unavailable },
		merge: func(context.Context, time.Time, []AdaptiveHistoryRecord) error { return unavailable },
	}
	telemetry := newAdaptiveTelemetry(store)
	settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
	ctx := WithGatewayAdaptiveIngress(context.Background(), settings, "test_redis_down", "http")
	slow := adaptiveTestAccount(1, PlatformOpenAI, 1)
	fast := adaptiveTestAccount(2, PlatformOpenAI, 1)
	for range adaptiveMinimum {
		observeAdaptiveCompletion(ctx, &slow, "model", time.Now().Add(-2*time.Second), 200, true)
		observeAdaptiveCompletion(ctx, &fast, "model", time.Now().Add(-time.Second), 200, true)
	}
	pool := []*Account{&slow, &fast}
	require.True(t, rankAdaptivePool(ctx, pool, adaptiveAccount, func(*Account) string { return "model" }, adaptiveAnyPeer, false))
	require.Equal(t, &fast, pool[0])
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.ErrorIs(t, settings.StopGatewayAdaptiveHistory(stopCtx), unavailable)
	require.ErrorIs(t, settings.StopGatewayAdaptiveHistory(stopCtx), unavailable)
}

func TestGatewayAdaptiveHistoryRetriesPersistentReadFailureWithoutBlockingWrites(t *testing.T) {
	reads, writes := make(chan struct{}, 1), make(chan struct{}, 1)
	store := adaptiveHistoryTestStore{
		read: func(context.Context, time.Time, time.Time) ([]AdaptiveHistoryRecord, error) {
			reads <- struct{}{}
			return nil, errors.New("read unavailable")
		},
		merge: func(context.Context, time.Time, []AdaptiveHistoryRecord) error {
			writes <- struct{}{}
			return nil
		},
	}
	telemetry := &adaptiveTelemetry{}
	ctx := telemetry.attachAdaptiveHistory(store)
	ticks := make(chan time.Time, 1)
	go telemetry.history.run(ctx, telemetry, ticks)
	settings := &SettingService{adaptiveTelemetry: telemetry}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, settings.StopGatewayAdaptiveHistory(ctx))
	})
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("history worker did not process the tick")
		}
	}
	wait(reads)
	key := adaptiveSampleKey{route: adaptiveRoute{operation: "history_retry"}}
	for range 8 {
		now := time.Now()
		telemetry.observe(key, now, time.Second, true)
		ticks <- now
		wait(writes)
		wait(reads)
	}
}

func TestGatewayAdaptiveHistoryStopClosesAdmissionAndDrainsAcceptedSamples(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	draining := make(chan time.Time, 2)
	finish := make(chan struct{})
	defer close(finish)
	var written int
	store := adaptiveHistoryTestStore{
		read: func(ctx context.Context, _, _ time.Time) ([]AdaptiveHistoryRecord, error) {
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		},
		merge: func(ctx context.Context, _ time.Time, batch []AdaptiveHistoryRecord) error {
			deadline, _ := ctx.Deadline()
			draining <- deadline
			select {
			case <-finish:
				written += len(batch)
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}
	telemetry := newAdaptiveTelemetry(store)
	require.Eventually(t, func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	key := adaptiveSampleKey{route: adaptiveRoute{operation: "test"}}
	for range adaptiveHistoryBatch + 1 {
		telemetry.observe(key, time.Now(), time.Second, true)
	}
	settings := &SettingService{adaptiveTelemetry: telemetry}
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- settings.StopGatewayAdaptiveHistory(stopCtx) }()
	select {
	case deadline := <-draining:
		require.Greater(t, time.Until(deadline), time.Duration(0))
		require.LessOrEqual(t, time.Until(deadline), adaptiveHistoryTimeout)
	case <-stopCtx.Done():
		t.Fatal("final flush did not start")
	}
	select {
	case <-canceled:
	default:
		t.Fatal("startup restore was not canceled before the final flush")
	}
	telemetry.observe(key, time.Now(), time.Second, true)
	// Release both writes while keeping the deferred close safe on assertion failures.
	for range 2 {
		select {
		case finish <- struct{}{}:
		case <-stopCtx.Done():
			t.Fatal("final flush did not drain")
		}
	}
	require.NoError(t, <-stopped)
	require.Equal(t, adaptiveHistoryBatch+1, written)
	require.Empty(t, telemetry.history.queue)
}

func TestGatewayAdaptiveHistoryKeepsDegradationThroughInitialReadFailure(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond)
	key := adaptiveSampleKey{accountID: 34, groupID: 6, model: "wire", route: adaptiveRoute{operation: "POST /v1/messages", transport: "sse"}}
	other := key
	other.accountID++
	persisted := adaptiveHealth{Last: now.Add(-time.Minute), Fault: adaptiveUpstreamDegraded, RetryAt: now.Add(4 * time.Minute), Backoff: 1}
	original, ok := adaptiveHealthRecord(key, persisted)
	require.True(t, ok)
	wrapped := key
	wrapped.accountID += 2
	older, ok := adaptiveHealthRecord(wrapped, adaptiveHealth{Last: now.Add(-time.Minute)})
	require.True(t, ok)
	latest := map[string]AdaptiveHistoryRecord{original.StateKey: original, older.StateKey: older}
	samples := []AdaptiveHistoryRecord{}
	readAttempt := make(chan struct{}, 4)
	writeAttempt := make(chan []AdaptiveHistoryRecord, 4)
	allowRead := make(chan struct{})
	store := adaptiveHistoryTestStore{
		read: func(context.Context, time.Time, time.Time) ([]AdaptiveHistoryRecord, error) {
			readAttempt <- struct{}{}
			select {
			case <-allowRead:
				rows := append([]AdaptiveHistoryRecord(nil), samples...)
				for _, state := range latest {
					rows = append(rows, state)
				}
				return rows, nil
			default:
				return nil, errors.New("short outage")
			}
		},
		merge: func(_ context.Context, _ time.Time, batch []AdaptiveHistoryRecord) error {
			for _, row := range batch {
				if row.StateKey == "" {
					samples = append(samples, row)
					continue
				}
				if row.At.After(latest[row.StateKey].At) {
					latest[row.StateKey] = row
				}
			}
			writeAttempt <- append([]AdaptiveHistoryRecord(nil), batch...)
			return nil
		},
	}
	telemetry := &adaptiveTelemetry{}
	ctx := telemetry.attachAdaptiveHistory(store)
	ticks := make(chan time.Time, 1)
	go telemetry.history.run(ctx, telemetry, ticks)
	settings := &SettingService{adaptiveTelemetry: telemetry}
	stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
		defer cleanupCancel()
		require.NoError(t, settings.StopGatewayAdaptiveHistory(cleanupCtx))
	})
	wait := func() {
		t.Helper()
		select {
		case <-readAttempt:
		case <-stop.Done():
			t.Fatal("worker did not attempt Read")
		}
	}
	written := func() []AdaptiveHistoryRecord {
		t.Helper()
		select {
		case batch := <-writeAttempt:
			return batch
		case <-stop.Done():
			t.Fatal("worker did not write")
			return nil
		}
	}
	wait()
	telemetry.observe(key, now, 7*time.Second, true)
	for i := range 3 {
		telemetry.observe(other, now.Add(time.Duration(i)*time.Microsecond), 0, false)
		telemetry.observe(wrapped, now.Add(time.Duration(i)*time.Microsecond), 0, false)
	}
	for i := range adaptiveSamples {
		telemetry.observe(wrapped, now.Add(time.Duration(3+i)*time.Microsecond), 7*time.Second, true)
	}
	ticks <- now
	batch := written()
	require.Len(t, batch, 7+adaptiveSamples)
	for _, row := range batch {
		require.Empty(t, row.StateKey)
	}
	wait()
	close(allowRead)
	ticks <- now
	wait()
	ticks <- now
	_ = written()
	require.NoError(t, settings.StopGatewayAdaptiveHistory(stop))
	state := latest[original.StateKey]
	require.True(t, state.At.Equal(now))
	var restored adaptiveStoredSample
	require.NoError(t, json.Unmarshal([]byte(state.Payload), &restored))
	require.Equal(t, adaptiveUpstreamDegraded, restored.Health.Fault)
	require.True(t, persisted.RetryAt.Equal(restored.Health.RetryAt))
	require.Zero(t, restored.Health.Passed)
	require.Len(t, samples, 7+adaptiveSamples)
	var wrappedState adaptiveStoredSample
	require.NoError(t, json.Unmarshal([]byte(latest[older.StateKey].Payload), &wrappedState))
	require.Equal(t, adaptiveUpstreamDegraded, wrappedState.Health.Fault)
	otherRecord, ok := adaptiveHealthRecord(other, telemetry.series[other].health)
	require.True(t, ok)
	require.Contains(t, latest, otherRecord.StateKey)
	require.Equal(t, adaptiveUpstreamDegraded, telemetry.series[other].health.Fault)
}
