package service

import (
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

//go:embed data/domestic_model_prices.json
var domesticModelPricesFS embed.FS

var domesticSupplementModels = []string{"glm-5.3-flash", "glm-5.3-flashx"}

func domesticSupplementModelName(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", false
	}
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = strings.TrimSpace(name[index+1:])
	}
	switch name {
	case "glm-5.3-flash", "glm-5.3-flashx":
		return name, true
	default:
		return "", false
	}
}

func isDomesticSupplementModel(name string) bool {
	_, ok := domesticSupplementModelName(name)
	return ok
}

// reserveUpstreamDomesticEntries preserves the fact that an upstream source key
// exists even when parsePricingData cannot produce usable token pricing for it.
func (s *PricingService) reserveUpstreamDomesticEntries(rawBody []byte, data map[string]*LiteLLMModelPricing) (map[string]*LiteLLMModelPricing, error) {
	var rawEntries map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &rawEntries); err != nil {
		return nil, fmt.Errorf("parse upstream pricing keys: %w", err)
	}
	if data == nil {
		data = make(map[string]*LiteLLMModelPricing)
	}

	keysByModel := make(map[string][]string, len(domesticSupplementModels))
	for key := range rawEntries {
		if model, ok := domesticSupplementModelName(key); ok {
			keysByModel[model] = append(keysByModel[model], key)
		}
	}
	for _, model := range domesticSupplementModels {
		keys := keysByModel[model]
		if len(keys) == 0 {
			continue
		}
		candidates := make([]pricingKeyCandidate, 0, len(keys))
		for _, key := range keys {
			candidates = append(candidates, pricingKeyCandidate{key: key})
		}
		sortPricingKeyCandidates(candidates, model)
		selected := candidates[0].key
		pricing := data[selected]
		if pricing == nil {
			pricing = &LiteLLMModelPricing{TokenPricingAbsent: true}
		}

		for key := range data {
			if normalized, ok := domesticSupplementModelName(key); ok && normalized == model {
				delete(data, key)
			}
		}
		data[model] = pricing
	}
	return data, nil
}

func (s *PricingService) mergeBundledDomesticPricing(data map[string]*LiteLLMModelPricing) (map[string]*LiteLLMModelPricing, error) {
	body, err := domesticModelPricesFS.ReadFile("data/domestic_model_prices.json")
	if err != nil {
		return nil, fmt.Errorf("read bundled domestic pricing: %w", err)
	}
	if err := validateBundledDomesticPricing(body); err != nil {
		return nil, err
	}
	if data == nil {
		data = make(map[string]*LiteLLMModelPricing)
	}
	parsed, err := s.parsePricingData(body)
	if err != nil {
		return nil, fmt.Errorf("parse bundled domestic pricing: %w", err)
	}
	for _, model := range domesticSupplementModels {
		if hasDomesticSupplementEntry(data, model) {
			continue
		}
		if pricing := parsed[model]; pricing != nil {
			data[model] = pricing
		}
	}
	return data, nil
}

func hasDomesticSupplementEntry(data map[string]*LiteLLMModelPricing, model string) bool {
	for key := range data {
		if normalized, ok := domesticSupplementModelName(key); ok && normalized == model {
			return true
		}
	}
	return false
}

func validateBundledDomesticPricing(body []byte) error {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(body, &entries); err != nil {
		return fmt.Errorf("invalid bundled domestic pricing JSON: %w", err)
	}
	if len(entries) != len(domesticSupplementModels) {
		return fmt.Errorf("bundled domestic pricing must contain exactly %d models", len(domesticSupplementModels))
	}
	for _, model := range domesticSupplementModels {
		raw, ok := entries[model]
		if !ok {
			return fmt.Errorf("bundled domestic pricing is missing %s", model)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return fmt.Errorf("bundled domestic pricing %s must be an object", model)
		}
		for _, field := range []string{"input_cost_per_token", "output_cost_per_token", "cache_read_input_token_cost"} {
			var value float64
			if err := json.Unmarshal(fields[field], &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
				return fmt.Errorf("bundled domestic pricing %s requires a positive finite %s", model, field)
			}
		}
		for field := range fields {
			if strings.HasPrefix(field, "cache_creation_input_token_cost") {
				return fmt.Errorf("bundled domestic pricing %s must not invent cache write prices", model)
			}
			if strings.HasSuffix(field, "cost_per_token") {
				var value float64
				if err := json.Unmarshal(fields[field], &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
					return fmt.Errorf("bundled domestic pricing %s has invalid %s", model, field)
				}
			}
		}
		var provider, mode string
		var cacheSupport bool
		if err := json.Unmarshal(fields["litellm_provider"], &provider); err != nil || provider != "z_ai" {
			return fmt.Errorf("bundled domestic pricing %s must use z_ai", model)
		}
		if err := json.Unmarshal(fields["mode"], &mode); err != nil || mode != "chat" {
			return fmt.Errorf("bundled domestic pricing %s must use chat mode", model)
		}
		if err := json.Unmarshal(fields["supports_prompt_caching"], &cacheSupport); err != nil || !cacheSupport {
			return fmt.Errorf("bundled domestic pricing %s must enable prompt caching", model)
		}
	}
	return nil
}
