//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeGroupDisplayCategory(t *testing.T) {
	cases := map[string]string{
		"":           GroupDisplayCategoryAuto,
		"auto":       GroupDisplayCategoryAuto,
		" Domestic ": GroupDisplayCategoryDomestic,
		"anthropic":  GroupDisplayCategoryAnthropic,
		"openai":     GroupDisplayCategoryOpenAI,
		"OTHER":      GroupDisplayCategoryOther,
	}
	for raw, want := range cases {
		got, err := NormalizeGroupDisplayCategory(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}
	_, err := NormalizeGroupDisplayCategory("doubao")
	require.Error(t, err)
}

// openai 平台的豆包分组：创建时可直接指定归入国产模型。
func TestAdminService_CreateGroup_StoresDisplayCategory(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:            "Doubao",
		Platform:        PlatformOpenAI,
		RateMultiplier:  1,
		DisplayCategory: " domestic ",
	})

	require.NoError(t, err)
	require.Equal(t, GroupDisplayCategoryDomestic, repo.created.DisplayCategory)
}

func TestAdminService_CreateGroup_RejectsUnknownDisplayCategory(t *testing.T) {
	repo := &groupRepoStubForAdmin{}
	svc := &adminServiceImpl{groupRepo: repo}

	_, err := svc.CreateGroup(context.Background(), &CreateGroupInput{
		Name:            "Doubao",
		Platform:        PlatformOpenAI,
		RateMultiplier:  1,
		DisplayCategory: "doubao",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "display_category")
	require.Nil(t, repo.created)
}

func TestAdminService_UpdateGroup_DisplayCategory(t *testing.T) {
	newExisting := func() *Group {
		return &Group{ID: 7, Name: "Doubao", Platform: PlatformOpenAI, RateMultiplier: 1, Status: StatusActive, DisplayCategory: GroupDisplayCategoryDomestic}
	}
	strPtr := func(s string) *string { return &s }

	t.Run("nil keeps current", func(t *testing.T) {
		repo := &groupRepoStubForAdmin{getByID: newExisting()}
		svc := &adminServiceImpl{groupRepo: repo}
		_, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{Name: "Doubao 2"})
		require.NoError(t, err)
		require.Equal(t, GroupDisplayCategoryDomestic, repo.updated.DisplayCategory)
	})
	t.Run("empty restores auto", func(t *testing.T) {
		repo := &groupRepoStubForAdmin{getByID: newExisting()}
		svc := &adminServiceImpl{groupRepo: repo}
		_, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{DisplayCategory: strPtr("")})
		require.NoError(t, err)
		require.Equal(t, GroupDisplayCategoryAuto, repo.updated.DisplayCategory)
	})
	t.Run("set other", func(t *testing.T) {
		repo := &groupRepoStubForAdmin{getByID: newExisting()}
		svc := &adminServiceImpl{groupRepo: repo}
		_, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{DisplayCategory: strPtr("other")})
		require.NoError(t, err)
		require.Equal(t, GroupDisplayCategoryOther, repo.updated.DisplayCategory)
	})
	t.Run("invalid rejected", func(t *testing.T) {
		repo := &groupRepoStubForAdmin{getByID: newExisting()}
		svc := &adminServiceImpl{groupRepo: repo}
		_, err := svc.UpdateGroup(context.Background(), 7, &UpdateGroupInput{DisplayCategory: strPtr("bogus")})
		require.Error(t, err)
		require.Nil(t, repo.updated)
	})
}

func TestDuplicateGroupKeepsDisplayCategory(t *testing.T) {
	source := &Group{ID: 3, Name: "Doubao", Platform: PlatformOpenAI, DisplayCategory: GroupDisplayCategoryDomestic}
	dup := cloneGroupForDuplicate(source, "op-1")
	require.Equal(t, GroupDisplayCategoryDomestic, dup.DisplayCategory)
}
