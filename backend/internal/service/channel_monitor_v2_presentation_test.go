package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2PresentationAnchorAndRetention(t *testing.T) {
	svc := NewChannelMonitorV2Service(&channelMonitorV2RepoStub{})
	asOf := time.Date(2026, 9, 29, 12, 34, 56, 0, time.UTC)
	tests := []struct {
		rangeValue string
		want       []time.Duration
	}{
		{"90m", []time.Duration{5 * time.Minute}},
		{"24h", []time.Duration{5 * time.Minute}},
		{"7d", []time.Duration{time.Hour, 5 * time.Minute}},
		{"30d", []time.Duration{12 * time.Hour, time.Hour, 5 * time.Minute}},
	}
	for _, test := range tests {
		t.Run(test.rangeValue, func(t *testing.T) {
			filter, err := svc.ParseFilterAt(test.rangeValue, nil, nil, nil, asOf)
			require.NoError(t, err)
			parts := channelMonitorV2PresentationPartitions(filter)
			got := make([]time.Duration, len(parts))
			for i, part := range parts {
				got[i] = part.bucket
				if i > 0 {
					require.Equal(t, parts[i-1].end, part.start, "rollup partitions must be adjacent and disjoint")
				}
			}
			require.Equal(t, test.want, got)
			require.Equal(t, filter.Start, parts[0].start)
			require.Equal(t, filter.End, parts[len(parts)-1].end)
		})
	}
}

func TestChannelMonitorV2PresentationTypeSafeAndCompositeEmptyRowsRespectSwitch(t *testing.T) {
	asOf := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	svc := NewChannelMonitorV2Service(&channelMonitorV2RepoStub{})
	filter, err := svc.ParseFilterAt("90m", nil, nil, nil, asOf)
	require.NoError(t, err)
	dimensions := &ChannelMonitorV2Dimensions{
		Groups: []ChannelMonitorV2GroupDimension{
			{ID: 1, Name: "TypeSafe", Platform: "typesafe"},
			{ID: 2, Name: "Composite", Platform: "composite", Platforms: []string{"typesafe"}},
		},
	}
	cfg := ChannelMonitorV2Config{
		SyntheticEmptyBucketsEnabled: true,
		RefreshIntervalSeconds:       300,
		Platforms: []ChannelMonitorV2PlatformConfig{
			{Platform: "typesafe", Enabled: true, Models: []string{"jev-latest"}},
			{Platform: "composite", Enabled: true},
		},
		HealthThresholds: DefaultChannelMonitorV2HealthThresholds(),
	}
	repo := &channelMonitorV2RepoStub{
		matrices: map[int]*ChannelMonitorV2Matrix{300: {Items: []ChannelMonitorV2MatrixRow{}}},
	}
	svc = NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	presentation, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, cfg, dimensions)
	require.NoError(t, err)
	require.Len(t, presentation.Rows, 2)
	require.Equal(t, []string{"typesafe", "typesafe"}, []string{presentation.Rows[0].Platform, presentation.Rows[1].Platform})
	require.Equal(t, []int64{1, 2}, []int64{*presentation.Rows[0].GroupID, *presentation.Rows[1].GroupID})
	for _, row := range presentation.Rows {
		require.Empty(t, row.Buckets, "empty TypeSafe and Composite/TypeSafe leaves remain in the presentation directory")
	}

	cfg.Platforms[0].Enabled = false
	disabled, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, cfg, dimensions)
	require.NoError(t, err)
	require.Empty(t, disabled.Rows, "disabled TypeSafe must not produce rows for direct or Composite membership")
}

func TestChannelMonitorV2PresentationWindowOverlapStableAtoms(t *testing.T) {
	asOf := time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC)
	service := NewChannelMonitorV2Service(&channelMonitorV2RepoStub{})
	filters := make(map[string]ChannelMonitorV2Filter, 4)
	for _, rangeValue := range []string{"90m", "24h", "7d", "30d"} {
		filter, err := service.ParseFilterAt(rangeValue, nil, nil, nil, asOf)
		require.NoError(t, err)
		filters[rangeValue] = filter
	}
	groupID := int64(9)
	dimensions := &ChannelMonitorV2Dimensions{
		Groups: []ChannelMonitorV2GroupDimension{{ID: groupID, Name: "primary", Platform: "openai"}},
		Models: []ChannelMonitorV2Dimension{{Value: "gpt-5", Label: "gpt-5", Platform: "openai"}},
	}
	config := ChannelMonitorV2Config{
		SyntheticEmptyBucketsEnabled: true, RefreshIntervalSeconds: 300,
		Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
		HealthThresholds: DefaultChannelMonitorV2HealthThresholds(),
	}
	coverage := ChannelMonitorV2Coverage{CoverageStart: filters["30d"].Start, DataThrough: asOf, ComputedAt: asOf}
	repo := &channelMonitorV2RepoStub{matrices: map[int]*ChannelMonitorV2Matrix{
		300:   {Coverage: coverage},
		3600:  {Coverage: coverage},
		43200: {Coverage: coverage},
	}}
	service = NewChannelMonitorV2Service(repo)
	service.now = func() time.Time { return asOf }
	project := func(rangeValue string) *ChannelMonitorV2Presentation {
		result, err := service.buildChannelMonitorV2Presentation(context.Background(), filters[rangeValue], config, dimensions)
		require.NoError(t, err)
		return result
	}
	byRange := map[string]*ChannelMonitorV2Presentation{
		"90m": project("90m"), "24h": project("24h"), "7d": project("7d"), "30d": project("30d"),
	}
	key := channelMonitorV2PresentationKey{platform: "openai", groupID: groupID, model: "gpt-5"}
	shortRow := findPresentationLeaf(t, byRange["90m"], key)
	shortHour := newChannelMonitorV2MetricFold()
	for _, point := range shortRow.Buckets {
		if !point.BucketStart.Before(time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)) {
			shortHour.add(point.Metrics)
		}
	}
	fullDay := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	requirePresentationMetricsEquivalent(t, shortHour.metric(time.Hour.Minutes()), findPresentationBucket(t, findPresentationLeaf(t, byRange["24h"], key), fullDay.Add(23*time.Hour)).Metrics)

	dayMetrics := func(rangeValue string, starts ...time.Time) ChannelMonitorV2Metric {
		fold := newChannelMonitorV2MetricFold()
		row := findPresentationLeaf(t, byRange[rangeValue], key)
		for _, start := range starts {
			fold.add(findPresentationBucket(t, row, start).Metrics)
		}
		return fold.metric(24 * time.Hour.Minutes())
	}
	day30 := findPresentationBucket(t, findPresentationLeaf(t, byRange["30d"], key), fullDay).Metrics
	day24 := dayMetrics("24h", fullDay, fullDay.Add(time.Hour), fullDay.Add(2*time.Hour), fullDay.Add(3*time.Hour), fullDay.Add(4*time.Hour), fullDay.Add(5*time.Hour), fullDay.Add(6*time.Hour), fullDay.Add(7*time.Hour), fullDay.Add(8*time.Hour), fullDay.Add(9*time.Hour), fullDay.Add(10*time.Hour), fullDay.Add(11*time.Hour), fullDay.Add(12*time.Hour), fullDay.Add(13*time.Hour), fullDay.Add(14*time.Hour), fullDay.Add(15*time.Hour), fullDay.Add(16*time.Hour), fullDay.Add(17*time.Hour), fullDay.Add(18*time.Hour), fullDay.Add(19*time.Hour), fullDay.Add(20*time.Hour), fullDay.Add(21*time.Hour), fullDay.Add(22*time.Hour), fullDay.Add(23*time.Hour))
	day7d := dayMetrics("7d", fullDay, fullDay.Add(12*time.Hour))
	requirePresentationMetricsEquivalent(t, day24, day30)
	requirePresentationMetricsEquivalent(t, day7d, day30)
}

