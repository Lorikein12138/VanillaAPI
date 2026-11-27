//go:build unit

package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestChannelDetectionPostgresConcurrentUserGroupRates(t *testing.T) {
	first, second := openDetectionTestDatabases(t)
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	defer cancel()
	_, err := first.ExecContext(ctx, `
		CREATE TABLE users (id BIGINT PRIMARY KEY, deleted_at TIMESTAMPTZ);
		CREATE TABLE groups (id BIGINT PRIMARY KEY, deleted_at TIMESTAMPTZ);
		INSERT INTO users (id) VALUES (7);
		INSERT INTO groups (id) VALUES (11), (12);
	`)
	require.NoError(t, err)
	for _, name := range []string{"047_add_user_group_rate_multipliers.sql", "127_add_user_group_rpm_override.sql"} {
		migration, err := migrations.FS.ReadFile(name)
		require.NoError(t, err)
		_, err = first.ExecContext(ctx, string(migration))
		require.NoError(t, err)
	}
	_, err = first.ExecContext(ctx, `
		INSERT INTO user_group_rate_multipliers (user_id, group_id, rate_multiplier, rpm_override)
		VALUES (7, 11, 1, 30), (7, 12, 1, 40);
		CREATE FUNCTION pause_rate_clear() RETURNS trigger LANGUAGE plpgsql AS $body$
		BEGIN
			IF NEW.rate_multiplier IS NULL THEN PERFORM pg_sleep(0.2); END IF;
			RETURN NEW;
		END;
		$body$;
		CREATE TRIGGER pause_rate_clear AFTER UPDATE ON user_group_rate_multipliers
		FOR EACH ROW EXECUTE FUNCTION pause_rate_clear();
	`)
	require.NoError(t, err)

	r1, r2 := NewUserGroupRateRepository(first), NewUserGroupRateRepository(second)
	runTogether := func(a, b map[int64]*float64) {
		t.Helper()
		// Keep both requests queued at the user row before releasing them. The
		// trigger widens the clear/upsert interleaving if the user lock is absent.
		blocker, err := first.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = blocker.Rollback() }()
		_, err = blocker.ExecContext(ctx, `SELECT id FROM users WHERE id = 7 FOR NO KEY UPDATE`)
		require.NoError(t, err)
		ready, results := make(chan struct{}, 2), make(chan error, 2)
		go func() { ready <- struct{}{}; results <- r1.SyncUserGroupRates(ctx, 7, a) }()
		go func() { ready <- struct{}{}; results <- r2.SyncUserGroupRates(ctx, 7, b) }()
		<-ready
		<-ready
		select {
		case err := <-results:
			t.Fatalf("write completed while the user was locked: %v", err)
		case <-time.After(60 * time.Millisecond):
		}
		require.NoError(t, blocker.Commit())
		require.NoError(t, <-results)
		require.NoError(t, <-results)
	}
	read := func(id int64, rpm int) sql.NullFloat64 {
		t.Helper()
		var rate sql.NullFloat64
		var gotRPM int
		require.NoError(t, first.QueryRowContext(ctx, `SELECT rate_multiplier, rpm_override
			FROM user_group_rate_multipliers WHERE user_id = 7 AND group_id = $1`, id).Scan(&rate, &gotRPM))
		require.Equal(t, rpm, gotRPM)
		return rate
	}
	runTogether(map[int64]*float64{11: nil, 12: new(2.0)}, map[int64]*float64{12: nil, 11: new(3.0)})
	a, b := read(11, 30), read(12, 40)
	require.True(t, a.Valid != b.Valid, "exactly one override survives the serial writes")
	if a.Valid {
		require.Equal(t, 3.0, a.Float64)
	} else {
		require.Equal(t, 2.0, b.Float64)
	}
	runTogether(map[int64]*float64{}, map[int64]*float64{11: new(4.0)})
	a, b = read(11, 30), read(12, 40)
	require.False(t, b.Valid)
	if a.Valid {
		require.Equal(t, 4.0, a.Float64)
	}
}
