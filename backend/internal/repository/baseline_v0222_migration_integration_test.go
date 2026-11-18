//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

const (
	baselineV0222UltrafastMigration         = "251_group_force_openai_ultrafast.sql"
	baselineV0222NonblockingRollupMigration = "251_group_usage_rollup_nonblocking_build.sql"
	baselineV0222PriorMigrationCount        = 309
	baselineV0222MigrationCount             = baselineV0222PriorMigrationCount + 2
)

func TestBaselineV0222MigrationsUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	prior := readBaselineMigrationFS(t, "baseline_v0222_prior_migrations.sha256", baselineV0222PriorMigrationCount)
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, prior))
	before, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, before, baselineV0222PriorMigrationCount)
	require.Contains(t, before, advisorRequestIDMigration)

	for _, name := range []string{baselineV0222UltrafastMigration, baselineV0222NonblockingRollupMigration} {
		require.NotContains(t, before, name)
	}
	require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0222FS(t)))
	after, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Len(t, after, baselineV0222MigrationCount)
	for name, entry := range before {
		require.Equal(t, entry, after[name], "%s checksum/applied_at", name)
	}
	for _, name := range []string{baselineV0222UltrafastMigration, baselineV0222NonblockingRollupMigration} {
		data, readErr := dbmigrations.FS.ReadFile(name)
		require.NoError(t, readErr)
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(data))))
		require.Equal(t, hex.EncodeToString(sum[:]), after[name].checksum, name)
	}

	require.NoError(t, applyMigrationsFS(ctx, fixture.db, baselineV0222FS(t)))
	restarted, err := baselineV0216ReadMigrationRecords(ctx, fixture.db)
	require.NoError(t, err)
	require.Equal(t, after, restarted)
}
