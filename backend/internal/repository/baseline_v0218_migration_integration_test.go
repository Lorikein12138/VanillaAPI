//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const baselineV0218ImageMigrationName = "246_enable_gemini_image_generation_groups.sql"

func baselineV0218RequireLedger(t *testing.T, ctx context.Context, db *sql.DB) map[string]baselineV0216MigrationRecord {
	t.Helper()
	records, err := baselineV0216ReadMigrationRecords(ctx, db)
	require.NoError(t, err)
	require.Len(t, records, 302)
	for name := range baselineV0217PreviousFS(t) {
		require.Contains(t, records, name)
	}
	require.Contains(t, records, baselineV0218ImageMigrationName)
	data, err := dbmigrations.FS.ReadFile(baselineV0218ImageMigrationName)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(data))))
	require.Equal(t, hex.EncodeToString(sum[:]), records[baselineV0218ImageMigrationName].checksum)
	return records
}

func baselineV0218SeedImageGroups(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	commitChannelMonitorV2V0215(ctx, t, db, func(tx *sql.Tx) {
		_, err := tx.ExecContext(ctx, `UPDATE groups SET allow_image_generation=false WHERE name='baseline v0217'`)
		require.NoError(t, err)
		for _, item := range []struct{ name, platform string }{
			{name: "baseline-v0218-antigravity", platform: "antigravity"},
			{name: "baseline-v0218-composite", platform: "composite"},
			{name: "baseline-v0218-openai", platform: "openai"},
		} {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO groups (name,platform,status,rate_multiplier,model_pricing,model_allowlist,allow_image_generation)
				VALUES ($1,$2,'active',1,'[]'::jsonb,'{"enabled":true,"models":[]}'::jsonb,false)`, item.name, item.platform)
			require.NoError(t, err)
		}
	})
}

func baselineV0218ReadImageFlags(t *testing.T, ctx context.Context, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT name,allow_image_generation FROM groups WHERE name IN ('baseline v0217','baseline-v0218-antigravity','baseline-v0218-composite','baseline-v0218-openai') ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	flags := make(map[string]bool)
	for rows.Next() {
		var name string
		var enabled bool
		require.NoError(t, rows.Scan(&name, &enabled))
		flags[name] = enabled
	}
	require.NoError(t, rows.Err())
	require.Len(t, flags, 4)
	return flags
}

func baselineV0218BusinessSentinels(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	var snapshot []string
	for _, query := range []string{
		`SELECT to_jsonb(u)::text FROM users u WHERE email='baseline-v0217@example.test'`,
		`SELECT to_jsonb(a)::text FROM accounts a WHERE name='baseline v0217'`,
		`SELECT to_jsonb(k)::text FROM api_keys k WHERE key='sk-baseline-v0217'`,
		`SELECT COALESCE(jsonb_agg(to_jsonb(u) ORDER BY id),'[]')::text FROM usage_logs u`,
		`SELECT (to_jsonb(g) - 'allow_image_generation')::text FROM groups g WHERE name='baseline v0217'`,
		`SELECT to_jsonb(s)::text FROM settings s WHERE key='PAYMENT_QUICK_AMOUNTS'`,
		`SELECT to_jsonb(c)::text FROM channel_monitor_v2_config c WHERE id=1`,
		`SELECT to_jsonb(c)::text FROM gemini_cached_contents c WHERE public_id='baseline-v0217-cache'`,
	} {
		snapshot = append(snapshot, baselineV0216ReadJSON(t, ctx, db, query))
	}
	return snapshot
}

func TestBaselineV0218FreshInstallAndSecondStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, config := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	first := baselineV0218RequireLedger(t, ctx, fixture.db)

	// A newly created group can be edited after the initial install; a later startup
	// must not reapply the one-time migration to those new rows.
	baselineV0218SeedFreshImageGroups(t, ctx, fixture.db)
	flags := baselineV0218ReadFreshImageFlags(t, ctx, fixture.db)
	require.Equal(t, map[string]bool{
		"baseline-v0218-fresh-gemini":      false,
		"baseline-v0218-fresh-antigravity": false,
		"baseline-v0218-fresh-composite":   false,
		"baseline-v0218-fresh-openai":      false,
	}, flags)
	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	second := baselineV0218RequireLedger(t, ctx, fixture.db)
	require.Equal(t, first, second, "second startup must keep every checksum and applied_at unchanged")
	require.Equal(t, flags, baselineV0218ReadFreshImageFlags(t, ctx, fixture.db))
	baselineV0216RequireHarnessUnchanged(t, ctx, database, config)
}

func baselineV0218SeedFreshImageGroups(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	commitChannelMonitorV2V0215(ctx, t, db, func(tx *sql.Tx) {
		for _, item := range []struct{ name, platform string }{
			{name: "baseline-v0218-fresh-gemini", platform: "gemini"},
			{name: "baseline-v0218-fresh-antigravity", platform: "antigravity"},
			{name: "baseline-v0218-fresh-composite", platform: "composite"},
			{name: "baseline-v0218-fresh-openai", platform: "openai"},
		} {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO groups (name,platform,status,rate_multiplier,model_pricing,model_allowlist,allow_image_generation)
				VALUES ($1,$2,'active',1,'[]'::jsonb,'{"enabled":true,"models":[]}'::jsonb,false)`, item.name, item.platform)
			require.NoError(t, err)
		}
	})
}

