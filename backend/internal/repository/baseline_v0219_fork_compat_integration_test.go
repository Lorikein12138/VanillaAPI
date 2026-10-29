//go:build integration

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func baselineV0219SchedulerOutboxCount(t *testing.T, ctx context.Context, db *sql.DB, accountID int64) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND event_type=$2`,
		accountID, service.SchedulerOutboxEventAccountChanged,
	).Scan(&count))
	return count
}

func TestBaselineV0219AccountRepositoryTransactionScopeCommitsAndRollsBack(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	harnessDatabase, harnessConfig := baselineV0216HarnessSnapshot(t, ctx)
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, fixture.db)))
	t.Cleanup(func() { _ = client.Close() })

	var accountID int64
	require.NoError(t, fixture.db.QueryRowContext(ctx, `
		INSERT INTO accounts (name,platform,type,credentials,status,extra)
		VALUES ('baseline-v0219-transaction','openai','apikey','{}'::jsonb,'active','{"preserved":"yes"}'::jsonb)
		RETURNING id`).Scan(&accountID))
	schedulerCache := &schedulerCacheRecorder{}
	repo := newAccountRepositoryWithSQL(client, fixture.db, schedulerCache)

	committedBefore, err := repo.GetByID(ctx, accountID)
	require.NoError(t, err)
	baselineOutbox := baselineV0219SchedulerOutboxCount(t, ctx, fixture.db, accountID)

	commitTx, err := client.Tx(ctx)
	require.NoError(t, err)
	commitCtx := dbent.NewTxContext(ctx, commitTx)
	insideCommit, err := repo.GetByID(commitCtx, accountID)
	require.NoError(t, err)
	insideCommit.Name = "baseline-v0219-committed"
	insideCommit.Extra["committed_marker"] = "visible-inside"
	require.NoError(t, repo.Update(commitCtx, insideCommit))
	readCommitted, err := repo.GetByID(commitCtx, accountID)
	require.NoError(t, err)
	require.Equal(t, "baseline-v0219-committed", readCommitted.Name)
	require.Equal(t, "visible-inside", readCommitted.Extra["committed_marker"])

	outsideCommit, err := repo.GetByID(ctx, accountID)
	require.NoError(t, err)
	require.Equal(t, committedBefore.Name, outsideCommit.Name, "uncommitted account writes must be isolated")
	require.NotContains(t, outsideCommit.Extra, "committed_marker")
	require.Empty(t, schedulerCache.setAccounts, "transaction-scoped writes must not publish scheduler cache before commit")
	require.Equal(t, baselineOutbox, baselineV0219SchedulerOutboxCount(t, ctx, fixture.db, accountID), "outbox writes must stay inside the transaction")
	require.NoError(t, commitTx.Commit())
	require.Empty(t, schedulerCache.setAccounts, "the tx-scoped repository must not use its parent scheduler cache")
	require.Equal(t, baselineOutbox+1, baselineV0219SchedulerOutboxCount(t, ctx, fixture.db, accountID))

	committed, err := repo.GetByID(ctx, accountID)
	require.NoError(t, err)
	require.Equal(t, "baseline-v0219-committed", committed.Name)
	require.Equal(t, "yes", committed.Extra["preserved"], "JSONB updates must retain unrelated account fields")

	rollbackOutbox := baselineV0219SchedulerOutboxCount(t, ctx, fixture.db, accountID)
	rollbackTx, err := client.Tx(ctx)
	require.NoError(t, err)
	rollbackCtx := dbent.NewTxContext(ctx, rollbackTx)
	insideRollback, err := repo.GetByID(rollbackCtx, accountID)
	require.NoError(t, err)
	insideRollback.Name = "baseline-v0219-rolled-back"
	insideRollback.Extra["rollback_marker"] = "must-disappear"
	require.NoError(t, repo.Update(rollbackCtx, insideRollback))
	rolledBackInside, err := repo.GetByID(rollbackCtx, accountID)
	require.NoError(t, err)
	require.Equal(t, "baseline-v0219-rolled-back", rolledBackInside.Name)
	require.Contains(t, rolledBackInside.Extra, "rollback_marker")
	require.Empty(t, schedulerCache.setAccounts)
	require.NoError(t, rollbackTx.Rollback())

	afterRollback, err := repo.GetByID(ctx, accountID)
	require.NoError(t, err)
	require.Equal(t, "baseline-v0219-committed", afterRollback.Name)
	require.NotContains(t, afterRollback.Extra, "rollback_marker")
	require.Equal(t, rollbackOutbox, baselineV0219SchedulerOutboxCount(t, ctx, fixture.db, accountID), "rollback must remove its transactional outbox event")
	require.Empty(t, schedulerCache.setAccounts, "rollback must not publish a scheduler cache snapshot")
	baselineV0216RequireHarnessUnchanged(t, ctx, harnessDatabase, harnessConfig)
}
