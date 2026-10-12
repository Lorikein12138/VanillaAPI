//go:build integration

package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const channelMonitorV2V0215MigrationFilename = "245_channel_monitor_v2_v0215_platforms.sql"

var channelMonitorV2V0215Platforms = []string{
	"anthropic", "openai", "gemini", "antigravity", "grok", "kimi", "zhipu",
	"deepseek", "minimax", "opencode_go", "typesafe", "command_code", "cline", "composite",
}

type channelMonitorV2V0215ConfigSnapshot struct {
	Platforms       []byte
	Version         int
	Enabled         bool
	RefreshInterval int
	GroupIDs        []int64
	GroupScopeMode  string
	HealthThreshold []byte
	Ignored         []string
	UpdatedAt       time.Time
	UpdatedBy       sql.NullInt64
}

func applyChannelMonitorV2V0215MigrationInTx(ctx context.Context, t *testing.T, tx *sql.Tx) {
	t.Helper()
	content, err := dbmigrations.FS.ReadFile(channelMonitorV2V0215MigrationFilename)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(content))
	require.NoError(t, err, "execute migration 245 in the fixture transaction")
}

func TestChannelMonitorV2V0215MigrationUpgrade(t *testing.T) {
	oldTwelve := make([]map[string]any, 0, 12)
	for _, platform := range []string{
		"anthropic", "openai", "gemini", "antigravity", "grok", "kimi", "zhipu",
		"deepseek", "minimax", "opencode_go", "typesafe", "composite",
	} {
		oldTwelve = append(oldTwelve, map[string]any{"platform": platform, "enabled": true, "models": []string{}})
	}
	oldTwelve[1]["models"] = []string{"gpt-custom"}
	oldTwelve[4]["enabled"] = false
	oldTwelve[4]["operator_note"] = "retain"

	allFourteen := make([]map[string]any, 0, len(channelMonitorV2V0215Platforms))
	for _, platform := range []string{
		"cline", "openai", "command_code", "anthropic", "gemini", "antigravity", "grok",
		"kimi", "zhipu", "deepseek", "minimax", "opencode_go", "typesafe", "composite",
	} {
		allFourteen = append(allFourteen, map[string]any{"platform": platform, "enabled": true, "models": []string{}})
	}
	allFourteen[0]["enabled"] = false
	allFourteen[0]["models"] = []string{"cline-private"}
	allFourteen[2]["enabled"] = false
	allFourteen[2]["models"] = []string{"command-private"}
	allFourteen[1]["operator_note"] = "custom-order"

	duplicateAndUnknown := []map[string]any{
		{"platform": "command_code", "enabled": false, "models": []string{"first-key"}, "operator_note": "first object"},
		{"platform": "kiro", "enabled": true, "models": []string{"retired"}},
		{"platform": "mystery", "enabled": false, "models": []string{"unsupported"}},
		{"platform": "command_code", "enabled": true, "models": []string{"duplicate-must-not-win"}},
		{"platform": "cline", "enabled": false, "models": []string{"cline-private"}},
	}
	wantDuplicateNormalized := []map[string]any{
		duplicateAndUnknown[0], duplicateAndUnknown[4],
		{"platform": "anthropic", "enabled": true, "models": []string{}},
		{"platform": "openai", "enabled": true, "models": []string{}},
		{"platform": "gemini", "enabled": true, "models": []string{}},
		{"platform": "antigravity", "enabled": true, "models": []string{}},
		{"platform": "grok", "enabled": true, "models": []string{}},
		{"platform": "kimi", "enabled": true, "models": []string{}},
		{"platform": "zhipu", "enabled": true, "models": []string{}},
		{"platform": "deepseek", "enabled": true, "models": []string{}},
		{"platform": "minimax", "enabled": true, "models": []string{}},
		{"platform": "opencode_go", "enabled": true, "models": []string{}},
		{"platform": "typesafe", "enabled": true, "models": []string{}},
		{"platform": "composite", "enabled": true, "models": []string{}},
	}

	cases := []struct {
		name        string
		platforms   []map[string]any
		want        []map[string]any
		wantChanged bool
	}{
		{
			name:        "legacy twelve append only the two missing platforms",
			platforms:   oldTwelve,
			want:        append(append([]map[string]any{}, oldTwelve...), defaultV0215Platform("command_code"), defaultV0215Platform("cline")),
			wantChanged: true,
		},
		{name: "custom fourteen preserves disabled new platforms and order", platforms: allFourteen, want: allFourteen},
		{name: "first duplicate wins and unsupported entries are removed", platforms: duplicateAndUnknown, want: wantDuplicateNormalized, wantChanged: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx := testTx(t)
			ctx := context.Background()
			seedAt := time.Date(2020, time.April, 5, 6, 7, 8, 0, time.UTC)
			seedV0215ConfigTx(t, ctx, tx, tc.platforms, seedAt)
			before := readV0215ConfigSnapshot(t, ctx, tx)
			applyChannelMonitorV2V0215MigrationInTx(ctx, t, tx)
			after := readV0215ConfigSnapshot(t, ctx, tx)

			wantJSON, err := json.Marshal(tc.want)
			require.NoError(t, err)
			require.JSONEq(t, string(wantJSON), string(after.Platforms))
			require.Equal(t, before.Enabled, after.Enabled)
			require.Equal(t, before.RefreshInterval, after.RefreshInterval)
			require.Equal(t, before.GroupIDs, after.GroupIDs)
			require.Equal(t, before.GroupScopeMode, after.GroupScopeMode)
			require.JSONEq(t, string(before.HealthThreshold), string(after.HealthThreshold))
			require.Equal(t, before.Ignored, after.Ignored)
			require.Equal(t, before.UpdatedBy, after.UpdatedBy)
			if tc.wantChanged {
				require.Equal(t, before.Version+1, after.Version)
				require.NotEqual(t, before.UpdatedAt, after.UpdatedAt)
			} else {
				require.Equal(t, before.Version, after.Version)
				require.Equal(t, before.UpdatedAt, after.UpdatedAt)
			}

			applyChannelMonitorV2V0215MigrationInTx(ctx, t, tx)
			replayed := readV0215ConfigSnapshot(t, ctx, tx)
			require.Equal(t, after, replayed, "a second direct SQL replay must not change config or metadata")
		})
	}

	t.Run("full migration chain sets fourteen platform default", func(t *testing.T) {
		tx := testTx(t)
		got := readV0215PlatformDefaultTx(t, tx)
		require.Equal(t, channelMonitorV2V0215Platforms, got)
	})
}

func defaultV0215Platform(platform string) map[string]any {
	return map[string]any{"platform": platform, "enabled": true, "models": []string{}}
}

