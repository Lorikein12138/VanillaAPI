//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

var baselineV0216NewMigrationNames = []string{
	"242_group_i18n.sql",
	"243_gemini_cached_contents.sql",
	"244_gemini_cached_contents_channel_usage.sql",
	"245_gemini_cached_contents_mutation_lease.sql",
}

type baselineV0216MigrationRecord struct {
	checksum string
	applied  time.Time
}

type baselineV0216SentinelSnapshot struct {
	Balance string
	Usage   string
}

func baselineV0216PreviousFS(t *testing.T) fs.FS {
	t.Helper()
	fixture, err := os.ReadFile("../../migrations/testdata/baseline_v0216_prior_migrations.sha256")
	require.NoError(t, err)
	previous := make(fstest.MapFS)
	for _, line := range strings.Split(strings.TrimSpace(string(fixture)), "\n") {
		fields := strings.Fields(line)
		require.Len(t, fields, 2)
		name := fields[1]
		content, readErr := dbmigrations.FS.ReadFile(name)
		require.NoError(t, readErr)
		sum := sha256.Sum256(content)
		require.Equal(t, fields[0], hex.EncodeToString(sum[:]), "frozen v0.2.15 migration changed: %s", name)
		previous[name] = &fstest.MapFile{Data: append([]byte(nil), content...)}
	}
	require.Len(t, previous, 297, "the previous database must see exactly H's 297 migration files")
	return previous
}

func baselineV0216TargetFS(t *testing.T) fs.FS {
	t.Helper()
	return baselineV0217PreviousFS(t)
}

func TestBaselineV0216FreshInstall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	harnessDatabase, harnessConfig := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0216TargetFS(t)))
	baselineV0216RequireNewSchema(t, ctx, fixture.db)

	_, repo := baselineV0216GroupRepository(t, fixture.db)
	group := &service.Group{
		Name:             "baseline v0216 fresh install i18n",
		Description:      "base description",
		Platform:         service.PlatformGemini,
		Status:           service.StatusActive,
		SubscriptionType: service.SubscriptionTypeStandard,
		RateMultiplier:   1,
		ModelPricing:     baselineV0216SentinelModelPricing(),
		I18n:             service.GroupI18n{"zh": {Name: "新装分组", Description: "新装描述"}},
	}
	require.NoError(t, repo.Create(ctx, group))
	got, err := repo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.Equal(t, group.ModelPricing, got.ModelPricing, "the real repository must persist and read typed group model pricing")
	require.Equal(t, group.I18n, got.I18n, "the real repository must persist and read group.i18n")

	baselineV0216RequireHarnessUnchanged(t, ctx, harnessDatabase, harnessConfig)
}

