package migrations

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2TypeSafeMigrationCatalogAndIdempotentUpdate(t *testing.T) {
	content, err := FS.ReadFile("242_channel_monitor_v2_typesafe.sql")
	require.NoError(t, err)
	sql := strings.Join(strings.Fields(string(content)), " ")

	catalogSection := sql[strings.Index(sql, "WITH catalog(platform, sort_order) AS (")+len("WITH catalog(platform, sort_order) AS ("):]
	catalogSection = catalogSection[:strings.Index(catalogSection, "), existing AS (")]
	platformPattern := regexp.MustCompile(`\('([^']+)',\s*(\d+)\)`)
	matches := platformPattern.FindAllStringSubmatch(catalogSection, -1)
	gotPlatforms := make([]string, 0, len(matches))
	for index, match := range matches {
		sortOrder, parseErr := strconv.Atoi(match[2])
		require.NoError(t, parseErr)
		require.Equal(t, index+1, sortOrder)
		gotPlatforms = append(gotPlatforms, match[1])
	}
	historicalPlatforms := []string{
		"anthropic",
		"openai",
		"gemini",
		"antigravity",
		"grok",
		"kimi",
		"zhipu",
		"deepseek",
		"minimax",
		"opencode_go",
		"typesafe",
		"composite",
	}
	require.Equal(t, historicalPlatforms, gotPlatforms)
	require.Len(t, gotPlatforms, 12)
	require.NotContains(t, gotPlatforms, "kiro")
	require.NotContains(t, gotPlatforms, "command_code")
	require.NotContains(t, gotPlatforms, "cline")

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
	require.Contains(t, sql, "version = config.version + 1")
	require.Contains(t, sql, "updated_at = NOW()")
	require.NotContains(t, sql, "updated_by =")
	require.NotContains(t, sql, "group_scope_mode =")
	require.NotContains(t, sql, "synthetic_empty_buckets_enabled =")
	require.NotContains(t, sql, "health_thresholds =")
	require.NotContains(t, sql, "ignored_error_categories =")
	require.NotContains(t, sql, "BEGIN;")
	require.NotContains(t, sql, "COMMIT;")
}
