//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func openDetectionTestDatabases(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("CHANNEL_DETECTION_TEST_DSN")
	if dsn == "" {
		t.Skip("set CHANNEL_DETECTION_TEST_DSN to an isolated PostgreSQL database")
	}
	base, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = base.Close() })
	schema := fmt.Sprintf("channel_detection_test_%d", time.Now().UnixNano())
	_, err = base.Exec(`CREATE SCHEMA ` + pq.QuoteIdentifier(schema))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := base.Exec(`DROP SCHEMA ` + pq.QuoteIdentifier(schema) + ` CASCADE`)
		require.NoError(t, err)
	})
	open := func() *sql.DB {
		db, err := sql.Open("postgres", dsn+" search_path="+schema)
		require.NoError(t, err)
		db.SetMaxOpenConns(2)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	return open(), open()
}

func TestChannelDetectionPostgresPlanCASPreservesConcurrentPublish(t *testing.T) {
	editorDB, workerDB := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := editorDB.ExecContext(ctx, `
		CREATE TABLE settings (key VARCHAR(100) PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
		INSERT INTO settings VALUES ('channel_detection_enabled','on',now());
	`)
	require.NoError(t, err)
	editor := &channelDetectionCoordinator{db: editorDB}
	worker := &channelDetectionCoordinator{db: workerDB}
	guard := service.ChannelDetectionSnapshotGuard{Feature: "on"}
	ok, err := editor.SavePlan(ctx, guard, "initial plan", "initial snapshot")
	require.NoError(t, err)
	require.True(t, ok, "first save does not require a worker lease")
	guard.Config, guard.Snapshot = "initial plan", "initial snapshot"
	ok, err = worker.TryAcquire(ctx, 1, "worker", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	attempted := guard
	ok, err = worker.CompareAndSetSnapshot(ctx, 1, "worker", guard, "worker finished")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = editor.SavePlan(ctx, attempted, "next plan", "stale retained snapshot")
	require.NoError(t, err)
	require.False(t, ok)
	state := func() (string, string) {
		t.Helper()
		var config, snapshot string
		require.NoError(t, editorDB.QueryRowContext(ctx, `
			SELECT a.value,b.value FROM settings a,settings b WHERE a.key=$1 AND b.key=$2`,
			service.SettingKeyChannelDetectionConfig, service.SettingKeyChannelDetectionLatest).Scan(&config, &snapshot))
		return config, snapshot
	}
	config, snapshot := state()
	require.Equal(t, "initial plan", config)
	require.Equal(t, "worker finished", snapshot)
	guard.Snapshot = snapshot

	_, err = editorDB.ExecContext(ctx, `ALTER TABLE settings ADD CHECK (value <> 'bad snapshot')`)
	require.NoError(t, err)
	ok, err = editor.SavePlan(ctx, guard, "next plan", "bad snapshot")
	require.Error(t, err)
	require.False(t, ok)
	config, snapshot = state()
	require.Equal(t, "initial plan", config)
	require.Equal(t, "worker finished", snapshot)

	ok, err = editor.SavePlan(ctx, guard, "next plan", "worker finished retained")
	require.NoError(t, err)
	require.True(t, ok)
	config, snapshot = state()
	require.Equal(t, "next plan", config)
	require.Equal(t, "worker finished retained", snapshot)
	ok, err = worker.CompareAndSetSnapshot(ctx, 1, "worker", guard, "old worker must not win")
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = editor.SavePlan(ctx, guard, "stale competing edit", "stale")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestChannelDetectionPostgresFencesExpiredWriter(t *testing.T) {
	firstDB, secondDB := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := firstDB.ExecContext(ctx, `
		CREATE TABLE settings (
			id BIGSERIAL PRIMARY KEY, key VARCHAR(100) UNIQUE NOT NULL,
			value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	require.NoError(t, err)
	guard := service.ChannelDetectionSnapshotGuard{Feature: "enable-1", Config: "config-1"}
	_, err = firstDB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2), ($3, $4)`,
		service.SettingKeyChannelDetectionFeature, guard.Feature, service.SettingKeyChannelDetectionConfig, guard.Config)
	require.NoError(t, err)

	first := &channelDetectionCoordinator{db: firstDB}
	second := &channelDetectionCoordinator{db: secondDB}
	acquired, err := first.TryAcquire(ctx, 1, "instance-A", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, err = second.TryAcquire(ctx, 1, "instance-B", time.Minute)
	require.NoError(t, err)
	require.False(t, acquired)

	reservationA := `{"attempted_at":{"1":"2026-11-27T00:00:00Z"}}`
	wrote, err := first.CompareAndSetSnapshot(ctx, 1, "instance-A", guard, reservationA)
	require.NoError(t, err)
	require.True(t, wrote)
	guard.Snapshot = reservationA
	held, err := first.Holds(ctx, 1, "instance-A")
	require.NoError(t, err)
	require.True(t, held, "A has already checked its lease")

	_, err = secondDB.ExecContext(ctx, `UPDATE settings SET value = jsonb_set(value::jsonb, '{groups,1,expires_at}', to_jsonb(clock_timestamp() - interval '1 second'))::text, updated_at = clock_timestamp() - interval '1 second' WHERE key = $1`, channelDetectionLeaseSetting)
	require.NoError(t, err)
	wrote, err = first.CompareAndSetSnapshot(ctx, 1, "instance-A", guard, "expired A")
	require.NoError(t, err)
	require.False(t, wrote)
	acquired, err = second.TryAcquire(ctx, 1, "instance-B", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	reservationB := `{"attempted_at":{"1":"2026-11-27T00:00:00Z","2":"2026-11-27T00:01:00Z"}}`
	wrote, err = second.CompareAndSetSnapshot(ctx, 1, "instance-B", guard, reservationB)
	require.NoError(t, err)
	require.True(t, wrote)

	wrote, err = first.PublishSnapshot(ctx, "instance-A", guard, `{"proofs":{"1":{}}}`, 1)
	require.NoError(t, err)
	require.False(t, wrote)
	reserved, err := first.ReserveSend(ctx, "instance-A", guard, 1, 0, 672)
	require.NoError(t, err)
	require.False(t, reserved)
	require.NoError(t, first.Release(ctx, 1, "instance-A"))
	held, err = second.Holds(ctx, 1, "instance-B")
	require.NoError(t, err)
	require.True(t, held)
	wrote, err = second.CompareAndSetSnapshot(ctx, 1, "instance-B", guard, "stale snapshot")
	require.NoError(t, err)
	require.False(t, wrote)
	guard.Snapshot = reservationB
	_, err = firstDB.ExecContext(ctx, `UPDATE settings SET value = 'config-2' WHERE key = $1`, service.SettingKeyChannelDetectionConfig)
	require.NoError(t, err)
	wrote, err = second.CompareAndSetSnapshot(ctx, 1, "instance-B", guard, "stale config")
	require.NoError(t, err)
	require.False(t, wrote)
	guard.Config = "config-2"
	_, err = firstDB.ExecContext(ctx, `UPDATE settings SET value = '' WHERE key = $1`, service.SettingKeyChannelDetectionFeature)
	require.NoError(t, err)
	wrote, err = second.CompareAndSetSnapshot(ctx, 1, "instance-B", guard, "feature disabled")
	require.NoError(t, err)
	require.False(t, wrote)
	var final string
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, service.SettingKeyChannelDetectionLatest).Scan(&final))
	require.Equal(t, reservationB, final)
	require.NoError(t, second.Release(ctx, 1, "instance-B"))
}

func TestChannelDetectionPostgresSendBudgetSurvivesLeaseAndConfig(t *testing.T) {
	firstDB, secondDB := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := firstDB.ExecContext(ctx, `
		CREATE TABLE settings (key VARCHAR(100) PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW());
		INSERT INTO settings (key, value) VALUES ('channel_detection_enabled','on'), ('channel_detection_config','old-config');
	`)
	require.NoError(t, err)
	first := &channelDetectionCoordinator{db: firstDB}
	second := &channelDetectionCoordinator{db: secondDB}
	old := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: "old-config"}
	reserve := func(coordinator service.ChannelDetectionCoordinator, owner string, guard service.ChannelDetectionSnapshotGuard, want bool) {
		t.Helper()
		ok, err := coordinator.ReserveSend(ctx, owner, guard, 1, 0, 2)
		require.NoError(t, err)
		require.Equal(t, want, ok)
	}
	acquire := func(coordinator service.ChannelDetectionCoordinator, owner string) {
		t.Helper()
		ok, err := coordinator.TryAcquire(ctx, 1, owner, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
	}
	acquire(first, "worker-one")
	reserve(second, "worker-two", old, false)
	reserve(first, "worker-one", old, true)
	require.NoError(t, first.Release(ctx, 1, "worker-one"))
	_, err = secondDB.ExecContext(ctx, `UPDATE settings SET value='new-config' WHERE key='channel_detection_config'`)
	require.NoError(t, err)
	next := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: "new-config"}
	acquire(second, "worker-two")
	reserve(first, "worker-one", old, false)
	reserve(second, "worker-two", old, false)
	reserve(second, "worker-two", next, true)
	reserve(second, "worker-two", next, false)
	_, err = secondDB.ExecContext(ctx, `UPDATE settings SET updated_at = updated_at - interval '1 day' WHERE key='channel_detection_daily_sends'`)
	require.NoError(t, err)
	reserve(second, "worker-two", next, true)
	_, err = secondDB.ExecContext(ctx, `UPDATE settings SET value = jsonb_set(value::jsonb, '{groups,1,expires_at}', to_jsonb(clock_timestamp() - interval '1 second'))::text, updated_at = clock_timestamp() - interval '1 second' WHERE key = 'channel_detection_lease'`)
	require.NoError(t, err)
	reserve(second, "worker-two", next, false)
}

func TestChannelDetectionPostgresGroupRPMSharedAcrossLeases(t *testing.T) {
	firstDB, secondDB := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := firstDB.ExecContext(ctx, `
		CREATE TABLE settings (key VARCHAR(100) PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW());
		INSERT INTO settings (key, value) VALUES ('channel_detection_enabled','on'), ('channel_detection_config','old-config');
	`)
	require.NoError(t, err)
	guard := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: "old-config"}
	first := &channelDetectionCoordinator{db: firstDB}
	second := &channelDetectionCoordinator{db: secondDB}
	acquired, err := first.TryAcquire(ctx, 7, "first", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	ok, err := first.ReserveSend(ctx, "first", guard, 7, 0, 3)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, first.Release(ctx, 7, "first"))

	_, err = secondDB.ExecContext(ctx, `UPDATE settings SET value='new-config' WHERE key='channel_detection_config'`)
	require.NoError(t, err)
	guard.Config = "new-config"
	acquired, err = second.TryAcquire(ctx, 7, "second", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	ok, err = second.ReserveSend(ctx, "second", guard, 7, 1, 3)
	require.NoError(t, err)
	require.False(t, ok, "the previous lease's send counts after enabling the limit")
	acquired, err = first.TryAcquire(ctx, 8, "third", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	ok, err = first.ReserveSend(ctx, "third", guard, 8, 1, 3)
	require.NoError(t, err)
	require.True(t, ok, "a different group has its own window")
	_, err = secondDB.ExecContext(ctx, `UPDATE settings SET value = jsonb_build_array(clock_timestamp() - interval '61 seconds')::text WHERE key='channel_detection_group_rpm:7'`)
	require.NoError(t, err)
	ok, err = second.ReserveSend(ctx, "second", guard, 7, 1, 3)
	require.NoError(t, err)
	require.True(t, ok, "denial rolled back its daily debit")
	ok, err = first.ReserveSend(ctx, "third", guard, 8, 0, 3)
	require.NoError(t, err)
	require.False(t, ok, "three successful reservations exhausted the daily budget")
}

func TestChannelDetectionPostgresQuotaCrossingsPublishOutbox(t *testing.T) {
	db, observer := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `
		CREATE TABLE accounts (id BIGINT PRIMARY KEY, extra JSONB, updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), deleted_at TIMESTAMPTZ);
		CREATE TABLE scheduler_outbox (
			id BIGSERIAL PRIMARY KEY, event_type TEXT NOT NULL, account_id BIGINT,
			group_id BIGINT, payload JSONB, dedup_key TEXT, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE UNIQUE INDEX scheduler_outbox_pending_dedup_key ON scheduler_outbox (dedup_key) WHERE dedup_key IS NOT NULL;
		INSERT INTO accounts (id, extra) VALUES
			(1, '{"quota_limit":2}'),
			(2, '{"quota_daily_limit":2}'),
			(3, '{"quota_weekly_limit":2}');
	`)
	require.NoError(t, err)
	repo := &accountRepository{sql: db}
	for id, dimension := range []string{"total", "daily", "weekly"} {
		t.Run(dimension, func(t *testing.T) {
			accountID := int64(id + 1)
			count := func() int {
				t.Helper()
				var outbox int
				require.NoError(t, observer.QueryRowContext(ctx,
					`SELECT count(*) FROM scheduler_outbox WHERE account_id = $1 AND event_type = $2`,
					accountID, service.SchedulerOutboxEventAccountChanged).Scan(&outbox))
				return outbox
			}
			require.NoError(t, repo.IncrementDetectionQuotaUsed(ctx, accountID, 1))
			require.Zero(t, count())
			require.NoError(t, repo.IncrementDetectionQuotaUsed(ctx, accountID, 1))
			require.Equal(t, 1, count())
			// Simulate the consumer releasing the pending dedup key.
			_, err := db.ExecContext(ctx, `UPDATE scheduler_outbox SET dedup_key = NULL WHERE account_id = $1`, accountID)
			require.NoError(t, err)
			require.NoError(t, repo.IncrementDetectionQuotaUsed(ctx, accountID, 1))
			require.Equal(t, 1, count(), "staying over the quota does not trigger another crossing")
		})
	}
	// Outbox write failure must roll back the account debit.
	_, err = db.ExecContext(ctx, `INSERT INTO accounts (id, extra) VALUES (4, '{"quota_daily_limit":1}'); DROP TABLE scheduler_outbox;`)
	require.NoError(t, err)
	require.Error(t, repo.IncrementDetectionQuotaUsed(ctx, 4, 1))
	var used float64
	require.NoError(t, observer.QueryRowContext(ctx, `SELECT COALESCE((extra->>'quota_daily_used')::numeric, 0) FROM accounts WHERE id=4`).Scan(&used))
	require.Zero(t, used)
}