func TestBaselineV0216UpgradePreservesHistory(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	harnessDatabase, harnessConfig := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	previous := baselineV0216PreviousFS(t)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, previous))
	beforeRecords, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, beforeRecords, 297)

	var userID, groupID, accountID, apiKeyID int64
	modelPricing := baselineV0216SentinelModelPricing()
	modelPricingJSON, err := json.Marshal(modelPricing)
	require.NoError(t, err)
	commitChannelMonitorV2V0215(ctx, t, fixture.db, func(tx *sql.Tx) {
		err := tx.QueryRowContext(ctx, `
			INSERT INTO users (email, password_hash, balance, frozen_balance)
			VALUES ('baseline-v0216-preserve@example.test', 'hash', 123.45678901, 7.25)
			RETURNING id`).Scan(&userID)
		require.NoError(t, err)
		err = tx.QueryRowContext(ctx, `
			INSERT INTO groups (name, description, platform, status, rate_multiplier, model_pricing, image_price_1k)
			VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)
			RETURNING id`, "CMV2 V0215 BASELINE V0216 group", "preserve price fields", service.PlatformGemini,
			service.StatusActive, 1.375, string(modelPricingJSON), 0.042).Scan(&groupID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE channel_monitor_v2_config SET group_ids = array_append(group_ids, $1::bigint) WHERE id = 1`, groupID)
		require.NoError(t, err)
		err = tx.QueryRowContext(ctx, `
			INSERT INTO accounts (name, platform, type, credentials, status)
			VALUES ('baseline-v0216-preserve-account', 'gemini', 'apikey', '{}'::jsonb, 'active')
			RETURNING id`).Scan(&accountID)
		require.NoError(t, err)
		err = tx.QueryRowContext(ctx, `
			INSERT INTO api_keys (user_id, key, name, group_id)
			VALUES ($1, 'sk-baseline-v0216-preserve', 'preserve key', $2)
			RETURNING id`, userID, groupID).Scan(&apiKeyID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO usage_logs (user_id, api_key_id, account_id, group_id, model, input_tokens, output_tokens, total_cost, actual_cost)
			VALUES ($1, $2, $3, $4, 'sentinel-model', 17, 23, 0.0125, 0.00975)`, userID, apiKeyID, accountID, groupID)
		require.NoError(t, err)
		channelID, err := insertBaselineV0216Channel(ctx, tx)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO channel_groups (channel_id, group_id) VALUES ($1, $2)`, channelID, groupID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO channel_model_pricing (channel_id, models, input_price, output_price)
			VALUES ($1, '["sentinel-model"]'::jsonb, 0.000003, 0.000004)`, channelID)
		require.NoError(t, err)
		bucket := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO channel_monitor_v2_metrics_1m (bucket_start, platform, group_id, model, success_requests, input_tokens)
			VALUES ($1, 'gemini', $2, 'sentinel-model', 3, 510)`, bucket, groupID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO channel_monitor_v2_metrics_rollup (bucket_start, bucket_seconds, platform, group_id, model, success_requests, input_tokens)
			VALUES ($1, 300, 'gemini', $2, 'sentinel-model', 3, 510)`, bucket, groupID)
		require.NoError(t, err)
	})

	beforeModelPricingJSON := baselineV0216ReadJSON(t, ctx, fixture.db,
		`SELECT model_pricing::text FROM groups WHERE id = $1`, groupID)
	require.JSONEq(t, string(modelPricingJSON), beforeModelPricingJSON, "the committed prior-schema seed must contain valid typed-array JSON")
	beforeSideEffects, err := readChannelMonitorV2V0215SideEffectSnapshot(ctx, fixture.db)
	require.NoError(t, err)
	beforeSentinels, err := baselineV0216ReadSentinels(ctx, fixture.db, userID)
	require.NoError(t, err)
	var connectedDatabase string
	require.NoError(t, fixture.db.QueryRowContext(ctx, "SELECT current_database()").Scan(&connectedDatabase))
	require.Equal(t, fixture.name, connectedDatabase)

	require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0216TargetFS(t)))
	baselineV0216RequireNewSchema(t, ctx, fixture.db)
	afterRecords, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, afterRecords, 301)
	for name, before := range beforeRecords {
		require.Equal(t, before, afterRecords[name], "old checksum/applied_at must remain exact for %s", name)
	}
	for _, name := range baselineV0216NewMigrationNames {
		require.Contains(t, afterRecords, name)
	}
	afterSideEffects, err := readChannelMonitorV2V0215SideEffectSnapshot(ctx, fixture.db)
	require.NoError(t, err)
	requireChannelMonitorV2V0215SideEffectSnapshotEqual(t, beforeSideEffects, afterSideEffects)
	afterSentinels, err := baselineV0216ReadSentinels(ctx, fixture.db, userID)
	require.NoError(t, err)
	require.Equal(t, beforeSentinels, afterSentinels, "price/balance/usage sentinels must not change")
	afterModelPricingJSON := baselineV0216ReadJSON(t, ctx, fixture.db,
		`SELECT model_pricing::text FROM groups WHERE id = $1`, groupID)
	require.JSONEq(t, beforeModelPricingJSON, afterModelPricingJSON, "raw JSONB model pricing must survive the upgrade unchanged")

	_, repo := baselineV0216GroupRepository(t, fixture.db)
	got, err := repo.GetByID(ctx, groupID)
	require.NoError(t, err)
	require.Equal(t, service.GroupI18n{}, got.I18n, "existing groups receive the new empty i18n default")
	require.Equal(t, modelPricing, got.ModelPricing, "the real repository must retain the typed pricing values")
	require.Equal(t, service.PlatformGemini, got.ModelPricing[0].Platform)
	require.Equal(t, []string{"sentinel-model"}, got.ModelPricing[0].Models)
	require.Equal(t, service.BillingModeToken, got.ModelPricing[0].BillingMode)
	require.InDelta(t, 0.000001, *got.ModelPricing[0].InputPrice, 1e-15)
	require.InDelta(t, 0.000002, *got.ModelPricing[0].OutputPrice, 1e-15)
	require.Equal(t, 1.375, got.RateMultiplier)
	require.Equal(t, "{}", baselineV0216ReadJSON(t, ctx, fixture.db,
		`SELECT i18n::text FROM groups WHERE id = $1`, groupID))

	baselineV0216RequireHarnessUnchanged(t, ctx, harnessDatabase, harnessConfig)
}