func requirePresentationMetricsEquivalent(t *testing.T, got, want ChannelMonitorV2Metric) {
	t.Helper()
	require.Equal(t, want.SuccessRequests, got.SuccessRequests)
	require.Equal(t, want.ErrorRequests, got.ErrorRequests)
	require.Equal(t, want.RequestCount, got.RequestCount)
	require.Equal(t, want.InputTokens, got.InputTokens)
	require.Equal(t, want.OutputTokens, got.OutputTokens)
	require.Equal(t, want.CacheCreationTokens, got.CacheCreationTokens)
	require.Equal(t, want.CacheReadTokens, got.CacheReadTokens)
	require.Equal(t, want.TokenCount, got.TokenCount)
	require.Equal(t, want.CacheRateNumerator, got.CacheRateNumerator)
	require.Equal(t, want.CacheRateDenominator, got.CacheRateDenominator)
	require.InDelta(t, want.ErrorRate, got.ErrorRate, .000001)
	require.InDelta(t, want.SuccessRate, got.SuccessRate, .000001)
	require.InDelta(t, want.CacheRate, got.CacheRate, .000001)
	require.InDelta(t, want.RPM, got.RPM, .000001)
	require.InDelta(t, want.TPM, got.TPM, .000001)
	require.InDelta(t, *want.TTFT.P50Ms, *got.TTFT.P50Ms, 2)
	require.InDelta(t, *want.TTFT.P90Ms, *got.TTFT.P90Ms, 2)
	require.InDelta(t, *want.Duration.P50Ms, *got.Duration.P50Ms, 2)
}

func channelMonitorV2Presentation30dFixture(t *testing.T) (*ChannelMonitorV2Service, ChannelMonitorV2Filter, ChannelMonitorV2Config, *ChannelMonitorV2Dimensions, *channelMonitorV2RepoStub) {
	t.Helper()
	asOf := time.Date(2026, 9, 29, 12, 34, 56, 0, time.UTC)
	svc := NewChannelMonitorV2Service(&channelMonitorV2RepoStub{})
	filter, err := svc.ParseFilterAt("30d", nil, nil, nil, asOf)
	require.NoError(t, err)
	models := make([]string, 8)
	for index := range models {
		models[index] = fmt.Sprintf("model-%02d", index+1)
	}
	groups := make([]ChannelMonitorV2GroupDimension, 21)
	for index := range groups {
		groups[index] = ChannelMonitorV2GroupDimension{ID: int64(index + 1), Name: fmt.Sprintf("group-%d", index+1), Platform: "openai"}
	}
	dimensions := &ChannelMonitorV2Dimensions{Groups: groups}
	for _, model := range models {
		dimensions.Models = append(dimensions.Models, ChannelMonitorV2Dimension{Value: model, Label: model, Platform: "openai"})
	}
	cfg := ChannelMonitorV2Config{
		SyntheticEmptyBucketsEnabled: true, RefreshIntervalSeconds: 300,
		Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: models}},
		HealthThresholds: DefaultChannelMonitorV2HealthThresholds(),
	}
	coverage := ChannelMonitorV2Coverage{CoverageStart: filter.Start, DataThrough: asOf, ComputedAt: asOf, BucketSeconds: 300}
	repo := &channelMonitorV2RepoStub{
		config: cfg, dimensions: dimensions,
		matrices: map[int]*ChannelMonitorV2Matrix{
			300:   {Coverage: coverage},
			3600:  {Coverage: coverage},
			43200: {Coverage: coverage},
		},
	}
	svc = NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	return svc, filter, cfg, dimensions, repo
}

func TestChannelMonitorV2Presentation30dBoundsTimeAndMemory(t *testing.T) {
	svc, filter, cfg, dimensions, repo := channelMonitorV2Presentation30dFixture(t)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	presentation, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, cfg, dimensions)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	require.Len(t, presentation.Rows, 168)
	totalBuckets := 0
	for _, row := range presentation.Rows {
		totalBuckets += len(row.Buckets)
	}
	require.Equal(t, 5040, totalBuckets, "only 30 response buckets per leaf are retained, not all 8,640 five-minute atoms")
	require.Len(t, repo.matrixFilters, 3, "each disjoint retention partition is read exactly once")
	t.Logf("leaves=%d buckets=%d partitions=%d allocated=%d bytes", len(presentation.Rows), totalBuckets, len(repo.matrixFilters), after.TotalAlloc-before.TotalAlloc)
	require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<30), "streamed projection allocation stays below 1 GiB")
	runtime.GC()
	var retained runtime.MemStats
	runtime.ReadMemStats(&retained)
	t.Logf("retained_heap_delta=%d bytes", int64(retained.HeapAlloc)-int64(before.HeapAlloc))
	require.Less(t, retained.HeapAlloc, before.HeapAlloc+(64<<20), "retained response projection stays below 64 MiB")
}

