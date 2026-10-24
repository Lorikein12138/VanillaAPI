//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

// The fixture is generated from fixed H, never from a modified candidate.
func baselineV0217PreviousFS(t *testing.T) fstest.MapFS {
	t.Helper()
	fixture, err := os.ReadFile("../../migrations/testdata/baseline_v0217_prior_migrations.sha256")
	require.NoError(t, err)
	previous := fstest.MapFS{}
	for _, line := range strings.Split(strings.TrimSpace(string(fixture)), "\n") {
		fields := strings.Fields(line)
		require.Len(t, fields, 2)
		require.NotContains(t, previous, fields[1])
		data, err := dbmigrations.FS.ReadFile(fields[1])
		require.NoError(t, err)
		sum := sha256.Sum256(data)
		require.Equal(t, fields[0], hex.EncodeToString(sum[:]), "old database input must match fixed H: %s", fields[1])
		previous[fields[1]] = &fstest.MapFile{Data: append([]byte(nil), data...)}
	}
	require.Len(t, previous, 301)
	return previous
}

func baselineV0217RequireLedger(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	records, err := baselineV0216ReadMigrationRecords(ctx, db)
	require.NoError(t, err)
	require.Len(t, records, 301)
	previous := baselineV0217PreviousFS(t)
	for name := range previous {
		require.Contains(t, records, name)
	}
	baselineV0216RequireNewSchema(t, ctx, db)
}

func TestBaselineV0217FreshInstall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, config := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0217PreviousFS(t)))
	baselineV0217RequireLedger(t, ctx, fixture.db)
	baselineV0216RequireHarnessUnchanged(t, ctx, database, config)
}

func baselineV0217Seed(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	commitChannelMonitorV2V0215(ctx, t, db, func(tx *sql.Tx) {
		var userID, groupID, accountID, keyID int64
		require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO users (email,password_hash,balance,frozen_balance) VALUES ('baseline-v0217@example.test','hash',123.45678901,7.25) RETURNING id`).Scan(&userID))
		require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO groups (name,platform,status,rate_multiplier,model_pricing,model_allowlist) VALUES ('baseline v0217','gemini','active',1.375,$1::jsonb,'{"enabled":true,"models":["gemini-3.1-pro"]}'::jsonb) RETURNING id`, `[ {"platform":"gemini","models":["gemini-3.1-pro"],"billing_mode":"token","input_price":0.000001,"output_price":0.000002} ]`).Scan(&groupID))
		require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts (name,platform,type,credentials,status) VALUES ('baseline v0217','gemini','apikey','{}','active') RETURNING id`).Scan(&accountID))
		require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO api_keys (user_id,key,name,group_id) VALUES ($1,'sk-baseline-v0217','baseline v0217',$2) RETURNING id`, userID, groupID).Scan(&keyID))
		_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs (user_id,api_key_id,account_id,group_id,model,input_tokens,output_tokens,total_cost,actual_cost) VALUES ($1,$2,$3,$4,'gemini-3.1-pro',17,23,0.0125,0.00975)`, userID, keyID, accountID, groupID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO settings (key,value) VALUES ('PAYMENT_QUICK_AMOUNTS','[10,50,100]') ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value`)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE channel_monitor_v2_config SET group_ids=array_append(group_ids,$1::bigint) WHERE id=1`, groupID)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO gemini_cached_contents (public_id,user_id,api_key_id,group_id,account_id,upstream_name,model,upstream_model,expire_time,channel_usage) VALUES ('baseline-v0217-cache',$1,$2,$3,$4,'cachedContents/sentinel','gemini-3.1-pro','gemini-3.1-pro',NOW()+interval '1 day','{"sentinel":7}')`, userID, keyID, groupID, accountID)
		require.NoError(t, err)
	})
}

func baselineV0217Sentinels(t *testing.T, ctx context.Context, db *sql.DB) []string {
	t.Helper()
	var snapshot []string
	// Whole persisted rows include timestamps as well as business fields.
	for _, query := range []string{
		`SELECT to_jsonb(u)::text FROM users u WHERE email='baseline-v0217@example.test'`,
		`SELECT COALESCE(jsonb_agg(to_jsonb(u) ORDER BY id),'[]')::text FROM usage_logs u`,
		`SELECT to_jsonb(g)::text FROM groups g WHERE name='baseline v0217'`,
		`SELECT to_jsonb(s)::text FROM settings s WHERE key='PAYMENT_QUICK_AMOUNTS'`,
		`SELECT to_jsonb(c)::text FROM channel_monitor_v2_config c WHERE id=1`,
		`SELECT to_jsonb(c)::text FROM gemini_cached_contents c WHERE public_id='baseline-v0217-cache'`,
	} {
		snapshot = append(snapshot, baselineV0216ReadJSON(t, ctx, db, query))
	}
	return snapshot
}

func baselineV0217Upgrade(t *testing.T, secondStartup bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	database, config := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0217PreviousFS(t)))
	baselineV0217Seed(t, ctx, fixture.db)
	records, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, records, 301)
	sentinels := baselineV0217Sentinels(t, ctx, fixture.db)
	startups := 1
	if secondStartup {
		startups = 2
	}
	for i := 0; i < startups; i++ {
		require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0217PreviousFS(t)))
		baselineV0217RequireLedger(t, ctx, fixture.db)
		after, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
		require.NoError(t, err)
		require.Equal(t, records, after, "checksum/applied_at must survive startup %d", i+1)
		require.Equal(t, sentinels, baselineV0217Sentinels(t, ctx, fixture.db), "business sentinels must survive startup %d", i+1)
	}
	baselineV0216RequireHarnessUnchanged(t, ctx, database, config)
}

func TestBaselineV0217UpgradePreservesHistory(t *testing.T) { baselineV0217Upgrade(t, false) }
func TestBaselineV0217SecondStartup(t *testing.T)           { baselineV0217Upgrade(t, true) }
