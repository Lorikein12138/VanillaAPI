//go:build unit

package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var baselineV0216NewMigrations = []string{
	"242_group_i18n.sql",
	"243_gemini_cached_contents.sql",
	"244_gemini_cached_contents_channel_usage.sql",
	"245_gemini_cached_contents_mutation_lease.sql",
}

func TestBaselineV0216MigrationIdentity(t *testing.T) {
	prior, err := readBaselineV0216PriorMigrations()
	require.NoError(t, err)
	require.Len(t, prior, 297, "fixture must freeze every migration in H")

	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	var current, historical []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			current = append(current, entry.Name())
			if entry.Name() != "246_enable_gemini_image_generation_groups.sql" &&
				entry.Name() != "247_scoped_admin_api_keys.sql" &&
				entry.Name() != "248_group_image_count_billing.sql" {
				historical = append(historical, entry.Name())
			}
		}
	}
	sort.Strings(current)
	sort.Strings(historical)
	require.Len(t, current, 304, "the current tree adds exactly 247 and 248 after the frozen v0.2.18 target")
	require.Len(t, historical, 301, "v0.2.16 target plus later frozen migrations remains 301")

	for name, wantHash := range prior {
		content, readErr := FS.ReadFile(name)
		require.NoError(t, readErr, "frozen migration %s must remain present", name)
		sum := sha256.Sum256(content)
		require.Equal(t, wantHash, hex.EncodeToString(sum[:]), "historical migration %s bytes changed", name)
		require.Contains(t, current, name)
	}

	for _, name := range baselineV0216NewMigrations {
		require.Contains(t, historical, name)
		_, existed := prior[name]
		require.False(t, existed, "new upstream migration filename must not replace a frozen file")
	}
	require.Equal(t, append([]string(nil), historical...), sortedMigrationNames(historical), "historical target must be sorted")
	require.Equal(t, append([]string(nil), baselineV0216NewMigrations...), sortedMigrationNames(baselineV0216NewMigrations))
	require.Equal(t, 3, countMigrationPrefix(current, "242_"), "both local 242 files and the upstream 242 must coexist")
	require.Less(t, sort.SearchStrings(current, "242_channel_monitor_v2_typesafe.sql"), sort.SearchStrings(current, "242_group_i18n.sql"))
	require.Less(t, sort.SearchStrings(current, "242_group_i18n.sql"), sort.SearchStrings(current, "243_finalize_retired_schema_hygiene.sql"))
	require.Less(t, sort.SearchStrings(current, "243_finalize_retired_schema_hygiene.sql"), sort.SearchStrings(current, "243_gemini_cached_contents.sql"))
	require.Less(t, sort.SearchStrings(current, "244_clear_retired_runtime_data.sql"), sort.SearchStrings(current, "244_gemini_cached_contents_channel_usage.sql"))
	require.Less(t, sort.SearchStrings(current, "245_channel_monitor_v2_v0215_platforms.sql"), sort.SearchStrings(current, "245_gemini_cached_contents_mutation_lease.sql"))
}

func TestBaselineV0216NewMigrationDDL(t *testing.T) {
	statements := make(map[string]string, len(baselineV0216NewMigrations))
	stripLineComments := regexp.MustCompile(`(?m)--[^\r\n]*`)
	for _, name := range baselineV0216NewMigrations {
		content, err := FS.ReadFile(name)
		require.NoError(t, err)
		sql := strings.ToUpper(strings.Join(strings.Fields(stripLineComments.ReplaceAllString(string(content), "")), " "))
		require.NotEmpty(t, sql)
		require.NotRegexp(t, `\b(DROP|TRUNCATE|DELETE)\b`, sql, "%s must not destructively change existing data or schema", name)
		statements[name] = sql
	}

	require.Contains(t, statements["242_group_i18n.sql"], "ADD COLUMN IF NOT EXISTS I18N JSONB")
	require.Contains(t, statements["242_group_i18n.sql"], "COMMENT ON COLUMN GROUPS.I18N")
	require.Contains(t, statements["243_gemini_cached_contents.sql"], "CREATE TABLE IF NOT EXISTS GEMINI_CACHED_CONTENTS")
	require.Contains(t, statements["243_gemini_cached_contents.sql"], "CREATE UNIQUE INDEX IF NOT EXISTS IDX_GEMINI_CACHED_CONTENTS_PUBLIC_ID")
	require.Contains(t, statements["243_gemini_cached_contents.sql"], "CREATE INDEX IF NOT EXISTS IDX_GEMINI_CACHED_CONTENTS_API_KEY_ACTIVE")
	require.Contains(t, statements["243_gemini_cached_contents.sql"], "CREATE INDEX IF NOT EXISTS IDX_GEMINI_CACHED_CONTENTS_ACCOUNT")
	require.Contains(t, statements["244_gemini_cached_contents_channel_usage.sql"], "ADD COLUMN IF NOT EXISTS CHANNEL_USAGE JSONB")
	require.Contains(t, statements["245_gemini_cached_contents_mutation_lease.sql"], "ADD COLUMN IF NOT EXISTS MUTATION_TOKEN")
	require.Contains(t, statements["245_gemini_cached_contents_mutation_lease.sql"], "ADD COLUMN IF NOT EXISTS MUTATION_LEASE_UNTIL")
	require.Contains(t, statements["245_gemini_cached_contents_mutation_lease.sql"], "CREATE INDEX IF NOT EXISTS IDX_GEMINI_CACHED_CONTENTS_PENDING_MUTATION")
}

func readBaselineV0216PriorMigrations() (map[string]string, error) {
	content, err := readMigrationFixture("testdata/baseline_v0216_prior_migrations.sha256")
	if err != nil {
		return nil, err
	}
	prior := make(map[string]string, 297)
	var names []string
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 || !strings.HasSuffix(fields[1], ".sql") {
			return nil, fmt.Errorf("invalid baseline migration checksum fixture line %d: %q", lineNumber+1, line)
		}
		if _, exists := prior[fields[1]]; exists {
			return nil, fmt.Errorf("duplicate migration in checksum fixture: %s", fields[1])
		}
		prior[fields[1]] = fields[0]
		names = append(names, fields[1])
	}
	if !equalMigrationNames(names, sortedMigrationNames(names)) {
		return nil, fmt.Errorf("baseline migration checksum fixture must be sorted by filename")
	}
	return prior, nil
}

func readMigrationFixture(name string) ([]byte, error) {
	return os.ReadFile(name)
}

func equalMigrationNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
