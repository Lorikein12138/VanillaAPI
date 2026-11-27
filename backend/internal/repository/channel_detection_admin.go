package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbproxy "github.com/Wei-Shaw/sub2api/ent/proxy"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

const channelDetectionReceiptsSetting = "channel_detection_confirmation_receipts"
const channelDetectionSuspensionReason = "channel_detection_admin_confirmation"

type channelDetectionReceipt struct {
	SampleID  string    `json:"sample_id"`
	ActorID   int64     `json:"actor_id"`
	AccountID int64     `json:"account_id"`
	At        time.Time `json:"at"`
	Until     time.Time `json:"until"`
}

func (r *channelDetectionCoordinator) ConfirmationScope(ctx context.Context, selection service.ChannelDetectionSelection, proof service.ChannelDetectionProof) (*service.ChannelDetectionScope, error) {
	var now time.Time
	var receiptsJSON string
	var protected bool
	err := r.db.QueryRowContext(ctx, `
		SELECT clock_timestamp(), COALESCE((SELECT value FROM settings WHERE key = $1), '{}'),
			COALESCE((SELECT temp_unschedulable_protected_until > clock_timestamp() FROM accounts WHERE id = $2), false)
	`, channelDetectionReceiptsSetting, proof.AccountID).Scan(&now, &receiptsJSON, &protected)
	if err != nil {
		return nil, err
	}
	if protected || !freshDetectionProof(proof, now) {
		return nil, nil
	}
	receipts, err := decodeDetectionReceipts(receiptsJSON)
	if err != nil || receipts[selection.GroupID].SampleID == proof.SampleID {
		return nil, err
	}
	scope, err := detectionAccountScopeForConfirmation(ctx, r.accounts, selection, proof)
	if errors.Is(err, service.ErrDetectionChanged) {
		return nil, nil
	}
	return scope, err
}

func (r *channelDetectionCoordinator) Confirm(ctx context.Context, input service.ChannelDetectionConfirmationRequest) (service.ChannelDetectionConfirmation, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var receipt channelDetectionReceipt
	var err error
	for range 2 {
		receipt, err = r.confirmInTransaction(ctx, input)
		var pgErr *pq.Error
		if !errors.As(err, &pgErr) || (pgErr.Code != "40P01" && pgErr.Code != "40001") {
			break
		}
	}
	if err != nil {
		return service.ChannelDetectionConfirmation{}, err
	}
	accountSynced := r.accounts.syncSchedulerAccountSnapshotDetached(ctx, receipt.AccountID)
	cacheSynced := true
	if r.tempCache != nil {
		cacheCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		cacheSynced = r.tempCache.DeleteTempUnsched(cacheCtx, receipt.AccountID) == nil
	}
	return service.ChannelDetectionConfirmation{Until: receipt.Until, SyncPending: !accountSynced || !cacheSynced}, nil
}

