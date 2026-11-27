package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

const channelDetectionLeaseSetting = "channel_detection_lease"

// The lease and the snapshot live in the same PostgreSQL settings store.
// Acquiring/releasing a lease competes for the same row lock as publishing.
// No database connection or transaction is held across a gateway request.
type channelDetectionCoordinator struct {
	db        *sql.DB
	accounts  *accountRepository
	tempCache service.TempUnschedCache
}

func NewChannelDetectionCoordinator(db *sql.DB, client *dbent.Client, schedulerCache service.SchedulerCache, tempCache service.TempUnschedCache) service.ChannelDetectionCoordinator {
	return &channelDetectionCoordinator{db: db, accounts: newAccountRepositoryWithSQL(client, db, schedulerCache), tempCache: tempCache}
}

type channelDetectionLeaseSlot struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

type channelDetectionLease struct {
	Version int                                 `json:"v"`
	Groups  map[int64]channelDetectionLeaseSlot `json:"groups"`
}

func decodeDetectionLease(raw string, expiry, now time.Time) (channelDetectionLease, string, bool) {
	lease := channelDetectionLease{Version: 2, Groups: make(map[int64]channelDetectionLeaseSlot)}
	if raw == "" {
		return lease, "", false
	}
	if _, err := uuid.Parse(raw); err == nil {
		if expiry.After(now) {
			return lease, raw, false
		}
		return lease, "", false
	}
	lease = channelDetectionLease{}
	if err := json.Unmarshal([]byte(raw), &lease); err != nil || lease.Version != 2 || lease.Groups == nil {
		return channelDetectionLease{}, "", true
	}
	for id, slot := range lease.Groups {
		if id <= 0 || slot.Owner == "" || slot.ExpiresAt.IsZero() {
			return channelDetectionLease{}, "", true
		}
	}
	return lease, "", false
}

func (lease channelDetectionLease) prune(now time.Time) {
	for id, slot := range lease.Groups {
		if !slot.ExpiresAt.After(now) {
			delete(lease.Groups, id)
		}
	}
}

