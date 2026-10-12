//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const retiredRuntimeDataMigration = "244_clear_retired_runtime_data.sql"

var channelMonitorV2SampleTables = []string{
	"channel_monitor_v2_metrics_1m",
	"channel_monitor_v2_metrics_rollup",
	"channel_monitor_v2_error_metrics_1m",
	"channel_monitor_v2_error_metrics_rollup",
	"channel_monitor_v2_user_metrics_1m",
	"channel_monitor_v2_user_metrics_rollup",
	"channel_monitor_v2_latency_histograms_1m",
	"channel_monitor_v2_latency_histograms_rollup",
}

// 244 只清软删除账号的凭据，未删除账号不动；可重放。
func TestMigration244ClearsSoftDeletedAccountCredentials(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	var deletedID, liveID int64
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, credentials, deleted_at)
VALUES ('migration-244-deleted', 'anthropic', 'apikey', '{"api_key":"sk-deleted"}'::jsonb, NOW())
RETURNING id
`).Scan(&deletedID))
	require.NoError(t, tx.QueryRowContext(ctx, `
INSERT INTO accounts (name, platform, type, credentials)
VALUES ('migration-244-live', 'anthropic', 'apikey', '{"api_key":"sk-live"}'::jsonb)
RETURNING id
`).Scan(&liveID))

	for range 2 {
		applyRetiredRuntimeDataMigration(ctx, t, tx)

		var deletedCredentials, liveCredentials string
		require.NoError(t, tx.QueryRowContext(ctx,
			"SELECT credentials::text FROM accounts WHERE id = $1", deletedID).Scan(&deletedCredentials))
		require.JSONEq(t, `{}`, deletedCredentials)
		require.NoError(t, tx.QueryRowContext(ctx,
			"SELECT credentials::text FROM accounts WHERE id = $1", liveID).Scan(&liveCredentials))
		require.JSONEq(t, `{"api_key":"sk-live"}`, liveCredentials)
	}
}

// v2 停用时清空冻结采样，配置与水位保留。
func TestMigration244TruncatesChannelMonitorV2SamplesWhenDisabled(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	seedChannelMonitorV2Sample(ctx, t, tx)
	_, err := tx.ExecContext(ctx, "UPDATE channel_monitor_v2_config SET enabled = FALSE WHERE id = 1")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "UPDATE channel_monitor_v2_watermarks SET data_through = '2026-10-06T00:00:00Z' WHERE id = 1")
	require.NoError(t, err)

	applyRetiredRuntimeDataMigration(ctx, t, tx)

	for _, table := range channelMonitorV2SampleTables {
		require.Zero(t, countRows(ctx, t, tx, table), table)
	}
	require.Equal(t, 1, countRows(ctx, t, tx, "channel_monitor_v2_config"))
	var dataThroughSet bool
	require.NoError(t, tx.QueryRowContext(ctx,
		"SELECT data_through IS NOT NULL FROM channel_monitor_v2_watermarks WHERE id = 1").Scan(&dataThroughSet))
	require.True(t, dataThroughSet, "watermarks must be kept")

	applyRetiredRuntimeDataMigration(ctx, t, tx)
}

// v2 启用时采样一行不动。
func TestMigration244KeepsChannelMonitorV2SamplesWhenEnabled(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()

	seedChannelMonitorV2Sample(ctx, t, tx)
	_, err := tx.ExecContext(ctx, "UPDATE channel_monitor_v2_config SET enabled = TRUE WHERE id = 1")
	require.NoError(t, err)
	before := countRows(ctx, t, tx, "channel_monitor_v2_metrics_1m")
	require.Positive(t, before)

	applyRetiredRuntimeDataMigration(ctx, t, tx)

	require.Equal(t, before, countRows(ctx, t, tx, "channel_monitor_v2_metrics_1m"))
}

func applyRetiredRuntimeDataMigration(ctx context.Context, t *testing.T, tx *sql.Tx) {
	t.Helper()

	migrationSQL, err := dbmigrations.FS.ReadFile(retiredRuntimeDataMigration)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)
}

func seedChannelMonitorV2Sample(ctx context.Context, t *testing.T, tx *sql.Tx) {
	t.Helper()

	_, err := tx.ExecContext(ctx, `
INSERT INTO channel_monitor_v2_metrics_1m (bucket_start, platform, model, success_requests)
VALUES ('2026-10-05T00:00:00Z', 'anthropic', 'migration-244-model', 1)
`)
	require.NoError(t, err)
}

func countRows(ctx context.Context, t *testing.T, tx *sql.Tx, table string) int {
	t.Helper()

	var count int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count))
	return count
}