func TestChannelMonitorV2SyntheticBandsAndMetricCoherence(t *testing.T) {
	thresholds := DefaultChannelMonitorV2HealthThresholds()
	asOf := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	allocationsPerAtom := testing.AllocsPerRun(100, func() {
		_, _, _ = channelMonitorV2SyntheticMetricForEmptyInterval("openai", 8, "gpt-5", asOf, 5*time.Minute, 5, thresholds, nil, true)
	})
	t.Logf("synthetic_5m_allocations_per_atom=%.1f", allocationsPerAtom)
	counts := map[string]int{}
	errorRates := map[string]float64{}
	bandCounts := map[string]int{}
	for i := 0; i < 500; i++ {
		groupID := int64(i%37 + 1)
		model := fmt.Sprintf("model-%02d", i%23)
		start := asOf.Add(time.Duration(i) * 5 * time.Minute)
		desired := channelMonitorV2SyntheticTargetBand("openai", groupID, model, start)
		counts[desired]++
		bandCounts[desired]++
		metric, actualBand, ok := channelMonitorV2SyntheticMetricForBand("openai", groupID, model, start, thresholds, true)
		require.True(t, ok, "a default-threshold target should be achievable")
		require.Equal(t, desired, actualBand)
		errorRates[desired] += metric.ErrorRate
		health := ChannelMonitorV2HealthForWithThresholds(metric, thresholds)
		require.Equal(t, actualBand, channelMonitorV2BandForScore(*health.Score))
		require.Equal(t, metric.SuccessRequests+metric.ErrorRequests, metric.RequestCount)
		require.Equal(t, metric.RequestCount, metric.TTFT.SampleCount+metric.ErrorRequests)
		require.Equal(t, metric.RequestCount, metric.Duration.SampleCount)
		require.Equal(t, metric.InputTokens+metric.OutputTokens+metric.CacheCreationTokens+metric.CacheReadTokens, metric.TokenCount)
		require.Equal(t, metric.CacheReadTokens, metric.CacheRateNumerator)
		require.Equal(t, metric.InputTokens+metric.CacheCreationTokens+metric.CacheReadTokens, metric.CacheRateDenominator)
		require.InDelta(t, float64(metric.ErrorRequests)/float64(metric.RequestCount), metric.ErrorRate, .000001)
		require.InDelta(t, float64(metric.SuccessRequests)/float64(metric.RequestCount), metric.SuccessRate, .000001)
		require.InDelta(t, float64(metric.RequestCount)/5, metric.RPM, .000001)
		require.InDelta(t, float64(metric.TokenCount)/5, metric.TPM, .000001)
		require.GreaterOrEqual(t, metric.CacheRate, .8)
		require.LessOrEqual(t, metric.CacheRate, .9)
		require.NotEqual(t, 1.0, metric.CacheRate)
		require.GreaterOrEqual(t, *metric.TTFT.P50Ms, int64(1000))
		require.LessOrEqual(t, *metric.TTFT.P50Ms, int64(10000))
		require.GreaterOrEqual(t, *metric.Duration.P50Ms, int64(12000))
		require.Greater(t, *metric.Duration.P50Ms, *metric.TTFT.P95Ms)
		require.GreaterOrEqual(t, *health.Score, float64(55))
		assertOneDecimalLatencyOrder(t, metric.TTFT)
		assertOneDecimalLatencyOrder(t, metric.Duration)
		require.NotNil(t, metric.UpstreamAffectedRequests)
		require.NotNil(t, metric.UpstreamAttemptCount)
		require.LessOrEqual(t, *metric.UpstreamAffectedRequests, metric.ErrorRequests)
		require.GreaterOrEqual(t, *metric.UpstreamAttemptCount, metric.RequestCount)

		repeat, repeatBand, repeatOK := channelMonitorV2SyntheticMetricForBand("openai", groupID, model, start, thresholds, true)
		require.True(t, repeatOK)
		require.Equal(t, actualBand, repeatBand)
		require.Equal(t, metric, repeat, "identical leaf atoms remain stable")
	}
	for band, ratio := range map[string]float64{"score10": .10, "score9": .25, "score8": .20, "score7": .30, "score6": .15} {
		require.InDelta(t, ratio, float64(counts[band])/500, .05, band)
		errorRates[band] /= float64(bandCounts[band])
	}
	require.LessOrEqual(t, errorRates["score10"], errorRates["score9"])
	require.LessOrEqual(t, errorRates["score9"], errorRates["score8"])
	require.LessOrEqual(t, errorRates["score8"], errorRates["score7"])
	require.LessOrEqual(t, errorRates["score7"], errorRates["score6"])

	start := asOf.Add(24 * time.Hour)
	coarse, _, ok := channelMonitorV2SyntheticMetricForEmptyInterval("openai", 8, "gpt-5", start, time.Hour, 5, thresholds, nil, true)
	require.True(t, ok)
	children := newChannelMonitorV2MetricFold()
	for atom := start; atom.Before(start.Add(time.Hour)); atom = atom.Add(5 * time.Minute) {
		child, _, childOK := channelMonitorV2SyntheticMetricForBand("openai", 8, "gpt-5", atom, thresholds, true)
		require.True(t, childOK)
		children.add(child)
	}
	require.Equal(t, children.metric(5), coarse, "coarse empty rollups fold the same 5m child seeds")

	ignoredAll := append([]string(nil), ChannelMonitorV2ErrorCategories...)
	withoutErrors, categories, generated := channelMonitorV2SyntheticMetricForEmptyInterval("openai", 8, "gpt-5", start, 5*time.Minute, 5, thresholds, ignoredAll, false)
	if generated {
		require.Zero(t, withoutErrors.ErrorRequests, "when every error category is ignored, scoring candidates must not introduce an unclassifiable error")
		require.Empty(t, categories)
	}
	ignoredAll = nil
	for _, category := range ChannelMonitorV2ErrorCategories {
		if category != "timeout" {
			ignoredAll = append(ignoredAll, category)
		}
	}
	classified := channelMonitorV2DistributeSyntheticErrors(17, "openai", 8, "gpt-5", start, ignoredAll)
	require.Equal(t, map[string]int64{"timeout": 17}, classified, "all generated errors use one stable nonignored taxonomy category")

	extreme := thresholds
	extreme.WarningErrorRate, extreme.CriticalErrorRate = .000001, .000002
	extreme.TargetTTFTMs, extreme.CriticalTTFTMs = 100, 101
	extreme.CriticalCacheRate, extreme.WarningCacheRate = .999, 1.0
	_, extremeBand, extremeOK := channelMonitorV2SyntheticMetricForBand("openai", 8, "gpt-5", start, extreme, true)
	if extremeOK {
		bandNumber := 0
		_, scanErr := fmt.Sscanf(extremeBand, "score%d", &bandNumber)
		require.NoError(t, scanErr)
		require.GreaterOrEqual(t, bandNumber, 6, "unattainable custom thresholds may fall back only to a nonred band")
	}
}

