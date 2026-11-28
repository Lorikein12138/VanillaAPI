//go:build unit

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/config"
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
			('channel_detection_enabled','on',now()), ('channel_detection_config','{"revision":"fixture"}',now());
	`)
	require.NoError(t, err)
	first := NewChannelDetectionCoordinator(firstDB, firstClient, nil, nil).(*channelDetectionCoordinator)
	second := NewChannelDetectionCoordinator(secondDB, secondClient, nil, nil).(*channelDetectionCoordinator)
	guard := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: `{"revision":"fixture"}`}
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
	samples := make(map[int64]*service.ChannelDetectionSample)
	var snapshot struct {
		Proofs map[int64]service.ChannelDetectionProof `json:"proofs"`
	}
	publish := func(groupID int64) service.ChannelDetectionProof {
		t.Helper()
		proofs[groupID] = service.ChannelDetectionProof{
			Version: 2, SampleID: uuid.NewString(), Source: "manual", AccountID: accountID,
			RouteFingerprint: hex.EncodeToString(digest[:]), Sends: 14, // Legacy seven-question v2 proof.
		}
		samples[groupID] = &service.ChannelDetectionSample{Pelican: "error"}
		next, err := json.Marshal(map[string]any{"proofs": proofs, "samples": samples})
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
	scope, err := first.ConfirmationScope(ctx, selection, input.Proof, guard.Config)
	require.NoError(t, err)
	require.NotNil(t, scope)
	require.Len(t, scope.Groups, 1)
	input.ScopeVersion = scope.Version
	bind(otherGroupID)
	_, err = first.Confirm(ctx, input)
	require.ErrorIs(t, err, service.ErrDetectionScopeChanged)
	scope, err = first.ConfirmationScope(ctx, selection, input.Proof, guard.Config)
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
	priorRevision := firstClient.Account.GetX(ctx, accountID).TempUnschedulableRevision
	preserved, err := first.Confirm(ctx, input)
	require.NoError(t, err)
	require.True(t, longer.Equal(preserved.Until))
	require.ErrorIs(t, second.accounts.ClearTempUnschedulable(ctx, accountID), service.ErrTempUnschedulableProtected)
	paused = firstClient.Account.GetX(ctx, accountID)
	require.Equal(t, priorRevision+1, paused.TempUnschedulableRevision, "confirmation advances revision even with a longer existing deadline")
	require.True(t, longer.Equal(*paused.TempUnschedulableUntil))
	require.Equal(t, "existing", *paused.TempUnschedulableReason)
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, accountID).Scan(&outboxEvents))
	require.Equal(t, 1, outboxEvents, "the protection changes the scheduler projection")

	schedulerCache, _ := newSchedulerCacheUnitWithRedis(t)
	beforeRecovery, err := first.accounts.GetByID(ctx, accountID)
	require.NoError(t, err)
	require.NoError(t, schedulerCache.SetAccount(ctx, beforeRecovery))

	// A manual clear commits the new projection and its outbox event together.
	_, err = firstDB.ExecContext(ctx, `DELETE FROM scheduler_outbox WHERE account_id=$1`, accountID)
	require.NoError(t, err)
	_, err = firstDB.ExecContext(ctx, `ALTER TABLE scheduler_outbox ADD CONSTRAINT fail_recovery_event CHECK (false)`)
	require.NoError(t, err)
	require.ErrorContains(t, second.accounts.ClearTempUnschedulableManually(ctx, accountID), "fail_recovery_event")
	require.Equal(t, *paused.TempUnschedulableUntil, *firstClient.Account.GetX(ctx, accountID).TempUnschedulableUntil)
	_, err = firstDB.ExecContext(ctx, `ALTER TABLE scheduler_outbox DROP CONSTRAINT fail_recovery_event`)
	require.NoError(t, err)
	recovery := service.NewRateLimitService(second.accounts, nil, &config.Config{}, nil, nil)
	result, err := recovery.RecoverAccountState(ctx, accountID, service.AccountRecoveryOptions{OverrideTempProtection: true})
	require.NoError(t, err)
	require.True(t, result.ClearedRateLimit)
	recovered := firstClient.Account.GetX(ctx, accountID)
	require.Nil(t, recovered.TempUnschedulableUntil)
	require.Nil(t, recovered.TempUnschedulableProtectedUntil)
	require.Nil(t, recovered.TempUnschedulableReason)
	require.Equal(t, paused.TempUnschedulableRevision+1, recovered.TempUnschedulableRevision)
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1 AND event_type=$2`, accountID, service.SchedulerOutboxEventAccountChanged).Scan(&outboxEvents))
	require.Equal(t, 1, outboxEvents)
	// The outbox consumer refetches with GetByID and writes through SetAccount.
	// Neither coordinator has a direct scheduler cache attached in this fixture.
	recoveryProjection, err := first.accounts.GetByID(ctx, accountID)
	require.NoError(t, err)
	require.Empty(t, recoveryProjection.TempUnschedulableReason)
	require.Equal(t, recovered.TempUnschedulableRevision, recoveryProjection.TempUnschedulableRevision)
	require.NoError(t, schedulerCache.SetAccount(ctx, recoveryProjection))
	require.NoError(t, schedulerCache.SetAccount(ctx, beforeRecovery))
	cachedRecovery, err := schedulerCache.GetAccount(ctx, accountID)
	require.NoError(t, err)
	require.Nil(t, cachedRecovery.TempUnschedulableUntil)
	require.Nil(t, cachedRecovery.TempUnschedulableProtectedUntil)
	require.Equal(t, recovered.TempUnschedulableRevision, cachedRecovery.TempUnschedulableRevision)
	for _, id := range []int64{groupID, otherGroupID} {
		candidates, err := first.accounts.ListSchedulableByGroupIDAndPlatform(ctx, id, service.PlatformOpenAI)
		require.NoError(t, err)
		require.Len(t, candidates, 1)
	}

	// Retrying the same proof returns its receipt without recreating a block.
	scope, err = first.ConfirmationScope(ctx, selection, input.Proof, guard.Config)
	require.NoError(t, err)
	require.Nil(t, scope)
	replayed, err := first.Confirm(ctx, input)
	require.NoError(t, err)
	require.Equal(t, preserved.Until, replayed.Until)
	require.Nil(t, firstClient.Account.GetX(ctx, accountID).TempUnschedulableUntil)
	require.Equal(t, recovered.TempUnschedulableRevision, firstClient.Account.GetX(ctx, accountID).TempUnschedulableRevision)

	// The revision advances on an ordinary suspension and its automatic clear.
	require.NoError(t, second.accounts.SetTempUnschedulable(ctx, accountID, time.Now().Add(time.Hour), "ordinary"))
	require.Equal(t, "ordinary", *firstClient.Account.GetX(ctx, accountID).TempUnschedulableReason)
	require.NoError(t, second.accounts.ClearTempUnschedulable(ctx, accountID))
	ordinaryCleared := firstClient.Account.GetX(ctx, accountID)
	require.Equal(t, recovered.TempUnschedulableRevision+2, ordinaryCleared.TempUnschedulableRevision)

	input.Proof = publish(groupID)
	_, err = first.Confirm(ctx, input)
	require.NoError(t, err)
	reconfirmed := firstClient.Account.GetX(ctx, accountID)
	require.Greater(t, reconfirmed.TempUnschedulableRevision, ordinaryCleared.TempUnschedulableRevision)
	require.ErrorIs(t, second.accounts.ClearTempUnschedulable(ctx, accountID), service.ErrTempUnschedulableProtected)

	// Natural expiry restores the account through the existing eligibility path.
	_, err = firstDB.ExecContext(ctx, `
		UPDATE accounts SET temp_unschedulable_until=now()-interval '1 second',
			temp_unschedulable_protected_until=now()-interval '1 second' WHERE id=$1`, accountID)
	require.NoError(t, err)
	candidates, err := first.accounts.ListSchedulableByGroupIDAndPlatform(ctx, groupID, service.PlatformOpenAI)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	require.NoError(t, second.accounts.ClearTempUnschedulable(ctx, accountID))
	expired := firstClient.Account.GetX(ctx, accountID)
	require.Nil(t, expired.TempUnschedulableUntil)
	require.Nil(t, expired.TempUnschedulableProtectedUntil)

	_, err = firstDB.ExecContext(ctx, `
		UPDATE settings SET value=jsonb_set(value::jsonb, ARRAY['proofs',$1::text,'completed_at'],
			to_jsonb(clock_timestamp()-interval '24 hours'))::text WHERE key=$2`, groupID, service.SettingKeyChannelDetectionLatest)
	require.NoError(t, err)
	require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, service.SettingKeyChannelDetectionLatest).Scan(&guard.Snapshot))
	require.NoError(t, json.Unmarshal([]byte(guard.Snapshot), &snapshot))
	input.Proof = snapshot.Proofs[groupID]
	_, err = first.Confirm(ctx, input)
	require.ErrorIs(t, err, service.ErrDetectionChanged)

	t.Run("automatic publication and configurable manual confirmation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		setPolicy := func(enabled bool, minutes int, version string) {
			t.Helper()
			guard.Config = fmt.Sprintf(`{"revision":"fixture","enabled":true,"suspend_minutes":%d,"auto_confirm_missing_svg":%t,"suspension_revision":%q,"groups":[{"group_id":%d,"model":"fixture-model"}]}`, minutes, enabled, version, groupID)
			_, err := firstDB.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.SettingKeyChannelDetectionConfig, guard.Config)
			require.NoError(t, err)
		}
		stage := func(runVersion, proofVersion string, attributable bool) (string, string) {
			t.Helper()
			sampleID := uuid.NewString()
			proof := service.ChannelDetectionProof{Version: 2, SampleID: sampleID, Source: "manual", SuspensionRevision: proofVersion}
			if attributable {
				proof.AccountID, proof.Sends, proof.RouteFingerprint = accountID, 1, hex.EncodeToString(digest[:])
			}
			before := fmt.Sprintf(`{"feature":"on","revision":"fixture","running_by_group":{"%d":{"owner":"scheduled-worker","sample_id":%q,"suspension_revision":%q,"group_platform":"openai"}}}`, groupID, sampleID, runVersion)
			_, err := firstDB.ExecContext(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, service.SettingKeyChannelDetectionLatest, before)
			require.NoError(t, err)
			guard.Snapshot = before
			next, err := json.Marshal(map[string]any{
				"feature": "on", "revision": "fixture",
				"proofs":  map[int64]service.ChannelDetectionProof{groupID: proof},
				"samples": map[int64]service.ChannelDetectionSample{groupID: {Pelican: "error"}},
			})
			require.NoError(t, err)
			return sampleID, string(next)
		}
		publish := func(next string) {
			t.Helper()
			ok, err := first.PublishSnapshot(ctx, "scheduled-worker", guard, next, groupID)
			require.NoError(t, err)
			require.True(t, ok)
		}
		readLatest := func() string {
			t.Helper()
			var raw string
			require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, service.SettingKeyChannelDetectionLatest).Scan(&raw))
			return raw
		}
		for _, tc := range []struct {
			name, runVersion, proofVersion string
			enabled, attributable          bool
		}{
			{"disabled", "policy", "policy", false, true},
			{"old policy", "old", "old", true, true},
			{"old admission", "old", "policy", true, true},
			{"no attribution", "policy", "policy", true, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				setPolicy(tc.enabled, 35, "policy")
				id, next := stage(tc.runVersion, tc.proofVersion, tc.attributable)
				publish(next)
				require.Contains(t, readLatest(), id)
				require.Nil(t, firstClient.Account.GetX(ctx, accountID).TempUnschedulableUntil)
			})
		}
		setPolicy(true, 35, "policy")
		longer := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
		require.NoError(t, first.accounts.SetTempUnschedulable(ctx, accountID, longer, "existing"))
		prior := firstClient.Account.GetX(ctx, accountID)
		_, next := stage("policy", "policy", true)
		publish(next)
		retained := firstClient.Account.GetX(ctx, accountID)
		require.True(t, longer.Equal(*retained.TempUnschedulableUntil))
		require.Equal(t, "existing", *retained.TempUnschedulableReason)
		require.Equal(t, prior.TempUnschedulableRevision, retained.TempUnschedulableRevision)
		require.NoError(t, first.accounts.ClearTempUnschedulableManually(ctx, accountID))

		id, next := stage("policy", "policy", true)
		_, err := firstDB.ExecContext(ctx, `DELETE FROM scheduler_outbox WHERE account_id=$1`, accountID)
		require.NoError(t, err)
		_, err = firstDB.ExecContext(ctx, `ALTER TABLE scheduler_outbox ADD CONSTRAINT fail_auto_confirmation CHECK (false)`)
		require.NoError(t, err)
		ok, err := first.PublishSnapshot(ctx, "scheduled-worker", guard, next, groupID)
		require.False(t, ok)
		require.ErrorContains(t, err, "fail_auto_confirmation")
		require.Equal(t, guard.Snapshot, readLatest(), "a failed suspension rolls back the published sample too")
		require.Nil(t, firstClient.Account.GetX(ctx, accountID).TempUnschedulableUntil)
		_, err = firstDB.ExecContext(ctx, `ALTER TABLE scheduler_outbox DROP CONSTRAINT fail_auto_confirmation`)
		require.NoError(t, err)
		staged := guard
		publish(next)
		suspended := firstClient.Account.GetX(ctx, accountID)
		require.WithinDuration(t, time.Now().Add(35*time.Minute), *suspended.TempUnschedulableUntil, 5*time.Second)
		require.Equal(t, *suspended.TempUnschedulableUntil, *suspended.TempUnschedulableProtectedUntil)
		require.Equal(t, automaticDetectionSuspensionReason, *suspended.TempUnschedulableReason)
		var receiptsJSON string
		require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, channelDetectionReceiptsSetting).Scan(&receiptsJSON))
		receipts, err := decodeDetectionReceipts(receiptsJSON)
		require.NoError(t, err)
		require.True(t, receipts[groupID].Automatic)
		require.Equal(t, id, receipts[groupID].SampleID)
		require.Zero(t, receipts[groupID].ActorID)
		require.NoError(t, firstDB.QueryRowContext(ctx, `SELECT count(*) FROM scheduler_outbox WHERE account_id=$1`, accountID).Scan(&outboxEvents))
		require.Equal(t, 1, outboxEvents)
		guard.Snapshot = readLatest()
		require.NoError(t, json.Unmarshal([]byte(guard.Snapshot), &snapshot))
		input.Guard, input.Proof = guard, snapshot.Proofs[groupID]
		require.NoError(t, first.accounts.ClearTempUnschedulableManually(ctx, accountID))
		ok, err = second.PublishSnapshot(ctx, "scheduled-worker", staged, next, groupID)
		require.NoError(t, err)
		require.False(t, ok, "an automatic publish cannot replay after recovery")
		replay, err := second.Confirm(ctx, input)
		require.NoError(t, err)
		require.Equal(t, *suspended.TempUnschedulableUntil, replay.Until)
		require.Nil(t, firstClient.Account.GetX(ctx, accountID).TempUnschedulableUntil)

		_, next = stage("policy", "", true)
		publish(next)
		guard.Snapshot = readLatest()
		require.NoError(t, json.Unmarshal([]byte(guard.Snapshot), &snapshot))
		input.Proof = snapshot.Proofs[groupID]
		scope, err := first.ConfirmationScope(ctx, selection, input.Proof, guard.Config)
		require.NoError(t, err)
		require.Equal(t, 35, scope.SuspendMinutes)
		setPolicy(true, 12, "revised-policy")
		input.Guard = guard
		input.ScopeVersion = scope.Version
		_, err = second.Confirm(ctx, input)
		require.ErrorIs(t, err, service.ErrDetectionScopeChanged)
		scope, err = first.ConfirmationScope(ctx, selection, input.Proof, guard.Config)
		require.NoError(t, err)
		require.Equal(t, 12, scope.SuspendMinutes)
		input.ScopeVersion = scope.Version
		confirmed, err := second.Confirm(ctx, input)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now().Add(12*time.Minute), confirmed.Until, 5*time.Second)
		protected := firstClient.Account.GetX(ctx, accountID)
		_, next = stage("revised-policy", "revised-policy", true)
		publish(next)
		stillProtected := firstClient.Account.GetX(ctx, accountID)
		require.Equal(t, protected.TempUnschedulableRevision, stillProtected.TempUnschedulableRevision)
		require.Equal(t, protected.TempUnschedulableUntil, stillProtected.TempUnschedulableUntil)
		require.Equal(t, channelDetectionSuspensionReason, *stillProtected.TempUnschedulableReason)
	})
}

