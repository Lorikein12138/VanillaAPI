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

const baselineV0220MCPOAuthMigration = "249_mcp_oauth.sql"

func TestBaselineV0220MigrationIdentity(t *testing.T) {
	fixture, err := os.ReadFile("testdata/baseline_v0220_prior_migrations.sha256")
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(fixture)), "\n")
	require.Len(t, lines, 304)

	expected := make([]string, 0, len(lines)+1)
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

	data, err := FS.ReadFile(baselineV0220MCPOAuthMigration)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	require.Equal(t, "881ff2e8ecf430202c593c699b1441db1d2196ac2c312d8a3849acb6c97609e5", hex.EncodeToString(sum[:]))
	expected = append(expected, baselineV0220MCPOAuthMigration)
	require.True(t, slices.IsSorted(expected))
	actual, err := fs.Glob(FS, "*.sql")
	require.NoError(t, err)
	require.Equal(t, expected, actual)
}
