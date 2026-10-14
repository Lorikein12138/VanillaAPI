//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestHaikuPricingKeepsStableOfficialSnapshot(t *testing.T) {
	const catalogA = `{"claude-haiku-5-5":{"litellm_provider":"anthropic","mode":"chat",
		"input_cost_per_token":1e-7,"output_cost_per_token":5e-7,
		"cache_read_input_token_cost":1e-8,"cache_storage_cost_per_token_per_hour":7e-9}}`
	const catalogB = `{"claude-haiku-5-5":{"litellm_provider":"anthropic","mode":"chat",
		"input_cost_per_token":2e-7,"output_cost_per_token":8e-7,
		"cache_read_input_token_cost":2e-8,"cache_storage_cost_per_token_per_hour":9e-9}}`

	first := newStubPricingServiceFromJSON(t, catalogA)
	second := newStubPricingServiceFromJSON(t, catalogB)
	catalog := first
	billing := NewBillingService(&config.Config{}, catalog)
	plaza := &ModelPlazaService{billingService: billing, resolver: NewModelPricingResolver(nil, billing)}

	cachePrice := catalog.GetModelPricing("claude-haiku-5-5-20251001")
	require.NotNil(t, cachePrice)
	require.InDelta(t, 7e-9, cachePrice.CacheStorageCostPerTokenPerHour, 1e-18)

	memo := make(map[string]*PlazaOfficialPricing)
	snapshot := plaza.lookupOfficialPricing(context.Background(), "claude-haiku-5-5", memo)
	require.NotNil(t, snapshot)
	require.InDelta(t, 1e-7, *snapshot.InputPrice, 1e-15)
	require.InDelta(t, 5e-7, *snapshot.OutputPrice, 1e-15)

	catalog.mu.Lock()
	catalog.pricingData = second.pricingData
	catalog.mu.Unlock()
	require.Same(t, snapshot, plaza.lookupOfficialPricing(context.Background(), "claude-haiku-5-5", memo),
		"a plaza request memo keeps one immutable official snapshot")
	require.InDelta(t, 1e-7, *snapshot.InputPrice, 1e-15)
	require.InDelta(t, 5e-7, *snapshot.OutputPrice, 1e-15)
	updated := plaza.lookupOfficialPricing(context.Background(), "claude-haiku-5-5", make(map[string]*PlazaOfficialPricing))
	require.NotNil(t, updated)
	require.InDelta(t, 2e-7, *updated.InputPrice, 1e-15)
	require.InDelta(t, 8e-7, *updated.OutputPrice, 1e-15)
}

func TestStorageOverridePreservesDomesticLayer(t *testing.T) {
	f := newDomesticPricingFixture(t)
	f.write(t, "fallback.json", []byte(`{}`))
	f.write(t, "override.json", []byte(`{"gemini-cache":{"cache_storage_cost_per_token_per_hour":9e-9}}`))
	cfg := &config.Config{Pricing: config.PricingConfig{
		FallbackFile: f.path("fallback.json"),
		OverrideFile: f.path("override.json"),
	}}
	svc := newDomesticPricingService(cfg)
	data := domesticBuild(t, svc, `{"sentinel-model":{"input_cost_per_token":1e-6,"output_cost_per_token":2e-6},
		"gemini-cache":{"litellm_provider":"gemini","mode":"chat","input_cost_per_token":3e-6,"output_cost_per_token":4e-6}}`)

	require.InDelta(t, 9e-9, data["gemini-cache"].CacheStorageCostPerTokenPerHour, 1e-18,
		"the operator cache-storage override is parsed and applied")
	require.InDelta(t, 0.15e-6, data["glm-5.3-flash"].InputCostPerToken, 1e-20)
	require.InDelta(t, 0.50e-6, data["glm-5.3-flash"].OutputCostPerToken, 1e-20)
	require.InDelta(t, 0.37e-6, data["glm-5.3-flashx"].InputCostPerToken, 1e-20)
	require.InDelta(t, 1.25e-6, data["glm-5.3-flashx"].OutputCostPerToken, 1e-20)
}
