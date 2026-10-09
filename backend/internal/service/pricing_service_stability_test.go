//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPricingLookupStableChoice(t *testing.T) {
	const model = "claude-opus-4-7"
	price := func(input float64) *LiteLLMModelPricing {
		return &LiteLLMModelPricing{InputCostPerToken: input}
	}

	t.Run("canonical key wins before dated candidates", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			data := make(map[string]*LiteLLMModelPricing)
			if i%2 == 0 {
				data["provider/claude-opus-4-7-20270101"] = price(3)
				data[model+"-20260101"] = price(2)
				data[model] = price(1)
			} else {
				data[model] = price(1)
				data[model+"-20260101"] = price(2)
				data["provider/claude-opus-4-7-20270101"] = price(3)
			}
			svc := &PricingService{pricingData: data}
			got := svc.GetIdentifiedModelPricing(model + "-20260201")
			require.NotNil(t, got)
			require.Equal(t, float64(1), got.InputCostPerToken)
		}
	})

	t.Run("newest valid date wins and ties use lowercase full key", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			data := make(map[string]*LiteLLMModelPricing)
			keys := []string{
				"z/claude-opus-4-7-20260301",
				"b/claude-opus-4-7-20260101",
				"a/claude-opus-4-7-20260301",
				"claude-opus-4-7-20261301",
			}
			prices := map[string]float64{
				"z/claude-opus-4-7-20260301": 4,
				"b/claude-opus-4-7-20260101": 2,
				"a/claude-opus-4-7-20260301": 3,
				"claude-opus-4-7-20261301":   1,
			}
			if i%2 == 1 {
				for left, right := 0, len(keys)-1; left < right; left, right = left+1, right-1 {
					keys[left], keys[right] = keys[right], keys[left]
				}
			}
			for _, key := range keys {
				data[key] = price(prices[key])
			}
			svc := &PricingService{pricingData: data}
			got := svc.GetIdentifiedModelPricing(model + "-20260401")
			require.NotNil(t, got)
			require.Equal(t, float64(3), got.InputCostPerToken, "a/… is the lowercase-key tie winner")
		}
	})

	t.Run("nil is skipped and zero is a valid exact price", func(t *testing.T) {
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
			model:                      nil,
			"provider/claude-opus-4-7": price(4),
		}}
		got := svc.GetIdentifiedModelPricing(model)
		require.NotNil(t, got)
		require.Equal(t, float64(4), got.InputCostPerToken)

		svc = &PricingService{pricingData: map[string]*LiteLLMModelPricing{model: price(0)}}
		got = svc.GetIdentifiedModelPricing(model)
		require.NotNil(t, got)
		require.Zero(t, got.InputCostPerToken)
	})

	t.Run("only real calendar date suffixes are removed", func(t *testing.T) {
		svc := &PricingService{}
		require.Equal(t, model, svc.extractBaseName(model+"-20260228"))
		require.Equal(t, model+"-20260230", svc.extractBaseName(model+"-20260230"))
		require.Equal(t, model+"-12345678", svc.extractBaseName(model+"-12345678"))
	})

	t.Run("family fuzzy candidates use the same stable sort", func(t *testing.T) {
		svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
			"z/claude-opus-4-7-20270101": price(3),
			"b/claude-opus-4-7-20260101": price(1),
			"a/claude-opus-4-7-20270101": price(2),
			"claude-opus-4-6":            price(4),
		}}
		got := svc.matchByModelFamily("claude-opus-4-7")
		require.NotNil(t, got)
		require.Equal(t, float64(2), got.InputCostPerToken, "most-specific family then latest-date/lower-key choice")
	})
}

func TestClaudeThinkingPricingAliasPrecedence(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6-thinking", "claude-opus-4-6-thinking"} {
		t.Run(model, func(t *testing.T) {
			svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
				model:                                  {InputCostPerToken: 1},
				strings.TrimSuffix(model, "-thinking"): {InputCostPerToken: 2},
				strings.Replace(strings.TrimSuffix(model, "-thinking"), "-4-6", "-4.6", 1): {InputCostPerToken: 3},
			}}
			got := svc.GetIdentifiedModelPricing(model)
			require.NotNil(t, got)
			require.Equal(t, float64(1), got.InputCostPerToken, "raw exact ID wins")

			svc = &PricingService{pricingData: map[string]*LiteLLMModelPricing{
				strings.TrimSuffix(model, "-thinking"):                                     {InputCostPerToken: 2},
				strings.Replace(strings.TrimSuffix(model, "-thinking"), "-4-6", "-4.6", 1): {InputCostPerToken: 3},
			}}
			got = svc.GetIdentifiedModelPricing("models/" + model)
			require.NotNil(t, got)
			require.Equal(t, float64(2), got.InputCostPerToken, "ordinary dashed alias precedes dotted alias")

			svc = &PricingService{pricingData: map[string]*LiteLLMModelPricing{
				"provider/" + model: {InputCostPerToken: 4},
			}}
			got = svc.GetIdentifiedModelPricing("provider/" + model)
			require.NotNil(t, got)
			require.Equal(t, float64(4), got.InputCostPerToken, "provider-qualified exact ID is preserved")
		})
	}

	svc := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"claude-sonnet-4-6": {InputCostPerToken: 1},
	}}
	require.Nil(t, svc.GetIdentifiedModelPricing("claude-sonnet-4-7-thinking"), "arbitrary thinking IDs do not gain aliases")
	require.Nil(t, svc.GetIdentifiedModelPricing("claude-opus-custom-thinking"), "unknown identified Claude families remain unguessed")
}
