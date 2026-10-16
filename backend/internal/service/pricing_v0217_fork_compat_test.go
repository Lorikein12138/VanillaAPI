//go:build unit

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestPricingV0217ExactTiersAndStableDatedCandidates(t *testing.T) {
	for _, base := range []string{"claude-sonnet-4-6-thinking", "claude-opus-4-6-thinking", "claude-sonnet-5-5-high", "claude-opus-5-5-medium", "gemini-4-argon-high", "gemini-4-argon-cyber-permissive"} {
		t.Run(base, func(t *testing.T) {
			exact := &LiteLLMModelPricing{InputCostPerToken: 7e-6}
			provider := &LiteLLMModelPricing{InputCostPerToken: 9e-6}
			svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{base: exact, "provider/" + base: provider}}
			require.Same(t, exact, svc.GetModelPricing(base))
			require.Same(t, provider, svc.GetModelPricing("provider/"+base))
		})
	}
	keys := []string{"claude-sonnet-4-6-20260101", "claude-sonnet-4-6-20260201", "provider/claude-sonnet-4-6-20260301"}
	var expected float64
	for offset := 0; offset < len(keys); offset++ {
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{}}
		for j := 0; j < len(keys); j++ {
			i := (j + offset) % len(keys)
			svc.pricingData[keys[i]] = &LiteLLMModelPricing{InputCostPerToken: float64(i + 1)}
		}
		got := svc.GetModelPricing("claude-sonnet-4-6")
		require.NotNil(t, got)
		if offset == 0 {
			expected = got.InputCostPerToken
		}
		require.Equal(t, expected, got.InputCostPerToken, "catalog insertion order must not choose a different dated price")
	}
	for _, base := range []string{"claude-opus-5-5", "claude-sonnet-5-5", "gemini-4-argon"} {
		price := &LiteLLMModelPricing{InputCostPerToken: 3e-6}
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{base: price}}
		for _, tier := range []string{"low", "medium", "high"} {
			require.Same(t, price, svc.GetIdentifiedModelPricing("models/"+base+"-"+tier))
		}
	}
	svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{"claude-sonnet-4-6": {InputCostPerToken: 3e-6}}}
	require.NotNil(t, svc.GetIdentifiedModelPricing("claude-sonnet-4-6-thinking"))
	require.Nil(t, svc.GetIdentifiedModelPricing("claude-sonnet-4-7-thinking"))
	require.Nil(t, svc.GetIdentifiedModelPricing("claude-custom-thinking"))
}

func TestPricingV0217DomesticRawOverrideAndFrozenRequest(t *testing.T) {
	f := newDomesticPricingFixture(t)
	f.write(t, "fallback.json", []byte(`{}`))
	f.write(t, "override.json", []byte(`{"gemini-4-argon-high":{"input_cost_per_token":0.000009,"output_cost_per_token":0.000011}}`))
	cfg := &config.Config{Pricing: config.PricingConfig{FallbackFile: f.path("fallback.json"), OverrideFile: f.path("override.json")}}
	svc := newDomesticPricingService(cfg)
	svc.pricingData = domesticBuild(t, svc, `{"provider/glm-5.3-flash":{"input_cost_per_token":0.000099,"output_cost_per_token":0.000199},"gemini-4-argon-high":{"input_cost_per_token":0.000002,"output_cost_per_token":0.000003}}`)
	require.InDelta(t, 99e-6, svc.GetModelPricing("provider/glm-5.3-flash").InputCostPerToken, 1e-20)
	require.InDelta(t, 99e-6, svc.GetModelPricing("glm-5.3-flash").InputCostPerToken, 1e-20)
	require.InDelta(t, 0.37e-6, svc.GetModelPricing("glm-5.3-flashx").InputCostPerToken, 1e-20)
	require.InDelta(t, 9e-6, svc.GetModelPricing("gemini-4-argon-high").InputCostPerToken, 1e-20)
	billing := NewBillingService(cfg, svc)
	resolver := NewModelPricingResolver(nil, billing)
	requested, upstream := "gemini-4-argon-high", "gemini-4-argon"
	require.NotEqual(t, requested, upstream)
	group := &Group{ID: 77, Platform: PlatformGemini, RateMultiplier: 0.375, ModelPricing: []ChannelModelPricing{{Models: []string{requested}, BillingMode: BillingModeToken, InputPrice: testPtrFloat64(0.05), OutputPrice: testPtrFloat64(0.1)}}}
	resolved := resolver.Resolve(context.Background(), PricingInput{Model: requested, Group: group})
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	request := TokenCostRequest{Ctx: context.Background(), Model: requested, Group: group, Tokens: UsageTokens{InputTokens: 10, OutputTokens: 5}, RateMultiplier: group.RateMultiplier, PricingAt: at, Resolver: resolver, Resolved: resolved}
	before, err := billing.CalculateTokenCostForRequest(request)
	require.NoError(t, err)
	require.InDelta(t, 1, before.TotalCost, 1e-12)
	require.InDelta(t, 0.375, before.ActualCost, 1e-12)
	input := billing.tokenCostInput(request, resolved)
	require.Same(t, resolved, input.Resolved)
	require.Equal(t, at, input.PricingAt)
	svc.mu.Lock()
	svc.pricingData = map[string]*LiteLLMModelPricing{upstream: {InputCostPerToken: 100}}
	svc.mu.Unlock()
	after, err := billing.CalculateTokenCostForRequest(request)
	require.NoError(t, err)
	require.Equal(t, before, after, fmt.Sprintf("one resolved snapshot at %s must survive catalog reload", at))
}