func assertOneDecimalLatencyOrder(t *testing.T, latency ChannelMonitorV2Latency) {
	t.Helper()
	require.NotNil(t, latency.P50Ms)
	require.NotNil(t, latency.AvgMs)
	require.NotNil(t, latency.P90Ms)
	require.NotNil(t, latency.P95Ms)
	require.Less(t, float64(*latency.P50Ms)+200, *latency.AvgMs)
	require.Less(t, *latency.AvgMs+200, float64(*latency.P90Ms))
	require.Less(t, float64(*latency.P90Ms)+200, float64(*latency.P95Ms))
	require.NotEqual(t, math.Round(float64(*latency.P50Ms)/100)/10, math.Round(float64(*latency.P90Ms)/100)/10)
}

func TestChannelMonitorV2PresentationFillsTailAndPrefersTruth(t *testing.T) {
	start := time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)
	asOf := start.Add(90 * time.Minute)
	groupID := int64(3)
	truth := ChannelMonitorV2Metric{SuccessRequests: 8, ErrorRequests: 2, RequestCount: 10, ErrorRate: .2, SuccessRate: .8}
	dimensions := &ChannelMonitorV2Dimensions{
		Groups: []ChannelMonitorV2GroupDimension{{ID: groupID, Name: "primary", Platform: "openai"}},
		Models: []ChannelMonitorV2Dimension{{Value: "gpt-5", Label: "gpt-5", Platform: "openai"}},
	}
	coverage := ChannelMonitorV2Coverage{
		CoverageStart: start, DataThrough: asOf.Add(-8 * time.Minute), ComputedAt: asOf.Add(-4 * time.Minute), BucketSeconds: 300,
	}
	repo := &channelMonitorV2RepoStub{
		config: ChannelMonitorV2Config{Enabled: true, SyntheticEmptyBucketsEnabled: true, RefreshIntervalSeconds: 300,
			Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
			HealthThresholds: DefaultChannelMonitorV2HealthThresholds()},
		dimensions: dimensions,
		matrices: map[int]*ChannelMonitorV2Matrix{300: {Coverage: coverage, Items: []ChannelMonitorV2MatrixRow{{
			Platform: "openai", GroupID: &groupID, GroupName: "primary", Model: "gpt-5", Metrics: truth,
			Buckets: []ChannelMonitorV2TrendPoint{{BucketStart: start.Add(10 * time.Minute), Metrics: truth}},
		}}}},
	}
	svc := NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	filter := ChannelMonitorV2Filter{Start: start, End: asOf, AsOf: asOf, Bucket: 5 * time.Minute}
	presentation, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, nil)
	require.NoError(t, err)
	require.Len(t, presentation.Rows, 1)
	row := presentation.Rows[0]
	byStart := map[time.Time]ChannelMonitorV2TrendPoint{}
	for _, point := range row.Buckets {
		byStart[point.BucketStart] = point
	}
	require.Equal(t, truth, byStart[start.Add(10*time.Minute)].Metrics, "real facts survive unchanged")
	require.Contains(t, byStart, start.Add(85*time.Minute), "a slightly lagged latest completed atom can be filled")
	require.NotContains(t, byStart, asOf, "the current unended atom remains gray")
	require.Equal(t, coverage.DataThrough, presentation.Coverage.DataThrough, "coverage is not advanced by display projection")
	require.Greater(t, presentation.SyntheticMetrics.RequestCount, int64(0))
	repo.matrices[300].Coverage.ComputedAt = asOf.Add(-11 * time.Minute)
	stale, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, nil)
	require.NoError(t, err)
	require.Zero(t, stale.SyntheticMetrics.RequestCount, "stale aggregate watermarks cannot create display values")
	require.NotContains(t, bucketStarts(stale.Rows[0]), start.Add(15*time.Minute))
	repo.matrices[300].Coverage = coverage
	lateAtom := start.Add(85 * time.Minute)
	lateMetric := ChannelMonitorV2Metric{SuccessRequests: 15, ErrorRequests: 5, RequestCount: 20, ErrorRate: .25, SuccessRate: .75}
	repo.matrices[300].Items[0].Buckets = append(repo.matrices[300].Items[0].Buckets, ChannelMonitorV2TrendPoint{BucketStart: lateAtom, Metrics: lateMetric})
	updated, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, nil)
	require.NoError(t, err)
	updatedBucket := findPresentationBucket(t, updated.Rows[0], lateAtom)
	require.Equal(t, lateMetric, updatedBucket.Metrics, "late arriving real fact replaces the same previously projected atom")
	require.Less(t, updated.SyntheticMetrics.RequestCount, presentation.SyntheticMetrics.RequestCount)
	categoryTotal := int64(0)
	for _, count := range presentation.SyntheticErrorCategories {
		categoryTotal += count
	}
	require.Equal(t, presentation.SyntheticErrorCount, categoryTotal)

	for _, groupBy := range []ChannelMonitorV2GroupBy{
		ChannelMonitorV2GroupByPlatform,
		ChannelMonitorV2GroupByPlatformGroup,
		ChannelMonitorV2GroupByPlatformModel,
		ChannelMonitorV2GroupByPlatformGroupModel,
	} {
		matrix := channelMonitorV2PresentationMatrix(presentation, filter, groupBy, repo.config.HealthThresholds)
		if groupBy == ChannelMonitorV2GroupByPlatformGroup || groupBy == ChannelMonitorV2GroupByPlatformGroupModel {
			require.Equal(t, "primary", matrix.Items[0].GroupName)
			require.NotNil(t, matrix.Items[0].GroupID)
		} else {
			require.Empty(t, matrix.Items[0].GroupName)
			require.Nil(t, matrix.Items[0].GroupID)
		}
		fold := newChannelMonitorV2MetricFold()
		for _, item := range matrix.Items {
			fold.add(item.Metrics)
		}
		got := fold.metric(channelMonitorV2PresentationCoveredMinutes(filter, presentation.Coverage))
		require.Equal(t, presentation.SyntheticMetrics.RequestCount+truth.RequestCount, got.RequestCount, string(groupBy))
	}
}

func bucketStarts(row ChannelMonitorV2MatrixRow) []time.Time {
	starts := make([]time.Time, len(row.Buckets))
	for index, point := range row.Buckets {
		starts[index] = point.BucketStart
	}
	return starts
}

func findPresentationBucket(t *testing.T, row ChannelMonitorV2MatrixRow, start time.Time) ChannelMonitorV2TrendPoint {
	t.Helper()
	for _, point := range row.Buckets {
		if point.BucketStart.Equal(start) {
			return point
		}
	}
	t.Fatalf("presentation has no bucket at %s", start)
	return ChannelMonitorV2TrendPoint{}
}

