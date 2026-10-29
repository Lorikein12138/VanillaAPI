//go:build unit

package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type scopedGroupModelsRepo struct {
	GroupRepository
	config GroupModelAllowlist
	writes int
	err    error
}

func (r *scopedGroupModelsRepo) ScopedAdminUpdateGroupModels(_ context.Context, id int64, cfg GroupModelAllowlist) (*Group, error) {
	r.writes++
	r.config = cfg
	return &Group{ID: id, ModelAllowlist: cfg}, r.err
}

func TestScopedAdminGroupModelsNormalizeAndInvalidateOnlyOnSuccess(t *testing.T) {
	repo := &scopedGroupModelsRepo{}
	cache := &authCacheInvalidatorStub{}
	s := &adminServiceImpl{groupRepo: repo, authCacheInvalidator: cache}
	cfg := GroupModelAllowlist{Enabled: true, Models: []string{" second ", "first", "second"}}
	g, err := s.ScopedAdminUpdateGroupModels(context.Background(), 2, cfg)
	require.NoError(t, err)
	require.True(t, g.ModelAllowlist.Enabled)
	require.Empty(t, g.ModelAllowlist.Models, "enabled allowlists are derived from bound account mappings")
	require.Equal(t, []int64{2}, cache.groupIDs)
	repo.err = errors.New("提交失败")
	_, err = s.ScopedAdminUpdateGroupModels(context.Background(), 3, cfg)
	require.Error(t, err)
	require.Equal(t, []int64{2}, cache.groupIDs, "仓储失败不得提前清理缓存")
	writes := repo.writes
	repo.err = nil
	empty, err := s.ScopedAdminUpdateGroupModels(context.Background(), 2, GroupModelAllowlist{Enabled: true})
	require.NoError(t, err, "empty automatic allowlists remain closed and are valid")
	require.True(t, empty.ModelAllowlist.Enabled)
	require.Empty(t, empty.ModelAllowlist.Models)
	for _, models := range [][]string{{""}, {"   "}, {strings.Repeat("x", 513)}, make([]string, 1025)} {
		_, err = s.ScopedAdminUpdateGroupModels(context.Background(), 2, GroupModelAllowlist{Models: models})
		require.Error(t, err)
	}
	require.Equal(t, writes+1, repo.writes, "invalid models must be rejected before repository writes")
}
