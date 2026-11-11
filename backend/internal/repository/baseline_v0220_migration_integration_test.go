//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const baselineV0220MCPOAuthMigration = "249_mcp_oauth.sql"

func baselineV0219FS(t *testing.T) fstest.MapFS {
	t.Helper()
	return readBaselineMigrationFS(t, "baseline_v0220_prior_migrations.sha256", 304)
}

func readBaselineMigrationFS(t *testing.T, fixtureName string, wantCount int) fstest.MapFS {
	t.Helper()
	fixture, err := os.ReadFile("../../migrations/testdata/" + fixtureName)
	require.NoError(t, err)
	historical := make(fstest.MapFS)
	for _, line := range strings.Split(strings.TrimSpace(string(fixture)), "\n") {
		fields := strings.Fields(line)
		require.Len(t, fields, 2)
		require.NotContains(t, historical, fields[1])
		data, readErr := dbmigrations.FS.ReadFile(fields[1])
		require.NoError(t, readErr)
		sum := sha256.Sum256(data)
		require.Equal(t, fields[0], hex.EncodeToString(sum[:]), fields[1])
		historical[fields[1]] = &fstest.MapFile{Data: data}
	}
	require.Len(t, historical, wantCount)
	return historical
}

func TestBaselineV0220Migrations(t *testing.T) {
	for _, mode := range []string{"fresh-install", "upgrade-from-304"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			fixture := newChannelMonitorV2V0215TestDatabase(t)
			target := baselineV0220FS(t)
			var before map[string]baselineV0216MigrationRecord
			var sentinels []string
			if mode == "upgrade-from-304" {
				require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0219FS(t)))
				before = baselineV0219RequireLedger(t, ctx, fixture.db)
				baselineV0217Seed(t, ctx, fixture.db)
				sentinels = baselineV0219BusinessSentinels(t, ctx, fixture.db)
			}

			require.NoError(t, applyMigrationsFS(ctx, fixture.db, target))
			after, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
			require.NoError(t, err)
			// 305 = v0.2.20 上游全量；+1 为二开 249_group_display_category.sql。
			require.Len(t, after, 306)
			for name, entry := range before {
				require.Equal(t, entry, after[name], name)
			}
			sql, err := dbmigrations.FS.ReadFile(baselineV0220MCPOAuthMigration)
			require.NoError(t, err)
			sum := sha256.Sum256([]byte(strings.TrimSpace(string(sql))))
			require.Equal(t, hex.EncodeToString(sum[:]), after[baselineV0220MCPOAuthMigration].checksum)
			for _, table := range []string{"mcp_oauth_grants", "mcp_oauth_codes", "mcp_oauth_credentials"} {
				var exists bool
				require.NoError(t, fixture.db.QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists))
				require.True(t, exists, table)
			}

			_, err = fixture.db.ExecContext(ctx, string(sql))
			require.NoError(t, err)
			require.NoError(t, applyMigrationsFS(ctx, fixture.db, target))
			restarted, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
			require.NoError(t, err)
			require.Equal(t, after, restarted)
			if mode == "upgrade-from-304" {
				require.Equal(t, sentinels, baselineV0219BusinessSentinels(t, ctx, fixture.db))
				// 249 新增展示分类：已有分组默认空串，保持按平台归类。
				var displayCategory string
				require.NoError(t, fixture.db.QueryRowContext(ctx, `SELECT display_category FROM groups WHERE name='baseline v0217'`).Scan(&displayCategory))
				require.Empty(t, displayCategory)
			}
		})
	}
}