func TestChannelMonitorV2PresentationUsesConcreteCompositePlatformsAndEmptyScopes(t *testing.T) {
	start := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	asOf := start.Add(90 * time.Minute)
	groupID := int64(11)
	repo := &channelMonitorV2RepoStub{
		config: ChannelMonitorV2Config{Enabled: true, SyntheticEmptyBucketsEnabled: true,
			Platforms: []ChannelMonitorV2PlatformConfig{
				{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}},
				{Platform: "gemini", Enabled: true, Models: []string{"gemini-3"}},
				{Platform: "composite", Enabled: true},
			}, HealthThresholds: DefaultChannelMonitorV2HealthThresholds()},
		dimensions: &ChannelMonitorV2Dimensions{
			Groups: []ChannelMonitorV2GroupDimension{
				{ID: groupID, Name: "combined", Platform: "composite", Platforms: []string{"openai", "gemini", "kiro", "openai"}},
			},
			Models: []ChannelMonitorV2Dimension{
				{Value: "gpt-5", Label: "gpt-5", Platform: "openai"},
				{Value: "gemini-3", Label: "gemini-3", Platform: "gemini"},
			},
		},
		matrix: &ChannelMonitorV2Matrix{Coverage: ChannelMonitorV2Coverage{CoverageStart: start, DataThrough: asOf, ComputedAt: asOf}},
	}
	svc := NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	filter := ChannelMonitorV2Filter{Start: start, End: asOf, AsOf: asOf, Bucket: 5 * time.Minute}
	presentation, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, nil)
	require.NoError(t, err)
	require.Len(t, presentation.Rows, 2)
	require.Equal(t, []string{"gemini", "openai"}, []string{presentation.Rows[0].Platform, presentation.Rows[1].Platform})
	for _, row := range presentation.Rows {
		require.NotEqual(t, "composite", row.Platform)
		require.NotEqual(t, "kiro", row.Platform)
	}

	selectedEmpty := repo.config
	selectedEmpty.GroupScopeMode = "selected"
	selectedEmpty.GroupIDs = []int64{}
	repo.matrixFilters = nil
	filtered, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, selectedEmpty, nil)
	require.NoError(t, err)
	require.Empty(t, filtered.Rows)
	require.Empty(t, repo.matrixFilters, "empty selected scope does not query rollups")

	noGroups := &ChannelMonitorV2Dimensions{Models: repo.dimensions.Models}
	filtered, err = svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, noGroups)
	require.NoError(t, err)
	require.Empty(t, filtered.Rows)

	unknownDimensions := &ChannelMonitorV2Dimensions{Groups: []ChannelMonitorV2GroupDimension{{ID: groupID, Name: "primary", Platform: "openai"}}}
	unknownConfig := ChannelMonitorV2Config{
		Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true}},
		HealthThresholds: DefaultChannelMonitorV2HealthThresholds(),
	}
	unknown, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, unknownConfig, unknownDimensions)
	require.NoError(t, err)
	require.Len(t, unknown.Rows, 1)
	require.Equal(t, "unknown", unknown.Rows[0].Model)
	unknownModels := channelMonitorV2PresentationModels(unknown, filter, unknownConfig.HealthThresholds)
	require.Equal(t, "unknown", unknownModels.Items[0].Model)
	channelMonitorV2PresentationAddModels(unknownDimensions, unknown)
	require.Equal(t, "unknown", unknownDimensions.Models[0].Value)
}

func TestChannelMonitorV2PresentationFiltersKeepStableLeafAtoms(t *testing.T) {
	asOf := time.Date(2026, 9, 29, 12, 34, 56, 0, time.UTC)
	svc := NewChannelMonitorV2Service(&channelMonitorV2RepoStub{})
	filter, err := svc.ParseFilterAt("90m", nil, nil, nil, asOf)
	require.NoError(t, err)
	groupOne, groupTwo := int64(1), int64(2)
	dimensions := &ChannelMonitorV2Dimensions{
		Groups: []ChannelMonitorV2GroupDimension{
			{ID: groupOne, Name: "openai", Platform: "openai"},
			{ID: groupTwo, Name: "composite", Platform: "composite", Platforms: []string{"openai", "gemini"}},
		},
		Models: []ChannelMonitorV2Dimension{
			{Value: "gpt-5", Label: "gpt-5", Platform: "openai"},
			{Value: "gemini-3", Label: "gemini-3", Platform: "gemini"},
		},
	}
	config := ChannelMonitorV2Config{
		SyntheticEmptyBucketsEnabled: true, RefreshIntervalSeconds: 300,
		Platforms: []ChannelMonitorV2PlatformConfig{
			{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}},
			{Platform: "gemini", Enabled: true, Models: []string{"gpt-5", "gemini-3"}},
		},
		HealthThresholds: DefaultChannelMonitorV2HealthThresholds(),
	}
	coverage := ChannelMonitorV2Coverage{CoverageStart: filter.Start, DataThrough: asOf, ComputedAt: asOf, BucketSeconds: 300}
	repo := &channelMonitorV2RepoStub{matrices: map[int]*ChannelMonitorV2Matrix{300: {Coverage: coverage}}}
	svc = NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	build := func(selected ChannelMonitorV2Filter) *ChannelMonitorV2Presentation {
		selected.AsOf = asOf
		selected.Start, selected.End, selected.Bucket = filter.Start, filter.End, filter.Bucket
		result, buildErr := svc.buildChannelMonitorV2Presentation(context.Background(), selected, config, dimensions)
		require.NoError(t, buildErr)
		return result
	}
	all := build(filter)
	openAI := filter
	openAI.Platforms = []string{"openai"}
	platformSubset := build(openAI)
	group := filter
	group.GroupIDs = []int64{groupTwo}
	groupSubset := build(group)
	model := filter
	model.Models = []string{"gpt-5"}
	modelSubset := build(model)
	combined := filter
	combined.Platforms, combined.GroupIDs, combined.Models = []string{"openai"}, []int64{groupTwo}, []string{"gpt-5"}
	combinedSubset := build(combined)

	require.Len(t, all.Rows, 4)
	require.Len(t, platformSubset.Rows, 2)
	require.Len(t, groupSubset.Rows, 3)
	require.Len(t, modelSubset.Rows, 3)
	require.Len(t, combinedSubset.Rows, 1)
	for _, row := range platformSubset.Rows {
		require.Equal(t, "openai", row.Platform)
	}
	leafKey := channelMonitorV2PresentationKey{platform: "openai", groupID: groupTwo, model: "gpt-5"}
	fullLeaf := findPresentationLeaf(t, all, leafKey)
	for _, subset := range []*ChannelMonitorV2Presentation{platformSubset, groupSubset, modelSubset, combinedSubset} {
		require.Equal(t, fullLeaf.Metrics, findPresentationLeaf(t, subset, leafKey).Metrics)
		require.Equal(t, fullLeaf.Buckets, findPresentationLeaf(t, subset, leafKey).Buckets)
	}
}