func seedV0215ConfigTx(t *testing.T, ctx context.Context, tx *sql.Tx, platforms []map[string]any, updatedAt time.Time) {
	t.Helper()
	platformJSON, err := json.Marshal(platforms)
	require.NoError(t, err)
	health := []byte(`{"minimum_sample":17,"warning_error_rate":0.07,"operator_flag":"keep"}`)
	_, err = tx.ExecContext(ctx, `
		UPDATE channel_monitor_v2_config
		SET version = 31, enabled = FALSE, refresh_interval_seconds = 300, platforms = $1,
		    group_ids = $2, group_scope_mode = 'selected', health_thresholds = $3,
		    ignored_error_categories = $4, updated_by = 7801, updated_at = $5
		WHERE id = 1`, platformJSON, pq.Array([]int64{}), health,
		pq.Array([]string{"timeout", "internal"}), updatedAt)
	require.NoError(t, err)
}

func readV0215ConfigSnapshot(t *testing.T, ctx context.Context, tx channelMonitorV2V0215SQLWriter) channelMonitorV2V0215ConfigSnapshot {
	t.Helper()
	var got channelMonitorV2V0215ConfigSnapshot
	err := tx.QueryRowContext(ctx, `
		SELECT platforms, version, enabled, refresh_interval_seconds, group_ids,
		       group_scope_mode, health_thresholds, ignored_error_categories, updated_at, updated_by
		FROM channel_monitor_v2_config WHERE id = 1`).Scan(
		&got.Platforms, &got.Version, &got.Enabled, &got.RefreshInterval, pq.Array(&got.GroupIDs),
		&got.GroupScopeMode, &got.HealthThreshold, pq.Array(&got.Ignored), &got.UpdatedAt, &got.UpdatedBy)
	require.NoError(t, err)
	return got
}

func readV0215PlatformDefaultTx(t *testing.T, tx *sql.Tx) []string {
	t.Helper()
	var expression string
	err := tx.QueryRowContext(context.Background(), `
		SELECT pg_catalog.pg_get_expr(default_value.adbin, default_value.adrelid)
		FROM pg_catalog.pg_attrdef default_value
		JOIN pg_catalog.pg_class table_info ON table_info.oid = default_value.adrelid
		JOIN pg_catalog.pg_namespace schema_info ON schema_info.oid = table_info.relnamespace
		JOIN pg_catalog.pg_attribute column_info
		  ON column_info.attrelid = table_info.oid AND column_info.attnum = default_value.adnum
		WHERE schema_info.nspname = 'public'
		  AND table_info.relname = 'channel_monitor_v2_config'
		  AND column_info.attname = 'platforms'`).Scan(&expression)
	require.NoError(t, err)
	expression = strings.TrimSpace(expression)
	for strings.HasPrefix(expression, "(") && strings.HasSuffix(expression, ")") {
		expression = strings.TrimSpace(expression[1 : len(expression)-1])
	}
	match := regexp.MustCompile(`^'((?:[^']|'')*)'::jsonb$`).FindStringSubmatch(expression)
	require.Len(t, match, 2, "unexpected pg_catalog default expression: %s", expression)
	jsonText := strings.ReplaceAll(match[1], "''", "'")
	var platforms []struct {
		Platform string `json:"platform"`
	}
	require.NoError(t, json.Unmarshal([]byte(jsonText), &platforms))
	got := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		got = append(got, platform.Platform)
	}
	return got
}

type channelMonitorV2V0215DatabaseFixture struct {
	name    string
	db      *sql.DB
	created bool
}

func newChannelMonitorV2V0215TestDatabase(t *testing.T) *channelMonitorV2V0215DatabaseFixture {
	t.Helper()
	var token [8]byte
	_, err := rand.Read(token[:])
	require.NoError(t, err)
	name := "cmv2_v0215_" + hex.EncodeToString(token[:])
	require.NoError(t, ensureV0215OnlyDatabaseName(name))
	fixture := &channelMonitorV2V0215DatabaseFixture{name: name}
	t.Cleanup(func() { fixture.cleanup(t) })

	createCtx, cancelCreate := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = integrationDB.ExecContext(createCtx, "CREATE DATABASE "+pq.QuoteIdentifier(name))
	cancelCreate()
	require.NoError(t, err, "create isolated integration database")
	fixture.created = true

	databaseURL, err := url.Parse(integrationPostgresDSN)
	require.NoError(t, err, "parse temporary harness DSN")
	databaseURL.Path = "/" + name
	databaseURL.RawPath = ""
	fixture.db, err = openSQLWithRetry(context.Background(), databaseURL.String(), 30*time.Second)
	require.NoError(t, err, "open isolated integration database pool")
	var connectedDatabase string
	require.NoError(t, fixture.db.QueryRowContext(context.Background(), "SELECT current_database()").Scan(&connectedDatabase))
	require.Equal(t, name, connectedDatabase)
	return fixture
}

func (fixture *channelMonitorV2V0215DatabaseFixture) cleanup(t *testing.T) {
	t.Helper()
	var closeDone chan error
	closeTimedOut := false
	if fixture.db != nil {
		closeDone = make(chan error, 1)
		go func() { closeDone <- fixture.db.Close() }()
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 30*time.Second)
		select {
		case err := <-closeDone:
			if err != nil {
				t.Errorf("close isolated database pool %s: %v", fixture.name, err)
			}
		case <-closeCtx.Done():
			closeTimedOut = true
		}
		cancelClose()
	}

	if fixture.created {
		dropCtx, cancelDrop := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := integrationDB.ExecContext(dropCtx, "DROP DATABASE IF EXISTS "+pq.QuoteIdentifier(fixture.name)+" WITH (FORCE)")
		cancelDrop()
		if err != nil {
			t.Errorf("drop isolated database %s: %v", fixture.name, err)
		}
		fixture.created = false
	}

	if closeTimedOut {
		t.Errorf("close isolated database pool %s exceeded 30 seconds", fixture.name)
		closeCtx, cancelClose := context.WithTimeout(context.Background(), 30*time.Second)
		select {
		case err := <-closeDone:
			if err != nil {
				t.Errorf("finish closing isolated database pool %s after DROP FORCE: %v", fixture.name, err)
			}
		case <-closeCtx.Done():
			t.Errorf("isolated database pool %s did not close after DROP FORCE", fixture.name)
		}
		cancelClose()
	}
}

func channelMonitorV2V0215BaselineFS(t *testing.T) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(dbmigrations.FS, ".")
	require.NoError(t, err)
	baseline := make(fstest.MapFS)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") || name >= channelMonitorV2V0215MigrationFilename {
			continue
		}
		content, readErr := dbmigrations.FS.ReadFile(name)
		require.NoError(t, readErr)
		baseline[name] = &fstest.MapFile{Data: append([]byte(nil), content...)}
	}
	require.NotEmpty(t, baseline)
	return baseline
}