func baselineV0216SentinelModelPricing() []service.ChannelModelPricing {
	inputPrice, outputPrice := 0.000001, 0.000002
	return []service.ChannelModelPricing{{
		Platform:    service.PlatformGemini,
		Models:      []string{"sentinel-model"},
		BillingMode: service.BillingModeToken,
		InputPrice:  &inputPrice,
		OutputPrice: &outputPrice,
	}}
}

func TestBaselineV0216SecondStartup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	target := baselineV0216TargetFS(t)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, target))
	first, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, first, 301)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, target))
	second, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Equal(t, first, second, "second startup must not rewrite checksums or applied_at")
}

func baselineV0216GroupRepository(t *testing.T, db *sql.DB) (*dbent.Client, service.GroupRepository) {
	t.Helper()
	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("close baseline v0.2.16 Ent client: %v", err)
		}
	})
	return client, NewGroupRepository(client, db)
}

func baselineV0216RequireNewSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	checks := []struct{ table, column string }{
		{"groups", "i18n"},
		{"gemini_cached_contents", "channel_usage"},
		{"gemini_cached_contents", "mutation_token"},
		{"gemini_cached_contents", "mutation_lease_until"},
	}
	for _, check := range checks {
		var exists bool
		err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_catalog.pg_attribute column_info
				JOIN pg_catalog.pg_class table_info ON table_info.oid = column_info.attrelid
				JOIN pg_catalog.pg_namespace schema_info ON schema_info.oid = table_info.relnamespace
				WHERE schema_info.nspname = 'public' AND table_info.relname = $1
				  AND column_info.attname = $2 AND NOT column_info.attisdropped
			)`, check.table, check.column).Scan(&exists)
		require.NoError(t, err)
		require.True(t, exists, "%s.%s must exist after upgrade", check.table, check.column)
	}
	var cacheTableExists bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT to_regclass('public.gemini_cached_contents') IS NOT NULL`).Scan(&cacheTableExists))
	require.True(t, cacheTableExists)
}

func baselineV0216ReadMigrationRecords(ctx context.Context, db *sql.DB) (map[string]baselineV0216MigrationRecord, error) {
	rows, err := db.QueryContext(ctx, `SELECT filename, checksum, applied_at FROM schema_migrations ORDER BY filename`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := make(map[string]baselineV0216MigrationRecord)
	for rows.Next() {
		var name string
		var record baselineV0216MigrationRecord
		if err := rows.Scan(&name, &record.checksum, &record.applied); err != nil {
			return nil, err
		}
		records[name] = record
	}
	return records, rows.Err()
}

func baselineV0216ReadSentinels(ctx context.Context, db *sql.DB, userID int64) (baselineV0216SentinelSnapshot, error) {
	var snapshot baselineV0216SentinelSnapshot
	if err := db.QueryRowContext(ctx, `
		SELECT jsonb_build_object('balance', balance, 'frozen_balance', frozen_balance)::text
		FROM users WHERE id = $1`, userID).Scan(&snapshot.Balance); err != nil {
		return snapshot, err
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(jsonb_agg(jsonb_build_object(
			'model', model, 'input_tokens', input_tokens, 'output_tokens', output_tokens,
			'total_cost', total_cost, 'actual_cost', actual_cost, 'group_id', group_id
		) ORDER BY id), '[]'::jsonb)::text
		FROM usage_logs WHERE user_id = $1`, userID).Scan(&snapshot.Usage); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func baselineV0216ReadJSON(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var value string
	require.NoError(t, db.QueryRowContext(ctx, query, args...).Scan(&value))
	return value
}

func baselineV0216HarnessSnapshot(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	var database, config string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT current_database()").Scan(&database))
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT to_jsonb(config)::text FROM public.channel_monitor_v2_config config WHERE id = 1`).Scan(&config))
	return database, config
}

func baselineV0216RequireHarnessUnchanged(t *testing.T, ctx context.Context, database, config string) {
	t.Helper()
	var afterDatabase, afterConfig string
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT current_database()").Scan(&afterDatabase))
	require.Equal(t, database, afterDatabase, "migration must not switch or contaminate the shared harness database")
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT to_jsonb(config)::text FROM public.channel_monitor_v2_config config WHERE id = 1`).Scan(&afterConfig))
	require.JSONEq(t, config, afterConfig, "shared harness config must remain unchanged")
}

func insertBaselineV0216Channel(ctx context.Context, tx *sql.Tx) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO channels (name, description, status)
		VALUES ('CMV2 V0215 BASELINE V0216 channel', 'preserve channel pricing', 'active')
		RETURNING id`).Scan(&id)
	return id, err
}
