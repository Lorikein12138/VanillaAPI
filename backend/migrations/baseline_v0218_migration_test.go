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

const baselineV0218GeminiImageMigrationSHA256 = "f49b458cfbd52bd17f5bb02e30e3813c8e192e4ae38bce6358713ef65f3874b5"

func TestBaselineV0218MigrationIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/baseline_v0218_prior_migrations.sha256")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	require.Len(t, lines, 301, "fixture must freeze every migration from H")

	prior := make(map[string]string, len(lines))
	names := make([]string, 0, len(lines))
	for lineNumber, line := range lines {
		fields := strings.Fields(line)
		require.Len(t, fields, 2, "invalid v0.2.17 fixture line %d", lineNumber+1)
		require.Len(t, fields[0], sha256.Size*2)
		require.True(t, strings.HasSuffix(fields[1], ".sql"), "invalid migration filename %q", fields[1])
		require.NotContains(t, prior, fields[1], "fixture must not duplicate a filename")
		prior[fields[1]] = fields[0]
		names = append(names, fields[1])

		data, readErr := FS.ReadFile(fields[1])
		require.NoError(t, readErr, "historical migration %s must remain present", fields[1])
		hash := sha256.Sum256(data)
		require.Equal(t, fields[0], hex.EncodeToString(hash[:]), "historical migration %s bytes changed", fields[1])
	}
	require.True(t, sort.StringsAreSorted(names), "the historical migration fixture must be filename-sorted")

	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	var actual, v0218Projection, historical, sameNumber []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		actual = append(actual, name)
		if name != "247_scoped_admin_api_keys.sql" && name != "248_group_image_count_billing.sql" {
			v0218Projection = append(v0218Projection, name)
		}
		if name != "246_enable_gemini_image_generation_groups.sql" &&
			name != "247_scoped_admin_api_keys.sql" &&
			name != "248_group_image_count_billing.sql" {
			historical = append(historical, name)
		}
		if name >= "241_" && name < "246_" {
			sameNumber = append(sameNumber, name)
		}
	}
	sort.Strings(actual)
	sort.Strings(v0218Projection)
	sort.Strings(historical)
	require.Len(t, actual, 304, "the current migration directory must contain the v0.2.18 projection plus 247 and 248")
	require.Len(t, v0218Projection, 302, "the fixed v0.2.18 projection must omit later 247 and 248")
	require.Equal(t, names, historical, "the current historical projection must match the frozen H set exactly")
	require.Contains(t, actual, "246_enable_gemini_image_generation_groups.sql")
	require.Equal(t, []string{
		"241_add_payment_order_bonus_amount.sql", "241_add_typesafe_platform.sql", "241_channel_monitor_v2_passive_coverage.sql",
		"242_channel_monitor_v2_typesafe.sql", "242_drop_platform_check_constraints.sql", "242_group_i18n.sql",
		"243_finalize_retired_schema_hygiene.sql", "243_gemini_cached_contents.sql",
		"244_clear_retired_runtime_data.sql", "244_gemini_cached_contents_channel_usage.sql",
		"245_channel_monitor_v2_v0215_platforms.sql", "245_gemini_cached_contents_mutation_lease.sql",
	}, sameNumber, "full filenames determine coexistence and migration order")
}

func TestBaselineV0218GeminiImageMigrationIdentity(t *testing.T) {
	data, err := FS.ReadFile("246_enable_gemini_image_generation_groups.sql")
	require.NoError(t, err)
	hash := sha256.Sum256(data)
	require.Equal(t, baselineV0218GeminiImageMigrationSHA256, hex.EncodeToString(hash[:]), "migration bytes must match pinned U")

	statements := make([]string, 0)
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statements = append(statements, line)
		}
	}
	sql := strings.Join(strings.Fields(strings.Join(statements, " ")), " ")
	require.Equal(t,
		"UPDATE groups SET allow_image_generation = true WHERE platform IN ('gemini', 'antigravity') AND allow_image_generation = false;",
		sql)
}
