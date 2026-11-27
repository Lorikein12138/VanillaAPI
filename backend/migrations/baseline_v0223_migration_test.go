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
	accountTempUnschedulableRevisionMigration   = "254_account_temp_unschedulable_revision.sql"
	groupLifecycleMigration                     = "255_group_lifecycle_cleanup.sql"
	currentMigrationCount                       = baselineV0223MigrationCount + 3
)

func isAccountTempUnschedulableMigration(name string) bool {
	return name == accountTempUnschedulableProtectionMigration || name == accountTempUnschedulableRevisionMigration
}

func TestBaselineV0223MigrationIdentity(t *testing.T) {
	names, err := fs.Glob(FS, "*.sql")
	require.NoError(t, err)
	require.Len(t, names, currentMigrationCount)
	require.Equal(t, []string{
		baselineV0223UltrafastMultiplierMigration,
		accountTempUnschedulableProtectionMigration,
		accountTempUnschedulableRevisionMigration,
		groupLifecycleMigration,
	}, names[baselineV0223MigrationCount-1:])
	data, err := FS.ReadFile(baselineV0223UltrafastMultiplierMigration)
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	require.Equal(t, "01eb249a933e5c3bef3a8e2106098abf56d20a5f2890fcc6490b1f83ec05687f", hex.EncodeToString(sum[:]))
	data, err = FS.ReadFile(accountTempUnschedulableProtectionMigration)
	require.NoError(t, err)
	sum = sha256.Sum256(data)
	require.Equal(t, "33b93cd085c5667f93d79fc2535779396765e9e5abf054f092e2473075f587e5", hex.EncodeToString(sum[:]))
}
