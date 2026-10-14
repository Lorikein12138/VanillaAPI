//go:build unit

package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	baselineV0215MigrationCount       = 295
	baselineV0215Pre243MigrationCount = 294
	baselineV0215Unknown239SHA256     = "12a03c49e6c9c15edbbc992e36cbc140da849556176569fd987ade49610707ac"
	baselineV0215Existing242SHA256    = "69109068a1e53969028b325d3b3489d6db8d02d65634b79b7ddc50e78ef3cb91"
	baselineV0215Existing243SHA256    = "43c5e1bd7aafbfc07bff5e974b20e8401448306ab1896d34f513fbff749143c4"
	baselineV0215New242Migration      = "242_drop_platform_check_constraints.sql"
	baselineV0215Existing242Migration = "242_channel_monitor_v2_typesafe.sql"
	baselineV0215TailMigration        = "243_finalize_retired_schema_hygiene.sql"
	baselineV0215Unknown239Migration  = "239_channel_reasoning_effort_multipliers.sql"
)

func TestBaselineV0215MigrationIdentity(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	historicalNames := readMigrationNameManifest(t, "testdata/baseline_v0215_migrations.txt")
	require.Len(t, historicalNames, baselineV0215MigrationCount)
	require.GreaterOrEqual(t, len(names), len(historicalNames), "v0.2.15 基线后的迁移只能追加")
	for _, name := range historicalNames {
		require.Contains(t, names, name, "current migrations must retain historical filename %s", name)
	}
	require.Contains(t, historicalNames, baselineV0215Existing242Migration)
	require.Contains(t, historicalNames, baselineV0215New242Migration)
	require.Contains(t, historicalNames, baselineV0215TailMigration)
	require.Contains(t, historicalNames, baselineV0215Unknown239Migration)

	old242Index := sort.SearchStrings(historicalNames, baselineV0215Existing242Migration)
	new242Index := sort.SearchStrings(historicalNames, baselineV0215New242Migration)
	tailIndex := sort.SearchStrings(historicalNames, baselineV0215TailMigration)
	require.Less(t, old242Index, new242Index, "新 242 必须按完整 filename 排在本地 242 之后")
	require.Less(t, new242Index, tailIndex, "新 242 必须在本地 243 之前执行")
	require.Equal(t, 2, countMigrationPrefix(historicalNames, "242_"), "v0.2.15 历史视图必须有两个 242 文件")
	require.Equal(t, baselineV0215Pre243MigrationCount, tailIndex, "新装在 243 前应有 294 个迁移文件")

	for _, migration := range []struct {
		name string
		want string
	}{
		{name: baselineV0215Unknown239Migration, want: baselineV0215Unknown239SHA256},
		{name: baselineV0215Existing242Migration, want: baselineV0215Existing242SHA256},
		{name: baselineV0215TailMigration, want: baselineV0215Existing243SHA256},
	} {
		content, readErr := FS.ReadFile(migration.name)
		require.NoError(t, readErr)
		sum := sha256.Sum256(content)
		require.Equal(t, migration.want, hex.EncodeToString(sum[:]), "%s 的历史 SQL 字节必须保持", migration.name)
	}

	content, err := FS.ReadFile(baselineV0215New242Migration)
	require.NoError(t, err)
	comments := regexp.MustCompile(`(?m)--.*$`)
	sql := strings.Join(strings.Fields(comments.ReplaceAllString(string(content), "")), " ")
	require.Equal(t,
		"ALTER TABLE user_platform_quotas DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check; "+
			"ALTER TABLE composite_model_routes DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;",
		sql,
		"新 242 只能移除额度与复合路由两处命名 CHECK",
	)
}

func readMigrationNameManifest(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	require.Equal(t, names, sortedMigrationNames(names), "migration manifest must be sorted and unique")
	return names
}

func sortedMigrationNames(names []string) []string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return sorted
}

func countMigrationPrefix(names []string, prefix string) int {
	count := 0
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			count++
		}
	}
	return count
}