func lockDetectionLease(ctx context.Context, tx *sql.Tx) (channelDetectionLease, time.Time, bool, error) {
	var raw string
	var expiry, now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT value, updated_at FROM settings WHERE key = $1 FOR UPDATE`,
		channelDetectionLeaseSetting).Scan(&raw, &expiry); err != nil {
		return channelDetectionLease{}, now, false, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return channelDetectionLease{}, now, false, err
	}
	lease, legacy, unknown := decodeDetectionLease(raw, expiry, now)
	return lease, now, legacy != "" || unknown, nil
}

func storeDetectionLease(ctx context.Context, tx *sql.Tx, lease channelDetectionLease) error {
	if len(lease.Groups) == 0 {
		_, err := tx.ExecContext(ctx, `UPDATE settings SET value = '', updated_at = clock_timestamp() WHERE key = $1`, channelDetectionLeaseSetting)
		return err
	}
	var expiry time.Time
	for _, slot := range lease.Groups {
		if slot.ExpiresAt.After(expiry) {
			expiry = slot.ExpiresAt
		}
	}
	data, err := json.Marshal(lease)
	if err != nil {
		return err
	}
	// Old workers compare only the outer timestamp. Keep them out until the
	// last v2 slot expires, including when an automatic rollback starts them.
	_, err = tx.ExecContext(ctx, `UPDATE settings SET value = $2, updated_at = $3 WHERE key = $1`, channelDetectionLeaseSetting, string(data), expiry)
	return err
}

func (r *channelDetectionCoordinator) TryAcquire(ctx context.Context, groupID int64, owner string, ttl time.Duration) (bool, error) {
	if groupID <= 0 || owner == "" || ttl <= 0 {
		return false, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key,value,updated_at) VALUES ($1,'',clock_timestamp())
		ON CONFLICT (key) DO NOTHING`, channelDetectionLeaseSetting); err != nil {
		return false, err
	}
	lease, now, blocked, err := lockDetectionLease(ctx, tx)
	lease.prune(now)
	if err != nil || blocked || lease.Groups[groupID].Owner != "" {
		return false, err
	}
	// SavePlan also locks the lease before the config. Read the limit under
	// those locks so a worker with an older plan cannot bypass a lowered cap.
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1 FOR SHARE`,
		service.SettingKeyChannelDetectionConfig).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	limit, err := service.ChannelDetectionConcurrency(raw)
	if err != nil || len(lease.Groups) >= limit {
		return false, err
	}
	lease.Groups[groupID] = channelDetectionLeaseSlot{Owner: owner, ExpiresAt: now.Add(ttl)}
	if err := storeDetectionLease(ctx, tx, lease); err != nil {
		return false, err
	}
	err = tx.Commit()
	return err == nil, err
}

func (r *channelDetectionCoordinator) ActiveLeases(ctx context.Context) (service.ChannelDetectionActiveLeases, error) {
	var raw string
	var expiry, now time.Time
	err := r.db.QueryRowContext(ctx, `SELECT value, updated_at, clock_timestamp() FROM settings WHERE key = $1`,
		channelDetectionLeaseSetting).Scan(&raw, &expiry, &now)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ChannelDetectionActiveLeases{}, nil
	}
	if err != nil {
		return service.ChannelDetectionActiveLeases{}, err
	}
	lease, legacy, unknown := decodeDetectionLease(raw, expiry, now)
	lease.prune(now)
	active := service.ChannelDetectionActiveLeases{Groups: make(map[int64]string, len(lease.Groups)), LegacyOwner: legacy, Blocked: unknown}
	for id, slot := range lease.Groups {
		active.Groups[id] = slot.Owner
	}
	return active, nil
}

func (r *channelDetectionCoordinator) Holds(ctx context.Context, groupID int64, owner string) (bool, error) {
	active, err := r.ActiveLeases(ctx)
	return err == nil && owner != "" && active.Groups[groupID] == owner, err
}

func (r *channelDetectionCoordinator) Release(ctx context.Context, groupID int64, owner string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lease, now, blocked, err := lockDetectionLease(ctx, tx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || blocked || owner == "" || lease.Groups[groupID].Owner != owner {
		return err
	}
	delete(lease.Groups, groupID)
	lease.prune(now)
	if err := storeDetectionLease(ctx, tx, lease); err != nil {
		return err
	}
	return tx.Commit()
}

// lockChannelDetectionState serializes operations with lease transfer and settings writes.
func lockChannelDetectionState(ctx context.Context, tx sqlExecutor, groupID int64, owner string, expected service.ChannelDetectionSnapshotGuard) (time.Time, bool, error) {
	var raw string
	var expiry time.Time
	err := scanDetectionRow(ctx, tx, `SELECT value, updated_at FROM settings WHERE key = $1 FOR UPDATE`, []any{&raw, &expiry}, channelDetectionLeaseSetting)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	var deadline time.Time
	if owner != "" {
		var now time.Time
		if err := scanDetectionRow(ctx, tx, `SELECT clock_timestamp()`, []any{&now}); err != nil {
			return time.Time{}, false, err
		}
		lease, _, _ := decodeDetectionLease(raw, expiry, now)
		slot := lease.Groups[groupID]
		if slot.Owner != owner || !slot.ExpiresAt.After(now) {
			return time.Time{}, false, nil
		}
		deadline = slot.ExpiresAt
	}
	// Settings writers and confirmation use the same lock order.
	for _, setting := range []struct{ key, value string }{
		{service.SettingKeyChannelDetectionConfig, expected.Config},
		{service.SettingKeyChannelDetectionFeature, expected.Feature},
	} {
		var actual string
		err := scanDetectionRow(ctx, tx, `SELECT value FROM settings WHERE key = $1 FOR SHARE`, []any{&actual}, setting.key)
		if errors.Is(err, sql.ErrNoRows) {
			if setting.value != "" {
				return time.Time{}, false, nil
			}
			continue
		}
		if err != nil || actual != setting.value {
			return time.Time{}, false, err
		}
	}
	return deadline, true, nil
}

func (r *channelDetectionCoordinator) SavePlan(ctx context.Context, expected service.ChannelDetectionSnapshotGuard, nextConfig, nextSnapshot string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// The first save may predate any worker. Create the released lease row so
	// all writers can still serialize in lease → config → feature → snapshot order.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key,value,updated_at) VALUES ($1,'',clock_timestamp())
		ON CONFLICT (key) DO NOTHING`, channelDetectionLeaseSetting); err != nil {
		return false, err
	}
	if _, ok, err := lockChannelDetectionState(ctx, tx, 0, "", expected); err != nil || !ok {
		return false, err
	}
	var current string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1 FOR UPDATE`, service.SettingKeyChannelDetectionLatest).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if current != expected.Snapshot {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key,value,updated_at) VALUES ($1,$2,clock_timestamp()),($3,$4,clock_timestamp())
		ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=EXCLUDED.updated_at`,
		service.SettingKeyChannelDetectionConfig, nextConfig, service.SettingKeyChannelDetectionLatest, nextSnapshot); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func scanDetectionRow(ctx context.Context, exec sqlExecutor, query string, dest []any, args ...any) error {
	rows, err := exec.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return rows.Scan(dest...)
}

