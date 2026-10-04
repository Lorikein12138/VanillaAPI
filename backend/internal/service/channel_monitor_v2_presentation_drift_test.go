package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2SyntheticDriftStaysInBoundsAndMovesSlowly(t *testing.T) {
	origin := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	const atoms = 7 * 24 * 12 // seven days of 5 minute atoms
	moved := 0
	for leaf := 0; leaf < 12; leaf++ {
		seed := channelMonitorV2SyntheticLeafSeed("openai", int64(leaf+1), fmt.Sprintf("model-%d", leaf))
		minCache, maxCache := math.MaxFloat64, -math.MaxFloat64
		minTPS, maxTPS := math.MaxFloat64, -math.MaxFloat64
		var previousCache, previousTPS float64
		for index := 0; index < atoms; index++ {
			atom := origin.Add(time.Duration(index) * 5 * time.Minute)
			cache := channelMonitorV2SyntheticCacheRate(seed, atom)
			tps := channelMonitorV2SyntheticTokensPerSecond(seed, atom)
			require.GreaterOrEqual(t, cache, .80)
			require.LessOrEqual(t, cache, .99)
			require.GreaterOrEqual(t, tps, 10.0)
			require.LessOrEqual(t, tps, 50.0)
			if index > 0 {
				// Slow: neighbouring atoms differ by at most the curve slope plus the jitter.
				require.LessOrEqual(t, math.Abs(cache-previousCache), .025, "cache jump at atom %d of leaf %d", index, leaf)
				require.LessOrEqual(t, math.Abs(tps-previousTPS), 8.0, "tps jump at atom %d of leaf %d", index, leaf)
			}
			previousCache, previousTPS = cache, tps
			minCache, maxCache = math.Min(minCache, cache), math.Max(maxCache, cache)
			minTPS, maxTPS = math.Min(minTPS, tps), math.Max(maxTPS, tps)
		}
		// Not fixed: over a week every leaf wanders across a visible part of both ranges.
		if maxCache-minCache >= .06 && maxTPS-minTPS >= 15 {
			moved++
		}
	}
	require.GreaterOrEqual(t, moved, 11, "leaves must move irregularly, not sit on a constant")
}

func TestChannelMonitorV2SyntheticDriftIsIndependentPerLeafAndStablePerAtom(t *testing.T) {
	atom := time.Date(2026, 9, 12, 8, 35, 0, 0, time.UTC)
	cacheValues, tpsValues := map[string]struct{}{}, map[string]struct{}{}
	for groupID := int64(1); groupID <= 20; groupID++ {
		seed := channelMonitorV2SyntheticLeafSeed("anthropic", groupID, "claude-sonnet-4")
		cacheValues[fmt.Sprintf("%.6f", channelMonitorV2SyntheticCacheRate(seed, atom))] = struct{}{}
		tpsValues[fmt.Sprintf("%.6f", channelMonitorV2SyntheticTokensPerSecond(seed, atom))] = struct{}{}
		require.Equal(t, channelMonitorV2SyntheticCacheRate(seed, atom), channelMonitorV2SyntheticCacheRate(seed, atom))
		require.Equal(t, channelMonitorV2SyntheticTokensPerSecond(seed, atom), channelMonitorV2SyntheticTokensPerSecond(seed, atom))
	}
	require.Greater(t, len(cacheValues), 15, "leaves must not share one cache rate")
	require.Greater(t, len(tpsValues), 15, "leaves must not share one throughput")
}

func TestChannelMonitorV2SyntheticCacheReadTokensStayInsideRangeForAnyDenominator(t *testing.T) {
	// Below 5 tokens no whole number lies inside 80%–99%; atoms never get that small
	// (channelMonitorV2SyntheticMinAtomTokens).
	for denominator := int64(5); denominator <= 4000; denominator++ {
		for _, rate := range []float64{.80, .8049, .895, .9851, .99} {
			read := channelMonitorV2SyntheticCacheReadTokens(denominator, rate)
			require.LessOrEqual(t, read, denominator*99/100)
			require.GreaterOrEqual(t, read*100, denominator*80, "denominator=%d rate=%v", denominator, rate)
			if denominator >= 100 {
				require.Less(t, read, denominator, "never 100%%")
			}
		}
	}
}