func (r *channelDetectionCoordinator) confirmInTransaction(ctx context.Context, input service.ChannelDetectionConfirmationRequest) (channelDetectionReceipt, error) {
	var receipt channelDetectionReceipt
	tx, err := r.accounts.client.Tx(ctx)
	if err != nil {
		return receipt, err
	}
	defer func() { _ = tx.Rollback() }()
	ctx = dbent.NewTxContext(ctx, tx)
	client := tx.Client()
	_, ok, err := lockChannelDetectionState(ctx, client, 0, "", input.Guard)
	if err != nil {
		return receipt, err
	}
	if !ok {
		return receipt, service.ErrDetectionChanged
	}

	var latestJSON string
	if err := scanDetectionRow(ctx, client, `SELECT value FROM settings WHERE key = $1 FOR UPDATE`, []any{&latestJSON}, service.SettingKeyChannelDetectionLatest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = service.ErrDetectionChanged
		}
		return receipt, err
	}
	var latest struct {
		Proofs map[int64]service.ChannelDetectionProof `json:"proofs"`
	}
	if err := json.Unmarshal([]byte(latestJSON), &latest); err != nil {
		return receipt, err
	}
	groupID, proof := input.Selection.GroupID, input.Proof
	stored := latest.Proofs[groupID]
	// A different group can reserve or publish without replacing this sample.
	// The versioned proof of the selected group must still match in full.
	if !proof.Attributable() || !stored.Attributable() || proof.SampleID != stored.SampleID ||
		proof.Version != stored.Version || proof.AccountID != stored.AccountID || proof.Sends != stored.Sends ||
		proof.RouteFingerprint != stored.RouteFingerprint || !proof.CompletedAt.Equal(*stored.CompletedAt) {
		return receipt, service.ErrDetectionChanged
	}

	if _, err := client.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES ($1, '{}', now()) ON CONFLICT (key) DO NOTHING`, channelDetectionReceiptsSetting); err != nil {
		return receipt, err
	}
	var receiptsJSON string
	var now time.Time
	if err := scanDetectionRow(ctx, client, `SELECT value, clock_timestamp() FROM settings WHERE key = $1 FOR UPDATE`, []any{&receiptsJSON, &now}, channelDetectionReceiptsSetting); err != nil {
		return receipt, err
	}
	if !freshDetectionProof(proof, now) {
		return receipt, service.ErrDetectionChanged
	}
	receipts, err := decodeDetectionReceipts(receiptsJSON)
	if err != nil {
		return receipt, err
	}
	if existing := receipts[groupID]; existing.SampleID == proof.SampleID {
		return existing, tx.Commit()
	}

	lockedGroups, err := lockDetectionMembershipGroups(ctx, client, proof.AccountID)
	if err != nil {
		return receipt, err
	}
	if !slices.Contains(lockedGroups, groupID) {
		return receipt, service.ErrDetectionChanged
	}
	var currentUntil, protection sql.NullTime
	err = scanDetectionRow(ctx, client, `
		SELECT temp_unschedulable_until, temp_unschedulable_protected_until
		FROM accounts WHERE id = $1 AND deleted_at IS NULL FOR UPDATE
	`, []any{&currentUntil, &protection}, proof.AccountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = service.ErrDetectionChanged
		}
		return receipt, err
	}
	if err := lockDetectionMemberships(ctx, client, proof.AccountID); err != nil {
		return receipt, err
	}
	accounts := r.accounts.accountTransactionScope(ctx)
	account, err := client.Account.Query().Where(dbaccount.IDEQ(proof.AccountID)).Only(ctx)
	if err != nil {
		return receipt, err
	}
	if account.ProxyID != nil {
		_, err := client.Proxy.Query().Where(dbproxy.IDEQ(*account.ProxyID)).ForShare().Only(ctx)
		if dbent.IsNotFound(err) {
			return receipt, service.ErrDetectionChanged
		}
		if err != nil {
			return receipt, err
		}
	}
	scope, err := detectionAccountScopeForConfirmation(ctx, accounts, input.Selection, proof)
	if err != nil {
		return receipt, err
	}
	if scope.Version != input.ScopeVersion || len(scope.Groups) != len(lockedGroups) {
		return receipt, service.ErrDetectionScopeChanged
	}
	for i, group := range scope.Groups {
		if group.ID != lockedGroups[i] {
			return receipt, service.ErrDetectionScopeChanged
		}
	}
	groups := &groupRepository{client: client, sql: client}
	group, err := groups.GetByIDLite(ctx, groupID)
	if err != nil {
		return receipt, err
	}
	if !group.IsActive() || group.ClaudeCodeOnly || group.Platform != input.GroupPlatform {
		return receipt, service.ErrDetectionChanged
	}
	members, err := accounts.ListAllByGroup(ctx, groupID)
	if err != nil {
		return receipt, err
	}
	if !slices.Contains(service.ResolveEffectiveGroupModelAllowlist(group.ModelAllowlist, members).Models, input.Selection.Model) {
		return receipt, service.ErrDetectionChanged
	}
	if err := scanDetectionRow(ctx, client, `SELECT clock_timestamp()`, []any{&now}); err != nil {
		return receipt, err
	}
	if !freshDetectionProof(proof, now) {
		return receipt, service.ErrDetectionChanged
	}
	if protection.Valid && protection.Time.After(now) {
		return receipt, service.ErrDetectionProtected
	}

	until := now.Add(24 * time.Hour)
	_, err = setTempUnschedulable(ctx, client, proof.AccountID, until, channelDetectionSuspensionReason)
	if err != nil {
		return receipt, err
	}
	if _, err := client.ExecContext(ctx, `
		UPDATE accounts SET temp_unschedulable_protected_until = $1,
			temp_unschedulable_revision = temp_unschedulable_revision + 1,
			updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond') WHERE id = $2
	`, until, proof.AccountID); err != nil {
		return receipt, err
	}
	// The protection itself changes the scheduler projection even when the
	// existing suspension deadline is already longer.
	if err := enqueueTempUnschedulable(ctx, client, proof.AccountID); err != nil {
		return receipt, err
	}
	if currentUntil.Valid && currentUntil.Time.After(until) {
		until = currentUntil.Time
	}
	receipt = channelDetectionReceipt{SampleID: proof.SampleID, ActorID: input.ActorID, AccountID: proof.AccountID, At: now, Until: until}
	for id, old := range receipts {
		if latest.Proofs[id].SampleID != old.SampleID {
			delete(receipts, id)
		}
	}
	receipts[groupID] = receipt
	encoded, err := json.Marshal(receipts)
	if err != nil {
		return receipt, err
	}
	if _, err := client.ExecContext(ctx, `UPDATE settings SET value = $1, updated_at = clock_timestamp() WHERE key = $2`, string(encoded), channelDetectionReceiptsSetting); err != nil {
		return receipt, err
	}
	return receipt, tx.Commit()
}

func freshDetectionProof(proof service.ChannelDetectionProof, now time.Time) bool {
	return proof.Attributable() && !proof.CompletedAt.After(now) && now.Sub(*proof.CompletedAt) < 24*time.Hour
}

func decodeDetectionReceipts(raw string) (map[int64]channelDetectionReceipt, error) {
	result := make(map[int64]channelDetectionReceipt)
	err := json.Unmarshal([]byte(raw), &result)
	return result, err
}

func lockDetectionMembershipGroups(ctx context.Context, client *dbent.Client, accountID int64) ([]int64, error) {
	rows, err := client.QueryContext(ctx, `
		SELECT g.id FROM groups AS g JOIN account_groups AS ag ON ag.group_id = g.id
		WHERE ag.account_id = $1 AND g.deleted_at IS NULL
		ORDER BY g.id FOR SHARE OF g`, accountID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func lockDetectionMemberships(ctx context.Context, client *dbent.Client, accountID int64) error {
	rows, err := client.QueryContext(ctx, `
		SELECT group_id FROM account_groups WHERE account_id = $1
		ORDER BY group_id FOR SHARE`, accountID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		// Drain the result so the database has acquired all row locks.
	}
	return rows.Err()
}

func detectionAccountScopeForConfirmation(ctx context.Context, accounts *accountRepository, selection service.ChannelDetectionSelection, proof service.ChannelDetectionProof) (*service.ChannelDetectionScope, error) {
	account, err := accounts.GetByID(ctx, proof.AccountID)
	if errors.Is(err, service.ErrAccountNotFound) {
		return nil, service.ErrDetectionChanged
	}
	if err != nil {
		return nil, err
	}
	if !service.DetectionAccountMatchesProof(account, proof) || !slices.Contains(account.GroupIDs, selection.GroupID) {
		return nil, service.ErrDetectionChanged
	}
	hasShadow, err := accounts.client.Account.Query().
		Where(dbaccount.ParentAccountIDEQ(account.ID), dbaccount.DeletedAtIsNil()).Exist(ctx)
	if err != nil {
		return nil, err
	}
	if hasShadow {
		return nil, service.ErrDetectionChanged
	}
	scope := &service.ChannelDetectionScope{AccountName: account.Name, Groups: make([]service.ChannelDetectionImpactGroup, 0, len(account.Groups))}
	for _, group := range account.Groups {
		scope.Groups = append(scope.Groups, service.ChannelDetectionImpactGroup{ID: group.ID, Name: group.Name})
	}
	slices.SortFunc(scope.Groups, func(a, b service.ChannelDetectionImpactGroup) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		default:
			return 0
		}
	})
	encoded, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	scope.Version = hex.EncodeToString(digest[:])
	return scope, nil
}