func (r *channelDetectionCoordinator) ReserveSend(ctx context.Context, owner string, expected service.ChannelDetectionSnapshotGuard, groupID int64, groupRPM, dailyLimit int) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	deadline, ok, err := lockChannelDetectionState(ctx, tx, groupID, owner, expected)
	if err != nil || !ok {
		return false, err
	}
	// A fixed settings row survives restarts and config changes. Use the
	// database clock for the lease and UTC day, and debit before sending.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		SELECT 'channel_detection_daily_sends', '1', statement_timestamp()
		WHERE $2::int > 0 AND $1::timestamptz > clock_timestamp()
		ON CONFLICT (key) DO UPDATE SET
			value = CASE WHEN (settings.updated_at AT TIME ZONE 'UTC')::date <
				(EXCLUDED.updated_at AT TIME ZONE 'UTC')::date THEN '1'
				ELSE (settings.value::int + 1)::text END,
			updated_at = EXCLUDED.updated_at
		WHERE (settings.updated_at AT TIME ZONE 'UTC')::date <
			(EXCLUDED.updated_at AT TIME ZONE 'UTC')::date OR (
			(settings.updated_at AT TIME ZONE 'UTC')::date =
			(EXCLUDED.updated_at AT TIME ZONE 'UTC')::date AND settings.value::int < $2::int
		)`, deadline, dailyLimit)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if ok, err := reserveChannelDetectionGroupRPM(ctx, tx, groupID, groupRPM, deadline); err != nil || !ok {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// The active lease row already serializes writers. Store a rolling window
// per group under the same transaction as the daily debit, including sends
// while RPM is unlimited in case the administrator enables the limit mid-window.
func reserveChannelDetectionGroupRPM(ctx context.Context, tx *sql.Tx, groupID int64, limit int, deadline time.Time) (bool, error) {
	if groupID <= 0 || limit < 0 {
		return false, nil
	}
	key := "channel_detection_group_rpm:" + strconv.FormatInt(groupID, 10)
	var now time.Time
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT clock_timestamp(), COALESCE((SELECT value FROM settings WHERE key = $1), '[]')`, key).Scan(&now, &raw)
	if err != nil {
		return false, err
	}
	var previous []time.Time
	if err := json.Unmarshal([]byte(raw), &previous); err != nil {
		return false, err
	}
	cutoff := now.Add(-time.Minute)
	recent := previous[:0]
	for _, sentAt := range previous {
		if sentAt.After(cutoff) {
			recent = append(recent, sentAt)
		}
	}
	if !deadline.After(now) || (limit > 0 && len(recent) >= limit) {
		return false, nil
	}
	encoded, err := json.Marshal(append(recent, now.UTC()))
	if err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) SELECT $1, $2, $3 WHERE $4::timestamptz > clock_timestamp()
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
	`, key, string(encoded), now, deadline)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// IncrementDetectionQuotaUsed shares the normal quota update and transactional
// outbox without claiming an ordinary request or writing a user billing row.
func (r *accountRepository) IncrementDetectionQuotaUsed(ctx context.Context, id int64, amount float64) error {
	db, ok := r.sql.(*sql.DB)
	if !ok {
		return errors.New("detection account quota requires a database transaction")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := incrementUsageBillingAccountQuota(ctx, tx, id, amount, true, true)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if accountQuotaCrossed(state, amount) {
		r.syncSchedulerAccountSnapshot(ctx, id)
	}
	return nil
}

func (r *channelDetectionCoordinator) CompareAndSetSnapshot(ctx context.Context, groupID int64, owner string, expected service.ChannelDetectionSnapshotGuard, next string) (bool, error) {
	return r.writeSnapshot(ctx, groupID, owner, expected, next, 0)
}

func (r *channelDetectionCoordinator) PublishSnapshot(ctx context.Context, owner string, expected service.ChannelDetectionSnapshotGuard, next string, completedGroupID int64) (bool, error) {
	return r.writeSnapshot(ctx, completedGroupID, owner, expected, next, completedGroupID)
}

func (r *channelDetectionCoordinator) writeSnapshot(ctx context.Context, groupID int64, owner string, expected service.ChannelDetectionSnapshotGuard, next string, completedGroupID int64) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	deadline, ok, err := lockChannelDetectionState(ctx, tx, groupID, owner, expected)
	if err != nil || !ok {
		return false, err
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		SELECT $1::varchar, CASE WHEN $5::text = '0' THEN $2::text ELSE
			jsonb_set($2::jsonb, ARRAY['proofs', $5::text, 'completed_at'], to_jsonb(clock_timestamp()))::text
			END, clock_timestamp()
		WHERE $4::timestamptz > clock_timestamp()
			AND ($3::text = '' OR EXISTS (SELECT 1 FROM settings WHERE key = $1::varchar))
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
		WHERE settings.value = $3::text`,
		service.SettingKeyChannelDetectionLatest, next, expected.Snapshot, deadline, strconv.FormatInt(completedGroupID, 10))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
