//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

type postgresDetectionSettings struct {
	service.SettingRepository
	db *sql.DB
}

func (s postgresDetectionSettings) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key,value FROM settings WHERE key = ANY($1)", pq.Array(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]string, len(keys))
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

type postgresDetectionGroups struct{ service.AdminService }

func (postgresDetectionGroups) GetGroup(_ context.Context, id int64) (*service.Group, error) {
	return &service.Group{ID: id, Name: fmt.Sprintf("Group %d", id), Status: service.StatusActive,
		ModelAllowlist: service.GroupModelAllowlist{Models: []string{"fixture", "replacement"}}}, nil
}

type detectionPublishBarrier struct {
	armed     atomic.Bool
	arrivals  atomic.Int32
	conflicts atomic.Int32
	ready     chan struct{}
}

type postgresDetectionCoordinator struct {
	*channelDetectionCoordinator
	barrier *detectionPublishBarrier
}

func (c postgresDetectionCoordinator) PublishSnapshot(ctx context.Context, owner string, guard service.ChannelDetectionSnapshotGuard, next string, id int64) (bool, error) {
	if c.barrier.armed.Load() {
		switch n := c.barrier.arrivals.Add(1); {
		case n == 1:
			select {
			case <-c.barrier.ready:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		case n == 2:
			close(c.barrier.ready)
		}
	}
	ok, err := c.channelDetectionCoordinator.PublishSnapshot(ctx, owner, guard, next, id)
	if !ok && err == nil {
		c.barrier.conflicts.Add(1)
	}
	return ok, err
}

const postgresDetectionSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 480 360"><circle cx="2" cy="2" r="1"/></svg>`

func TestChannelDetectionPostgresLegacyConfigAndPolicy(t *testing.T) {
	first, second := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := first.ExecContext(ctx, `
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
		INSERT INTO settings (key,value) VALUES ('channel_detection_enabled','on'),
			('channel_detection_config','{"enabled":true,"interval_minutes":60,"concurrent_groups":"obsolete","groups":[{"group_id":1,"model":"fixture"}],"revision":"legacy"}');
	`)
	require.NoError(t, err)
	worker := func(db *sql.DB) *service.ChannelDetection {
		return service.NewChannelDetection(postgresDetectionSettings{db: db}, postgresDetectionGroups{}, nil, &channelDetectionCoordinator{db: db})
	}
	one, two := worker(first), worker(second)
	config, err := two.Config(ctx)
	require.NoError(t, err)
	require.Equal(t, 1440, *config.SuspendMinutes)
	require.False(t, *config.AutoConfirmMissingSVG)
	require.Empty(t, *config.DailyAnchorTime)
	config.SuspendMinutes = new(35)
	config.AutoConfirmMissingSVG = new(true)
	config.DailyAnchorTime = new("08:00")
	require.NoError(t, one.Save(ctx, config))
	var stored string
	require.NoError(t, first.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='channel_detection_config'").Scan(&stored))
	require.NotContains(t, stored, "concurrent_groups")
	require.NoError(t, two.Save(ctx, service.ChannelDetectionConfig{Enabled: true, IntervalMinutes: 61, Groups: config.Groups}))
	saved, err := one.Config(ctx)
	require.NoError(t, err)
	require.Equal(t, 35, *saved.SuspendMinutes)
	require.True(t, *saved.AutoConfirmMissingSVG)
	require.Equal(t, "08:00", *saved.DailyAnchorTime)
	saved.AutoConfirmMissingSVG = new(false)
	saved.DailyAnchorTime = new("")
	require.NoError(t, one.Save(ctx, saved))
	saved, err = two.Config(ctx)
	require.NoError(t, err)
	require.False(t, *saved.AutoConfirmMissingSVG)
	require.Empty(t, *saved.DailyAnchorTime)
}

func TestChannelDetectionPostgresWorkersShareSlotsAndMergeResults(t *testing.T) {
	first, second := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := first.ExecContext(ctx, `
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
		CREATE TABLE groups (id BIGINT PRIMARY KEY, deleted_at TIMESTAMPTZ);
		INSERT INTO groups (id) SELECT generate_series(1, 13);
		INSERT INTO settings (key,value) VALUES ('channel_detection_enabled','on');
	`)
	require.NoError(t, err)

	barrier := &detectionPublishBarrier{ready: make(chan struct{})}
	coordinator := func(db *sql.DB) postgresDetectionCoordinator {
		return postgresDetectionCoordinator{&channelDetectionCoordinator{db: db}, barrier}
	}
	one, two := coordinator(first), coordinator(second)
	workers := []*service.ChannelDetection{
		service.NewChannelDetection(postgresDetectionSettings{db: first}, postgresDetectionGroups{}, nil, one),
		service.NewChannelDetection(postgresDetectionSettings{db: second}, postgresDetectionGroups{}, nil, two),
	}
	plan := service.ChannelDetectionConfig{Enabled: true, IntervalMinutes: 60}
	started := make(chan int64, 16)
	finish := make(map[int64]chan struct{})
	for id := int64(1); id <= 13; id++ {
		plan.Groups = append(plan.Groups, service.ChannelDetectionSelection{GroupID: id, Model: "fixture"})
		finish[id] = make(chan struct{})
	}
	require.NoError(t, workers[0].Save(ctx, plan))
	// Keep the old example visible while the first round is in flight.
	_, err = first.ExecContext(ctx, `
		UPDATE settings SET value = (value::jsonb || jsonb_build_object('samples', $1::jsonb, 'attempted_at', jsonb_build_object('13', clock_timestamp())))::text
		WHERE key = 'channel_detection_latest'`, fmt.Sprintf(`{"1":{"pelican_status":"generated","svg":%q},"2":{"pelican_status":"generated","svg":%q}}`, postgresDetectionSVG, postgresDetectionSVG))
	require.NoError(t, err)

	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := service.ChannelDetectionIngress(r)
		if !ok {
			http.Error(w, "invalid fixture", http.StatusBadRequest)
			return
		}
		id := *key.GroupID
		started <- id
		select {
		case <-finish[id]:
		case <-ctx.Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "completed", "output": []any{map[string]any{
				"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": postgresDetectionSVG}},
			}},
		})
	})
	for _, worker := range workers {
		worker.Start(gateway)
	}
	defer func() {
		cancel()
		stop, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		for _, worker := range workers {
			require.NoError(t, worker.Stop(stop))
		}
	}()
	nextStart := func() int64 {
		t.Helper()
		select {
		case id := <-started:
			return id
		case <-ctx.Done():
			t.Fatal("expected an additional group to start")
			return 0
		}
	}
	seen := make(map[int64]bool)
	for range 12 {
		id := nextStart()
		require.False(t, seen[id])
		require.NotEqualValues(t, 13, id)
		seen[id] = true
	}
	active, err := one.ActiveLeases(ctx)
	require.NoError(t, err)
	require.Len(t, active.Groups, 12)
	_, err = workers[1].Trigger(ctx, 1)
	require.ErrorIs(t, err, service.ErrDetectionBusy)
	cards, err := workers[1].AdminGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, "running", cards[0].TriggerState)
	require.Equal(t, "idle", cards[12].TriggerState)
	require.Empty(t, cards[0].SampleID)
	require.NotEmpty(t, cards[0].Sample.SVG, "the previous SVG remains visible while running")

	// Both completions arrive at the SQL CAS with the same original snapshot.
	barrier.armed.Store(true)
	close(finish[5])
	close(finish[3])
	require.Eventually(t, func() bool {
		cards, err := workers[0].AdminGroups(ctx)
		return err == nil && cards[2].SampleID != "" && cards[4].SampleID != ""
	}, 5*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, barrier.conflicts.Load())
	barrier.armed.Store(false)
	for id := int64(4); id <= 12; id++ {
		if id != 5 {
			close(finish[id])
		}
	}
	require.Eventually(t, func() bool {
		active, err := one.ActiveLeases(ctx)
		return err == nil && len(active.Groups) == 2
	}, 5*time.Second, 10*time.Millisecond)

	// Save while the two remaining gateway requests are still in flight.
	plan.Groups[1].Model = "replacement"
	plan.Enabled = false
	require.NoError(t, workers[1].Save(ctx, plan))
	close(finish[2])
	close(finish[1])
	require.Eventually(t, func() bool {
		active, err := one.ActiveLeases(ctx)
		return err == nil && len(active.Groups) == 0
	}, 5*time.Second, 10*time.Millisecond)
	for _, worker := range workers {
		require.NoError(t, worker.Stop(ctx))
	}
	plan.Enabled = true
	require.NoError(t, workers[1].Save(ctx, plan))
	cards, err = workers[0].AdminGroups(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, cards[0].Sample.SVG)
	require.Empty(t, cards[0].SampleID, "the superseded group 1 must not publish")
	require.Nil(t, cards[1].Sample, "old samples for the changed model must be dropped")
	for _, card := range cards[2:12] {
		require.Equal(t, "generated", card.Sample.Pelican)
		require.NotEmpty(t, card.SampleID)
	}
}

