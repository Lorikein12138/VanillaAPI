//go:build integration

package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestGetByKeyForAuthComputesCurrentAccountModelAllowlistUnion(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	group := mustCreateGroup(t, integrationEntClient, &service.Group{
		Name: fmt.Sprintf("model-allowlist-proj-group-%d", suffix), Platform: service.PlatformOpenAI,
		RateMultiplier:       1,
		AllowImageGeneration: true, ImageCountBilling: true,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"legacy-manual-model"},
		},
	})
	groupRepo := NewGroupRepository(integrationEntClient, integrationDB)
	accountRepo := newAccountRepositoryWithSQL(integrationEntClient, integrationDB, nil)
	memberA := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name: "model-allowlist-proj-a", Platform: service.PlatformOpenAI, Status: service.StatusDisabled,
		Credentials: map[string]any{"model_mapping": map[string]any{
			"gpt-5.4": "gpt-5.4", "gpt-5.5-*": "gpt-5.5-*", "renamed": "upstream-name",
		}},
	})
	memberB := mustCreateAccount(t, integrationEntClient, &service.Account{
		Name: "model-allowlist-proj-b", Platform: service.PlatformOpenAI, Status: service.StatusError,
		Credentials: map[string]any{"model_mapping": map[string]any{"gpt-5.5-*": "gpt-5.5-*", "gpt-5.6": "gpt-5.6"}},
	})
	require.NoError(t, accountRepo.BindGroups(ctx, memberA.ID, []int64{group.ID}))
	require.NoError(t, accountRepo.BindGroups(ctx, memberB.ID, []int64{group.ID}))
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email: fmt.Sprintf("model-allowlist-proj-%d@example.com", suffix), Concurrency: 5,
	})
	groupID := group.ID
	keyValue := fmt.Sprintf("sk-model-allowlist-proj-%d", suffix)
	apiKeyRepo := NewAPIKeyRepository(integrationEntClient, integrationDB)
	key := &service.APIKey{UserID: user.ID, GroupID: &groupID, Key: keyValue, Name: "model-allowlist-proj", Status: service.StatusActive}
	require.NoError(t, apiKeyRepo.Create(ctx, key))
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM auth_cache_invalidation_outbox WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')", keyValue)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM accounts WHERE id = ANY($1)", pq.Array([]int64{memberA.ID, memberB.ID}))
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM groups WHERE id = $1", group.ID)
		require.NoError(t, err)
	})

	got, err := apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.NotNil(t, got.Group)
	require.True(t, got.Group.AllowImageGeneration)
	require.True(t, got.Group.ImageCountBilling)
	require.True(t, got.Group.ModelAllowlist.Enabled)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5-*", "gpt-5.6", "renamed"}, got.Group.ModelAllowlist.Models)
	gotGroup, err := groupRepo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.True(t, gotGroup.AllowImageGeneration)
	require.True(t, gotGroup.ImageCountBilling)

	// The new field can be disabled independently while the historical
	// allow_image_generation flag remains true in both repository projections.
	gotGroup.ImageCountBilling = false
	require.NoError(t, groupRepo.Update(ctx, gotGroup))
	gotGroup, err = groupRepo.GetByID(ctx, group.ID)
	require.NoError(t, err)
	require.True(t, gotGroup.AllowImageGeneration)
	require.False(t, gotGroup.ImageCountBilling)
	got, err = apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.True(t, got.Group.AllowImageGeneration)
	require.False(t, got.Group.ImageCountBilling)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5-*", "gpt-5.6", "renamed"}, got.Group.ModelAllowlist.Models)

	require.NoError(t, accountRepo.UpdateCredentials(ctx, memberA.ID, map[string]any{
		"model_mapping": map[string]any{"gpt-5.7": "gpt-5.7"},
	}))
	got, err = apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.True(t, got.Group.AllowImageGeneration)
	require.False(t, got.Group.ImageCountBilling)
	require.Equal(t, []string{"gpt-5.5-*", "gpt-5.6", "gpt-5.7"}, got.Group.ModelAllowlist.Models)

	require.NoError(t, accountRepo.BindGroups(ctx, memberA.ID, nil))
	got, err = apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.True(t, got.Group.AllowImageGeneration)
	require.False(t, got.Group.ImageCountBilling)
	require.Equal(t, []string{"gpt-5.5-*", "gpt-5.6"}, got.Group.ModelAllowlist.Models)

	require.NoError(t, accountRepo.Delete(ctx, memberB.ID))
	got, err = apiKeyRepo.GetByKeyForAuth(ctx, keyValue)
	require.NoError(t, err)
	require.True(t, got.Group.AllowImageGeneration)
	require.False(t, got.Group.ImageCountBilling)
	require.Empty(t, got.Group.ModelAllowlist.Models)
}
