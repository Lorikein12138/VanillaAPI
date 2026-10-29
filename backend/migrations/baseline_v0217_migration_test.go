//go:build unit

package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBaselineV0217MigrationIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/baseline_v0217_prior_migrations.sha256")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	require.Len(t, lines, 301)
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		require.Len(t, fields, 2)
		require.Len(t, fields[0], sha256.Size*2)
		require.NotContains(t, names, fields[1], "fixture must not duplicate a filename")
		data, err := FS.ReadFile(fields[1])
		require.NoError(t, err)
		hash := sha256.Sum256(data)
		require.Equal(t, fields[0], hex.EncodeToString(hash[:]), fields[1])
		names = append(names, fields[1])
	}
	require.True(t, sort.StringsAreSorted(names))
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	var actual, historical, sameNumber []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".sql") {
			actual = append(actual, name)
			if name != "246_enable_gemini_image_generation_groups.sql" &&
				name != "247_scoped_admin_api_keys.sql" &&
				name != "248_group_image_count_billing.sql" {
				historical = append(historical, name)
			}
			if name >= "241_" && name < "246_" {
				sameNumber = append(sameNumber, name)
			}
		}
	}
	sort.Strings(actual)
	sort.Strings(historical)
	require.Len(t, actual, 304, "the current tree adds exactly 247 and 248 after the frozen v0.2.18 target")
	require.Equal(t, names, historical, "the frozen v0.2.17 target remains byte-identical")
	require.Contains(t, actual, "246_enable_gemini_image_generation_groups.sql")
	require.Equal(t, []string{
		"241_add_payment_order_bonus_amount.sql", "241_add_typesafe_platform.sql", "241_channel_monitor_v2_passive_coverage.sql",
		"242_channel_monitor_v2_typesafe.sql", "242_drop_platform_check_constraints.sql", "242_group_i18n.sql",
		"243_finalize_retired_schema_hygiene.sql", "243_gemini_cached_contents.sql",
		"244_clear_retired_runtime_data.sql", "244_gemini_cached_contents_channel_usage.sql",
		"245_channel_monitor_v2_v0215_platforms.sql", "245_gemini_cached_contents_mutation_lease.sql",
	}, sameNumber, "full filenames determine coexistence and migration order")
}