func TestTempUnschedulablePostgresOrdersWritesAfterRecovery(t *testing.T) {
	db, _ := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	require.NoError(t, client.Schema.Create(ctx))
	_, err := db.ExecContext(ctx, `CREATE TABLE scheduler_outbox (
		event_type TEXT NOT NULL, account_id BIGINT, group_id BIGINT, payload JSONB, dedup_key TEXT
	);
	CREATE UNIQUE INDEX test_outbox_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL;`)
	require.NoError(t, err)
	repo := newAccountRepositoryWithSQL(client, db, nil)
	credentials := map[string]any{"access_token": "fixture"}
	until := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	id := client.Account.Create().SetName("Grok fixture").SetPlatform(service.PlatformGrok).
		SetType(service.AccountTypeOAuth).SetCredentials(credentials).
		SetTempUnschedulableUntil(until).SetTempUnschedulableProtectedUntil(until).
		SetTempUnschedulableReason("legacy").SaveX(ctx).ID

	// Simulate a populated schema at migration 253, then apply the embedded 254.
	_, err = db.ExecContext(ctx, `ALTER TABLE accounts DROP COLUMN temp_unschedulable_revision`)
	require.NoError(t, err)
	var originalFile, migratedFile int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_relation_filenode('accounts'::regclass)`).Scan(&originalFile))
	migration, err := migrations.FS.ReadFile("254_account_temp_unschedulable_revision.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT pg_relation_filenode('accounts'::regclass)`).Scan(&migratedFile))
	require.Equal(t, originalFile, migratedFile, "adding the constant default does not rewrite existing rows")
	legacy, err := repo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Zero(t, legacy.TempUnschedulableRevision)
	require.Equal(t, until, *legacy.TempUnschedulableProtectedUntil)
	require.Equal(t, "legacy", legacy.TempUnschedulableReason)
	require.ErrorIs(t, repo.ClearTempUnschedulable(ctx, id), service.ErrTempUnschedulableProtected)

	var previousRevision int64
	assertNext := func(deadline *time.Time, reason string) {
		t.Helper()
		account, err := repo.GetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, previousRevision+1, account.TempUnschedulableRevision)
		previousRevision = account.TempUnschedulableRevision
		require.Nil(t, account.TempUnschedulableProtectedUntil)
		require.Equal(t, reason, account.TempUnschedulableReason)
		if deadline == nil {
			require.Nil(t, account.TempUnschedulableUntil)
		} else {
			require.True(t, deadline.Equal(*account.TempUnschedulableUntil))
		}
		// Other updates are allowed to reset UpdatedAt to an older transaction time.
		_, err = db.ExecContext(ctx, `UPDATE accounts SET updated_at = updated_at - interval '1 day' WHERE id = $1`, id)
		require.NoError(t, err)
	}
	require.NoError(t, repo.ClearTempUnschedulableManually(ctx, id))
	assertNext(nil, "")
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	require.Equal(t, previousRevision, client.Account.GetX(ctx, id).TempUnschedulableRevision)
	require.NoError(t, repo.SetTempUnschedulable(ctx, id, until, "ordinary"))
	assertNext(&until, "ordinary")
	require.NoError(t, repo.ClearTempUnschedulable(ctx, id))
	assertNext(nil, "")

	retryReason := "token refresh retry exhausted: fixture"
	applied, err := repo.SetGrokOAuthRefreshTempUnschedulableIfCredentialsUnchanged(ctx, id, credentials, nil, until, retryReason)
	require.NoError(t, err)
	require.True(t, applied)
	assertNext(&until, retryReason)
	require.NoError(t, repo.ClearTempUnschedulable(ctx, id))
	assertNext(nil, "")

	snapshot := service.GrokCredentialMutationSnapshot{CredentialsJSON: `{"access_token":"fixture"}`}
	applied, err = repo.SetGrokCredentialTempUnschedulableIfMatch(ctx, id, snapshot, until, "credential check")
	require.NoError(t, err)
	require.True(t, applied)
	assertNext(&until, "credential check")
	require.NoError(t, repo.ClearTempUnschedulableManually(ctx, id))
	assertNext(nil, "")
}
