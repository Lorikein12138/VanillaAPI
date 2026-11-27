//go:build unit

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
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

	first := NewChannelDetectionCoordinator(firstDB)
	second := NewChannelDetectionCoordinator(secondDB)
	acquired, err := first.TryAcquire(ctx, "instance-A", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	acquired, err = second.TryAcquire(ctx, "instance-B", time.Minute)
	require.NoError(t, err)
	require.False(t, acquired)

	reservationA := `{"attempted_at":{"1":"2026-11-27T00:00:00Z"}}`
	wrote, err := first.CompareAndSetSnapshot(ctx, "instance-A", guard, reservationA)
	require.NoError(t, err)
	require.True(t, wrote)
	guard.Snapshot = reservationA
	held, err := first.Holds(ctx, "instance-A")
	require.NoError(t, err)
	require.True(t, held, "A has already checked its lease")

	_, err = secondDB.ExecContext(ctx, `UPDATE settings SET updated_at = clock_timestamp() - interval '1 second' WHERE key = $1`, channelDetectionLeaseSetting)
	require.NoError(t, err)
	wrote, err = first.CompareAndSetSnapshot(ctx, "instance-A", guard, "expired A")
	require.NoError(t, err)
	require.False(t, wrote)
	acquired, err = second.TryAcquire(ctx, "instance-B", time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)
	reservationB := `{"attempted_at":{"1":"2026-11-27T00:00:00Z","2":"2026-11-27T00:01:00Z"}}`
	wrote, err = second.CompareAndSetSnapshot(ctx, "instance-B", guard, reservationB)
	require.NoError(t, err)
	require.True(t, wrote)

	wrote, err = first.CompareAndSetSnapshot(ctx, "instance-A", guard, "stale A")
	require.NoError(t, err)
	require.False(t, wrote)
	require.NoError(t, first.Release(ctx, "instance-A"))
	held, err = second.Holds(ctx, "instance-B")
	require.NoError(t, err)
	require.True(t, held)
	wrote, err = second.CompareAndSetSnapshot(ctx, "instance-B", guard, "stale snapshot")
	require.NoError(t, err)
	require.False(t, wrote)
	guard.Snapshot = reservationB
	_, err = firstDB.ExecContext(ctx, `UPDATE settings SET value = 'config-2' WHERE key = $1`, service.SettingKeyChannelDetectionConfig)
	require.NoError(t, err)
	wrote, err = second.CompareAndSetSnapshot(ctx, "instance-B", guard, "stale config")
	require.NoError(t, err)
	require.False(t, wrote)
	guard.Config = "config-2"
	_, err = firstDB.ExecContext(ctx, `UPDATE settings SET value = '' WHERE key = $1`, service.SettingKeyChannelDetectionFeature)
	require.NoError(t, err)
	wrote, err = second.CompareAndSetSnapshot(ctx, "instance-B", guard, "feature disabled")
	require.NoError(t, err)
	require.False(t, wrote)
	var final string
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1`, service.SettingKeyChannelDetectionLatest).Scan(&final))
	require.Equal(t, reservationB, final)
	require.NoError(t, second.Release(ctx, "instance-B"))
}

func TestChannelDetectionPostgresKeyMetadataProjection(t *testing.T) {
	db, _ := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := db.ExecContext(ctx, `
		CREATE TABLE users (id BIGINT PRIMARY KEY, status TEXT NOT NULL, deleted_at TIMESTAMPTZ);
		CREATE TABLE groups (id BIGINT PRIMARY KEY, status TEXT NOT NULL, deleted_at TIMESTAMPTZ);
		CREATE TABLE api_keys (
			id BIGINT PRIMARY KEY, key TEXT NOT NULL, status TEXT NOT NULL, user_id BIGINT NOT NULL,
			group_id BIGINT, deleted_at TIMESTAMPTZ, quota FLOAT8, quota_used FLOAT8, expires_at TIMESTAMPTZ,
			rate_limit_5h FLOAT8, rate_limit_1d FLOAT8, rate_limit_7d FLOAT8,
			usage_5h FLOAT8, usage_1d FLOAT8, usage_7d FLOAT8,
			window_5h_start TIMESTAMPTZ, window_1d_start TIMESTAMPTZ, window_7d_start TIMESTAMPTZ
		);
		INSERT INTO users (id, status) VALUES (11, 'active');
		INSERT INTO groups (id, status) VALUES (12, 'active');
		INSERT INTO api_keys (id, key, status, user_id, group_id, quota, quota_used, rate_limit_5h, usage_5h, window_5h_start)
		VALUES (13, 'local-fixture-key', 'active', 11, 12, 2, 0.25, 1, 0.1, now())`)
	require.NoError(t, err)
	repo := NewChannelDetectionKeyRepository(dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db))))
	key, err := repo.GetChannelDetectionKey(ctx, 13, false)
	require.NoError(t, err)
	require.Empty(t, key.Key)
	require.Equal(t, int64(12), *key.GroupID)
	require.True(t, key.Group.IsActive())
	require.True(t, key.User.IsActive())
	require.Equal(t, float64(0.1), key.EffectiveUsage5h())

	key, err = repo.GetChannelDetectionKey(ctx, 13, true)
	require.NoError(t, err)
	require.Equal(t, "local-fixture-key", key.Key)
	_, err = db.ExecContext(ctx, `UPDATE api_keys SET deleted_at = now() WHERE id = 13`)
	require.NoError(t, err)
	_, err = repo.GetChannelDetectionKey(ctx, 13, false)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
}
