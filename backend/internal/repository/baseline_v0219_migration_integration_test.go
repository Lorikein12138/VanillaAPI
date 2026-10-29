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

const (
	baselineV0219ScopedAdminKeysMigrationName = "247_scoped_admin_api_keys.sql"
	baselineV0219ImageCountMigrationName      = "248_group_image_count_billing.sql"
)

func baselineV0219RequireLedger(t *testing.T, ctx context.Context, db *sql.DB) map[string]baselineV0216MigrationRecord {
	t.Helper()
	records, err := baselineV0216ReadMigrationRecords(ctx, db)
	require.NoError(t, err)
	require.Len(t, records, 304)
	for name := range baselineV0218PreviousFS(t) {
		require.Contains(t, records, name)
	}
	for _, name := range []string{baselineV0219ScopedAdminKeysMigrationName, baselineV0219ImageCountMigrationName} {
		data, readErr := dbmigrations.FS.ReadFile(name)
		require.NoError(t, readErr)
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(data))))
		require.Equal(t, hex.EncodeToString(sum[:]), records[name].checksum, "%s ledger uses trimmed SQL checksum", name)
	}
	return records
}

func baselineV0219BusinessSentinels(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	var snapshot []string
	for _, query := range []string{
		`SELECT to_jsonb(u)::text FROM users u WHERE email='baseline-v0217@example.test'`,
		`SELECT to_jsonb(a)::text FROM accounts a WHERE name='baseline v0217'`,
		`SELECT to_jsonb(k)::text FROM api_keys k WHERE key='sk-baseline-v0217'`,
		`SELECT COALESCE(jsonb_agg(to_jsonb(u) ORDER BY id),'[]')::text FROM usage_logs u`,
		`SELECT COALESCE(jsonb_agg(to_jsonb(g) - 'image_count_billing' ORDER BY g.name),'[]')::text FROM groups g WHERE g.name IN ('baseline v0217','baseline v0219 old image true','baseline v0219 old image false')`,
		`SELECT to_jsonb(s)::text FROM settings s WHERE key='PAYMENT_QUICK_AMOUNTS'`,
		`SELECT to_jsonb(c)::text FROM channel_monitor_v2_config c WHERE id=1`,
		`SELECT to_jsonb(c)::text FROM gemini_cached_contents c WHERE public_id='baseline-v0217-cache'`,
	} {
		snapshot = append(snapshot, baselineV0216ReadJSON(t, ctx, db, query))
	}
	return snapshot
}

func baselineV0219RequireDefaultFalse(t *testing.T, ctx context.Context, db *sql.DB, groupName string) {
	t.Helper()
	var enabled bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT image_count_billing FROM groups WHERE name=$1`, groupName).Scan(&enabled))
	require.False(t, enabled, "image_count_billing must default false for %s", groupName)
}

func baselineV0219SeedDefaultGroup(t *testing.T, ctx context.Context, db *sql.DB, name string) {
	t.Helper()
	_, err := db.ExecContext(ctx, `
		INSERT INTO groups (name,platform,status,rate_multiplier,model_pricing,model_allowlist,allow_image_generation)
		VALUES ($1,'openai','active',1,'[]'::jsonb,'{"enabled":true,"models":[]}'::jsonb,true)`, name)
	require.NoError(t, err)
	baselineV0219RequireDefaultFalse(t, ctx, db, name)
}

func baselineV0219ReplayNewMigrations(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, name := range []string{baselineV0219ScopedAdminKeysMigrationName, baselineV0219ImageCountMigrationName} {
		data, err := dbmigrations.FS.ReadFile(name)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, string(data))
		require.NoError(t, err, "%s must be directly replayable", name)
	}
}

func baselineV0219InsertScopedKeyAuditRow(t *testing.T, ctx context.Context, db *sql.DB, userID int64) string {
	t.Helper()
	plainKey := "adm_sc_baseline_v0219_plaintext_must_not_persist"
	digest := sha256.Sum256([]byte(plainKey))
	keyHash := hex.EncodeToString(digest[:])
	_, err := db.ExecContext(ctx, `
		INSERT INTO scoped_admin_api_keys (id,name,key_prefix,key_hash,created_by)
		VALUES ('baseln0219key000000001','baseline key','adm_sc_baseline', $1, $2)`, keyHash, userID)
	require.NoError(t, err)
	var storedHash string
	var storedRow string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT key_hash,to_jsonb(k)::text FROM scoped_admin_api_keys k WHERE id='baseln0219key000000001'`).Scan(&storedHash, &storedRow))
	require.Equal(t, keyHash, storedHash)
	require.NotContains(t, storedRow, plainKey, "only the digest may be persisted")
	_, err = db.ExecContext(ctx, `UPDATE scoped_admin_api_keys SET revoked_at=NOW(),deleted_at=NOW() WHERE id='baseln0219key000000001'`)
	require.NoError(t, err)
	return keyHash
}

