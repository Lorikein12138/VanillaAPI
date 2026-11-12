//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const (
	baselineV0221CostIndexMigration = "249_add_usage_logs_api_key_cost_index_notx.sql"
	baselineV0221CacheTTLMigration  = "250_openai_cache_write_ttl.sql"
	baselineGroupDisplayMigration   = "249_group_display_category.sql"
	advisorRequestIDMigration       = "251_extend_usage_log_request_id.sql"
)

func baselineV0220FS(t *testing.T) fstest.MapFS {
	t.Helper()
	return readBaselineMigrationFS(t, "baseline_v0221_prior_migrations.sha256", 306)
}

func TestBaselineV0221Migrations(t *testing.T) {
	for _, mode := range []string{"fresh-install", "upgrade-from-306"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			fixture := newChannelMonitorV2V0215TestDatabase(t)
			var before map[string]baselineV0216MigrationRecord
			var sentinels []string
			if mode == "upgrade-from-306" {
				require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0220FS(t)))
				var err error
				before, err = baselineV0216ReadMigrationRecords(ctx, fixture.db)
				require.NoError(t, err)
				require.Len(t, before, 306)
				require.Contains(t, before, baselineGroupDisplayMigration)
				require.Contains(t, before, baselineV0220MCPOAuthMigration)
				require.NotContains(t, before, baselineV0221CostIndexMigration)
				require.NotContains(t, before, baselineV0221CacheTTLMigration)
				baselineV0217Seed(t, ctx, fixture.db)
				_, err = fixture.db.ExecContext(ctx, `UPDATE groups SET display_category='domestic' WHERE name='baseline v0217'`)
				require.NoError(t, err)
				sentinels = baselineV0219BusinessSentinels(t, ctx, fixture.db)
			}

			require.NoError(t, ApplyMigrations(ctx, fixture.db))
			after, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
			require.NoError(t, err)
			require.Len(t, after, 309)
			for name, entry := range before {
				require.Equal(t, entry, after[name], "%s checksum/applied_at", name)
			}
			for _, name := range []string{baselineV0221CostIndexMigration, baselineGroupDisplayMigration, baselineV0220MCPOAuthMigration, baselineV0221CacheTTLMigration, advisorRequestIDMigration} {
				data, readErr := dbmigrations.FS.ReadFile(name)
				require.NoError(t, readErr)
				sum := sha256.Sum256([]byte(strings.TrimSpace(string(data))))
				require.Equal(t, hex.EncodeToString(sum[:]), after[name].checksum, name)
			}
			var requestIDWidth int
			require.NoError(t, fixture.db.QueryRowContext(ctx, `
				SELECT character_maximum_length FROM information_schema.columns
				WHERE table_schema='public' AND table_name='usage_logs' AND column_name='request_id'
			`).Scan(&requestIDWidth))
			require.Equal(t, 255, requestIDWidth)

			var valid bool
			var indexDef string
			require.NoError(t, fixture.db.QueryRowContext(ctx, `
				SELECT indisvalid AND indisready, pg_get_indexdef(indexrelid)
				FROM pg_index WHERE indexrelid='idx_usage_logs_api_key_created_cost'::regclass
			`).Scan(&valid, &indexDef))
			require.True(t, valid)
			require.Contains(t, indexDef, "(api_key_id, created_at) INCLUDE (actual_cost)")

			var priceColumns int
			require.NoError(t, fixture.db.QueryRowContext(ctx, `
				SELECT count(*) FROM information_schema.columns
				WHERE table_schema='public' AND column_name='cache_write_12h_price' AND is_nullable='YES'
					AND table_name IN ('channel_model_pricing', 'channel_pricing_intervals',
						'channel_account_stats_model_pricing', 'channel_account_stats_pricing_intervals')
			`).Scan(&priceColumns))
			require.Equal(t, 4, priceColumns)

			require.NoError(t, ApplyMigrations(ctx, fixture.db))
			restarted, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
			require.NoError(t, err)
			require.Equal(t, after, restarted)

			if mode == "upgrade-from-306" {
				require.Equal(t, sentinels, baselineV0219BusinessSentinels(t, ctx, fixture.db))
				var category string
				require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT display_category FROM groups WHERE name='baseline v0217'`).Scan(&category))
				require.Equal(t, "domestic", category)
				var short, long int
				require.NoError(t, fixture.db.QueryRowContext(ctx, `
					SELECT u.cache_creation_30m_tokens, u.cache_creation_12h_tokens
					FROM usage_logs u JOIN users ON users.id=u.user_id
					WHERE users.email='baseline-v0217@example.test'
				`).Scan(&short, &long))
				require.Zero(t, short)
				require.Zero(t, long)
			}
		})
	}
}
