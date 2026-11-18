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
	baselineV0222UltrafastMigration         = "251_group_force_openai_ultrafast.sql"
	baselineV0222NonblockingRollupMigration = "251_group_usage_rollup_nonblocking_build.sql"
	baselineV0222PriorMigrationCount        = 309
	baselineV0222MigrationCount             = baselineV0222PriorMigrationCount + 2
)

func isV0222UpstreamMigration(name string) bool {
	return name == baselineV0222UltrafastMigration || name == baselineV0222NonblockingRollupMigration
}

func TestBaselineV0222MigrationIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/baseline_v0222_prior_migrations.sha256")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	require.Len(t, lines, baselineV0222PriorMigrationCount)

	expected := make([]string, 0, baselineV0222MigrationCount)
	for _, line := range lines {
		fields := strings.Fields(line)
		require.Len(t, fields, 2)
		require.NotContains(t, expected, fields[1])
		data, readErr := FS.ReadFile(fields[1])
		require.NoError(t, readErr)
		sum := sha256.Sum256(data)
		require.Equal(t, fields[0], hex.EncodeToString(sum[:]), fields[1])
		expected = append(expected, fields[1])
	}
	require.True(t, slices.IsSorted(expected))

	for name, want := range map[string]string{
		baselineV0222UltrafastMigration:         "f91352f2d3b1bffbbea5f6b7fc17e37c5ad64f04c3426cb7168778d10135a7bf",
		baselineV0222NonblockingRollupMigration: "da504646104cec14aa7bcf5a0af6f06f5cd40883459379ae915db27c1d4cec8b",
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
	actual = slices.DeleteFunc(actual, func(name string) bool { return name == baselineV0223UltrafastMultiplierMigration })
	require.Equal(t, expected, actual)
	require.Equal(t, []string{
		advisorRequestIDMigration, baselineV0222UltrafastMigration, baselineV0222NonblockingRollupMigration,
	}, actual[len(actual)-3:])
}
