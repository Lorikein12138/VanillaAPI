package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func lockDetectionGroupDeletion(ctx context.Context, tx sqlExecutor) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO settings (key,value,updated_at) VALUES ($1,'',clock_timestamp())
		ON CONFLICT (key) DO NOTHING`, channelDetectionLeaseSetting); err != nil {
		return err
	}
	var key string
	return scanDetectionRow(ctx, tx, `SELECT key FROM settings WHERE key = $1 FOR UPDATE`, []any{&key}, channelDetectionLeaseSetting)
}

// Called while holding the common detection lease row, in the group deletion
// transaction. Keep the other groups' samples and active ownership intact.
func removeGroupFromDetection(ctx context.Context, tx sqlExecutor, id int64) error {
	if err := deleteDetectionGroupRPM(ctx, tx, id); err != nil {
		return err
	}
	if err := revokeDetectionLeases(ctx, tx, []int64{id}); err != nil {
		return err
	}

	config, err := readDetectionJSONObject(ctx, tx, service.SettingKeyChannelDetectionConfig)
	if err != nil {
		return err
	}
	var oldRevision, newRevision json.RawMessage
	var selections []json.RawMessage
	if json.Unmarshal(config["groups"], &selections) == nil {
		count := len(selections)
		selections = slices.DeleteFunc(selections, func(selection json.RawMessage) bool {
			return detectionJSONGroupID(selection) == id
		})
		if len(selections) != count {
			oldRevision = config["revision"]
			newRevision, err = json.Marshal(uuid.NewString())
			if err != nil {
				return err
			}
			config["revision"] = newRevision
			config["groups"], err = json.Marshal(selections)
			if err != nil {
				return err
			}
			if len(selections) == 0 {
				config["enabled"] = json.RawMessage("false")
			}
			if err := writeDetectionJSONObject(ctx, tx, service.SettingKeyChannelDetectionConfig, config); err != nil {
				return err
			}
		}
	}

	snapshot, err := readDetectionJSONObject(ctx, tx, service.SettingKeyChannelDetectionLatest)
	if err != nil || snapshot == nil {
		return err
	}
	changed := false
	if newRevision != nil && string(snapshot["revision"]) == string(oldRevision) {
		snapshot["revision"] = newRevision
		changed = true
	}
	groupKey := strconv.FormatInt(id, 10)
	for _, field := range []string{"samples", "attempted_at", "running_by_group", "proofs", "no_send"} {
		var groups map[string]json.RawMessage
		if json.Unmarshal(snapshot[field], &groups) != nil || groups[groupKey] == nil {
			continue
		}
		delete(groups, groupKey)
		snapshot[field], err = json.Marshal(groups)
		if err != nil {
			return err
		}
		changed = true
	}
	if detectionJSONGroupID(snapshot["running"]) == id {
		delete(snapshot, "running")
		changed = true
	}
	if !changed {
		return nil
	}
	return writeDetectionJSONObject(ctx, tx, service.SettingKeyChannelDetectionLatest, snapshot)
}

func detectionJSONGroupID(raw json.RawMessage) int64 {
	var group struct {
		ID int64 `json:"group_id"`
	}
	if json.Unmarshal(raw, &group) != nil {
		return 0
	}
	return group.ID
}

func readDetectionJSONObject(ctx context.Context, tx sqlExecutor, key string) (map[string]json.RawMessage, error) {
	var raw string
	if err := scanDetectionRow(ctx, tx, `SELECT value FROM settings WHERE key = $1 FOR UPDATE`, []any{&raw}, key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(raw), &fields) != nil {
		// An unknown legacy payload cannot safely be rewritten.
		return nil, nil
	}
	return fields, nil
}

func writeDetectionJSONObject(ctx context.Context, tx sqlExecutor, key string, fields map[string]json.RawMessage) error {
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE settings SET value = $2, updated_at = clock_timestamp() WHERE key = $1`, key, string(raw))
	return err
}