func TestChannelMonitorV2SyntheticLeafThroughputIsTenToFiftyTokensPerSecond(t *testing.T) {
	thresholds := DefaultChannelMonitorV2HealthThresholds()
	asOf := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 400; i++ {
		start := asOf.Add(time.Duration(i) * 5 * time.Minute)
		groupID, model := int64(i%7+1), fmt.Sprintf("model-%d", i%5)
		raw, _, ok := channelMonitorV2SyntheticRawForBand("openai", groupID, model, start, thresholds, true, 1)
		require.True(t, ok)
		metric := raw.metric(5)
		tokensPerSecond := metric.TPM / 60
		require.GreaterOrEqual(t, tokensPerSecond, 10.0)
		require.LessOrEqual(t, tokensPerSecond, 50.0)
		require.Equal(t, metric.InputTokens+metric.OutputTokens+metric.CacheCreationTokens+metric.CacheReadTokens, metric.TokenCount)
		require.Greater(t, metric.OutputTokens, int64(0))
		require.Less(t, metric.OutputTokens*100, metric.TokenCount*15, "output stays a small share of the total")
		require.GreaterOrEqual(t, metric.CacheRate, .80)
		require.LessOrEqual(t, metric.CacheRate, .99)
		require.Equal(t, metric.CacheReadTokens, metric.CacheRateNumerator)

		// A sibling-model share divides the leaf so a row over all siblings keeps 10–50.
		shared, _, sharedOK := channelMonitorV2SyntheticRawForBand("openai", groupID, model, start, thresholds, true, 5)
		require.True(t, sharedOK)
		require.InDelta(t, float64(raw.cacheRead+raw.cacheCreation+raw.input+raw.output)/5, float64(shared.cacheRead+shared.cacheCreation+shared.input+shared.output), 2)
		require.GreaterOrEqual(t, shared.cacheRead*100, shared.cacheDenominator*80)
		require.LessOrEqual(t, shared.cacheRead*100, shared.cacheDenominator*99)
	}
}

func TestChannelMonitorV2SyntheticEveryBandStaysAchievableWithProductionCacheThresholds(t *testing.T) {
	thresholds := DefaultChannelMonitorV2HealthThresholds()
	thresholds.WarningCacheRate, thresholds.CriticalCacheRate = .85, .6
	asOf := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 1500; i++ {
		start := asOf.Add(time.Duration(i) * 5 * time.Minute)
		groupID, model := int64(i%11+1), fmt.Sprintf("model-%d", i%9)
		desired := channelMonitorV2SyntheticTargetBand("openai", groupID, model, start)
		metric, actual, ok := channelMonitorV2SyntheticMetricForBand("openai", groupID, model, start, thresholds, true)
		require.True(t, ok, "band %s must be achievable at atom %d", desired, i)
		require.Equal(t, desired, actual)
		require.GreaterOrEqual(t, metric.CacheRate, .80)
		require.LessOrEqual(t, metric.CacheRate, .99)
	}
}

func presentationUniverseDimensions(groups map[int64]string, models map[string][]string) *ChannelMonitorV2Dimensions {
	dimensions := &ChannelMonitorV2Dimensions{}
	for id := int64(1); id <= int64(len(groups)); id++ {
		dimensions.Groups = append(dimensions.Groups, ChannelMonitorV2GroupDimension{ID: id, Name: fmt.Sprintf("group-%d", id), Platform: groups[id]})
	}
	for platform, names := range models {
		for _, name := range names {
			dimensions.Models = append(dimensions.Models, ChannelMonitorV2Dimension{Value: name, Label: name, Platform: platform})
		}
	}
	return dimensions
}

func presentationUniverseConfig(platforms ...string) ChannelMonitorV2Config {
	cfg := ChannelMonitorV2Config{HealthThresholds: DefaultChannelMonitorV2HealthThresholds()}
	for _, platform := range platforms {
		cfg.Platforms = append(cfg.Platforms, ChannelMonitorV2PlatformConfig{Platform: platform, Enabled: true})
	}
	return cfg
}

func TestChannelMonitorV2PresentationUniverseSpreadsThroughputOverWindowModels(t *testing.T) {
	dimensions := presentationUniverseDimensions(
		map[int64]string{1: "openai", 2: "openai", 3: "gemini"},
		map[string][]string{"openai": {"gpt-5", "gpt-5-mini", "o3"}, "gemini": {"gemini-3"}},
	)
	loads := 0
	keys, shares, err := channelMonitorV2PresentationUniverse(ChannelMonitorV2Filter{}, presentationUniverseConfig("openai", "gemini"), dimensions,
		func() (*ChannelMonitorV2Dimensions, error) { loads++; return nil, nil })
	require.NoError(t, err)
	require.Zero(t, loads, "the catalog is only read for platforms without window models")
	require.Len(t, keys, 3*2+1)
	require.Equal(t, map[string]int{"openai": 3, "gemini": 1}, shares)

	// A model selection narrows the leaves but never changes the share.
	keys, shares, err = channelMonitorV2PresentationUniverse(ChannelMonitorV2Filter{Models: []string{"o3"}}, presentationUniverseConfig("openai", "gemini"), dimensions, nil)
	require.NoError(t, err)
	require.Len(t, keys, 2)
	require.Equal(t, 3, shares["openai"])
}

