//go:build unit

package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	baselineV0223UltrafastMultiplierMigration   = "252_channel_pricing_ultrafast_multiplier.sql"
	baselineV0223MigrationCount                 = baselineV0222MigrationCount + 1
	accountTempUnschedulableProtectionMigration = "253_account_temp_unschedulable_protection.sql"
	currentMigrationCount                       = baselineV0223MigrationCount + 1
)

func TestBaselineV0223MigrationIdentity(t *testing.T) {
	names, err := fs.Glob(FS, "*.sql")
	require.NoError(t, err)
	require.Len(t, names, currentMigrationCount)
	require.Equal(t, []string{baselineV0223UltrafastMultiplierMigration, accountTempUnschedulableProtectionMigration}, names[baselineV0223MigrationCount-1:])
	data, err := FS.ReadFile(baselineV0223UltrafastMultiplierMigration)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	require.Equal(t, "01eb249a933e5c3bef3a8e2106098abf56d20a5f2890fcc6490b1f83ec05687f", hex.EncodeToString(sum[:]))
}