func baselineV0219RequireScopedKeyAuditRow(t *testing.T, ctx context.Context, db *sql.DB, wantHash string) {
	t.Helper()
	var gotHash string
	var revokedAt, deletedAt sql.NullTime
	require.NoError(t, db.QueryRowContext(ctx, `SELECT key_hash,revoked_at,deleted_at FROM scoped_admin_api_keys WHERE id='baseln0219key000000001'`).Scan(&gotHash, &revokedAt, &deletedAt))
	require.Equal(t, wantHash, gotHash)
	require.True(t, revokedAt.Valid)
	require.True(t, deletedAt.Valid)
}

func TestBaselineV0219FreshInstallSecondStartupAndMigrationReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	harnessDatabase, harnessConfig := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	first := baselineV0219RequireLedger(t, ctx, fixture.db)

	baselineV0219SeedDefaultGroup(t, ctx, fixture.db, "baseline-v0219-fresh-default")
	_, err := fixture.db.ExecContext(ctx, `UPDATE groups SET image_count_billing=true WHERE name='baseline-v0219-fresh-default'`)
	require.NoError(t, err)
	baselineV0219ReplayNewMigrations(t, ctx, fixture.db)
	second := baselineV0219RequireLedger(t, ctx, fixture.db)
	require.Equal(t, first, second, "direct SQL replay must not add or alter migration ledger entries")

	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	third := baselineV0219RequireLedger(t, ctx, fixture.db)
	require.Equal(t, first, third, "second startup must keep every checksum and applied_at unchanged")
	var enabled bool
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT image_count_billing FROM groups WHERE name='baseline-v0219-fresh-default'`).Scan(&enabled))
	require.True(t, enabled, "a later administrator setting must survive startup")
	baselineV0216RequireHarnessUnchanged(t, ctx, harnessDatabase, harnessConfig)
}

func TestBaselineV0219UpgradeFrom302PreservesBusinessRowsAndAppliesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	harnessDatabase, harnessConfig := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0218PreviousFS(t)))
	before, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, before, 302)

	baselineV0217Seed(t, ctx, fixture.db)
	_, err = fixture.db.ExecContext(ctx, `UPDATE groups SET allow_image_generation=true WHERE name='baseline v0217'`)
	require.NoError(t, err)
	for _, seed := range []struct {
		name    string
		enabled bool
	}{
		{name: "baseline v0219 old image true", enabled: true},
		{name: "baseline v0219 old image false", enabled: false},
	} {
		_, err = fixture.db.ExecContext(ctx, `
			INSERT INTO groups (name,platform,status,rate_multiplier,model_pricing,model_allowlist,allow_image_generation)
			VALUES ($1,'openai','active',1,'[]'::jsonb,'{"enabled":true,"models":[]}'::jsonb,$2)`, seed.name, seed.enabled)
		require.NoError(t, err)
	}
	sentinels := baselineV0219BusinessSentinels(t, ctx, fixture.db)

	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	after := baselineV0219RequireLedger(t, ctx, fixture.db)
	for name, record := range before {
		require.Equal(t, record, after[name], "historical checksum/applied_at changed for %s", name)
	}
	baselineV0219RequireDefaultFalse(t, ctx, fixture.db, "baseline v0217")
	baselineV0219RequireDefaultFalse(t, ctx, fixture.db, "baseline v0219 old image true")
	baselineV0219RequireDefaultFalse(t, ctx, fixture.db, "baseline v0219 old image false")
	require.Equal(t, sentinels, baselineV0219BusinessSentinels(t, ctx, fixture.db), "unrelated persisted business rows must not change")

	var userID int64
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT id FROM users WHERE email='baseline-v0217@example.test'`).Scan(&userID))
	keyHash := baselineV0219InsertScopedKeyAuditRow(t, ctx, fixture.db, userID)
	_, err = fixture.db.ExecContext(ctx, `UPDATE groups SET image_count_billing=true WHERE name='baseline v0217'`)
	require.NoError(t, err)
	baselineV0219ReplayNewMigrations(t, ctx, fixture.db)
	baselineV0219RequireScopedKeyAuditRow(t, ctx, fixture.db, keyHash)

	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	second := baselineV0219RequireLedger(t, ctx, fixture.db)
	require.Equal(t, after, second, "second startup must not reapply either new migration")
	baselineV0219RequireScopedKeyAuditRow(t, ctx, fixture.db, keyHash)
	var enabled bool
	require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT image_count_billing FROM groups WHERE name='baseline v0217'`).Scan(&enabled))
	require.True(t, enabled, "the administrator's post-migration setting must survive startup")
	require.Equal(t, sentinels, baselineV0219BusinessSentinels(t, ctx, fixture.db))
	baselineV0216RequireHarnessUnchanged(t, ctx, harnessDatabase, harnessConfig)
}