func TestChannelMonitorV2PresentationUniverseAssignsCatalogModelsPerGroup(t *testing.T) {
	groups := map[int64]string{1: "kimi", 2: "kimi", 3: "kimi", 4: "grok"}
	dimensions := presentationUniverseDimensions(groups, nil)
	catalog := presentationUniverseDimensions(nil, map[string][]string{
		"kimi": {"kimi-k2", "kimi-k2-turbo", ChannelMonitorV2OtherModel, "unknown", "UNKNOWN"},
		"grok": {"grok-4"},
		// A platform that is not enabled never contributes a model.
		"zhipu": {"glm-5"},
	})
	loads := 0
	loader := func() (*ChannelMonitorV2Dimensions, error) { loads++; return catalog, nil }
	cfg := presentationUniverseConfig("kimi", "grok")

	keys, shares, err := channelMonitorV2PresentationUniverse(ChannelMonitorV2Filter{}, cfg, dimensions, loader)
	require.NoError(t, err)
	require.Equal(t, 1, loads, "the catalog is read once per build")
	require.Equal(t, map[string]int{"kimi": 1, "grok": 1}, shares)
	require.Len(t, keys, 4)
	modelByGroup := map[int64]string{}
	for _, key := range keys {
		modelByGroup[key.groupID] = key.model
		require.NotEqual(t, "unknown", key.model)
		require.NotEqual(t, ChannelMonitorV2OtherModel, key.model)
	}
	require.Equal(t, "grok-4", modelByGroup[4])
	for _, group := range []int64{1, 2, 3} {
		require.Contains(t, []string{"kimi-k2", "kimi-k2-turbo"}, modelByGroup[group])
	}
	require.NotEqual(t, modelByGroup[1], modelByGroup[2], "groups of one platform take different models while the catalog has enough")

	// Identity does not depend on the platform/group/model selection or the build.
	for _, filter := range []ChannelMonitorV2Filter{
		{GroupIDs: []int64{2}},
		{Platforms: []string{"kimi"}},
		{GroupIDs: []int64{3}, Models: []string{modelByGroup[3]}},
	} {
		filtered, filteredShares, err := channelMonitorV2PresentationUniverse(filter, cfg, dimensions, loader)
		require.NoError(t, err)
		require.NotEmpty(t, filtered)
		require.Equal(t, 1, filteredShares["kimi"])
		for _, key := range filtered {
			require.Equal(t, modelByGroup[key.groupID], key.model, "leaf identity must not depend on the filter")
		}
	}
	repeat, _, err := channelMonitorV2PresentationUniverse(ChannelMonitorV2Filter{}, cfg, dimensions, loader)
	require.NoError(t, err)
	require.Equal(t, keys, repeat)
}

func TestChannelMonitorV2PresentationUniverseKeepsUnknownWithoutCatalogModels(t *testing.T) {
	dimensions := presentationUniverseDimensions(map[int64]string{1: "deepseek"}, nil)
	cfg := presentationUniverseConfig("deepseek")
	for name, loader := range map[string]channelMonitorV2PresentationCatalogLoader{
		"nil loader":    nil,
		"nil catalog":   func() (*ChannelMonitorV2Dimensions, error) { return nil, nil },
		"empty catalog": func() (*ChannelMonitorV2Dimensions, error) { return &ChannelMonitorV2Dimensions{}, nil },
		"only unknown": func() (*ChannelMonitorV2Dimensions, error) {
			return presentationUniverseDimensions(nil, map[string][]string{"deepseek": {"unknown", ChannelMonitorV2OtherModel}}), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			keys, shares, err := channelMonitorV2PresentationUniverse(ChannelMonitorV2Filter{}, cfg, dimensions, loader)
			require.NoError(t, err)
			require.Len(t, keys, 1)
			require.Equal(t, "unknown", keys[0].model)
			require.Equal(t, 1, shares["deepseek"])
		})
	}
}