func TestChannelMonitorV2V0215DatabaseFixtureIsolation(t *testing.T) {
	var harnessDatabase string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), "SELECT current_database()").Scan(&harnessDatabase))
	var sharedConfig []byte
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT to_jsonb(config) FROM public.channel_monitor_v2_config config WHERE id = 1`).Scan(&sharedConfig))

	var fixtureNames []string
	t.Run("two independent committed probes are dropped", func(t *testing.T) {
		first := newChannelMonitorV2V0215TestDatabase(t)
		second := newChannelMonitorV2V0215TestDatabase(t)
		fixtureNames = []string{first.name, second.name}
		require.NotEqual(t, first.name, second.name)

		for _, fixture := range []*channelMonitorV2V0215DatabaseFixture{first, second} {
			_, err := fixture.db.ExecContext(context.Background(), "CREATE TABLE fixture_probe (value TEXT PRIMARY KEY)")
			require.NoError(t, err)
		}
		_, err := first.db.ExecContext(context.Background(), "INSERT INTO fixture_probe(value) VALUES ('first-only')")
		require.NoError(t, err)
		_, err = second.db.ExecContext(context.Background(), "INSERT INTO fixture_probe(value) VALUES ('second-only')")
		require.NoError(t, err)
		var firstValue, secondValue string
		require.NoError(t, first.db.QueryRowContext(context.Background(), "SELECT value FROM fixture_probe").Scan(&firstValue))
		require.NoError(t, second.db.QueryRowContext(context.Background(), "SELECT value FROM fixture_probe").Scan(&secondValue))
		require.Equal(t, "first-only", firstValue)
		require.Equal(t, "second-only", secondValue)
		var firstHasSecond, secondHasFirst bool
		require.NoError(t, first.db.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM fixture_probe WHERE value = 'second-only')`).Scan(&firstHasSecond))
		require.NoError(t, second.db.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM fixture_probe WHERE value = 'first-only')`).Scan(&secondHasFirst))
		require.False(t, firstHasSecond)
		require.False(t, secondHasFirst)
	})

	require.Len(t, fixtureNames, 2)
	var remaining int
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM pg_catalog.pg_database WHERE datname = ANY($1)", pq.Array(fixtureNames)).Scan(&remaining))
	require.Zero(t, remaining, "fixture cleanup must drop both self-created databases")
	var afterHarnessDatabase string
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), "SELECT current_database()").Scan(&afterHarnessDatabase))
	require.Equal(t, harnessDatabase, afterHarnessDatabase)
	var afterConfig []byte
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `SELECT to_jsonb(config) FROM public.channel_monitor_v2_config config WHERE id = 1`).Scan(&afterConfig))
	require.JSONEq(t, string(sharedConfig), string(afterConfig), "the shared harness config must remain unchanged")
}

type channelMonitorV2V0215MigrationRecord struct {
	checksum string
	applied  time.Time
}

type channelMonitorV2V0215SideEffectSnapshot struct {
	GroupPricing       string
	Channels           string
	ChannelGroups      string
	ChannelModelPrices string
	Samples            map[string]string
	Watermarks         string
}

var channelMonitorV2V0215SampleTables = []string{
	"channel_monitor_v2_metrics_1m",
	"channel_monitor_v2_user_metrics_1m",
	"channel_monitor_v2_error_metrics_1m",
	"channel_monitor_v2_latency_histograms_1m",
	"channel_monitor_v2_metrics_rollup",
	"channel_monitor_v2_user_metrics_rollup",
	"channel_monitor_v2_error_metrics_rollup",
	"channel_monitor_v2_latency_histograms_rollup",
}

var channelMonitorV2V0215HistoricalMigrationNames = []string{
	"241_channel_monitor_v2_passive_coverage.sql",
	"242_channel_monitor_v2_typesafe.sql",
	"243_finalize_retired_schema_hygiene.sql",
	"244_clear_retired_runtime_data.sql",
}

func readChannelMonitorV2V0215SideEffectSnapshot(ctx context.Context, db *sql.DB) (channelMonitorV2V0215SideEffectSnapshot, error) {
	var snapshot channelMonitorV2V0215SideEffectSnapshot
	readJSON := func(query string, args ...any) (string, error) {
		var value string
		if err := db.QueryRowContext(ctx, query, args...).Scan(&value); err != nil {
			return "", err
		}
		return value, nil
	}
	var err error
	snapshot.GroupPricing, err = readJSON(`
		SELECT COALESCE(jsonb_agg(jsonb_build_object(
			'name', name, 'model_pricing', model_pricing,
			'image_price_1k', image_price_1k, 'rate_multiplier', rate_multiplier
		) ORDER BY name, id), '[]'::jsonb)::text
		FROM public.groups WHERE name LIKE 'CMV2 V0215 %'`)
	if err != nil {
		return snapshot, err
	}
	snapshot.Channels, err = readJSON(`
		SELECT COALESCE(jsonb_agg(to_jsonb(channel_row) ORDER BY channel_row.id), '[]'::jsonb)::text
		FROM public.channels AS channel_row WHERE channel_row.name LIKE 'CMV2 V0215 %'`)
	if err != nil {
		return snapshot, err
	}
	snapshot.ChannelGroups, err = readJSON(`
		SELECT COALESCE(jsonb_agg(to_jsonb(group_row) ORDER BY group_row.channel_id, group_row.group_id), '[]'::jsonb)::text
		FROM public.channel_groups AS group_row
		JOIN public.channels AS channel_row ON channel_row.id = group_row.channel_id
		WHERE channel_row.name LIKE 'CMV2 V0215 %'`)
	if err != nil {
		return snapshot, err
	}
	snapshot.ChannelModelPrices, err = readJSON(`
		SELECT COALESCE(jsonb_agg(to_jsonb(pricing_row) ORDER BY pricing_row.channel_id, pricing_row.platform, pricing_row.id), '[]'::jsonb)::text
		FROM public.channel_model_pricing AS pricing_row
		JOIN public.channels AS channel_row ON channel_row.id = pricing_row.channel_id
		WHERE channel_row.name LIKE 'CMV2 V0215 %'`)
	if err != nil {
		return snapshot, err
	}
	snapshot.Samples = make(map[string]string, len(channelMonitorV2V0215SampleTables))
	for _, table := range channelMonitorV2V0215SampleTables {
		query := fmt.Sprintf(`
			SELECT COALESCE(jsonb_agg(to_jsonb(sample_row) ORDER BY to_jsonb(sample_row)::text), '[]'::jsonb)::text
			FROM public.%s AS sample_row`, pq.QuoteIdentifier(table))
		snapshot.Samples[table], err = readJSON(query)
		if err != nil {
			return snapshot, fmt.Errorf("read %s: %w", table, err)
		}
	}
	snapshot.Watermarks, err = readJSON(`
		SELECT COALESCE(jsonb_agg(to_jsonb(watermark_row) ORDER BY watermark_row.id), '[]'::jsonb)::text
		FROM public.channel_monitor_v2_watermarks AS watermark_row`)
	if err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func requireChannelMonitorV2V0215HistoryPreserved(t *testing.T, before, after map[string]channelMonitorV2V0215MigrationRecord) {
	t.Helper()
	for _, name := range channelMonitorV2V0215HistoricalMigrationNames {
		beforeRecord, beforeOK := before[name]
		afterRecord, afterOK := after[name]
		require.True(t, beforeOK, "historical migration record missing before 245: %s", name)
		require.True(t, afterOK, "historical migration record missing after 245: %s", name)
		require.Equal(t, beforeRecord, afterRecord, "241–244 checksum/applied_at must remain unchanged: %s", name)
	}
}

func requireChannelMonitorV2V0215SideEffectSnapshotEqual(t *testing.T, before, after channelMonitorV2V0215SideEffectSnapshot) {
	t.Helper()
	require.JSONEq(t, before.GroupPricing, after.GroupPricing, "groups.model_pricing/image_price_1k/rate_multiplier changed")
	require.JSONEq(t, before.Channels, after.Channels, "channels changed")
	require.JSONEq(t, before.ChannelGroups, after.ChannelGroups, "channel_groups changed")
	require.JSONEq(t, before.ChannelModelPrices, after.ChannelModelPrices, "channel_model_pricing changed")
	require.Equal(t, before.Samples, after.Samples, "complete V2 sample rows changed")
	require.JSONEq(t, before.Watermarks, after.Watermarks, "complete watermark rows changed")
}

func requireChannelMonitorV2V0215MigrationChecksum(t *testing.T, records map[string]channelMonitorV2V0215MigrationRecord) {
	t.Helper()
	content, err := dbmigrations.FS.ReadFile(channelMonitorV2V0215MigrationFilename)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(content))))
	record, ok := records[channelMonitorV2V0215MigrationFilename]
	require.True(t, ok, "245 migration record must exist")
	require.Equal(t, hex.EncodeToString(sum[:]), record.checksum, "245 migration checksum must match the runner's TrimSpace semantics")
}

func readChannelMonitorV2V0215MigrationRecords(ctx context.Context, db *sql.DB) (map[string]channelMonitorV2V0215MigrationRecord, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT filename, checksum, applied_at FROM schema_migrations
		WHERE filename = ANY($1)
		ORDER BY filename`, pq.Array([]string{
		"241_channel_monitor_v2_passive_coverage.sql",
		"242_channel_monitor_v2_typesafe.sql",
		"243_finalize_retired_schema_hygiene.sql",
		"244_clear_retired_runtime_data.sql",
		channelMonitorV2V0215MigrationFilename,
	}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]channelMonitorV2V0215MigrationRecord, 5)
	for rows.Next() {
		var name string
		var record channelMonitorV2V0215MigrationRecord
		if err := rows.Scan(&name, &record.checksum, &record.applied); err != nil {
			return nil, err
		}
		result[name] = record
	}
	return result, rows.Err()
}

