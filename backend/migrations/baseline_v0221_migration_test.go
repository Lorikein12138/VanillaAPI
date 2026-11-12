//go:build unit

package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	baselineV0221CostIndexMigration = "249_add_usage_logs_api_key_cost_index_notx.sql"
	baselineV0221CacheTTLMigration  = "250_openai_cache_write_ttl.sql"
	baselineGroupDisplayMigration   = "249_group_display_category.sql"
	advisorRequestIDMigration       = "251_extend_usage_log_request_id.sql"
)

func isMigrationAfterV0219(name string) bool {
	switch name {
	case baselineV0220MCPOAuthMigration, baselineGroupDisplayMigration, baselineV0221CostIndexMigration, baselineV0221CacheTTLMigration, advisorRequestIDMigration:
		return true
	default:
		return false
	}
}

func TestBaselineV0221MigrationIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/baseline_v0221_prior_migrations.sha256")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	require.Len(t, lines, 306)

	expected := make([]string, 0, 308)
	for _, line := range lines {
		fields := strings.Fields(line)
		require.Len(t, fields, 2)
		name := fields[1]
		require.NotContains(t, expected, name)
		data, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		sum := sha256.Sum256(data)
		require.Equal(t, fields[0], hex.EncodeToString(sum[:]), name)
		expected = append(expected, name)
	}
	require.True(t, slices.IsSorted(expected))
	for name, want := range map[string]string{
		baselineV0221CostIndexMigration: "789d4d5a5c186531cb6edd65ea2ecc07f5e488272f7876c6f46240e2fed0c30b",
		baselineV0221CacheTTLMigration:  "c470116c49042425f5663c421f0aea2a6aa6eae75b7e489dc95c12fc303562b2",
	} {
		require.NotContains(t, expected, name)
		data, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		sum := sha256.Sum256(data)
		require.Equal(t, want, hex.EncodeToString(sum[:]), name)
		expected = append(expected, name)
	}
	slices.Sort(expected)
	actual, err := fs.Glob(FS, "*.sql")
	require.NoError(t, err)
	require.Contains(t, actual, advisorRequestIDMigration)
	actual = slices.DeleteFunc(actual, func(name string) bool { return name == advisorRequestIDMigration })
	require.Equal(t, expected, actual)
	require.Equal(t, []string{
		baselineV0221CostIndexMigration, baselineGroupDisplayMigration, baselineV0220MCPOAuthMigration, baselineV0221CacheTTLMigration,
	}, actual[len(actual)-4:])
}