func TestChannelDetectionPostgresLeaseInteropAndConcurrentBudget(t *testing.T) {
	first, second := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := first.ExecContext(ctx, `
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
		INSERT INTO settings (key,value) VALUES ('channel_detection_enabled','on'), ('channel_detection_config','{"revision":"config"}'), ('channel_detection_daily_sends','670');
	`)
	require.NoError(t, err)
	one, two := &channelDetectionCoordinator{db: first}, &channelDetectionCoordinator{db: second}
	oldOwner := uuid.NewString()
	oldAcquire := func() bool {
		t.Helper()
		result, err := second.ExecContext(ctx, `
			INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,clock_timestamp() + interval '1 minute')
			ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value,updated_at=EXCLUDED.updated_at
			WHERE settings.value = '' OR settings.updated_at <= clock_timestamp()
		`, channelDetectionLeaseSetting, oldOwner)
		require.NoError(t, err)
		n, err := result.RowsAffected()
		require.NoError(t, err)
		return n == 1
	}
	require.True(t, oldAcquire())
	ok, err := one.TryAcquire(ctx, 1, "one", time.Minute)
	require.NoError(t, err)
	require.False(t, ok)
	_, err = first.ExecContext(ctx, `UPDATE settings SET updated_at = clock_timestamp() - interval '1 second' WHERE key=$1`, channelDetectionLeaseSetting)
	require.NoError(t, err)
	ok, err = one.TryAcquire(ctx, 1, "one", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = two.TryAcquire(ctx, 2, "two", 2*time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.False(t, oldAcquire())

	var finiteMax bool
	require.NoError(t, second.QueryRowContext(ctx, `
		SELECT isfinite(updated_at) AND
			updated_at = (SELECT max((slot->>'expires_at')::timestamptz)
				FROM jsonb_each(settings.value::jsonb->'groups') AS slots(group_id,slot))
		FROM settings WHERE key=$1`, channelDetectionLeaseSetting).Scan(&finiteMax))
	require.True(t, finiteMax)
	guard := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: `{"revision":"config"}`}

	var accepted atomic.Int32
	var wait sync.WaitGroup
	for i := range 12 {
		wait.Go(func() {
			id, owner, coord := int64(1), "one", one
			if i%2 != 0 {
				id, owner, coord = 2, "two", two
			}
			ok, err := coord.ReserveSend(ctx, owner, guard, id, 1, 672)
			if err != nil {
				t.Errorf("reserve send: %v", err)
			} else if ok {
				accepted.Add(1)
			}
		})
	}
	wait.Wait()
	require.EqualValues(t, 2, accepted.Load())
	var day, groups int
	require.NoError(t, first.QueryRowContext(ctx, `
		SELECT (SELECT value::int FROM settings WHERE key='channel_detection_daily_sends'),
			sum(jsonb_array_length(value::jsonb)) FROM settings WHERE key LIKE 'channel_detection_group_rpm:%'
	`).Scan(&day, &groups))
	require.Equal(t, 672, day)
	require.Equal(t, 2, groups)
	require.NoError(t, two.Release(ctx, 2, "two"))
	require.False(t, oldAcquire())
	require.NoError(t, one.Release(ctx, 1, "one"))
	var raw string
	require.NoError(t, first.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=$1", channelDetectionLeaseSetting).Scan(&raw))
	require.Empty(t, raw)
	require.True(t, oldAcquire(), "an old deployment can resume after the last v2 release")

	// Model an interrupted v2 process that never released its final slot.
	_, err = first.ExecContext(ctx, `UPDATE settings SET value = '', updated_at = clock_timestamp() WHERE key=$1`, channelDetectionLeaseSetting)
	require.NoError(t, err)
	ok, err = one.TryAcquire(ctx, 1, "abandoned", time.Millisecond)
	require.NoError(t, err)
	require.True(t, ok)
	require.Eventually(t, oldAcquire, time.Second, 10*time.Millisecond, "the v2 deadline bounds rollback lockout even without Release")

	_, err = first.ExecContext(ctx, `UPDATE settings SET value='{"v":3}', updated_at=clock_timestamp() - interval '1 second' WHERE key=$1`, channelDetectionLeaseSetting)
	require.NoError(t, err)
	ok, err = one.TryAcquire(ctx, 1, "one", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "unknown formats must never be overwritten by v2")
}

func TestChannelDetectionPostgresFixedSlotAcrossWorkersAndRestart(t *testing.T) {
	first, second := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_, err := first.ExecContext(ctx, `
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now());
		CREATE TABLE groups (id BIGINT PRIMARY KEY, deleted_at TIMESTAMPTZ);
		INSERT INTO groups (id) VALUES (1), (2);
		INSERT INTO settings (key,value,updated_at) VALUES ('channel_detection_enabled','on',clock_timestamp()-interval '3 days');
	`)
	require.NoError(t, err)
	newWorker := func(db *sql.DB) *service.ChannelDetection {
		return service.NewChannelDetection(postgresDetectionSettings{db: db}, postgresDetectionGroups{}, nil, &channelDetectionCoordinator{db: db})
	}
	var now time.Time
	require.NoError(t, first.QueryRowContext(ctx, "SELECT clock_timestamp()").Scan(&now))
	anchor := now.In(time.FixedZone("UTC+8", 8*60*60)).Truncate(time.Minute).Add(12 * time.Hour)
	plan := service.ChannelDetectionConfig{Enabled: true, IntervalMinutes: 1440, DailyAnchorTime: new(anchor.Format("15:04")),
		Groups: []service.ChannelDetectionSelection{{GroupID: 1, Model: "fixture"}, {GroupID: 2, Model: "fixture"}}}
	workers := []*service.ChannelDetection{newWorker(first), newWorker(second)}
	require.NoError(t, workers[0].Save(ctx, plan))
	var activation time.Time
	require.NoError(t, first.QueryRowContext(ctx, `
		SELECT (value::jsonb #>> '{schedule_since,1}')::timestamptz FROM settings WHERE key='channel_detection_latest'
	`).Scan(&activation))
	require.WithinDuration(t, now, activation, time.Second)
	slot := anchor.Add(-24 * time.Hour).UTC()
	_, err = first.ExecContext(ctx, `
		UPDATE settings SET value = (value::jsonb || jsonb_build_object(
			'last_slot', jsonb_build_object('1', $1::timestamptz),
			'schedule_since', jsonb_build_object('1', clock_timestamp()-interval '3 days', '2', clock_timestamp()-interval '3 days'),
			'attempted_at', jsonb_build_object('2', clock_timestamp()-interval '1 day')
		))::text WHERE key='channel_detection_latest'
	`, slot)
	require.NoError(t, err)
	started := make(chan int64, 8)
	release := make(chan struct{})
	gateway := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key, ok := service.ChannelDetectionIngress(req)
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		started <- *key.GroupID
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, `{"status":"completed","output":[]}`)
	})
	defer func() {
		for _, worker := range workers {
			require.NoError(t, worker.Stop(ctx))
		}
	}()
	start := func() {
		for _, worker := range workers {
			worker.Start(gateway)
		}
	}
	nextRun := func() int64 {
		t.Helper()
		select {
		case id := <-started:
			return id
		case <-ctx.Done():
			t.Fatal("expected a detection run")
			return 0
		}
	}
	readSlot := func() time.Time {
		t.Helper()
		var stored time.Time
		require.NoError(t, first.QueryRowContext(ctx, `
			SELECT (value::jsonb #>> '{last_slot,2}')::timestamptz FROM settings WHERE key='channel_detection_latest'
		`).Scan(&stored))
		return stored
	}
	start()
	require.EqualValues(t, 2, nextRun(), "the already consumed group sorts first")
	_, err = workers[1].Trigger(ctx, 2)
	require.ErrorIs(t, err, service.ErrDetectionBusy)
	require.True(t, slot.Equal(readSlot()))
	close(release)
	require.Eventually(t, func() bool {
		cards, err := workers[0].AdminGroups(ctx)
		return err == nil && cards[1].SampleID != "" && cards[1].TriggerState != "running"
	}, 3*time.Second, 10*time.Millisecond)
	plan.SuspendMinutes = new(50)
	require.NoError(t, workers[0].Save(ctx, plan))
	_, err = workers[1].Trigger(ctx, 2)
	require.NoError(t, err)
	require.EqualValues(t, 2, nextRun(), "manual runs stay available after consuming the automatic slot")
	require.True(t, slot.Equal(readSlot()))
	for i, worker := range workers {
		require.NoError(t, worker.Stop(ctx))
		workers[i] = newWorker([]*sql.DB{first, second}[i])
	}
	start()
	select {
	case id := <-started:
		t.Fatalf("group %d replayed a consumed slot", id)
	case <-time.After(300 * time.Millisecond):
	}
	require.True(t, slot.Equal(readSlot()))
}
