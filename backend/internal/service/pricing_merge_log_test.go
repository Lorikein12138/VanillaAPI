//go:build unit

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

// capturePricingMergeLogs 复用 captureStructuredLog 串行捕获 logger.L() 事件，用例结束时恢复 sink。
func capturePricingMergeLogs(t *testing.T) *inMemoryLogSink {
	t.Helper()
	sink, restore := captureStructuredLog(t)
	t.Cleanup(restore)
	return sink
}

// pricingMergeEventsContaining 返回消息包含 substr 的事件，便于断言级别与字段。
func pricingMergeEventsContaining(sink *inMemoryLogSink, substr string) []*logger.LogEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []*logger.LogEvent
	for _, event := range sink.events {
		if strings.Contains(event.Message, substr) {
			out = append(out, event)
		}
	}
	return out
}

func newPricingMergeService(fallbackFile string) *PricingService {
	return &PricingService{cfg: &config.Config{Pricing: config.PricingConfig{FallbackFile: fallbackFile}}}
}

func writePricingMergeFallback(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fallback.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestPricingFallbackMergeLogSuccessIsStructuredInfo(t *testing.T) {
	sink := capturePricingMergeLogs(t)
	data := map[string]*LiteLLMModelPricing{"remote-model": {InputCostPerToken: 9}}
	fallback := writePricingMergeFallback(t, `{
		"remote-model": {"litellm_provider":"openai","input_cost_per_token":1},
		"fallback-only-a": {"litellm_provider":"openai","input_cost_per_token":2},
		"fallback-only-b": {"litellm_provider":"openai","input_cost_per_token":3}
	}`)

	merged := newPricingMergeService(fallback).mergeFallbackPricingData(data)

	require.Len(t, merged, 3)
	events := pricingMergeEventsContaining(sink, "pricing_fallback_models_merged")
	require.Len(t, events, 1)
	require.Equal(t, "info", events[0].Level)
	require.Equal(t, "service.pricing", events[0].Fields["component"])
	require.EqualValues(t, 2, events[0].Fields["merged_model_count"])
	require.Empty(t, pricingMergeEventsContaining(sink, "fallback-only models"))
}

func TestPricingFallbackMergeLogRemoteEntryWins(t *testing.T) {
	sink := capturePricingMergeLogs(t)
	remote := &LiteLLMModelPricing{InputCostPerToken: 9}
	data := map[string]*LiteLLMModelPricing{"shared-model": remote}
	fallback := writePricingMergeFallback(t, `{"shared-model":{"litellm_provider":"openai","input_cost_per_token":1}}`)

	merged := newPricingMergeService(fallback).mergeFallbackPricingData(data)

	require.Len(t, merged, 1)
	require.Same(t, remote, merged["shared-model"])
	require.Empty(t, pricingMergeEventsContaining(sink, "pricing_fallback_models_merged"))
}

func TestPricingFallbackMergeLogWithoutFallbackFileMergesNothing(t *testing.T) {
	sink := capturePricingMergeLogs(t)
	data := map[string]*LiteLLMModelPricing{"remote-model": {InputCostPerToken: 9}}

	merged := newPricingMergeService("   ").mergeFallbackPricingData(data)

	require.Len(t, merged, 1)
	require.Empty(t, pricingMergeEventsContaining(sink, "pricing_fallback_models_merged"))
	require.Empty(t, pricingMergeEventsContaining(sink, "Fallback merge"))
}

func TestPricingFallbackMergeLogMissingFileWarnsAndKeepsData(t *testing.T) {
	sink := capturePricingMergeLogs(t)
	data := map[string]*LiteLLMModelPricing{"remote-model": {InputCostPerToken: 9}}
	missing := filepath.Join(t.TempDir(), "absent.json")

	merged := newPricingMergeService(missing).mergeFallbackPricingData(data)

	require.Len(t, merged, 1)
	failures := pricingMergeEventsContaining(sink, "Fallback merge skipped")
	require.Len(t, failures, 1)
	require.Equal(t, "warn", failures[0].Level)
	require.Empty(t, pricingMergeEventsContaining(sink, "pricing_fallback_models_merged"))
}

func TestPricingFallbackMergeLogParseFailureWarnsAndKeepsData(t *testing.T) {
	sink := capturePricingMergeLogs(t)
	data := map[string]*LiteLLMModelPricing{"remote-model": {InputCostPerToken: 9}}
	fallback := writePricingMergeFallback(t, `{not-json`)

	merged := newPricingMergeService(fallback).mergeFallbackPricingData(data)

	require.Len(t, merged, 1)
	failures := pricingMergeEventsContaining(sink, "Fallback merge parse skipped")
	require.Len(t, failures, 1)
	require.Equal(t, "warn", failures[0].Level)
	require.Empty(t, pricingMergeEventsContaining(sink, "pricing_fallback_models_merged"))
}
