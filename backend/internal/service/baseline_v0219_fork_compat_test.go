//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBaselineV0219AutomaticAllowlistAndImageBillingAuthProjection(t *testing.T) {
	accounts := []Account{
		{Platform: PlatformGemini, Status: StatusError, Credentials: map[string]any{"model_mapping": map[string]any{"client-a": "upstream-a"}}},
		{Platform: PlatformGemini, Status: StatusDisabled, Schedulable: false, Credentials: map[string]any{"model_mapping": map[string]any{"client-b": "upstream-b"}}},
	}
	allowlist := ResolveEffectiveGroupModelAllowlist(GroupModelAllowlist{Enabled: true}, accounts)
	require.Equal(t, []string{"client-a", "client-b"}, allowlist.Models, "health and schedulable state must not remove declared model keys")

	groupID := int64(19)
	key := &APIKey{
		ID: 20, UserID: 21, GroupID: &groupID, Status: StatusActive,
		User:  &User{ID: 21, Status: StatusActive, Role: RoleUser},
		Group: &Group{ID: groupID, Status: StatusActive, ImageCountBilling: true, ModelAllowlist: allowlist},
	}
	svc := &APIKeyService{}
	projected := svc.snapshotToAPIKey("api-key", svc.snapshotFromAPIKey(context.Background(), key))
	require.True(t, projected.Group.ImageCountBilling)
	require.True(t, projected.Group.ModelAllowlist.Enabled)
	require.Equal(t, allowlist.Models, projected.Group.ModelAllowlist.Models)
}

func TestBaselineV0219AccountStatsUsesRequestedKeyAndKeepsCacheTokensAndCustomerCost(t *testing.T) {
	channel := &Channel{ID: 1, Status: StatusActive, ModelPricing: []ChannelModelPricing{
		{Platform: PlatformOpenAI, Models: []string{"client-key"}, InputPrice: testPtrFloat64(0.02), CacheReadPrice: testPtrFloat64(0.01)},
		{Platform: PlatformOpenAI, Models: []string{"upstream-value"}, InputPrice: testPtrFloat64(0.09), CacheReadPrice: testPtrFloat64(0.08)},
	}}
	channelService := newTestChannelServiceForStats(t, channel, 10, PlatformOpenAI)
	upstreamModel := "upstream-value"
	usageLog := &UsageLog{UpstreamModel: &upstreamModel, ActualCost: 123.45}
	applyAccountStatsCost(context.Background(), usageLog, channelService, nil,
		1, 10, upstreamModel, "client-key",
		UsageTokens{InputTokens: 50, CacheReadTokens: 25}, 999, time.Time{}, true,
	)
	// Pricing rates are per token for account stats (not the customer bill).
	require.NotNil(t, usageLog.AccountStatsCost)
	require.InDelta(t, 1.25, *usageLog.AccountStatsCost, 1e-12)
	require.Equal(t, "upstream-value", *usageLog.UpstreamModel)
	require.Equal(t, 123.45, usageLog.ActualCost, "customer-facing cost must remain unchanged")
}

func TestBaselineV0219ImageEndpointByCountDoesNotChargeMixedTokenUsage(t *testing.T) {
	const groupID = 9901
	const model = "gpt-image-2"
	resolver := newOpenAITokenImageChannelPricingResolverForTest(t, groupID, model)
	group := &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true,
		AllowImageGeneration: true, ImageCountBilling: true, ImagePrice1K: f64p(0.4)}
	groupIDPointer := int64(groupID)
	apiKey := &APIKey{ID: 1, GroupID: &groupIDPointer, Group: group}
	svc := &GatewayService{billingService: resolver.billingService, resolver: resolver}

	imageCost := svc.calculateRecordUsageCost(context.Background(), &ForwardResult{
		Model: model, ImageEndpoint: true, ImageCount: 2, ImageSize: "1K",
		Usage: ClaudeUsage{InputTokens: 1_000_000, OutputTokens: 10_000, CacheReadInputTokens: 50_000},
	}, apiKey, model, 1, 1, time.Time{})
	require.NotNil(t, imageCost)
	require.InDelta(t, 0.8, imageCost.TotalCost, 1e-12)
	require.Equal(t, string(BillingModeImage), imageCost.BillingMode)

	mixedCost := svc.calculateRecordUsageCost(context.Background(), &ForwardResult{
		Model: model, ImageEndpoint: false, ImageCount: 2, ImageSize: "1K",
		Usage: ClaudeUsage{InputTokens: 1_000_000, OutputTokens: 10_000, CacheReadInputTokens: 50_000},
	}, apiKey, model, 1, 1, time.Time{})
	require.NotNil(t, mixedCost)
	require.Equal(t, string(BillingModeToken), mixedCost.BillingMode)
	require.NotEqual(t, imageCost.TotalCost, mixedCost.TotalCost)
}