func findPresentationLeaf(t *testing.T, presentation *ChannelMonitorV2Presentation, key channelMonitorV2PresentationKey) ChannelMonitorV2MatrixRow {
	t.Helper()
	for _, row := range presentation.Rows {
		if row.Platform == key.platform && channelMonitorV2MatrixGroupID(row.GroupID) == key.groupID && row.Model == key.model {
			return row
		}
	}
	t.Fatalf("presentation has no leaf %+v", key)
	return ChannelMonitorV2MatrixRow{}
}

func TestChannelMonitorV2SyntheticProjectionFeedsSixUserEndpoints(t *testing.T) {
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	asOf := start.Add(90 * time.Minute)
	groupID := int64(7)
	viewerID, otherID := int64(42), int64(43)
	realMetric := ChannelMonitorV2Metric{
		SuccessRequests: 8, ErrorRequests: 2, RequestCount: 10,
		InputTokens: 100, OutputTokens: 200, CacheCreationTokens: 20, CacheReadTokens: 400, TokenCount: 720,
		ErrorRate: .2, SuccessRate: .8, CacheRate: 400.0 / 520, CacheRateNumerator: 400, CacheRateDenominator: 520,
		TTFT:     ChannelMonitorV2Latency{SampleCount: 8, P50Ms: channelMonitorV2Int64Pointer(2000), P90Ms: channelMonitorV2Int64Pointer(4000), P95Ms: channelMonitorV2Int64Pointer(4500), AvgMs: channelMonitorV2Float64Pointer(3000)},
		Duration: ChannelMonitorV2Latency{SampleCount: 10, P50Ms: channelMonitorV2Int64Pointer(12000), P90Ms: channelMonitorV2Int64Pointer(15000), P95Ms: channelMonitorV2Int64Pointer(16000), AvgMs: channelMonitorV2Float64Pointer(13500)},
	}
	coverage := ChannelMonitorV2Coverage{
		RequestedStart: start, RequestedEnd: asOf, CoverageStart: start,
		DataThrough: asOf, ComputedAt: asOf, BucketSeconds: 300, CoverageComplete: true,
	}
	dimensions := &ChannelMonitorV2Dimensions{
		Platforms: []ChannelMonitorV2Dimension{{Value: "openai", Label: "openai", RequestCount: 10}},
		Groups:    []ChannelMonitorV2GroupDimension{{ID: groupID, Name: "primary", Platform: "openai", RequestCount: 10}},
		Models:    []ChannelMonitorV2Dimension{{Value: "gpt-5", Label: "gpt-5", Platform: "openai", RequestCount: 10}},
	}
	makeRepository := func() *channelMonitorV2RepoStub {
		return &channelMonitorV2RepoStub{
			config: ChannelMonitorV2Config{
				Enabled: true, SyntheticEmptyBucketsEnabled: true, RefreshIntervalSeconds: 300,
				Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
				HealthThresholds: DefaultChannelMonitorV2HealthThresholds(),
			},
			dimensions: dimensions,
			matrices: map[int]*ChannelMonitorV2Matrix{300: {Coverage: coverage, Items: []ChannelMonitorV2MatrixRow{{
				Platform: "openai", GroupID: &groupID, GroupName: "primary", Model: "gpt-5", Metrics: realMetric,
				Buckets: []ChannelMonitorV2TrendPoint{{BucketStart: start, Metrics: realMetric}},
			}}}},
			errors: &ChannelMonitorV2List[ChannelMonitorV2ErrorRow]{Coverage: coverage, Items: []ChannelMonitorV2ErrorRow{{
				Category: "timeout", Count: 2, Rate: 1, Ignored: false,
				Details: []ChannelMonitorV2ErrorDetail{{Platform: "openai", Message: "real detail", Count: 2}},
			}}},
			users: &ChannelMonitorV2List[ChannelMonitorV2UserRow]{Coverage: coverage, Items: []ChannelMonitorV2UserRow{
				{UserID: &otherID, Rank: 1, DisplayLabel: "other", Metrics: ChannelMonitorV2Metric{SuccessRequests: 50, RequestCount: 50}},
				{UserID: &viewerID, Rank: 2, DisplayLabel: "viewer", Metrics: ChannelMonitorV2Metric{SuccessRequests: 2, RequestCount: 2}},
			}},
			snap:   &ChannelMonitorV2Snapshot{Config: ChannelMonitorV2Config{Enabled: true}, Coverage: coverage, Metrics: realMetric, Trend: []ChannelMonitorV2TrendPoint{}},
			matrix: &ChannelMonitorV2Matrix{GroupBy: ChannelMonitorV2GroupByPlatformGroupModel, Coverage: coverage, Items: []ChannelMonitorV2MatrixRow{{Platform: "openai", GroupID: &groupID, GroupName: "primary", Model: "gpt-5", Metrics: realMetric}}},
		}
	}
	realErrorCount := int64(2)
	filter := ChannelMonitorV2Filter{Range: "90m", Start: start, End: asOf, AsOf: asOf, Bucket: 5 * time.Minute}

	repo := makeRepository()
	svc := NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	svc.SetRuntimeReader(channelMonitorV2RuntimeStub{})
	snapshot, err := svc.Snapshot(context.Background(), filter, false)
	require.NoError(t, err)
	matrix, err := svc.Matrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel, false)
	require.NoError(t, err)
	models, err := svc.Models(context.Background(), filter, false)
	require.NoError(t, err)
	errorsList, err := svc.ErrorsForViewer(context.Background(), filter, false)
	require.NoError(t, err)
	errorPresentation, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, repo.config, nil)
	require.NoError(t, err)
	expectedErrorTotal := realErrorCount + errorPresentation.SyntheticErrorCount
	users, err := svc.Users(context.Background(), filter, viewerID, false)
	require.NoError(t, err)
	dims, err := svc.Dimensions(context.Background(), filter, false)
	require.NoError(t, err)

	require.Equal(t, matrix.Coverage.RequestedStart, snapshot.Coverage.RequestedStart)
	require.Equal(t, matrix.Coverage.RequestedEnd, snapshot.Coverage.RequestedEnd)
	require.Equal(t, matrix.Items[0].Metrics.RequestCount, snapshot.Metrics.RequestCount)
	require.Equal(t, matrix.Items[0].Buckets, snapshot.Trend)
	require.Len(t, models.Items, 1)
	require.Equal(t, matrix.Items[0].Metrics.RequestCount, models.Items[0].Metrics.RequestCount)
	require.Zero(t, models.Items[0].Metrics.RequestCount, "public absolute model totals remain redacted")
	require.GreaterOrEqual(t, len(dims.Models), 1)
	RedactChannelMonitorV2Dimensions(dims)
	require.Zero(t, dims.Models[0].RequestCount, "service caller can apply the existing public dimension redaction")
	var rateTotal float64
	for _, row := range errorsList.Items {
		require.Zero(t, row.Count)
		require.Empty(t, row.Details)
		expectedCount := errorPresentation.SyntheticErrorCategories[row.Category]
		if row.Category == "timeout" {
			expectedCount += realErrorCount
		}
		require.InDelta(t, float64(expectedCount)/float64(expectedErrorTotal), row.Rate, .000001, row.Category)
		rateTotal += row.Rate
	}
	require.InDelta(t, 1.0, rateTotal, .000001)
	require.NotEmpty(t, users.Items)
	require.True(t, users.Items[0].IsSelf)
	require.Equal(t, 1, users.Items[0].Rank)
	require.NotEqual(t, "other", users.Items[0].DisplayLabel)
	require.Greater(t, users.Items[0].Metrics.RPM, 0.0, "throughput remains governed by the runtime privacy policy")

	encoded, err := json.Marshal(struct {
		Snapshot *ChannelMonitorV2Snapshot
		Matrix   *ChannelMonitorV2Matrix
		Models   *ChannelMonitorV2List[ChannelMonitorV2ModelRow]
		Errors   *ChannelMonitorV2List[ChannelMonitorV2ErrorRow]
		Users    *ChannelMonitorV2List[ChannelMonitorV2UserRow]
	}{snapshot, matrix, models, errorsList, users})
	require.NoError(t, err)
	require.NotContains(t, strings.ToLower(string(encoded)), "synthetic")
	require.NotContains(t, strings.ToLower(string(encoded)), "is_synthetic")
	require.False(t, snapshot.Config.SyntheticEmptyBucketsEnabled)

	adminRepo := makeRepository()
	adminService := NewChannelMonitorV2Service(adminRepo)
	adminService.now = func() time.Time { return asOf }
	adminDimensions, err := adminService.Dimensions(context.Background(), filter, true)
	require.NoError(t, err)
	adminSnapshot, err := adminService.Snapshot(context.Background(), filter, true)
	require.NoError(t, err)
	adminModels, err := adminService.Models(context.Background(), filter, true)
	require.NoError(t, err)
	adminMatrix, err := adminService.Matrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel, true)
	require.NoError(t, err)
	adminErrors, err := adminService.ErrorsForViewer(context.Background(), filter, true)
	require.NoError(t, err)
	adminUsers, err := adminService.Users(context.Background(), filter, viewerID, true)
	require.NoError(t, err)
	require.Len(t, adminDimensions.Models, 1, "admin dimensions stay repository truth")
	require.Equal(t, realMetric, adminSnapshot.Metrics)
	require.Nil(t, adminModels)
	require.Equal(t, realMetric, adminMatrix.Items[0].Metrics)
	require.Equal(t, int64(2), adminErrors.Items[0].Count)
	require.Len(t, adminErrors.Items[0].Details, 1)
	require.Equal(t, 2, adminUsers.Items[1].Rank)
	require.Equal(t, 1, adminRepo.dimensionCalls)
	require.Len(t, adminRepo.matrixFilters, 1, "admin matrix reads one true matrix without projection rollups")

	presentationRepo := makeRepository()
	presentationService := NewChannelMonitorV2Service(presentationRepo)
	presentationService.now = func() time.Time { return asOf }
	presentationDimensions, err := presentationService.PresentationDimensions(context.Background(), filter)
	require.NoError(t, err)
	presentationSnapshot, err := presentationService.PresentationSnapshot(context.Background(), filter)
	require.NoError(t, err)
	presentationModels, err := presentationService.PresentationModels(context.Background(), filter)
	require.NoError(t, err)
	presentationMatrix, err := presentationService.PresentationMatrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel)
	require.NoError(t, err)
	presentationErrors, err := presentationService.PresentationErrors(context.Background(), filter)
	require.NoError(t, err)
	presentationUsers, err := presentationService.PresentationUsers(context.Background(), filter, viewerID)
	require.NoError(t, err)

	require.Len(t, presentationDimensions.Models, 1)
	require.Greater(t, presentationSnapshot.Metrics.RequestCount, adminSnapshot.Metrics.RequestCount)
	require.NotEmpty(t, presentationModels.Items)
	require.NotEmpty(t, presentationMatrix.Items)
	require.NotEmpty(t, presentationErrors.Items)
	var presentedTimeout *ChannelMonitorV2ErrorRow
	for i := range presentationErrors.Items {
		if presentationErrors.Items[i].Category == "timeout" {
			presentedTimeout = &presentationErrors.Items[i]
			break
		}
	}
	require.NotNil(t, presentedTimeout)
	require.NotEmpty(t, presentedTimeout.Details, "admin presentation keeps raw error details")
	presentedErrorTotal := int64(0)
	for _, row := range presentationErrors.Items {
		presentedErrorTotal += row.Count
	}
	rawErrorTotal := int64(0)
	for _, row := range adminErrors.Items {
		rawErrorTotal += row.Count
	}
	require.Greater(t, presentedErrorTotal, rawErrorTotal, "admin presentation merges projected error categories")
	require.NotEmpty(t, presentationUsers.Items)
	for _, row := range presentationUsers.Items {
		require.NotNil(t, row.UserID, "admin presentation keeps user identity")
	}
}