func baselineV0218ReadFreshImageFlags(t *testing.T, ctx context.Context, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT name,allow_image_generation FROM groups WHERE name LIKE 'baseline-v0218-fresh-%' ORDER BY name`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	flags := make(map[string]bool)
	for rows.Next() {
		var name string
		var enabled bool
		require.NoError(t, rows.Scan(&name, &enabled))
		flags[name] = enabled
	}
	require.NoError(t, rows.Err())
	require.Len(t, flags, 4)
	return flags
}

func TestBaselineV0218UpgradeChangesOnlyTargetFlagsAndRunsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, config := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	oldFS := baselineV0217PreviousFS(t)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, oldFS))
	baselineV0217Seed(t, ctx, fixture.db)
	baselineV0218SeedImageGroups(t, ctx, fixture.db)
	before, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, before, 301)
	flagsBefore := baselineV0218ReadImageFlags(t, ctx, fixture.db)
	require.False(t, flagsBefore["baseline v0217"])
	require.False(t, flagsBefore["baseline-v0218-antigravity"])
	require.False(t, flagsBefore["baseline-v0218-composite"])
	require.False(t, flagsBefore["baseline-v0218-openai"])
	sentinels := baselineV0218BusinessSentinels(t, ctx, fixture.db)

	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	after := baselineV0218RequireLedger(t, ctx, fixture.db)
	for name, record := range before {
		require.Equal(t, record, after[name], "historical checksum/applied_at changed for %s", name)
	}
	flagsAfter := baselineV0218ReadImageFlags(t, ctx, fixture.db)
	require.Equal(t, map[string]bool{
		"baseline v0217":             true,
		"baseline-v0218-antigravity": true,
		"baseline-v0218-composite":   false,
		"baseline-v0218-openai":      false,
	}, flagsAfter)
	require.Equal(t, sentinels, baselineV0218BusinessSentinels(t, ctx, fixture.db), "unrelated persisted business rows must not change")

	// Simulate an administrator disabling image generation after the upgrade.
	_, err = fixture.db.ExecContext(ctx, `UPDATE groups SET allow_image_generation=false WHERE name IN ('baseline v0217','baseline-v0218-antigravity')`)
	require.NoError(t, err)
	afterAdminChange, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Equal(t, after, afterAdminChange)
	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	require.Equal(t, afterAdminChange, baselineV0218RequireLedger(t, ctx, fixture.db), "second startup must not reapply the migration")
	require.Equal(t, map[string]bool{
		"baseline v0217":             false,
		"baseline-v0218-antigravity": false,
		"baseline-v0218-composite":   false,
		"baseline-v0218-openai":      false,
	}, baselineV0218ReadImageFlags(t, ctx, fixture.db))
	require.Equal(t, sentinels, baselineV0218BusinessSentinels(t, ctx, fixture.db))
	baselineV0216RequireHarnessUnchanged(t, ctx, database, config)
}
