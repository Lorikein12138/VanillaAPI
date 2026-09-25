//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type groupModelAllowlistMembersRepoStub struct {
	AccountRepository
	accounts []Account
	err      error
	calls    int
}

func (s *groupModelAllowlistMembersRepoStub) ListAllByGroup(context.Context, int64) ([]Account, error) {
	s.calls++
	return s.accounts, s.err
}

func TestAdminService_CreateGroup_AllowsEmptyAutomaticModelAllowlist(t *testing.T) {
	repo := &groupRepoStubForAdmin{createID: 51}
	svc := &adminServiceImpl{groupRepo: repo, accountRepo: &groupModelAllowlistMembersRepoStub{}}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "allowlist-empty-group",
		Platform:       PlatformOpenAI,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: true},
	})

	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.True(t, repo.created.ModelAllowlist.Enabled)
	require.Empty(t, repo.created.ModelAllowlist.Models)
}

func TestAdminService_CreateGroup_IgnoresClientModelsInAutomaticMode(t *testing.T) {
	repo := &groupRepoStubForAdmin{createID: 51}
	svc := &adminServiceImpl{groupRepo: repo, accountRepo: &groupModelAllowlistMembersRepoStub{}}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "allowlist-wildcard-group",
		Platform:       PlatformOpenAI,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-*-5.4"}},
	})

	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.Empty(t, repo.created.ModelAllowlist.Models)
}

func TestAdminService_CreateGroup_NormalizesModelAllowlist(t *testing.T) {
	repo := &groupRepoStubForAdmin{createID: 52}
	svc := &adminServiceImpl{groupRepo: repo, accountRepo: &groupModelAllowlistMembersRepoStub{}}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:           "allowlist-normalized-group",
		Platform:       PlatformOpenAI,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{
			Enabled: true,
			Models:  []string{" gpt-5.4 ", "GPT-5.4", "claude-*", "  "},
		},
	})

	require.NoError(t, err)
	require.NotNil(t, repo.created)
	require.True(t, repo.created.ModelAllowlist.Enabled)
	require.Empty(t, repo.created.ModelAllowlist.Models)
}

func TestAdminService_UpdateGroup_AllowsEmptyAutomaticModelAllowlist(t *testing.T) {
	existing := &Group{ID: 1, Name: "existing", Platform: PlatformOpenAI, Status: StatusActive}
	repo := &groupRepoStubForAdmin{getByID: existing}
	svc := &adminServiceImpl{groupRepo: repo, accountRepo: &groupModelAllowlistMembersRepoStub{}}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		ModelAllowlist: &GroupModelAllowlist{Enabled: true},
	})

	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.Empty(t, repo.updated.ModelAllowlist.Models)
}

func TestAdminService_UpdateGroup_IgnoresClientModelsInAutomaticMode(t *testing.T) {
	existing := &Group{ID: 1, Name: "existing", Platform: PlatformOpenAI, Status: StatusActive}
	repo := &groupRepoStubForAdmin{getByID: existing}
	svc := &adminServiceImpl{groupRepo: repo, accountRepo: &groupModelAllowlistMembersRepoStub{}}

	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{
		ModelAllowlist: &GroupModelAllowlist{Enabled: true, Models: []string{"foo-*bar"}},
	})

	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.Empty(t, repo.updated.ModelAllowlist.Models)
}

func TestAdminService_UpdateGroup_NormalizesAndResetsModelAllowlist(t *testing.T) {
	existing := &Group{
		ID: 1, Name: "existing", Platform: PlatformOpenAI, Status: StatusActive,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-5.4"}},
	}
	repo := &groupRepoStubForAdmin{getByID: existing}
	svc := &adminServiceImpl{groupRepo: repo, accountRepo: &groupModelAllowlistMembersRepoStub{}}

	// 开启自动模式时清空客户端提交的旧手工条目。
	updated := GroupModelAllowlist{Enabled: true, Models: []string{" GPT-5.4 ", "claude-*"}}
	_, err := svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{ModelAllowlist: &updated})
	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.Empty(t, repo.updated.ModelAllowlist.Models)

	// 关闭且清空条目也应被接受（关闭白名单）。
	repo.updated = nil
	disabled := GroupModelAllowlist{Enabled: false}
	_, err = svc.UpdateGroup(context.Background(), existing.ID, &UpdateGroupInput{ModelAllowlist: &disabled})
	require.NoError(t, err)
	require.NotNil(t, repo.updated)
	require.False(t, repo.updated.ModelAllowlist.Enabled)
	require.Empty(t, repo.updated.ModelAllowlist.Models)
}

func TestAdminService_GetGroupReturnsReadOnlyAllowlistSummary(t *testing.T) {
	group := &Group{ID: 7, Name: "automatic", Platform: PlatformOpenAI, Status: StatusActive,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"legacy"}}}
	members := &groupModelAllowlistMembersRepoStub{accounts: []Account{
		{Status: StatusDisabled, Schedulable: false, Credentials: map[string]any{"model_mapping": map[string]any{
			"gpt-5.5": "gpt-5.5", "gpt-5.4": "gpt-5.4", "renamed": "upstream",
		}}},
		{Status: StatusError, ErrorMessage: "failed", Credentials: map[string]any{"model_mapping": map[string]any{
			"GPT-5.4": "GPT-5.4", "claude-sonnet-4.5": "claude-sonnet-4.5",
		}}},
		{Credentials: map[string]any{"model_mapping": map[string]any{}}},
	}}
	svc := &adminServiceImpl{groupRepo: &groupRepoStubForAdmin{getByID: group}, accountRepo: members}

	got, err := svc.GetGroup(context.Background(), group.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-4.5", "GPT-5.4", "gpt-5.5"}, got.ModelAllowlist.Models)
	require.Equal(t, 2, got.ModelAllowlistSourceAccountCount)
	require.Equal(t, 1, members.calls)
}

func TestAdminService_GetGroupDoesNotReadMembersWhenAllowlistDisabled(t *testing.T) {
	group := &Group{ID: 8, ModelAllowlist: GroupModelAllowlist{Enabled: false, Models: []string{"manual"}}}
	members := &groupModelAllowlistMembersRepoStub{err: context.DeadlineExceeded}
	svc := &adminServiceImpl{groupRepo: &groupRepoStubForAdmin{getByID: group}, accountRepo: members}

	got, err := svc.GetGroup(context.Background(), group.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"manual"}, got.ModelAllowlist.Models)
	require.Zero(t, got.ModelAllowlistSourceAccountCount)
	require.Zero(t, members.calls)
}

func TestAdminService_GetGroupFailsClosedWhenMemberReadFails(t *testing.T) {
	sentinel := context.DeadlineExceeded
	group := &Group{ID: 9, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"legacy"}}}
	svc := &adminServiceImpl{
		groupRepo:   &groupRepoStubForAdmin{getByID: group},
		accountRepo: &groupModelAllowlistMembersRepoStub{err: sentinel},
	}
	got, err := svc.GetGroup(context.Background(), group.ID)
	require.Nil(t, got)
	require.ErrorIs(t, err, sentinel)
}