func requireChannelMonitorV2V0215MigrationRecordsEqual(t *testing.T, want, got map[string]channelMonitorV2V0215MigrationRecord) {
	t.Helper()
	require.Equal(t, want, got, "historical migration checksum/applied_at records must not change")
}

func channelMonitorV2V0215DefaultPlatformConfig() []service.ChannelMonitorV2PlatformConfig {
	platforms := make([]service.ChannelMonitorV2PlatformConfig, 0, len(channelMonitorV2V0215Platforms))
	for _, platform := range channelMonitorV2V0215Platforms {
		platforms = append(platforms, service.ChannelMonitorV2PlatformConfig{Platform: platform, Enabled: true, Models: []string{}})
	}
	return platforms
}

type channelMonitorV2V0215SQLWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func insertChannelMonitorV2V0215Group(ctx context.Context, t *testing.T, db channelMonitorV2V0215SQLWriter, name, platform, status string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO groups (name, platform, status)
		VALUES ($1, $2, $3)
		RETURNING id`, name, platform, status).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertChannelMonitorV2V0215Account(ctx context.Context, t *testing.T, db channelMonitorV2V0215SQLWriter, name, platform string) int64 {
	t.Helper()
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO accounts (name, platform, type, credentials, status)
		VALUES ($1, $2, 'apikey', '{}'::jsonb, 'active')
		RETURNING id`, name, platform).Scan(&id)
	require.NoError(t, err)
	return id
}

func insertChannelMonitorV2V0215Fact(ctx context.Context, t *testing.T, db channelMonitorV2V0215SQLWriter, bucket time.Time, platform string, groupID int64, model string, requests int64) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO channel_monitor_v2_metrics_1m (bucket_start, platform, group_id, model, success_requests, input_tokens)
		VALUES ($1, $2, $3, $4, $5, $6)`, bucket, platform, groupID, model, requests, requests*10)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO channel_monitor_v2_metrics_rollup (bucket_start, bucket_seconds, platform, group_id, model, success_requests, input_tokens)
		VALUES ($1, 300, $2, $3, $4, $5, $6)`, bucket, platform, groupID, model, requests, requests*10)
	require.NoError(t, err)
}

func commitChannelMonitorV2V0215(ctx context.Context, t *testing.T, db *sql.DB, seed func(*sql.Tx)) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	seed(tx)
	require.NoError(t, tx.Commit(), "commit isolated repository fixture before reading")
}

