//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

const (
	baselineV0223UltrafastMultiplierMigration = "252_channel_pricing_ultrafast_multiplier.sql"
	baselineV0223MigrationCount               = baselineV0222MigrationCount + 1
)

func baselineV0222FS(t *testing.T) fstest.MapFS {
	t.Helper()
	prior := readBaselineMigrationFS(t, "baseline_v0222_prior_migrations.sha256", baselineV0222PriorMigrationCount)
	for _, name := range []string{baselineV0222UltrafastMigration, baselineV0222NonblockingRollupMigration} {
		data, err := dbmigrations.FS.ReadFile(name)
		require.NoError(t, err)
		prior[name] = &fstest.MapFile{Data: data}
	}
	return prior
}

func TestBaselineV0223MigrationsUpgrade(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	db := newChannelMonitorV2V0215TestDatabase(t).db
	require.NoError(t, applyMigrationsFS(ctx, db, baselineV0222FS(t)))
	before, err := baselineV0216ReadMigrationRecords(ctx, db)
	require.NoError(t, err)
	require.Len(t, before, baselineV0222MigrationCount)

	var channelID, pricingID int64
	require.NoError(t, db.QueryRowContext(ctx, `INSERT INTO channels (name) VALUES ('v0223 upgrade') RETURNING id`).Scan(&channelID))
	require.NoError(t, db.QueryRowContext(ctx, `
		INSERT INTO channel_model_pricing (channel_id, models, fast_multiplier)
		VALUES ($1, '["gpt-6.2-sol"]', 2.5) RETURNING id
	`, channelID).Scan(&pricingID))

	require.NoError(t, ApplyMigrations(ctx, db))
	after, err := baselineV0216ReadMigrationRecords(ctx, db)
	require.NoError(t, err)
	require.Len(t, after, baselineV0223MigrationCount)
	for name, entry := range before {
		require.Equal(t, entry, after[name], "%s checksum/applied_at", name)
	}
	migration, err := dbmigrations.FS.ReadFile(baselineV0223UltrafastMultiplierMigration)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte(strings.TrimSpace(string(migration))))
	require.Equal(t, hex.EncodeToString(sum[:]), after[baselineV0223UltrafastMultiplierMigration].checksum)
	require.False(t, after[baselineV0223UltrafastMultiplierMigration].applied.IsZero())

	readCard := func() sql.NullFloat64 {
		t.Helper()
		var multiplier sql.NullFloat64
		var fast float64
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT ultrafast_multiplier, fast_multiplier FROM channel_model_pricing WHERE id = $1
		`, pricingID).Scan(&multiplier, &fast))
		require.Equal(t, 2.5, fast)
		return multiplier
	}
	require.False(t, readCard().Valid)
	_, err = db.ExecContext(ctx, `UPDATE channel_model_pricing SET ultrafast_multiplier = 1.25 WHERE id = $1`, pricingID)
	require.NoError(t, err)
	for _, invalid := range []int{0, -1} {
		_, err = db.ExecContext(ctx, `UPDATE channel_model_pricing SET ultrafast_multiplier = $1 WHERE id = $2`, invalid, pricingID)
		var pgErr *pq.Error
		require.ErrorAs(t, err, &pgErr)
		require.Equal(t, pq.ErrorCode("23514"), pgErr.Code)
		require.Equal(t, "channel_model_pricing_ultrafast_multiplier_positive", pgErr.Constraint)
	}

	// Exercise the next startup against fresh PostgreSQL connections.
	db.SetMaxIdleConns(0)
	require.NoError(t, ApplyMigrations(ctx, db))
	restarted, err := baselineV0216ReadMigrationRecords(ctx, db)
	require.NoError(t, err)
	require.Equal(t, after, restarted)
	require.Equal(t, sql.NullFloat64{Float64: 1.25, Valid: true}, readCard())
}
