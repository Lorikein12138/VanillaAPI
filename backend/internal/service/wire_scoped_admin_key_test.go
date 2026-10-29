//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type settingRepoWithStartupMigration struct {
	*settingRepoStub
}

func (s *settingRepoWithStartupMigration) Set(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[key] = value
	return nil
}

func TestProvideSettingServiceWiresScopedAdminKeyRepository(t *testing.T) {
	keyRepo := &scopedKeyMemoryRepo{item: &ScopedAdminKey{ID: "key-1", KeyPrefix: "adm_sc_key-1"}}
	settings := &settingRepoWithStartupMigration{settingRepoStub: &settingRepoStub{}}
	svc := ProvideSettingService(keyRepo, settings, nil, nil, nil)

	keys, err := svc.ListScopedAdminKeys(context.Background())
	require.NoError(t, err)
	require.Equal(t, []*ScopedAdminKey{keyRepo.item}, keys)
}