func TestChannelMonitorV2AdminPresentationDisabledUsesRawAdminResponses(t *testing.T) {
	groupID, viewerID, otherID := int64(7), int64(42), int64(43)
	realMetric := ChannelMonitorV2Metric{SuccessRequests: 8, ErrorRequests: 2, RequestCount: 10, ErrorRate: .2, SuccessRate: .8}
	coverage := ChannelMonitorV2Coverage{RequestedStart: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC), RequestedEnd: time.Date(2026, 9, 29, 11, 30, 0, 0, time.UTC), BucketSeconds: 300}
	newRepo := func() *channelMonitorV2RepoStub {
		return &channelMonitorV2RepoStub{
			config:     ChannelMonitorV2Config{Enabled: true, SyntheticEmptyBucketsEnabled: false, RefreshIntervalSeconds: 300, HealthThresholds: DefaultChannelMonitorV2HealthThresholds()},
			dimensions: &ChannelMonitorV2Dimensions{Models: []ChannelMonitorV2Dimension{{Value: "gpt-5", Platform: "openai"}}},
			snap:       &ChannelMonitorV2Snapshot{Coverage: coverage, Metrics: realMetric},
			matrix:     &ChannelMonitorV2Matrix{Coverage: coverage, Items: []ChannelMonitorV2MatrixRow{{Platform: "openai", GroupID: &groupID, Metrics: realMetric, Buckets: []ChannelMonitorV2TrendPoint{{Metrics: realMetric}}}}},
			errors:     &ChannelMonitorV2List[ChannelMonitorV2ErrorRow]{Coverage: coverage, Items: []ChannelMonitorV2ErrorRow{{Category: "timeout", Count: 2, Rate: 1, Details: []ChannelMonitorV2ErrorDetail{{Count: 2}}}}},
			users:      &ChannelMonitorV2List[ChannelMonitorV2UserRow]{Coverage: coverage, Items: []ChannelMonitorV2UserRow{{UserID: &otherID, Rank: 1, DisplayLabel: "other"}, {UserID: &viewerID, Rank: 2, DisplayLabel: "viewer"}}},
		}
	}
	filter := ChannelMonitorV2Filter{Range: "90m", Start: coverage.RequestedStart, End: coverage.RequestedEnd, AsOf: coverage.RequestedEnd, Bucket: 5 * time.Minute}
	repo := newRepo()
	svc := NewChannelMonitorV2Service(repo)

	dimensions, err := svc.PresentationDimensions(context.Background(), filter)
	require.NoError(t, err)
	snapshot, err := svc.PresentationSnapshot(context.Background(), filter)
	require.NoError(t, err)
	models, err := svc.PresentationModels(context.Background(), filter)
	require.NoError(t, err)
	matrix, err := svc.PresentationMatrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel)
	require.NoError(t, err)
	errorsList, err := svc.PresentationErrors(context.Background(), filter)
	require.NoError(t, err)
	users, err := svc.PresentationUsers(context.Background(), filter, viewerID)
	require.NoError(t, err)

	require.Len(t, dimensions.Models, 1)
	require.Equal(t, realMetric, snapshot.Metrics)
	require.Nil(t, models)
	require.Equal(t, realMetric, matrix.Items[0].Metrics)
	require.Equal(t, int64(2), errorsList.Items[0].Count)
	require.Len(t, errorsList.Items[0].Details, 1)
	require.Len(t, users.Items, 2)
	require.Equal(t, otherID, *users.Items[0].UserID)
	require.True(t, repo.admin)
}

