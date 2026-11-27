package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const channelDetectionLeaseSetting = "channel_detection_lease"

// The lease and the snapshot live in the same PostgreSQL settings store.
// Acquiring/releasing a lease competes for the same row lock as publishing.
// No database connection or transaction is held across a gateway request.
type channelDetectionCoordinator struct {
	db *sql.DB
}

func NewChannelDetectionCoordinator(db *sql.DB) service.ChannelDetectionCoordinator {
	return &channelDetectionCoordinator{db: db}
}

func (r *channelDetectionCoordinator) TryAcquire(ctx context.Context, owner string, ttl time.Duration) (bool, error) {
	// updated_at records the expiry for this internal lease row. An empty owner
	// represents a released lease. The database clock is shared by all workers.
	result, err := r.db.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		VALUES ($1, $2, clock_timestamp() + $3::bigint * interval '1 millisecond')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value,
			updated_at = clock_timestamp() + $3::bigint * interval '1 millisecond'
		WHERE settings.value = '' OR settings.updated_at <= clock_timestamp()`,
		channelDetectionLeaseSetting, owner, ttl.Milliseconds())
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (r *channelDetectionCoordinator) Holds(ctx context.Context, owner string) (bool, error) {
	var held bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM settings WHERE key = $1 AND value = $2 AND updated_at > clock_timestamp()
		)`, channelDetectionLeaseSetting, owner).Scan(&held)
	return held, err
}

func (r *channelDetectionCoordinator) Release(ctx context.Context, owner string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE settings SET value = '', updated_at = clock_timestamp()
		WHERE key = $1 AND value = $2`, channelDetectionLeaseSetting, owner)
	return err
}

// lockChannelDetectionState serializes operations with lease transfer and settings writes.
func lockChannelDetectionState(ctx context.Context, tx *sql.Tx, owner string, expected service.ChannelDetectionSnapshotGuard) (bool, error) {
	// Hold the lease row first. A successor cannot reserve until this
	// transaction either commits its snapshot or rolls back.
	var leaseOwner string
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1 FOR UPDATE`, channelDetectionLeaseSetting).Scan(&leaseOwner)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || leaseOwner != owner {
		return false, err
	}
	// Enabling/disabling or editing the schedule locks these same settings
	// rows. The caller's exact versions must still be present at commit.
	for _, setting := range []struct{ key, value string }{
		{service.SettingKeyChannelDetectionConfig, expected.Config},
		{service.SettingKeyChannelDetectionFeature, expected.Feature},
	} {
		var actual string
		err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1 FOR SHARE`, setting.key).Scan(&actual)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil || actual != setting.value {
			return false, err
		}
	}
	return true, nil
}

func (r *channelDetectionCoordinator) ReserveSend(ctx context.Context, owner string, expected service.ChannelDetectionSnapshotGuard, groupID int64, groupRPM, dailyLimit int) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	if ok, err := lockChannelDetectionState(ctx, tx, owner, expected); err != nil || !ok {
		return false, err
	}
	// A fixed settings row survives restarts and config changes. Use the
	// database clock for the lease and UTC day, and debit before sending.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		SELECT 'channel_detection_daily_sends', '1', statement_timestamp()
		WHERE $2::int > 0 AND EXISTS (
			SELECT 1 FROM settings WHERE key = $1 AND updated_at > clock_timestamp()
		)
		ON CONFLICT (key) DO UPDATE SET
			value = CASE WHEN (settings.updated_at AT TIME ZONE 'UTC')::date <
				(EXCLUDED.updated_at AT TIME ZONE 'UTC')::date THEN '1'
				ELSE (settings.value::int + 1)::text END,
			updated_at = EXCLUDED.updated_at
		WHERE (settings.updated_at AT TIME ZONE 'UTC')::date <
			(EXCLUDED.updated_at AT TIME ZONE 'UTC')::date OR (
			(settings.updated_at AT TIME ZONE 'UTC')::date =
			(EXCLUDED.updated_at AT TIME ZONE 'UTC')::date AND settings.value::int < $2::int
		)`, channelDetectionLeaseSetting, dailyLimit)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if ok, err := reserveChannelDetectionGroupRPM(ctx, tx, groupID, groupRPM); err != nil || !ok {
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
func reserveChannelDetectionGroupRPM(ctx context.Context, tx *sql.Tx, groupID int64, limit int) (bool, error) {
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
	if limit > 0 && len(recent) >= limit {
		return false, nil
	}
	encoded, err := json.Marshal(append(recent, now.UTC()))
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at) VALUES ($1, $2, $3)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
	`, key, string(encoded), now)
	return err == nil, err
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

func (r *channelDetectionCoordinator) CompareAndSetSnapshot(ctx context.Context, owner string, expected service.ChannelDetectionSnapshotGuard, next string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	if ok, err := lockChannelDetectionState(ctx, tx, owner, expected); err != nil || !ok {
		return false, err
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key, value, updated_at)
		SELECT $1::varchar, $2::text, clock_timestamp()
		WHERE EXISTS (SELECT 1 FROM settings WHERE key = $4::varchar AND updated_at > clock_timestamp())
			AND ($3::text = '' OR EXISTS (SELECT 1 FROM settings WHERE key = $1::varchar))
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
		WHERE settings.value = $3::text`,
		service.SettingKeyChannelDetectionLatest, next, expected.Snapshot, channelDetectionLeaseSetting)
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
