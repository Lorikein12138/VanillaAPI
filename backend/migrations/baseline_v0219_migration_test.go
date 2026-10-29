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

const (
	baselineV0219ScopedAdminKeysMigrationSHA256   = "0c4c22f564f276fb305a54cf2d6c4b1a6179d748d5d8b74a3b99676282355bf6"
	baselineV0219ImageCountBillingMigrationSHA256 = "c91d6d31937658e71c68fa3af6a26fc47380f61243a681ac275aeb4706b242ab"
)

func TestBaselineV0219MigrationIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/baseline_v0219_prior_migrations.sha256")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	require.Len(t, lines, 302, "fixture must freeze the full fixed-H migration set")

	prior := make(map[string]string, len(lines))
	priorNames := make([]string, 0, len(lines))
	for lineNumber, line := range lines {
		fields := strings.Fields(line)
		require.Len(t, fields, 2, "invalid v0.2.19 fixture line %d", lineNumber+1)
		require.Len(t, fields[0], sha256.Size*2)
		require.NotContains(t, prior, fields[1], "fixture must not duplicate a filename")
		prior[fields[1]] = fields[0]
		priorNames = append(priorNames, fields[1])

		data, readErr := FS.ReadFile(fields[1])
		require.NoError(t, readErr, "fixed-H migration %s must remain present", fields[1])
		sum := sha256.Sum256(data)
		require.Equal(t, fields[0], hex.EncodeToString(sum[:]), "fixed-H migration %s bytes changed", fields[1])
	}
	require.True(t, sort.StringsAreSorted(priorNames), "fixed-H fixture must be filename-sorted")

	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	actual := make([]string, 0, 304)
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			actual = append(actual, entry.Name())
		}
	}
	sort.Strings(actual)
	require.Len(t, actual, 304, "the exact candidate set is fixed-H 302 plus upstream 247 and 248")

	expected := append([]string(nil), priorNames...)
	expected = append(expected, "247_scoped_admin_api_keys.sql", "248_group_image_count_billing.sql")
	sort.Strings(expected)
	require.Equal(t, expected, actual, "unknown additions, missing migrations, and renamed files are not permitted")

	for name, want := range map[string]string{
		"247_scoped_admin_api_keys.sql":     baselineV0219ScopedAdminKeysMigrationSHA256,
		"248_group_image_count_billing.sql": baselineV0219ImageCountBillingMigrationSHA256,
	} {
		data, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		sum := sha256.Sum256(data)
		require.Equal(t, want, hex.EncodeToString(sum[:]), "%s must retain the exact U bytes", name)
	}

	var sameNumber []string
	for _, name := range actual {
		if name >= "241_" && name < "249_" {
			sameNumber = append(sameNumber, name)
		}
	}
	require.Equal(t, []string{
		"241_add_payment_order_bonus_amount.sql", "241_add_typesafe_platform.sql", "241_channel_monitor_v2_passive_coverage.sql",
		"242_channel_monitor_v2_typesafe.sql", "242_drop_platform_check_constraints.sql", "242_group_i18n.sql",
		"243_finalize_retired_schema_hygiene.sql", "243_gemini_cached_contents.sql",
		"244_clear_retired_runtime_data.sql", "244_gemini_cached_contents_channel_usage.sql",
		"245_channel_monitor_v2_v0215_platforms.sql", "245_gemini_cached_contents_mutation_lease.sql",
		"246_enable_gemini_image_generation_groups.sql", "247_scoped_admin_api_keys.sql", "248_group_image_count_billing.sql",
	}, sameNumber, "full filenames determine coexistence and migration order")
}