func TestChannelMonitorV2SyntheticProjectionDisabledPreservesRepositoryResponses(t *testing.T) {
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	asOf := start.Add(90 * time.Minute)
	groupID, viewerID := int64(7), int64(42)
	otherID := int64(43)
	realMetric := ChannelMonitorV2Metric{SuccessRequests: 8, ErrorRequests: 2, RequestCount: 10, ErrorRate: .2, SuccessRate: .8}
	coverage := ChannelMonitorV2Coverage{RequestedStart: start, RequestedEnd: asOf, CoverageStart: start, DataThrough: asOf, ComputedAt: asOf, BucketSeconds: 300}
	repo := &channelMonitorV2RepoStub{
		config: ChannelMonitorV2Config{Enabled: true, SyntheticEmptyBucketsEnabled: false, RefreshIntervalSeconds: 300,
			Platforms:        []ChannelMonitorV2PlatformConfig{{Platform: "openai", Enabled: true, Models: []string{"gpt-5"}}},
			HealthThresholds: DefaultChannelMonitorV2HealthThresholds()},
		dimensions: &ChannelMonitorV2Dimensions{
			Platforms: []ChannelMonitorV2Dimension{{Value: "openai", Label: "openai"}},
			Groups:    []ChannelMonitorV2GroupDimension{{ID: groupID, Name: "primary", Platform: "openai"}},
			Models:    []ChannelMonitorV2Dimension{{Value: "gpt-5", Label: "gpt-5", Platform: "openai"}},
		},
		snap: &ChannelMonitorV2Snapshot{Coverage: coverage, Metrics: realMetric},
		matrix: &ChannelMonitorV2Matrix{Coverage: coverage, Items: []ChannelMonitorV2MatrixRow{{
			Platform: "openai", GroupID: &groupID, Model: "gpt-5", Metrics: realMetric,
			Buckets: []ChannelMonitorV2TrendPoint{{BucketStart: start, Metrics: realMetric}},
		}}},
		errors: &ChannelMonitorV2List[ChannelMonitorV2ErrorRow]{Coverage: coverage, Items: []ChannelMonitorV2ErrorRow{{Category: "timeout", Count: 2, Rate: 1}}},
		users: &ChannelMonitorV2List[ChannelMonitorV2UserRow]{Coverage: coverage, Items: []ChannelMonitorV2UserRow{
			{UserID: &otherID, Rank: 1, DisplayLabel: "other", Metrics: ChannelMonitorV2Metric{RequestCount: 50}},
			{UserID: &viewerID, Rank: 2, DisplayLabel: "viewer", Metrics: ChannelMonitorV2Metric{RequestCount: 2}},
		}},
	}
	svc := NewChannelMonitorV2Service(repo)
	svc.now = func() time.Time { return asOf }
	svc.SetRuntimeReader(channelMonitorV2RuntimeStub{})
	filter := ChannelMonitorV2Filter{Range: "90m", Start: start, End: asOf, AsOf: asOf, Bucket: 5 * time.Minute}

	snapshot, err := svc.Snapshot(context.Background(), filter, false)
	require.NoError(t, err)
	require.Equal(t, realMetric.ErrorRate, snapshot.Metrics.ErrorRate)
	matrix, err := svc.Matrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroupModel, false)
	require.NoError(t, err)
	require.Len(t, matrix.Items, 1)
	require.Len(t, matrix.Items[0].Buckets, 1, "disabled matrix response keeps the repository's original bucket set")
	models, err := svc.Models(context.Background(), filter, false)
	require.NoError(t, err)
	require.Nil(t, models, "disabled models response follows the repository path")
	errorsList, err := svc.ErrorsForViewer(context.Background(), filter, false)
	require.NoError(t, err)
	require.Len(t, errorsList.Items, 1)
	require.Equal(t, 1.0, errorsList.Items[0].Rate)
	users, err := svc.Users(context.Background(), filter, viewerID, false)
	require.NoError(t, err)
	require.Equal(t, "Other user #1", users.Items[0].DisplayLabel)
	require.Equal(t, 2, users.Items[1].Rank)
	dims, err := svc.Dimensions(context.Background(), filter, false)
	require.NoError(t, err)
	require.Len(t, dims.Models, 1, "disabled dimensions do not add projected categories")
	require.Len(t, repo.matrixFilters, 1, "only the direct matrix endpoint reads the repository matrix")
	require.Equal(t, 1, repo.dimensionCalls, "only the direct dimensions endpoint loads the dimensions catalog")
}
