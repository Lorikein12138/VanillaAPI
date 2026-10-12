package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

const channelMonitorV2V0215MigrationName = "245_channel_monitor_v2_v0215_platforms.sql"

func TestChannelMonitorV2V0215MigrationCatalogAndDefault(t *testing.T) {
	content, err := FS.ReadFile(channelMonitorV2V0215MigrationName)
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	catalogStart := strings.Index(sql, "WITH catalog(platform, sort_order) AS (")
	require.NotEqual(t, -1, catalogStart)
	catalogSection := sql[catalogStart+len("WITH catalog(platform, sort_order) AS ("):]
	catalogEnd := strings.Index(catalogSection, "), existing AS (")
	require.NotEqual(t, -1, catalogEnd)
	catalogSection = catalogSection[:catalogEnd]
	platformPattern := regexp.MustCompile(`\('([^']+)',\s*(\d+)\)`)
	matches := platformPattern.FindAllStringSubmatch(catalogSection, -1)
	gotPlatforms := make([]string, 0, len(matches))
	for index, match := range matches {
		sortOrder, parseErr := strconv.Atoi(match[2])
		require.NoError(t, parseErr)
		require.Equal(t, index+1, sortOrder)
		gotPlatforms = append(gotPlatforms, match[1])
	}
	require.Equal(t, domain.SupportedGroupPlatforms(), gotPlatforms)
	require.Len(t, gotPlatforms, 14)
	require.NotContains(t, gotPlatforms, "kiro")

	defaultPattern := regexp.MustCompile(`ALTER COLUMN platforms SET DEFAULT '([^']+)'::jsonb`)
	defaultMatch := defaultPattern.FindStringSubmatch(sql)
	require.Len(t, defaultMatch, 2)
	var defaults []struct {
		Platform string   `json:"platform"`
		Enabled  bool     `json:"enabled"`
		Models   []string `json:"models"`
	}
	require.NoError(t, json.Unmarshal([]byte(defaultMatch[1]), &defaults))
	require.Len(t, defaults, len(gotPlatforms))
	for index, item := range defaults {
		require.Equal(t, gotPlatforms[index], item.Platform)
		require.True(t, item.Enabled)
		require.Empty(t, item.Models)
	}

	require.Contains(t, sql, "DISTINCT ON (config.id, entry.value ->> 'platform')")
	require.Contains(t, sql, "existing.config_id = config.id AND existing.platform = catalog.platform")
	require.Contains(t, sql, "entry.ordinality AS sort_order")
	require.Contains(t, sql, "jsonb_agg(items.value ORDER BY items.phase, items.sort_order)")
	require.Contains(t, sql, "config.platforms IS DISTINCT FROM normalized.platforms")
	require.Contains(t, sql, "UPDATE channel_monitor_v2_config config")
	require.Contains(t, sql, "version = config.version + 1")
	require.Contains(t, sql, "updated_at = NOW()")
	require.NotContains(t, sql, "updated_by =")
	require.NotContains(t, sql, "group_scope_mode =")
	require.NotContains(t, sql, "health_thresholds =")
	require.NotContains(t, sql, "ignored_error_categories =")
	require.NotContains(t, sql, "BEGIN;")
	require.NotContains(t, sql, "COMMIT;")

	updates := regexp.MustCompile(`(?i)\bUPDATE\s+`).FindAllStringIndex(sql, -1)
	require.Len(t, updates, 1, "only the V2 config row may be updated")
	for _, forbidden := range []string{"INSERT INTO ", "DELETE FROM ", "TRUNCATE ", "UPDATE groups ", "UPDATE accounts "} {
		require.NotContains(t, strings.ToUpper(sql), forbidden)
	}
}

func TestChannelMonitorV2V0215MigrationPreservesHistory(t *testing.T) {
	historicalChecksums := map[string]string{
		"241_channel_monitor_v2_passive_coverage.sql": "5676a39a96963aabb1b5f00d2e5fa69ec34e5b97d4392c4df4e623ee0393b3ea",
		"242_channel_monitor_v2_typesafe.sql":         "69109068a1e53969028b325d3b3489d6db8d02d65634b79b7ddc50e78ef3cb91",
		"243_finalize_retired_schema_hygiene.sql":     "43c5e1bd7aafbfc07bff5e974b20e8401448306ab1896d34f513fbff749143c4",
		"244_clear_retired_runtime_data.sql":          "9646c2427f312a51d32d180f9076a0de21c8e8b9394f95190b998316f2126f82",
	}
	for name, want := range historicalChecksums {
		content, err := FS.ReadFile(name)
		require.NoError(t, err)
		sum := sha256.Sum256(content)
		require.Equal(t, want, hex.EncodeToString(sum[:]), "historical migration %s changed", name)
	}
}
