package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/apikey"
	"github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/ent/user"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const channelDetectionLeaseSetting = "channel_detection_lease"

func NewChannelDetectionKeyRepository(client *dbent.Client) service.ChannelDetectionKeyRepository {
	return &apiKeyRepository{client: client}
}

func (r *apiKeyRepository) GetChannelDetectionKey(ctx context.Context, id int64, includeSecret bool) (*service.APIKey, error) {
	fields := []string{
		apikey.FieldID, apikey.FieldUserID, apikey.FieldGroupID, apikey.FieldStatus,
		apikey.FieldQuota, apikey.FieldQuotaUsed, apikey.FieldExpiresAt,
		apikey.FieldRateLimit5h, apikey.FieldRateLimit1d, apikey.FieldRateLimit7d,
		apikey.FieldUsage5h, apikey.FieldUsage1d, apikey.FieldUsage7d,
		apikey.FieldWindow5hStart, apikey.FieldWindow1dStart, apikey.FieldWindow7dStart,
	}
	if includeSecret {
		fields = append(fields, apikey.FieldKey)
	}
	query := r.activeQuery().Where(apikey.IDEQ(id)).
		WithUser(func(q *dbent.UserQuery) { q.Select(user.FieldID, user.FieldStatus) }).
		WithGroup(func(q *dbent.GroupQuery) { q.Select(group.FieldID, group.FieldStatus) })
	query.Select(fields...)
	key, err := query.Only(ctx)
	if dbent.IsNotFound(err) {
		return nil, service.ErrAPIKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	return apiKeyEntityToService(key), nil
}

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

func (r *channelDetectionCoordinator) CompareAndSetSnapshot(ctx context.Context, owner string, expected service.ChannelDetectionSnapshotGuard, next string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	// Hold the lease row first. A successor cannot reserve until this
	// transaction either commits its snapshot or rolls back.
	var leaseOwner string
	err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = $1 FOR UPDATE`, channelDetectionLeaseSetting).Scan(&leaseOwner)
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