func TestChannelMonitorV2PresentationUniversePropagatesCatalogErrors(t *testing.T) {
	dimensions := presentationUniverseDimensions(map[int64]string{1: "grok"}, nil)
	boom := errors.New("catalog unavailable")
	_, _, err := channelMonitorV2PresentationUniverse(ChannelMonitorV2Filter{}, presentationUniverseConfig("grok"), dimensions,
		func() (*ChannelMonitorV2Dimensions, error) { return nil, boom })
	require.ErrorIs(t, err, boom)
}

func TestChannelMonitorV2PresentationShowsCatalogModelsAndKeepsRowThroughputInRange(t *testing.T) {
	asOf := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	repo := &channelMonitorV2RepoStub{
		config: ChannelMonitorV2Config{Enabled: true, SyntheticEmptyBucketsEnabled: true, RefreshIntervalSeconds: 300,
			Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}, {Platform: "grok", Enabled: true}},
			HealthThresholds: DefaultChannelMonitorV2HealthThresholds()},
		dimensions: &ChannelMonitorV2Dimensions{
			Groups: []ChannelMonitorV2GroupDimension{{ID: 1, Name: "primary", Platform: "openai"}, {ID: 2, Name: "secondary", Platform: "grok"}},
			Models: []ChannelMonitorV2Dimension{
				{Value: "gpt-5", Label: "gpt-5", Platform: "openai"},
				{Value: "gpt-5-mini", Label: "gpt-5-mini", Platform: "openai"},
				{Value: "grok-4", Label: "grok-4", Platform: "grok"},
			},
		},
		matrix: &ChannelMonitorV2Matrix{Coverage: ChannelMonitorV2Coverage{CoverageStart: asOf.Add(-90 * time.Minute), DataThrough: asOf, ComputedAt: asOf}},
	}
	svc := NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	filter, err := svc.ParseFilterAt("90m", nil, nil, nil, asOf)
	require.NoError(t, err)
	// The window itself carries no models (tiny real traffic): the page used to show "unknown".
	windowDimensions := &ChannelMonitorV2Dimensions{Groups: repo.dimensions.Groups}
	presentation, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, windowDimensions)
	require.NoError(t, err)
	require.Len(t, presentation.Rows, 2)
	for _, row := range presentation.Rows {
		require.NotEqual(t, "unknown", row.Model)
	}
	require.Len(t, repo.dimensionFilters, 1)
	catalog := repo.dimensionFilters[0]
	require.Empty(t, catalog.Platforms)
	require.Empty(t, catalog.GroupIDs)
	require.Empty(t, catalog.Models)
	require.Equal(t, 30*24*time.Hour, catalog.End.Sub(catalog.Start), "the model catalog spans 30 days")
	require.False(t, catalog.End.Before(asOf))

	// Platform rows stay inside 10–50 tokens/second: leaves of a platform with several
	// models divide the throughput between them.
	withModels, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, repo.dimensions)
	require.NoError(t, err)
	perGroup := map[int64]float64{}
	for _, row := range withModels.Rows {
		for _, bucket := range row.Buckets {
			require.GreaterOrEqual(t, bucket.Metrics.CacheRate, .80)
			require.LessOrEqual(t, bucket.Metrics.CacheRate, .99)
		}
		perGroup[*row.GroupID] += row.Metrics.TPM / 60
	}
	for groupID, tokensPerSecond := range perGroup {
		require.GreaterOrEqual(t, tokensPerSecond, 9.0, "group %d", groupID)
		require.LessOrEqual(t, tokensPerSecond, 51.0, "group %d", groupID)
	}
}

func TestChannelMonitorV2PresentationFailsWhenModelCatalogCannotBeLoaded(t *testing.T) {
	asOf := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	boom := errors.New("catalog unavailable")
	repo := &channelMonitorV2RepoStub{
		config: ChannelMonitorV2Config{Enabled: true, SyntheticEmptyBucketsEnabled: true,
			Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "grok", Enabled: true}},
			HealthThresholds: DefaultChannelMonitorV2HealthThresholds()},
		dimensionsErr: boom,
		matrix:        &ChannelMonitorV2Matrix{Coverage: ChannelMonitorV2Coverage{CoverageStart: asOf.Add(-90 * time.Minute), DataThrough: asOf, ComputedAt: asOf}},
	}
	svc := NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	filter, err := svc.ParseFilterAt("90m", nil, nil, nil, asOf)
	require.NoError(t, err)
	_, err = svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config,
		&ChannelMonitorV2Dimensions{Groups: []ChannelMonitorV2GroupDimension{{ID: 1, Name: "primary", Platform: "grok"}}})
	require.ErrorIs(t, err, boom)
}
