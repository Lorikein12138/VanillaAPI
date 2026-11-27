//go:build unit

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestChannelDetectionPostgresGroupLifecycle(t *testing.T) {
	for _, operation := range []string{"save-plan", "soft-delete", "upgrade"} {
		t.Run(operation, func(t *testing.T) {
			db, workerDB := openDetectionTestDatabases(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, db)))
			require.NoError(t, client.Schema.Create(ctx))
			for _, name := range []string{"047_add_user_group_rate_multipliers.sql", "081_create_channels.sql"} {
				migration, err := migrations.FS.ReadFile(name)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, string(migration))
				require.NoError(t, err)
			}
			_, err := db.ExecContext(ctx, `CREATE TABLE scheduler_outbox (
				event_type TEXT NOT NULL, account_id BIGINT, group_id BIGINT, payload JSONB, dedup_key TEXT
			);
			CREATE UNIQUE INDEX scheduler_outbox_fixture_dedup ON scheduler_outbox(dedup_key) WHERE dedup_key IS NOT NULL;`)
			require.NoError(t, err)
			leaving := client.Group.Create().SetName("Leaving").SaveX(ctx).ID
			staying := client.Group.Create().SetName("Staying").SaveX(ctx).ID
			user := client.User.Create().SetEmail("fixture@example.test").SetPasswordHash("fixture").SaveX(ctx)
			key := client.APIKey.Create().SetUserID(user.ID).SetName("Fixture").SetKey("fixture").SetGroupID(leaving).SaveX(ctx)
			account := client.Account.Create().SetName("Fixture account").SetType(service.AccountTypeAPIKey).
				SetPlatform(service.PlatformOpenAI).SaveX(ctx)
			log := client.UsageLog.Create().SetUserID(user.ID).SetAPIKeyID(key.ID).SetAccountID(account.ID).
				SetRequestID("group-history").SetModel("fixture").SetGroupID(leaving).SaveX(ctx)
			for _, id := range []int64{leaving, staying} {
				_, err = db.ExecContext(ctx, `WITH channel AS (INSERT INTO channels (name) VALUES ($1) RETURNING id)
					INSERT INTO channel_groups (channel_id, group_id) SELECT id, $2 FROM channel`, fmt.Sprint(id), id)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, `INSERT INTO user_group_rate_multipliers (user_id, group_id, rate_multiplier)
					VALUES ($1, $2, 2)`, user.ID, id)
				require.NoError(t, err)
			}
			plan := fmt.Sprintf(`{"revision":"before","enabled":true,"interval_minutes":60,"concurrent_groups":2,"groups":[{"group_id":%d,"model":"one"},{"group_id":%d,"model":"two"}]}`, leaving, staying)
			snapshot := fmt.Sprintf(`{"revision":"before","samples":{"%d":{"sample":"leaving"},"%d":{"sample":"staying"}}}`, leaving, staying)
			const missing = 999999
			if operation == "upgrade" {
				plan = fmt.Sprintf(`{"revision":"before","enabled":true,"interval_minutes":60,"concurrent_groups":2,"groups":[{"group_id":%d,"model":"one"},{"group_id":%d,"model":"two"},{"group_id":%d,"model":"missing"}]}`, leaving, staying, missing)
				snapshot = fmt.Sprintf(`{"revision":"before","samples":{"%d":{"sample":"leaving"},"%d":{"sample":"staying"},"%d":{"sample":"missing"}}}`, leaving, staying, missing)
				_, err = db.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES ($1,'[]',now())`, detectionGroupRPMKey(missing))
				require.NoError(t, err)
			}
			const receipts = `{"1":{"sample_id":"history","actor_id":3,"account_id":4}}`
			_, err = db.ExecContext(ctx, `INSERT INTO settings (key,value,updated_at) VALUES
				($1,'on',now()),($2,$3,now()),($4,$5,now()),($6,$7,now())`,
				service.SettingKeyChannelDetectionFeature, service.SettingKeyChannelDetectionConfig, plan,
				service.SettingKeyChannelDetectionLatest, snapshot, channelDetectionReceiptsSetting, receipts)
			require.NoError(t, err)
			editor, worker := &channelDetectionCoordinator{db: db}, &channelDetectionCoordinator{db: workerDB}
			old := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: plan, Snapshot: snapshot}
			for _, id := range []int64{leaving, staying} {
				ok, err := worker.TryAcquire(ctx, id, "worker", time.Minute)
				require.NoError(t, err)
				require.True(t, ok)
				ok, err = worker.ReserveSend(ctx, "worker", old, id, 0, 20)
				require.NoError(t, err)
				require.True(t, ok)
			}
			switch operation {
			case "save-plan":
				nextPlan := fmt.Sprintf(`{"revision":"after","enabled":true,"groups":[{"group_id":%d,"model":"two"}]}`, staying)
				nextSnapshot := fmt.Sprintf(`{"revision":"after","samples":{"%d":{"sample":"staying"}}}`, staying)
				ok, err := editor.SavePlan(ctx, old, nextPlan, nextSnapshot)
				require.NoError(t, err)
				require.True(t, ok)
			case "soft-delete":
				_, err := newGroupRepositoryWithSQL(client, db).DeleteCascade(ctx, leaving)
				require.NoError(t, err)
			case "upgrade":
				_, err := db.ExecContext(ctx, `UPDATE groups SET deleted_at=now() WHERE id=$1`, leaving)
				require.NoError(t, err)
				migration, err := migrations.FS.ReadFile("255_group_lifecycle_cleanup.sql")
				require.NoError(t, err)
				for range 2 {
					_, err := db.ExecContext(ctx, string(migration))
					require.NoError(t, err)
				}
			}

			holds, err := worker.Holds(ctx, leaving, "worker")
			require.NoError(t, err)
			require.False(t, holds)
			holds, err = worker.Holds(ctx, staying, "worker")
			require.NoError(t, err)
			require.True(t, holds)
			read := func(key string) string {
				t.Helper()
				var value string
				require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&value))
				return value
			}
			var rate string
			require.ErrorIs(t, db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`,
				detectionGroupRPMKey(leaving)).Scan(&rate), sql.ErrNoRows)
			require.ErrorIs(t, db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=$1`,
				detectionGroupRPMKey(missing)).Scan(&rate), sql.ErrNoRows)
			require.NotEmpty(t, read(detectionGroupRPMKey(staying)))
			require.Equal(t, receipts, read(channelDetectionReceiptsSetting))
			next := service.ChannelDetectionSnapshotGuard{Feature: "on", Config: read(service.SettingKeyChannelDetectionConfig),
				Snapshot: read(service.SettingKeyChannelDetectionLatest)}
			var config struct {
				service.ChannelDetectionConfig
				Revision string
			}
			var samples struct {
				Revision string
				Samples  map[string]json.RawMessage
			}
			require.NoError(t, json.Unmarshal([]byte(next.Config), &config))
			require.NoError(t, json.Unmarshal([]byte(next.Snapshot), &samples))
			require.NotEqual(t, "before", config.Revision)
			require.Equal(t, config.Revision, samples.Revision)
			require.Equal(t, []service.ChannelDetectionSelection{{GroupID: staying, Model: "two"}}, config.Groups)
			require.Len(t, samples.Samples, 1)
			require.JSONEq(t, `{"sample":"staying"}`, string(samples.Samples[strconv.FormatInt(staying, 10)]))
			ok, err := worker.ReserveSend(ctx, "worker", old, leaving, 0, 20)
			require.NoError(t, err)
			require.False(t, ok)
			ok, err = worker.CompareAndSetSnapshot(ctx, leaving, "worker", old, "stale")
			require.NoError(t, err)
			require.False(t, ok)
			ok, err = worker.CompareAndSetSnapshot(ctx, staying, "worker", next, next.Snapshot)
			require.NoError(t, err)
			require.True(t, ok)

			require.Equal(t, leaving, *client.APIKey.GetX(ctx, key.ID).GroupID)
			require.Equal(t, leaving, *client.UsageLog.GetX(ctx, log.ID).GroupID)
			if operation != "save-plan" {
				for _, table := range []string{"channel_groups", "user_group_rate_multipliers"} {
					var count int
					require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE group_id=$1`, table), leaving).Scan(&count))
					require.Zero(t, count)
					require.NoError(t, db.QueryRowContext(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE group_id=$1`, table), staying).Scan(&count))
					require.Equal(t, 1, count)
				}
				_, err := editor.SavePlan(ctx, next, old.Config, next.Snapshot)
				require.ErrorIs(t, err, service.ErrGroupNotFound)
			}
			if operation == "upgrade" {
				_, err := db.ExecContext(ctx, `UPDATE groups SET deleted_at=now() WHERE id=$1`, staying)
				require.NoError(t, err)
				migration, err := migrations.FS.ReadFile("255_group_lifecycle_cleanup.sql")
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, string(migration))
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal([]byte(read(service.SettingKeyChannelDetectionConfig)), &config))
				samples.Samples = nil
				require.NoError(t, json.Unmarshal([]byte(read(service.SettingKeyChannelDetectionLatest)), &samples))
				require.False(t, config.Enabled)
				require.Empty(t, config.Groups)
				require.Empty(t, samples.Samples)
				require.Equal(t, config.Revision, samples.Revision)
				require.Empty(t, read(channelDetectionLeaseSetting))
				require.Equal(t, receipts, read(channelDetectionReceiptsSetting))
			}
		})
	}
}

func TestChannelDetectionPostgresMigrationPrunesAlreadyDetachedOwners(t *testing.T) {
	db, _ := openDetectionTestDatabases(t)
	ctx := t.Context()
	_, err := db.ExecContext(ctx, `
		CREATE TABLE groups (id BIGINT PRIMARY KEY, deleted_at TIMESTAMPTZ);
		CREATE TABLE channel_groups (group_id BIGINT);
		CREATE TABLE user_group_rate_multipliers (group_id BIGINT);
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL);
		INSERT INTO groups VALUES (1, now()), (2, NULL), (3, NULL);
		INSERT INTO settings VALUES
			('channel_detection_config', '{"revision":"already-edited","enabled":true,"groups":[{"group_id":2,"model":"fixture"}]}', now()),
			('channel_detection_latest', '{"revision":"already-edited","samples":{"1":"old","2":"active","3":"unselected"},"running":{"group_id":1,"owner":"legacy"},"running_by_group":{"1":{"owner":"old"},"2":{"owner":"keep"}}}', now()),
			('channel_detection_group_rpm:1', '[]', now()),
			('channel_detection_group_rpm:2', '[]', now()),
			('channel_detection_group_rpm:3', '[]', now()),
			('channel_detection_confirmation_receipts', '{"1":{"sample_id":"history"}}', now()),
			('channel_detection_lease', jsonb_build_object('v', 2, 'groups', jsonb_build_object(
				'1', jsonb_build_object('owner', 'old', 'expires_at', now() + interval '5 minutes'),
				'2', jsonb_build_object('owner', 'keep', 'expires_at', now() + interval '2 minutes'),
				'3', jsonb_build_object('owner', 'unselected', 'expires_at', now() + interval '3 minutes'))), now() + interval '5 minutes');
	`)
	require.NoError(t, err)
	migration, err := migrations.FS.ReadFile("255_group_lifecycle_cleanup.sql")
	require.NoError(t, err)
	const state = `SELECT jsonb_object_agg(key, jsonb_build_array(value, updated_at))::text FROM settings`
	var first, second string
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, state).Scan(&first))
	_, err = db.ExecContext(ctx, string(migration))
	require.NoError(t, err)
	require.NoError(t, db.QueryRowContext(ctx, state).Scan(&second))
	require.Equal(t, first, second, "the second execution must not rewrite any setting")

	coordinator := &channelDetectionCoordinator{db: db}
	active, err := coordinator.ActiveLeases(ctx)
	require.NoError(t, err)
	require.Equal(t, map[int64]string{2: "keep"}, active.Groups)
	var snapshot, config, receipt string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT
		(SELECT value FROM settings WHERE key='channel_detection_latest'),
		(SELECT value FROM settings WHERE key='channel_detection_config'),
		(SELECT value FROM settings WHERE key='channel_detection_confirmation_receipts')`).Scan(&snapshot, &config, &receipt))
	require.JSONEq(t, `{"revision":"already-edited","samples":{"2":"active"},"running_by_group":{"2":{"owner":"keep"}}}`, snapshot)
	require.JSONEq(t, `{"revision":"already-edited","enabled":true,"groups":[{"group_id":2,"model":"fixture"}]}`, config)
	require.JSONEq(t, `{"1":{"sample_id":"history"}}`, receipt)
	var rpmCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key LIKE 'channel_detection_group_rpm:%'`).Scan(&rpmCount))
	require.Equal(t, 1, rpmCount)
}
