//go:build unit

package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const retiredRuntimeDataMigration = "244_clear_retired_runtime_data.sql"

// TestRetiredRuntimeDataMigrationContract 守卫 244 只清两类无人读取的运行数据：
// 软删除账号的凭据，以及 v2 停用期间冻结的采样。不删表、不删列、不级联，
// 配置行与聚合水位不动，未删除账号的凭据不动。
func TestRetiredRuntimeDataMigrationContract(t *testing.T) {
	content, err := FS.ReadFile(retiredRuntimeDataMigration)
	require.NoError(t, err)
	code := strings.Join(strings.Fields(schemaHygieneLineComment.ReplaceAllString(string(content), "")), " ")

	require.Contains(t, code, "SET LOCAL lock_timeout = '5s';")
	require.Contains(t, code, "SET LOCAL statement_timeout = '60s';")
	for _, forbidden := range []string{
		`(?i)\bCASCADE\b`,
		`(?i)\bDROP\b`,
		`(?i)\bALTER\b`,
		`(?i)\bDELETE\b`,
		`(?i)\bINSERT\b`,
		`(?i)\b(BEGIN|COMMIT|ROLLBACK)\s*;`,
		`(?i)channel_monitor_v2_watermarks`,
	} {
		require.NotRegexp(t, regexp.MustCompile(forbidden), code)
	}

	require.Equal(t, 1, strings.Count(code, "UPDATE "), "只允许一条 UPDATE")
	require.Contains(t, code, "UPDATE accounts SET credentials = '{}'::jsonb WHERE deleted_at IS NOT NULL AND credentials <> '{}'::jsonb;")

	require.Equal(t, 1, strings.Count(code, "TRUNCATE "), "只允许一条 TRUNCATE")
	gate := "AND EXISTS (SELECT 1 FROM public.channel_monitor_v2_config WHERE id = 1 AND NOT enabled) THEN"
	require.Contains(t, code, gate)
	require.Less(t, mustIndexOf(t, code, gate), mustIndexOf(t, code, "TRUNCATE TABLE"), "TRUNCATE 必须在停用判断之内")
	require.NotContains(t, code, "TRUNCATE TABLE public.channel_monitor_v2_config")

	truncated := code[mustIndexOf(t, code, "TRUNCATE TABLE")+len("TRUNCATE TABLE"):]
	truncated = strings.TrimSpace(truncated[:strings.Index(truncated, ";")])
	var tables []string
	for _, table := range strings.Split(truncated, ",") {
		tables = append(tables, strings.TrimSpace(table))
	}
	require.ElementsMatch(t, []string{
		"public.channel_monitor_v2_metrics_1m",
		"public.channel_monitor_v2_metrics_rollup",
		"public.channel_monitor_v2_error_metrics_1m",
		"public.channel_monitor_v2_error_metrics_rollup",
		"public.channel_monitor_v2_user_metrics_1m",
		"public.channel_monitor_v2_user_metrics_rollup",
		"public.channel_monitor_v2_latency_histograms_1m",
		"public.channel_monitor_v2_latency_histograms_rollup",
	}, tables)
}