func writeChannelMonitorV2V0215Platforms(ctx context.Context, t *testing.T, db channelMonitorV2V0215SQLWriter, platforms []service.ChannelMonitorV2PlatformConfig, groupIDs []int64, mode string) {
	t.Helper()
	encoded, err := json.Marshal(platforms)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		UPDATE channel_monitor_v2_config
		SET platforms = $1, group_ids = $2, group_scope_mode = $3, version = version + 1, updated_at = NOW()
		WHERE id = 1`, encoded, pq.Array(groupIDs), mode)
	require.NoError(t, err)
}

func migrationDefaultV0215Twelve() []map[string]any {
	platforms := []map[string]any{}
	for _, platform := range []string{
		"anthropic", "openai", "gemini", "antigravity", "grok", "kimi", "zhipu",
		"deepseek", "minimax", "opencode_go", "typesafe", "composite",
	} {
		platforms = append(platforms, defaultV0215Platform(platform))
	}
	platforms[4]["enabled"] = false
	platforms[4]["models"] = []string{"custom-grok-model"}
	platforms[1]["models"] = []string{"custom-openai-model"}
	return platforms
}

func requireV0215MigrationAbsent(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)", channelMonitorV2V0215MigrationFilename).Scan(&exists)
	require.NoError(t, err)
	require.False(t, exists, "baseline FS must stop before 245")
}

func verifyChannelMonitorV2V0215PlatformDefault(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	var expression string
	err := db.QueryRowContext(ctx, `
		SELECT pg_catalog.pg_get_expr(default_value.adbin, default_value.adrelid)
		FROM pg_catalog.pg_attrdef default_value
		JOIN pg_catalog.pg_class table_info ON table_info.oid = default_value.adrelid
		JOIN pg_catalog.pg_namespace schema_info ON schema_info.oid = table_info.relnamespace
		JOIN pg_catalog.pg_attribute column_info
		  ON column_info.attrelid = table_info.oid AND column_info.attnum = default_value.adnum
		WHERE schema_info.nspname = 'public'
		  AND table_info.relname = 'channel_monitor_v2_config'
		  AND column_info.attname = 'platforms'`).Scan(&expression)
	require.NoError(t, err)
	expression = strings.TrimSpace(expression)
	for strings.HasPrefix(expression, "(") && strings.HasSuffix(expression, ")") {
		expression = strings.TrimSpace(expression[1 : len(expression)-1])
	}
	match := regexp.MustCompile(`^'((?:[^']|'')*)'::jsonb$`).FindStringSubmatch(expression)
	require.Len(t, match, 2, "unexpected pg_catalog default expression: %s", expression)
	var defaults []struct {
		Platform string   `json:"platform"`
		Enabled  bool     `json:"enabled"`
		Models   []string `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.ReplaceAll(match[1], "''", "'")), &defaults))
	actual := make([]service.ChannelMonitorV2PlatformConfig, 0, len(defaults))
	for _, platform := range defaults {
		actual = append(actual, service.ChannelMonitorV2PlatformConfig{
			Platform: platform.Platform,
			Enabled:  platform.Enabled,
			Models:   platform.Models,
		})
	}
	require.Equal(t, channelMonitorV2V0215DefaultPlatformConfig(), actual)
}

func ensureV0215OnlyDatabaseName(name string) error {
	if !regexp.MustCompile(`^cmv2_v0215_[a-f0-9]{16}$`).MatchString(name) {
		return fmt.Errorf("unexpected isolated database name %q", name)
	}
	return nil
}

func TestChannelMonitorV2V0215PlatformReadsWithoutConfigSave(t *testing.T) {
	t.Run("old database upgrade is visible to repository without PUT", func(t *testing.T) {
		fixture := newChannelMonitorV2V0215TestDatabase(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		baseline := channelMonitorV2V0215BaselineFS(t)
		require.NoError(t, applyMigrationsFS(ctx, fixture.db, baseline))
		requireV0215MigrationAbsent(t, ctx, fixture.db)

		end := time.Now().UTC().Truncate(5 * time.Minute)
		start := end.Add(-90 * time.Minute)
		factBucket := end.Add(-5 * time.Minute)
		var groupIDs struct {
			commandLive, commandZero int64
			clineLive, clineZero     int64
			composite, deletedCline  int64
		}
		var accountIDs struct{ command, cline int64 }
		commitChannelMonitorV2V0215(ctx, t, fixture.db, func(tx *sql.Tx) {
			platforms, err := json.Marshal(migrationDefaultV0215Twelve())
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `
				UPDATE channel_monitor_v2_config
				SET version = 40, enabled = TRUE, refresh_interval_seconds = 300,
				    platforms = $1, group_ids = '{}', group_scope_mode = 'all',
				    health_thresholds = '{"minimum_sample":5,"operator_flag":"preserve"}'::jsonb,
				    ignored_error_categories = '{"timeout"}', updated_by = 9012,
				    updated_at = '2024-01-02T03:04:05Z'::timestamptz
				WHERE id = 1`, platforms)
			require.NoError(t, err)

			groupIDs.commandLive = insertChannelMonitorV2V0215Group(ctx, t, tx, "CMV2 V0215 Command Live", "command_code", "active")
			groupIDs.commandZero = insertChannelMonitorV2V0215Group(ctx, t, tx, "CMV2 V0215 Command Zero", "command_code", "disabled")
			groupIDs.clineLive = insertChannelMonitorV2V0215Group(ctx, t, tx, "CMV2 V0215 Cline Live", "cline", "active")
			groupIDs.clineZero = insertChannelMonitorV2V0215Group(ctx, t, tx, "CMV2 V0215 Cline Zero", "cline", "disabled")
			groupIDs.composite = insertChannelMonitorV2V0215Group(ctx, t, tx, "CMV2 V0215 Composite", "composite", "active")
			groupIDs.deletedCline = insertChannelMonitorV2V0215Group(ctx, t, tx, "CMV2 V0215 Deleted Cline", "cline", "active")
			groupInputPrice := 0.000000123456
			groupOutputPrice := 0.000000234567
			groupModelPricing, err := json.Marshal([]service.ChannelModelPricing{{
				Platform:    "command_code",
				Models:      []string{"migration-group-price-sentinel"},
				BillingMode: service.BillingModeToken,
				InputPrice:  &groupInputPrice,
				OutputPrice: &groupOutputPrice,
				Intervals:   []service.PricingInterval{},
			}})
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `
				UPDATE groups
				SET model_pricing = $2::jsonb, image_price_1k = 0.00001234, rate_multiplier = 1.2345
				WHERE id = $1`, groupIDs.commandLive, groupModelPricing)
			require.NoError(t, err, "seed non-zero group pricing sentinels")

			var pricingChannelID int64
			err = tx.QueryRowContext(ctx, `
				INSERT INTO channels (name, status)
				VALUES ('CMV2 V0215 Migration Pricing Channel', 'active')
				RETURNING id`).Scan(&pricingChannelID)
			require.NoError(t, err)
			for _, groupID := range []int64{groupIDs.commandLive, groupIDs.clineLive} {
				_, err = tx.ExecContext(ctx, `INSERT INTO channel_groups (channel_id, group_id) VALUES ($1, $2)`, pricingChannelID, groupID)
				require.NoError(t, err)
			}
			for _, pricing := range []struct {
				platform   string
				model      string
				input      string
				output     string
				cacheWrite string
				cacheRead  string
			}{
				{platform: "command_code", model: "migration-command-price-sentinel", input: "0.000000123456", output: "0.000000234567", cacheWrite: "0.000000345678", cacheRead: "0.000000456789"},
				{platform: "cline", model: "migration-cline-price-sentinel", input: "0.000000567890", output: "0.000000678901", cacheWrite: "0.000000789012", cacheRead: "0.000000890123"},
			} {
				modelsJSON, marshalErr := json.Marshal([]string{pricing.model})
				require.NoError(t, marshalErr)
				_, err = tx.ExecContext(ctx, `
					INSERT INTO channel_model_pricing
						(channel_id, platform, models, input_price, output_price, cache_write_price, cache_read_price)
					VALUES ($1, $2, $3::jsonb, $4::numeric, $5::numeric, $6::numeric, $7::numeric)`,
					pricingChannelID, pricing.platform, modelsJSON,
					pricing.input, pricing.output, pricing.cacheWrite, pricing.cacheRead)
				require.NoError(t, err, "seed non-zero %s channel pricing sentinels", pricing.platform)
			}
			accountIDs.command = insertChannelMonitorV2V0215Account(ctx, t, tx, "CMV2 V0215 Command Member", "command_code")
			accountIDs.cline = insertChannelMonitorV2V0215Account(ctx, t, tx, "CMV2 V0215 Cline Member", "cline")
			for _, member := range []struct{ account, group int64 }{
				{accountIDs.command, groupIDs.composite},
				{accountIDs.cline, groupIDs.composite},
			} {
				_, err = tx.ExecContext(ctx, `INSERT INTO account_groups (account_id, group_id) VALUES ($1, $2)`, member.account, member.group)
				require.NoError(t, err)
			}
			insertChannelMonitorV2V0215Fact(ctx, t, tx, factBucket, "command_code", groupIDs.commandLive, "command-live-model", 3)
			insertChannelMonitorV2V0215Fact(ctx, t, tx, factBucket, "cline", groupIDs.clineLive, "cline-live-model", 5)
			insertChannelMonitorV2V0215Fact(ctx, t, tx, factBucket, "command_code", groupIDs.composite, "composite-command-model", 2)
			insertChannelMonitorV2V0215Fact(ctx, t, tx, factBucket, "cline", groupIDs.deletedCline, "deleted-cline-model", 11)
			_, err = tx.ExecContext(ctx, `
				UPDATE channel_monitor_v2_watermarks
				SET usage_coverage_start = $1, error_coverage_start = $1, data_through = $2,
				    last_successful_at = $3, backfill_cursor = $1, updated_at = $3
				WHERE id = 1`, start, end, end)
			require.NoError(t, err)
		})

		repo := NewChannelMonitorV2Repository(fixture.db)
		beforeMigration, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		require.Len(t, beforeMigration.Platforms, 12)
		watermarkBefore, err := repo.GetAggregationWatermark(ctx)
		require.NoError(t, err)
		var sampleCountBefore int
		require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_monitor_v2_metrics_1m`).Scan(&sampleCountBefore))
		historyBefore, err := readChannelMonitorV2V0215MigrationRecords(ctx, fixture.db)
		require.NoError(t, err)
		require.Len(t, historyBefore, 4)
		sideEffectsBefore, err := readChannelMonitorV2V0215SideEffectSnapshot(ctx, fixture.db)
		require.NoError(t, err, "capture actual price/sample/watermark rows before 245")
		require.Contains(t, sideEffectsBefore.GroupPricing, "migration-group-price-sentinel")
		require.Contains(t, sideEffectsBefore.GroupPricing, "0.00001234")
		require.Contains(t, sideEffectsBefore.GroupPricing, "1.2345")
		require.Contains(t, sideEffectsBefore.ChannelModelPrices, "0.000000123456")
		require.Contains(t, sideEffectsBefore.ChannelModelPrices, "0.000000890123")
		require.NotEqual(t, "[]", sideEffectsBefore.Samples["channel_monitor_v2_metrics_1m"], "1m sample sentinel must be present")
		require.NotEqual(t, "[]", sideEffectsBefore.Samples["channel_monitor_v2_metrics_rollup"], "300-second rollup sentinel must be present")

		require.NoError(t, ApplyMigrations(ctx, fixture.db), "runner applies and commits migration 245")
		migrationRecordsAfterApply, err := readChannelMonitorV2V0215MigrationRecords(ctx, fixture.db)
		require.NoError(t, err)
		require.Len(t, migrationRecordsAfterApply, 5)
		require.Contains(t, migrationRecordsAfterApply, channelMonitorV2V0215MigrationFilename)
		requireChannelMonitorV2V0215HistoryPreserved(t, historyBefore, migrationRecordsAfterApply)
		requireChannelMonitorV2V0215MigrationChecksum(t, migrationRecordsAfterApply)
		sideEffectsAfter, err := readChannelMonitorV2V0215SideEffectSnapshot(ctx, fixture.db)
		require.NoError(t, err, "capture actual price/sample/watermark rows after 245")
		requireChannelMonitorV2V0215SideEffectSnapshotEqual(t, sideEffectsBefore, sideEffectsAfter)
		afterMigration, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		require.Len(t, afterMigration.Platforms, 14)
		require.Equal(t, beforeMigration.Version+1, afterMigration.Version)
		require.True(t, afterMigration.Enabled)
		require.Equal(t, 300, afterMigration.RefreshIntervalSeconds)
		require.Equal(t, "all", afterMigration.GroupScopeMode)
		require.Equal(t, beforeMigration.GroupIDs, afterMigration.GroupIDs)
		require.Equal(t, beforeMigration.HealthThresholds, afterMigration.HealthThresholds)
		require.Equal(t, beforeMigration.IgnoredErrorCategories, afterMigration.IgnoredErrorCategories)
		require.Equal(t, beforeMigration.UpdatedBy, afterMigration.UpdatedBy)
		require.Equal(t, beforeMigration.Platforms, afterMigration.Platforms[:12])
		require.Equal(t, service.ChannelMonitorV2PlatformConfig{Platform: "command_code", Enabled: true, Models: []string{}}, afterMigration.Platforms[12])
		require.Equal(t, service.ChannelMonitorV2PlatformConfig{Platform: "cline", Enabled: true, Models: []string{}}, afterMigration.Platforms[13])

		filter := service.ChannelMonitorV2Filter{
			Range: "90m", Start: start, End: end, Bucket: 5 * time.Minute,
		}
		dimensions, err := repo.GetDimensions(ctx, filter, *afterMigration)
		require.NoError(t, err)
		dimensionGroups := make(map[int64]service.ChannelMonitorV2GroupDimension, len(dimensions.Groups))
		for _, item := range dimensions.Groups {
			dimensionGroups[item.ID] = item
		}
		for _, id := range []int64{groupIDs.commandLive, groupIDs.commandZero, groupIDs.clineLive, groupIDs.clineZero, groupIDs.composite, groupIDs.deletedCline} {
			require.Contains(t, dimensionGroups, id)
		}
		require.EqualValues(t, 3, dimensionGroups[groupIDs.commandLive].RequestCount)
		require.Zero(t, dimensionGroups[groupIDs.commandZero].RequestCount)
		require.EqualValues(t, 5, dimensionGroups[groupIDs.clineLive].RequestCount)
		require.Zero(t, dimensionGroups[groupIDs.clineZero].RequestCount)
		require.EqualValues(t, 2, dimensionGroups[groupIDs.composite].RequestCount)
		require.EqualValues(t, 11, dimensionGroups[groupIDs.deletedCline].RequestCount)
		require.ElementsMatch(t, []string{"command_code", "cline"}, dimensionGroups[groupIDs.composite].Platforms)

		matrix, err := repo.GetMatrix(ctx, filter, *afterMigration, service.ChannelMonitorV2GroupByPlatformGroup, true)
		require.NoError(t, err)
		requireV0215MatrixMetric(t, matrix, "command_code", groupIDs.commandLive, 3)
		requireV0215MatrixMetric(t, matrix, "command_code", groupIDs.commandZero, 0)
		requireV0215MatrixMetric(t, matrix, "cline", groupIDs.clineLive, 5)
		requireV0215MatrixMetric(t, matrix, "cline", groupIDs.clineZero, 0)
		requireV0215MatrixMetric(t, matrix, "command_code", groupIDs.composite, 2)
		requireV0215MatrixMetric(t, matrix, "cline", groupIDs.composite, 0)
		requireV0215MatrixMetric(t, matrix, "cline", groupIDs.deletedCline, 11)
		snapshot, err := repo.GetSnapshot(ctx, filter, *afterMigration, true)
		require.NoError(t, err)
		require.EqualValues(t, 21, snapshot.Metrics.RequestCount)
		require.Equal(t, 14, len(snapshot.Config.Platforms))
		var sampleCountAfter int
		require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_monitor_v2_metrics_1m`).Scan(&sampleCountAfter))
		require.Equal(t, sampleCountBefore, sampleCountAfter, "migration must not rewrite real samples")
		watermarkAfter, err := repo.GetAggregationWatermark(ctx)
		require.NoError(t, err)
		require.Equal(t, watermarkBefore, watermarkAfter, "migration must not move coverage watermarks")

		// The deleted group's historical sample remains stored while reads exclude it.
		var deletedFacts int
		require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel_monitor_v2_metrics_1m WHERE group_id = $1`, groupIDs.deletedCline).Scan(&deletedFacts))
		require.Equal(t, 1, deletedFacts)
		commitChannelMonitorV2V0215(ctx, t, fixture.db, func(tx *sql.Tx) {
			_, err := tx.ExecContext(ctx, `UPDATE groups SET deleted_at = NOW() WHERE id = $1`, groupIDs.deletedCline)
			require.NoError(t, err)
		})
		afterDelete, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		dimensions, err = repo.GetDimensions(ctx, filter, *afterDelete)
		require.NoError(t, err)
		require.NotContains(t, dimensionGroupIDs(dimensions), groupIDs.deletedCline)
		matrix, err = repo.GetMatrix(ctx, filter, *afterDelete, service.ChannelMonitorV2GroupByPlatformGroup, true)
		require.NoError(t, err)
		require.NotContains(t, matrixGroupIDs(matrix), groupIDs.deletedCline)
		snapshot, err = repo.GetSnapshot(ctx, filter, *afterDelete, true)
		require.NoError(t, err)
		require.EqualValues(t, 10, snapshot.Metrics.RequestCount, "soft-deleted group facts must not contribute to totals")

		disabled := append([]service.ChannelMonitorV2PlatformConfig(nil), afterDelete.Platforms...)
		for i := range disabled {
			if disabled[i].Platform == "command_code" {
				disabled[i].Enabled = false
			}
		}
		commitChannelMonitorV2V0215(ctx, t, fixture.db, func(tx *sql.Tx) {
			writeChannelMonitorV2V0215Platforms(ctx, t, tx, disabled, []int64{}, "all")
		})
		disabledConfig, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		dimensions, err = repo.GetDimensions(ctx, filter, *disabledConfig)
		require.NoError(t, err)
		require.NotContains(t, dimensionPlatformIDs(dimensions), "command_code")
		require.Contains(t, dimensionPlatformIDs(dimensions), "cline")
		matrix, err = repo.GetMatrix(ctx, filter, *disabledConfig, service.ChannelMonitorV2GroupByPlatformGroup, true)
		require.NoError(t, err)
		require.NotContains(t, matrixPlatformGroupPairs(matrix), "command_code/"+fmt.Sprint(groupIDs.commandLive))
		requireV0215MatrixMetric(t, matrix, "cline", groupIDs.clineLive, 5)

		reenabled := append([]service.ChannelMonitorV2PlatformConfig(nil), disabledConfig.Platforms...)
		for i := range reenabled {
			if reenabled[i].Platform == "command_code" {
				reenabled[i].Enabled = true
			}
		}
		configuredIDs := []int64{groupIDs.commandLive, groupIDs.commandZero, groupIDs.clineLive, groupIDs.clineZero, groupIDs.composite}
		commitChannelMonitorV2V0215(ctx, t, fixture.db, func(tx *sql.Tx) {
			writeChannelMonitorV2V0215Platforms(ctx, t, tx, reenabled, configuredIDs, "selected")
		})
		selectedConfig, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		viewerFilter := filter
		viewerFilter.GroupIDs = []int64{groupIDs.commandLive, groupIDs.clineLive}
		viewerFilter.AllowedGroupIDs = []int64{groupIDs.commandLive}
		viewerFilter.RestrictGroups = true
		dimensions, err = repo.GetDimensions(ctx, viewerFilter, *selectedConfig)
		require.NoError(t, err)
		require.Equal(t, []int64{groupIDs.commandLive}, dimensionGroupIDs(dimensions))
		matrix, err = repo.GetMatrix(ctx, viewerFilter, *selectedConfig, service.ChannelMonitorV2GroupByPlatformGroup, false)
		require.NoError(t, err)
		require.Equal(t, []int64{groupIDs.commandLive}, matrixGroupIDs(matrix))
		snapshot, err = repo.GetSnapshot(ctx, viewerFilter, *selectedConfig, false)
		require.NoError(t, err)
		require.EqualValues(t, 3, snapshot.Metrics.RequestCount)

		viewerFilter.AllowedGroupIDs = []int64{}
		dimensions, err = repo.GetDimensions(ctx, viewerFilter, *selectedConfig)
		require.NoError(t, err)
		require.Empty(t, dimensions.Groups)
		matrix, err = repo.GetMatrix(ctx, viewerFilter, *selectedConfig, service.ChannelMonitorV2GroupByPlatformGroup, false)
		require.NoError(t, err)
		require.Empty(t, matrix.Items)
		snapshot, err = repo.GetSnapshot(ctx, viewerFilter, *selectedConfig, false)
		require.NoError(t, err)
		require.Zero(t, snapshot.Metrics.RequestCount)

		commitChannelMonitorV2V0215(ctx, t, fixture.db, func(tx *sql.Tx) {
			writeChannelMonitorV2V0215Platforms(ctx, t, tx, reenabled, []int64{}, "selected")
		})
		emptySelectedConfig, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		dimensions, err = repo.GetDimensions(ctx, filter, *emptySelectedConfig)
		require.NoError(t, err)
		require.Empty(t, dimensions.Groups)
		matrix, err = repo.GetMatrix(ctx, filter, *emptySelectedConfig, service.ChannelMonitorV2GroupByPlatformGroup, true)
		require.NoError(t, err)
		require.Empty(t, matrix.Items)
		snapshot, err = repo.GetSnapshot(ctx, filter, *emptySelectedConfig, true)
		require.NoError(t, err)
		require.Zero(t, snapshot.Metrics.RequestCount)

		// Direct SQL replay is committed in a distinct transaction, then the normal
		// runner verifies that the recorded migration is skipped without changing it.
		var migrationSQL []byte
		migrationSQL, err = dbmigrations.FS.ReadFile(channelMonitorV2V0215MigrationFilename)
		require.NoError(t, err)
		beforeReplay, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		beforeDirectReplay := readV0215ConfigSnapshot(t, ctx, fixture.db)
		replayTx, err := fixture.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = replayTx.Rollback() })
		_, err = replayTx.ExecContext(ctx, string(migrationSQL))
		require.NoError(t, err)
		require.NoError(t, replayTx.Commit())
		afterDirectReplay := readV0215ConfigSnapshot(t, ctx, fixture.db)
		require.Equal(t, beforeDirectReplay, afterDirectReplay, "direct replay must preserve every config field and metadata")
		afterReplay, err := repo.GetConfig(ctx)
		require.NoError(t, err)
		require.Equal(t, beforeReplay.Platforms, afterReplay.Platforms)
		require.Equal(t, beforeReplay.Version, afterReplay.Version)
		require.Equal(t, beforeReplay.UpdatedAt, afterReplay.UpdatedAt)
		recordsBeforeRunner, err := readChannelMonitorV2V0215MigrationRecords(ctx, fixture.db)
		require.NoError(t, err)
		requireChannelMonitorV2V0215MigrationRecordsEqual(t, migrationRecordsAfterApply, recordsBeforeRunner)
		require.NoError(t, ApplyMigrations(ctx, fixture.db))
		recordsAfterRunner, err := readChannelMonitorV2V0215MigrationRecords(ctx, fixture.db)
		require.NoError(t, err)
		requireChannelMonitorV2V0215MigrationRecordsEqual(t, migrationRecordsAfterApply, recordsAfterRunner)
		var has245 bool
		require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE filename = $1)`, channelMonitorV2V0215MigrationFilename).Scan(&has245))
		require.True(t, has245)
	})

	t.Run("new install preserves historical singleton and validates defaults and record independently", func(t *testing.T) {
		fixture := newChannelMonitorV2V0215TestDatabase(t)
		oracle := newChannelMonitorV2V0215TestDatabase(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		require.NoError(t, ApplyMigrations(ctx, fixture.db), "run the complete chain on the fresh install database")
		require.NoError(t, applyMigrationsFS(ctx, oracle.db, channelMonitorV2V0215BaselineFS(t)), "run original migration bytes through 244 in the independent oracle database")

		var historicalJSON, actualJSON []byte
		require.NoError(t, oracle.db.QueryRowContext(ctx, `SELECT platforms FROM channel_monitor_v2_config WHERE id = 1`).Scan(&historicalJSON))
		require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT platforms FROM channel_monitor_v2_config WHERE id = 1`).Scan(&actualJSON))
		var historicalPlatforms, actualPlatforms []json.RawMessage
		require.NoError(t, json.Unmarshal(historicalJSON, &historicalPlatforms))
		require.NoError(t, json.Unmarshal(actualJSON, &actualPlatforms))

		t.Run("singleton preserves history and appends only the missing platforms", func(t *testing.T) {
			require.NotEmpty(t, historicalPlatforms)
			require.Len(t, actualPlatforms, len(historicalPlatforms)+2)
			var actualEntries []struct {
				Platform string `json:"platform"`
			}
			require.NoError(t, json.Unmarshal(actualJSON, &actualEntries))
			actualSet := make(map[string]struct{}, len(actualEntries))
			for _, entry := range actualEntries {
				_, duplicate := actualSet[entry.Platform]
				require.False(t, duplicate, "singleton must not contain duplicate platform %q", entry.Platform)
				actualSet[entry.Platform] = struct{}{}
			}
			require.Len(t, actualSet, len(channelMonitorV2V0215Platforms))
			actualPlatformNames := make([]string, 0, len(actualSet))
			for platform := range actualSet {
				actualPlatformNames = append(actualPlatformNames, platform)
			}
			require.ElementsMatch(t, channelMonitorV2V0215Platforms, actualPlatformNames)
			for index, historical := range historicalPlatforms {
				require.JSONEq(t, string(historical), string(actualPlatforms[index]), "historical singleton object/order changed at index %d", index)
			}
			require.JSONEq(t, `{"platform":"command_code","enabled":true,"models":[]}`, string(actualPlatforms[len(historicalPlatforms)]))
			require.JSONEq(t, `{"platform":"cline","enabled":true,"models":[]}`, string(actualPlatforms[len(historicalPlatforms)+1]))
		})

		t.Run("column default is the complete catalog in directory order", func(t *testing.T) {
			verifyChannelMonitorV2V0215PlatformDefault(t, ctx, fixture.db)
		})

		t.Run("245 record has the expected checksum and runner replay is stable", func(t *testing.T) {
			recordsBeforeReplay, err := readChannelMonitorV2V0215MigrationRecords(ctx, fixture.db)
			require.NoError(t, err)
			require.Len(t, recordsBeforeReplay, 5)
			requireChannelMonitorV2V0215MigrationChecksum(t, recordsBeforeReplay)
			require.NoError(t, ApplyMigrations(ctx, fixture.db), "second full runner call skips the applied 245 record")
			recordsAfterReplay, err := readChannelMonitorV2V0215MigrationRecords(ctx, fixture.db)
			require.NoError(t, err)
			requireChannelMonitorV2V0215MigrationRecordsEqual(t, recordsBeforeReplay, recordsAfterReplay)
			requireChannelMonitorV2V0215MigrationChecksum(t, recordsAfterReplay)
		})
	})
}

