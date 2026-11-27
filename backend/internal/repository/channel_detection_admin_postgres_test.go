//go:build unit

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestChannelDetectionPostgresConfirmation(t *testing.T) {
	firstDB, secondDB := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	firstClient := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, firstDB)))
	secondClient := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, secondDB)))
	require.NoError(t, firstClient.Schema.Create(ctx))
	migration, err := migrations.FS.ReadFile("253_account_temp_unschedulable_protection.sql")
	require.NoError(t, err)
	_, err = firstDB.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN temp_unschedulable_protected_until`)
	require.NoError(t, err)
	for range 2 {
		_, err = firstDB.ExecContext(ctx, string(migration))
		require.NoError(t, err)
	}
	_, err = firstDB.ExecContext(ctx, `
		CREATE TABLE scheduler_outbox (
			event_type TEXT NOT NULL, account_id BIGINT, group_id BIGINT, payload JSONB, dedup_key TEXT
		);
		CREATE UNIQUE INDEX scheduler_outbox_test_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL;
		INSERT INTO settings (key,value,updated_at) VALUES
			('channel_detection_enabled','on',now()), ('channel_detection_config','fixture',now());
	`)
	require.NoError(t, err)
	first := NewChannelDetectionCoordinator(firstDB, firstClient, nil, nil).(*channelDetectionCoordinator)
	second := NewChannelDetectionCoordinator(secondDB, secondClient, nil, nil).(*channelDetectionCoordinator)
	guard := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: "fixture"}
	makeGroup := func(name string) int64 {
		return firstClient.Group.Create().SetName(name).SetPlatform(service.PlatformOpenAI).
			SetModelAllowlist(domain.GroupModelAllowlist{Models: []string{"fixture-model"}}).SaveX(ctx).ID
	}
	groupID, otherGroupID := makeGroup("Detection group"), makeGroup("Other group")
	for _, id := range []int64{groupID, otherGroupID} {
		ok, err := first.TryAcquire(ctx, id, "scheduled-worker", time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
	}
	accountID := firstClient.Account.Create().SetName("Fixture account").SetPlatform(service.PlatformOpenAI).
		SetType(service.AccountTypeAPIKey).SetCredentials(map[string]any{"api_key": "fixture"}).SaveX(ctx).ID
	bind := func(groupID int64) {
		firstClient.AccountGroup.Create().SetAccountID(accountID).SetGroupID(groupID).SaveX(ctx)
	}
	bind(groupID)
	// Known serialized route identity for this synthetic API-key account.
	digest := sha256.Sum256([]byte(`{"Platform":"openai","Type":"apikey","ParentAccountID":null,"QuotaDimension":"global","Credentials":{"api_key":"fixture"},"CustomBaseURL":"","ProxyID":null,"ProxyURL":"","OpenAIPassthrough":false,"OpenAIModelFiltering":false}`))
	proofs := make(map[int64]service.ChannelDetectionProof)
	var snapshot struct {
		Proofs map[int64]service.ChannelDetectionProof `json:"proofs"`
	}
	publish := func(groupID int64) service.ChannelDetectionProof {
		t.Helper()
		proofs[groupID] = service.ChannelDetectionProof{
			Version: 2, SampleID: uuid.NewString(), Source: "manual", AccountID: accountID,
			RouteFingerprint: hex.EncodeToString(digest[:]), Sends: 14, // Legacy seven-question v2 proof.
		}
		next, err := json.Marshal(map[string]any{"proofs": proofs})
		require.NoError(t, err)
		ok, err := first.PublishSnapshot(ctx, "scheduled-worker", guard, string(next), groupID)
		require.NoError(t, err)
		require.True(t, ok)
		require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, service.SettingKeyChannelDetectionLatest).Scan(&guard.Snapshot))
		require.NoError(t, json.Unmarshal([]byte(guard.Snapshot), &snapshot))
		proofs = snapshot.Proofs
		require.NotNil(t, proofs[groupID].CompletedAt)
		return proofs[groupID]
	}
	selection := service.ChannelDetectionSelection{GroupID: groupID, Model: "fixture-model"}
	input := service.ChannelDetectionConfirmationRequest{Selection: selection, ActorID: 1, Proof: publish(groupID), Guard: guard, GroupPlatform: service.PlatformOpenAI}
	scope, err := first.ConfirmationScope(ctx, selection, input.Proof)
	require.NoError(t, err)
	require.NotNil(t, scope)
	require.Len(t, scope.Groups, 1)
	input.ScopeVersion = scope.Version
	bind(otherGroupID)
	_, err = first.Confirm(ctx, input)
	require.ErrorIs(t, err, service.ErrDetectionScopeChanged)
	scope, err = first.ConfirmationScope(ctx, selection, input.Proof)
	require.NoError(t, err)
	require.NotNil(t, scope)
	require.Len(t, scope.Groups, 2)
	input.ScopeVersion = scope.Version

	// A credential mutation makes the sample ineligible until the exact route
	// identity is restored, and a shadow makes even a matching parent ambiguous.
	firstClient.Account.UpdateOneID(accountID).SetCredentials(map[string]any{"api_key": "rotated"}).SaveX(ctx)
	_, err = first.Confirm(ctx, input)
	require.ErrorIs(t, err, service.ErrDetectionChanged)
	firstClient.Account.UpdateOneID(accountID).SetCredentials(map[string]any{"api_key": "fixture"}).SaveX(ctx)
	shadowID := firstClient.Account.Create().SetName("Fixture shadow").SetPlatform(service.PlatformOpenAI).
		SetType(service.AccountTypeOAuth).SetParentAccountID(accountID).SaveX(ctx).ID
	_, err = first.Confirm(ctx, input)
	require.ErrorIs(t, err, service.ErrDetectionChanged)
	require.NoError(t, firstClient.Account.DeleteOneID(shadowID).Exec(ctx))

	// Roll back protection and the receipt if inserting the outbox event fails.
	_, err = firstDB.ExecContext(ctx, `ALTER TABLE scheduler_outbox ADD CONSTRAINT fail_detection_event CHECK (false)`)
	require.NoError(t, err)
	_, err = first.Confirm(ctx, input)
	require.ErrorContains(t, err, "fail_detection_event")
	afterRollback := firstClient.Account.GetX(ctx, accountID)
	require.Nil(t, afterRollback.TempUnschedulableUntil)
	require.Nil(t, afterRollback.TempUnschedulableProtectedUntil)
	_, err = firstDB.ExecContext(ctx, `ALTER TABLE scheduler_outbox DROP CONSTRAINT fail_detection_event`)
	require.NoError(t, err)
	// Model a previous row version ahead of this transaction's wall clock.
	var previousVersion time.Time
	require.NoError(t, firstDB.QueryRowContext(ctx, `
		UPDATE accounts SET updated_at=clock_timestamp()+interval '1 minute' WHERE id=$1 RETURNING updated_at`, accountID).Scan(&previousVersion))

	// A publish from another group leaves the original proof confirmable.
	publish(otherGroupID)

	// Two confirmations and an automatic clear compete on separate connections.
	// Confirmation needs only the lease row lock; the scheduled owner stays live.
	type outcome struct {
		result service.ChannelDetectionConfirmation
		err    error
	}
	results := make(chan outcome, 2)
	for _, coordinator := range []*channelDetectionCoordinator{first, second} {
		go func() {
			result, err := coordinator.Confirm(ctx, input)
			results <- outcome{result, err}
		}()
	}
	cleared := make(chan error, 1)
	go func() { cleared <- second.accounts.ClearTempUnschedulable(ctx, accountID) }()
	a, b := <-results, <-results
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	if err := <-cleared; err != nil {
		require.ErrorIs(t, err, service.ErrTempUnschedulableProtected)
	}
	require.Equal(t, a.result, b.result)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), a.result.Until, 5*time.Second)
	require.ErrorIs(t, second.accounts.ClearTempUnschedulable(ctx, accountID), service.ErrTempUnschedulableProtected)
	paused := firstClient.Account.GetX(ctx, accountID)
	require.Equal(t, a.result.Until, *paused.TempUnschedulableUntil)
	require.Equal(t, a.result.Until, *paused.TempUnschedulableProtectedUntil)
	require.True(t, paused.UpdatedAt.After(previousVersion))
	require.Equal(t, service.StatusActive, paused.Status)
	require.True(t, paused.Schedulable)
	for _, id := range []int64{groupID, otherGroupID} {
		candidates, err := first.accounts.ListSchedulableByGroupIDAndPlatform(ctx, id, service.PlatformOpenAI)
		require.NoError(t, err)
		require.Empty(t, candidates)
	}
	var outboxEvents int
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND event_type=$2`, accountID, service.SchedulerOutboxEventAccountChanged).Scan(&outboxEvents))
	require.Equal(t, 1, outboxEvents)

	otherInput := input
	otherInput.Selection.GroupID = otherGroupID
	otherInput.Proof = publish(otherGroupID)
	_, err = second.Confirm(ctx, otherInput)
	require.ErrorIs(t, err, service.ErrDetectionProtected)
	nextProof := publish(groupID)
	_, err = first.Confirm(ctx, input)
	require.ErrorIs(t, err, service.ErrDetectionChanged)
	input.Proof = nextProof

	// Existing longer deadlines and their reasons survive both confirmation and
	// subsequent automatic recovery during the protected window.
	longer := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Microsecond)
	_, err = firstDB.ExecContext(ctx, `DELETE FROM scheduler_outbox WHERE account_id=$1`, accountID)
	require.NoError(t, err)
	_, err = firstDB.ExecContext(ctx, `
		UPDATE accounts SET temp_unschedulable_until=$1, temp_unschedulable_reason='existing',
			temp_unschedulable_protected_until=now()-interval '1 second' WHERE id=$2`, longer, accountID)
	require.NoError(t, err)
	preserved, err := first.Confirm(ctx, input)
	require.NoError(t, err)
	require.True(t, longer.Equal(preserved.Until))
	require.ErrorIs(t, second.accounts.ClearTempUnschedulable(ctx, accountID), service.ErrTempUnschedulableProtected)
	paused = firstClient.Account.GetX(ctx, accountID)
	require.True(t, longer.Equal(*paused.TempUnschedulableUntil))
	require.Equal(t, "existing", *paused.TempUnschedulableReason)
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, accountID).Scan(&outboxEvents))
	require.Equal(t, 1, outboxEvents, "the protection changes the scheduler projection")

	// Natural expiry restores the account through the existing eligibility path.
	_, err = firstDB.ExecContext(ctx, `
		UPDATE accounts SET temp_unschedulable_until=now()-interval '1 second',
			temp_unschedulable_protected_until=now()-interval '1 second' WHERE id=$1`, accountID)
	require.NoError(t, err)
	candidates, err := first.accounts.ListSchedulableByGroupIDAndPlatform(ctx, groupID, service.PlatformOpenAI)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.NoError(t, second.accounts.ClearTempUnschedulable(ctx, accountID))
	require.Nil(t, firstClient.Account.GetX(ctx, accountID).TempUnschedulableProtectedUntil)

	_, err = firstDB.ExecContext(ctx, `
		UPDATE settings SET value=jsonb_set(value::jsonb, ARRAY['proofs',$1::text,'completed_at'],
			to_jsonb(clock_timestamp()-interval '24 hours'))::text WHERE key=$2`, groupID, service.SettingKeyChannelDetectionLatest)
	require.NoError(t, err)
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, service.SettingKeyChannelDetectionLatest).Scan(&guard.Snapshot))
	require.NoError(t, json.Unmarshal([]byte(guard.Snapshot), &snapshot))
	input.Proof = snapshot.Proofs[groupID]
	_, err = first.Confirm(ctx, input)
	require.ErrorIs(t, err, service.ErrDetectionChanged)
}