func requireV0215MatrixMetric(t *testing.T, matrix *service.ChannelMonitorV2Matrix, platform string, groupID int64, requests int64) {
	t.Helper()
	var found *service.ChannelMonitorV2MatrixRow
	for i := range matrix.Items {
		row := &matrix.Items[i]
		if row.Platform == platform && row.GroupID != nil && *row.GroupID == groupID {
			found = row
			break
		}
	}
	require.NotNil(t, found, "expected %s group %d matrix row", platform, groupID)
	require.EqualValues(t, requests, found.Metrics.RequestCount)
	if requests == 0 {
		require.Equal(t, "unknown", found.Health.Overall)
		require.Zero(t, found.Metrics.TTFT.SampleCount)
	}
}

func matrixGroupIDs(matrix *service.ChannelMonitorV2Matrix) []int64 {
	ids := make([]int64, 0, len(matrix.Items))
	seen := make(map[int64]struct{}, len(matrix.Items))
	for _, row := range matrix.Items {
		if row.GroupID == nil {
			continue
		}
		if _, ok := seen[*row.GroupID]; ok {
			continue
		}
		seen[*row.GroupID] = struct{}{}
		ids = append(ids, *row.GroupID)
	}
	return ids
}

func dimensionGroupIDs(dimensions *service.ChannelMonitorV2Dimensions) []int64 {
	ids := make([]int64, 0, len(dimensions.Groups))
	for _, group := range dimensions.Groups {
		ids = append(ids, group.ID)
	}
	return ids
}

func dimensionPlatformIDs(dimensions *service.ChannelMonitorV2Dimensions) []string {
	ids := make([]string, 0, len(dimensions.Platforms))
	for _, platform := range dimensions.Platforms {
		ids = append(ids, platform.Value)
	}
	return ids
}

func matrixPlatformGroupPairs(matrix *service.ChannelMonitorV2Matrix) []string {
	pairs := make([]string, 0, len(matrix.Items))
	for _, row := range matrix.Items {
		if row.GroupID != nil {
			pairs = append(pairs, row.Platform+"/"+fmt.Sprint(*row.GroupID))
		}
	}
	return pairs
}
