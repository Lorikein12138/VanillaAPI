package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

type ChannelMonitorV2Presentation struct {
	Coverage                 ChannelMonitorV2Coverage
	Rows                     []ChannelMonitorV2MatrixRow
	SyntheticMetrics         ChannelMonitorV2Metric
	SyntheticErrorCount      int64
	SyntheticErrorCategories map[string]int64
}

type channelMonitorV2PresentationKey struct {
	platform string
	groupID  int64
	model    string
}

type channelMonitorV2PresentationRow struct {
	platform  string
	groupID   int64
	groupName string
	model     string
	total     *channelMonitorV2MetricFold
	buckets   map[int64]*channelMonitorV2MetricFold
}

type channelMonitorV2PresentationPartition struct {
	start  time.Time
	end    time.Time
	bucket time.Duration
}

// buildChannelMonitorV2Presentation folds existing rollup truth and stable
// response-only samples into the requested display buckets. It never writes.
func (s *ChannelMonitorV2Service) buildChannelMonitorV2Presentation(
	ctx context.Context,
	filter ChannelMonitorV2Filter,
	cfg ChannelMonitorV2Config,
	dimensions *ChannelMonitorV2Dimensions,
) (*ChannelMonitorV2Presentation, error) {
	result := &ChannelMonitorV2Presentation{
		Coverage: ChannelMonitorV2Coverage{
			RequestedStart: filter.Start,
			RequestedEnd:   filter.End,
			BucketSeconds:  int(filter.Bucket.Seconds()),
		},
		Rows:                     []ChannelMonitorV2MatrixRow{},
		SyntheticErrorCategories: map[string]int64{},
	}
	if channelMonitorV2HasEmptyGroupScope(filter, cfg) {
		return result, nil
	}
	if dimensions == nil {
		catalogFilter := filter
		catalogFilter.Platforms = nil
		catalogFilter.GroupIDs = nil
		catalogFilter.Models = nil
		var err error
		dimensions, err = s.repo.GetDimensions(ctx, catalogFilter, cfg)
		if err != nil {
			return nil, fmt.Errorf("load channel monitor v2 presentation dimensions: %w", err)
		}
	}
	if dimensions == nil {
		return nil, fmt.Errorf("load channel monitor v2 presentation dimensions: empty response")
	}

	universe := channelMonitorV2PresentationUniverse(filter, cfg, dimensions)
	if len(universe) == 0 {
		return result, nil
	}
	partitions := channelMonitorV2PresentationPartitions(filter)
	if len(partitions) == 0 {
		return result, nil
	}
	rows := make(map[channelMonitorV2PresentationKey]*channelMonitorV2PresentationRow, len(universe))
	for _, key := range universe {
		rows[key] = &channelMonitorV2PresentationRow{
			platform: key.platform, groupID: key.groupID,
			groupName: channelMonitorV2PresentationGroupName(dimensions, key.groupID),
			model:     key.model, total: newChannelMonitorV2MetricFold(), buckets: map[int64]*channelMonitorV2MetricFold{},
		}
	}
	synthetic := newChannelMonitorV2MetricFold()
	categoryCounts := map[string]int64{}
	ignoredCategories := channelMonitorV2IgnoredCategorySet(cfg.IgnoredErrorCategories)
	allowSyntheticErrors := channelMonitorV2HasAvailableErrorCategory(ignoredCategories)

	for partitionIndex, partition := range partitions {
		partFilter := filter
		partFilter.Start, partFilter.End, partFilter.Bucket = partition.start, partition.end, partition.bucket
		matrix, err := s.repo.GetMatrix(ctx, partFilter, cfg, ChannelMonitorV2GroupByPlatformGroupModel, false)
		if err != nil {
			return nil, fmt.Errorf("load channel monitor v2 presentation rollup (%s): %w", partition.bucket, err)
		}
		if matrix == nil {
			return nil, fmt.Errorf("load channel monitor v2 presentation rollup (%s): empty response", partition.bucket)
		}
		channelMonitorV2MergePresentationCoverage(&result.Coverage, matrix.Coverage, filter, partitionIndex == 0, partitionIndex == len(partitions)-1)
		truthTotals, truth := channelMonitorV2PresentationTruthByLeaf(matrix)
		for _, key := range universe {
			row := rows[key]
			if metric, ok := truthTotals[key]; ok && channelMonitorV2MetricHasTruth(metric) {
				row.total.add(metric)
			}
			trueAtoms := truth[key]
			first := channelMonitorV2CeilTime(partition.start, partition.bucket)
			for atomStart := first; atomStart.Before(partition.end); atomStart = atomStart.Add(partition.bucket) {
				atomEnd := atomStart.Add(partition.bucket)
				if atomStart.Before(filter.Start) || !atomStart.Before(filter.End) || !atomStart.Before(filter.AsOf) {
					continue
				}
				metric, isTrue := trueAtoms[atomStart.UnixNano()]
				if atomEnd.After(filter.AsOf) && (!isTrue || filter.AsOf.Before(s.now().UTC().Add(-time.Minute))) {
					continue
				}
				if isTrue {
					bucketFold := channelMonitorV2PresentationBucketFold(row, channelMonitorV2BucketStart(atomStart, filter.Bucket))
					bucketFold.add(metric)
					continue
				}
				if !channelMonitorV2PresentationMayFill(matrix.Coverage, atomStart, atomEnd, partition.bucket, filter.AsOf, cfg.RefreshIntervalSeconds) {
					continue
				}
				for childStart := atomStart.UTC(); childStart.Before(atomEnd); childStart = childStart.Add(5 * time.Minute) {
					childEnd := childStart.Add(5 * time.Minute)
					if childStart.Before(filter.Start) || !childStart.Before(filter.End) || childEnd.After(filter.AsOf) {
						continue
					}
					raw, _, generated := channelMonitorV2SyntheticRawForBand(
						key.platform, key.groupID, key.model, childStart, cfg.HealthThresholds,
						allowSyntheticErrors,
					)
					if !generated {
						continue
					}
					synthetic.addSyntheticRaw(raw)
					row.total.addSyntheticRaw(raw)
					bucketFold := channelMonitorV2PresentationBucketFold(row, channelMonitorV2BucketStart(childStart, filter.Bucket))
					bucketFold.addSyntheticRaw(raw)
					if category, ok := channelMonitorV2SyntheticErrorCategoryFromSet(raw.errors, key.platform, key.groupID, key.model, childStart, ignoredCategories); ok {
						categoryCounts[category] = channelMonitorV2SaturatingAdd(categoryCounts[category], raw.errors)
					}
				}
			}
		}
	}

	minutes := channelMonitorV2PresentationCoveredMinutes(filter, result.Coverage)
	rowsList := make([]ChannelMonitorV2MatrixRow, 0, len(rows))
	for _, key := range universe {
		row := rows[key]
		bucketKeys := make([]int64, 0, len(row.buckets))
		for bucketKey := range row.buckets {
			bucketKeys = append(bucketKeys, bucketKey)
		}
		sort.Slice(bucketKeys, func(i, j int) bool { return bucketKeys[i] < bucketKeys[j] })
		buckets := make([]ChannelMonitorV2TrendPoint, 0, len(bucketKeys))
		for _, bucketKey := range bucketKeys {
			bucketMetrics := row.buckets[bucketKey].metric(filter.Bucket.Minutes())
			bucketStart := time.Unix(0, bucketKey).UTC()
			buckets = append(buckets, ChannelMonitorV2TrendPoint{
				BucketStart: bucketStart,
				Metrics:     bucketMetrics,
				Health:      ChannelMonitorV2HealthForWithThresholds(bucketMetrics, cfg.HealthThresholds),
			})
		}
		metrics := row.total.metric(minutes)
		groupID := row.groupID
		rowsList = append(rowsList, ChannelMonitorV2MatrixRow{
			Platform: row.platform, GroupID: &groupID, GroupName: row.groupName, Model: row.model,
			Metrics: metrics, Health: ChannelMonitorV2HealthForWithThresholds(metrics, cfg.HealthThresholds), Buckets: buckets,
		})
	}
	sort.Slice(rowsList, func(i, j int) bool {
		a, b := rowsList[i], rowsList[j]
		if a.Platform != b.Platform {
			return a.Platform < b.Platform
		}
		if a.GroupName != b.GroupName {
			return a.GroupName < b.GroupName
		}
		if channelMonitorV2MatrixGroupID(a.GroupID) != channelMonitorV2MatrixGroupID(b.GroupID) {
			return channelMonitorV2MatrixGroupID(a.GroupID) < channelMonitorV2MatrixGroupID(b.GroupID)
		}
		return a.Model < b.Model
	})
	result.Rows = rowsList
	result.SyntheticMetrics = synthetic.metric(minutes)
	result.SyntheticErrorCount = result.SyntheticMetrics.ErrorRequests
	result.SyntheticErrorCategories = categoryCounts
	return result, nil
}

func channelMonitorV2PresentationBucketFold(row *channelMonitorV2PresentationRow, bucketStart time.Time) *channelMonitorV2MetricFold {
	key := bucketStart.UTC().UnixNano()
	fold := row.buckets[key]
	if fold == nil {
		fold = newChannelMonitorV2MetricFold()
		row.buckets[key] = fold
	}
	return fold
}

func channelMonitorV2PresentationUniverse(filter ChannelMonitorV2Filter, cfg ChannelMonitorV2Config, dimensions *ChannelMonitorV2Dimensions) []channelMonitorV2PresentationKey {
	enabled := map[string]struct{}{}
	for _, platform := range domain.SupportedGroupPlatforms() {
		for _, item := range cfg.Platforms {
			if item.Platform == platform && item.Enabled && (len(filter.Platforms) == 0 || channelMonitorV2PresentationContains(filter.Platforms, platform)) {
				enabled[platform] = struct{}{}
				break
			}
		}
	}
	modelsByPlatform := map[string]map[string]struct{}{}
	for _, model := range dimensions.Models {
		platform, modelName := strings.ToLower(strings.TrimSpace(model.Platform)), strings.TrimSpace(model.Value)
		if _, ok := enabled[platform]; !ok || modelName == "" {
			continue
		}
		if len(filter.Models) > 0 && !channelMonitorV2PresentationContains(filter.Models, modelName) {
			continue
		}
		if modelsByPlatform[platform] == nil {
			modelsByPlatform[platform] = map[string]struct{}{}
		}
		modelsByPlatform[platform][modelName] = struct{}{}
	}
	for _, item := range cfg.Platforms {
		if _, ok := enabled[item.Platform]; !ok {
			continue
		}
		for _, model := range item.Models {
			model = strings.TrimSpace(model)
			if model == "" || (len(filter.Models) > 0 && !channelMonitorV2PresentationContains(filter.Models, model)) {
				continue
			}
			if modelsByPlatform[item.Platform] == nil {
				modelsByPlatform[item.Platform] = map[string]struct{}{}
			}
			modelsByPlatform[item.Platform][model] = struct{}{}
		}
	}
	keys := make([]channelMonitorV2PresentationKey, 0)
	for _, group := range dimensions.Groups {
		if group.ID <= 0 || (len(filter.GroupIDs) > 0 && !containsInt64(filter.GroupIDs, group.ID)) {
			continue
		}
		platforms := []string{group.Platform}
		if strings.EqualFold(group.Platform, domain.PlatformComposite) {
			platforms = append([]string(nil), group.Platforms...)
		}
		platforms = normalizePresentationPlatforms(platforms, enabled)
		for _, platform := range platforms {
			models := modelsByPlatform[platform]
			if len(models) == 0 && (len(filter.Models) == 0 || channelMonitorV2PresentationContains(filter.Models, "unknown")) {
				models = map[string]struct{}{"unknown": {}}
			}
			for model := range models {
				keys = append(keys, channelMonitorV2PresentationKey{platform: platform, groupID: group.ID, model: model})
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.platform != b.platform {
			return a.platform < b.platform
		}
		if a.groupID != b.groupID {
			return a.groupID < b.groupID
		}
		return a.model < b.model
	})
	return keys
}

func channelMonitorV2PresentationMinTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func channelMonitorV2PresentationMaxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func normalizePresentationPlatforms(platforms []string, enabled map[string]struct{}) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(platforms))
	for _, platform := range platforms {
		platform = strings.ToLower(strings.TrimSpace(platform))
		if _, ok := enabled[platform]; !ok {
			continue
		}
		if _, ok := seen[platform]; ok {
			continue
		}
		seen[platform] = struct{}{}
		result = append(result, platform)
	}
	sort.Strings(result)
	return result
}

func channelMonitorV2PresentationGroupName(dimensions *ChannelMonitorV2Dimensions, groupID int64) string {
	for _, group := range dimensions.Groups {
		if group.ID == groupID {
			return group.Name
		}
	}
	return ""
}

func channelMonitorV2PresentationPartitions(filter ChannelMonitorV2Filter) []channelMonitorV2PresentationPartition {
	if !filter.Start.Before(filter.End) || filter.Bucket < time.Minute {
		return nil
	}
	anchor := filter.AsOf
	if anchor.IsZero() {
		anchor = filter.End
	}
	day := anchor.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	bound6 := day.Add(-6 * 24 * time.Hour)
	bound29 := day.Add(-29 * 24 * time.Hour)
	parts := []channelMonitorV2PresentationPartition{
		{start: filter.Start, end: channelMonitorV2PresentationMinTime(filter.End, bound29), bucket: 12 * time.Hour},
		{start: channelMonitorV2PresentationMaxTime(filter.Start, bound29), end: channelMonitorV2PresentationMinTime(filter.End, bound6), bucket: time.Hour},
		{start: channelMonitorV2PresentationMaxTime(filter.Start, bound6), end: filter.End, bucket: 5 * time.Minute},
	}
	result := make([]channelMonitorV2PresentationPartition, 0, len(parts))
	for _, part := range parts {
		if part.start.Before(part.end) {
			result = append(result, part)
		}
	}
	return result
}

func channelMonitorV2MergePresentationCoverage(dst *ChannelMonitorV2Coverage, source ChannelMonitorV2Coverage, filter ChannelMonitorV2Filter, first, last bool) {
	if dst == nil {
		return
	}
	dst.RequestedStart, dst.RequestedEnd = filter.Start, filter.End
	dst.BucketSeconds = int(filter.Bucket.Seconds())
	if first {
		dst.CoverageStart = source.CoverageStart
		dst.CoverageComplete = source.CoverageComplete
	}
	if last {
		dst.DataThrough = source.DataThrough
		dst.ComputedAt = source.ComputedAt
		dst.AggregationLagSeconds = source.AggregationLagSeconds
		dst.Bootstrap = source.Bootstrap
	}
}

func channelMonitorV2PresentationTruthByLeaf(matrix *ChannelMonitorV2Matrix) (map[channelMonitorV2PresentationKey]ChannelMonitorV2Metric, map[channelMonitorV2PresentationKey]map[int64]ChannelMonitorV2Metric) {
	totals := map[channelMonitorV2PresentationKey]ChannelMonitorV2Metric{}
	truth := map[channelMonitorV2PresentationKey]map[int64]ChannelMonitorV2Metric{}
	if matrix == nil {
		return totals, truth
	}
	for _, row := range matrix.Items {
		if row.GroupID == nil || *row.GroupID <= 0 {
			continue
		}
		model := row.Model
		if model == "" {
			model = "unknown"
		}
		key := channelMonitorV2PresentationKey{platform: row.Platform, groupID: *row.GroupID, model: model}
		if channelMonitorV2MetricHasTruth(row.Metrics) {
			totals[key] = row.Metrics
		}
		for _, bucket := range row.Buckets {
			if !channelMonitorV2MetricHasTruth(bucket.Metrics) {
				continue
			}
			if truth[key] == nil {
				truth[key] = map[int64]ChannelMonitorV2Metric{}
			}
			truth[key][bucket.BucketStart.UTC().UnixNano()] = bucket.Metrics
		}
	}
	return totals, truth
}

func channelMonitorV2MetricHasTruth(metric ChannelMonitorV2Metric) bool {
	return metric.RequestCount != 0 || metric.SuccessRequests != 0 || metric.ErrorRequests != 0 ||
		metric.InputTokens != 0 || metric.OutputTokens != 0 || metric.CacheCreationTokens != 0 || metric.CacheReadTokens != 0 ||
		metric.TTFT.SampleCount != 0 || metric.Duration.SampleCount != 0
}

func channelMonitorV2PresentationMayFill(coverage ChannelMonitorV2Coverage, start, end time.Time, sourceBucket time.Duration, asOf time.Time, refreshSeconds int) bool {
	if coverage.CoverageStart.IsZero() || coverage.ComputedAt.IsZero() || coverage.DataThrough.IsZero() ||
		coverage.CoverageStart.After(start) || end.After(asOf) {
		return false
	}
	refresh := time.Duration(refreshSeconds) * time.Second
	if refresh < time.Second {
		refresh = 300 * time.Second
	}
	freshness := 2 * refresh
	if freshness < 5*time.Minute {
		freshness = 5 * time.Minute
	}
	if asOf.Sub(coverage.ComputedAt) > freshness {
		return false
	}
	if !end.After(coverage.DataThrough) {
		return true
	}
	if sourceBucket != 5*time.Minute {
		return false
	}
	latestCompleted := asOf.UTC().Truncate(5 * time.Minute).Add(-5 * time.Minute)
	return start.Equal(latestCompleted) && !coverage.DataThrough.Before(start.Add(-5*time.Minute))
}

func channelMonitorV2CeilTime(value time.Time, step time.Duration) time.Time {
	value = value.UTC()
	truncated := value.Truncate(step)
	if truncated.Before(value) {
		return truncated.Add(step)
	}
	return truncated
}

func channelMonitorV2BucketStart(value time.Time, step time.Duration) time.Time {
	if step <= 0 {
		return value.UTC()
	}
	return value.UTC().Truncate(step)
}

func channelMonitorV2PresentationCoveredMinutes(filter ChannelMonitorV2Filter, coverage ChannelMonitorV2Coverage) float64 {
	start, end := filter.Start, filter.End
	if !coverage.CoverageStart.IsZero() && coverage.CoverageStart.After(start) {
		start = coverage.CoverageStart
	}
	if !coverage.DataThrough.IsZero() && coverage.DataThrough.Before(end) {
		end = coverage.DataThrough
	}
	if !start.Before(end) {
		return 1
	}
	return end.Sub(start).Minutes()
}

func channelMonitorV2PresentationIgnoredCategorySet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func channelMonitorV2IgnoredCategorySet(values []string) map[string]struct{} {
	return channelMonitorV2PresentationIgnoredCategorySet(values)
}

func channelMonitorV2HasAvailableErrorCategory(ignored map[string]struct{}) bool {
	for _, category := range ChannelMonitorV2ErrorCategories {
		if _, skip := ignored[category]; !skip {
			return true
		}
	}
	return false
}

func channelMonitorV2SaturatingAdd(left, right int64) int64 {
	if right > 0 && left > math.MaxInt64-right {
		return math.MaxInt64
	}
	if right < 0 && left < math.MinInt64-right {
		return math.MinInt64
	}
	return left + right
}

func channelMonitorV2PresentationContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func channelMonitorV2SyntheticTargetBand(platform string, groupID int64, model string, bucketStart time.Time) string {
	hash := channelMonitorV2PresentationHash(platform, groupID, model, bucketStart)
	return channelMonitorV2SyntheticTargetBandFromHash(hash)
}

func channelMonitorV2SyntheticTargetBandFromHash(hash [32]byte) string {
	roll := binary.BigEndian.Uint64(hash[:8]) % 10000
	switch {
	case roll < 1000:
		return "score10"
	case roll < 3500:
		return "score9"
	case roll < 5500:
		return "score8"
	case roll < 8500:
		return "score7"
	default:
		return "score6"
	}
}

func channelMonitorV2PresentationHash(platform string, groupID int64, model string, bucketStart time.Time) [32]byte {
	var seedBuffer [512]byte
	seed := seedBuffer[:0]
	seed = append(seed, platform...)
	seed = append(seed, 0)
	seed = strconv.AppendInt(seed, groupID, 10)
	seed = append(seed, 0)
	seed = append(seed, model...)
	seed = append(seed, 0)
	seed = bucketStart.UTC().AppendFormat(seed, time.RFC3339Nano)
	return sha256.Sum256(seed)
}

type channelMonitorV2SyntheticBandBounds struct {
	minError, maxError float64
	minTTFT, maxTTFT   int64
	targetScore        float64
}

type channelMonitorV2SyntheticRawLatency struct {
	samples int64
	p50     int64
	p90     int64
	p95     int64
	avg     float64
}

type channelMonitorV2SyntheticRawMetric struct {
	success, errors                         int64
	input, output, cacheCreation, cacheRead int64
	errorRate, successRate, cacheRate       float64
	cacheNumerator, cacheDenominator        int64
	ttft, duration                          channelMonitorV2SyntheticRawLatency
	upstreamAffected, upstreamAttempts      int64
}

func (m channelMonitorV2SyntheticRawMetric) requestCount() int64 {
	return channelMonitorV2SaturatingAdd(m.success, m.errors)
}

func (m channelMonitorV2SyntheticRawMetric) tokenCount() int64 {
	return channelMonitorV2SaturatingAdd(channelMonitorV2SaturatingAdd(m.input, m.output), channelMonitorV2SaturatingAdd(m.cacheCreation, m.cacheRead))
}

func (m channelMonitorV2SyntheticRawMetric) metric(minutes float64) ChannelMonitorV2Metric {
	if minutes <= 0 {
		minutes = 1
	}
	requests, tokens := m.requestCount(), m.tokenCount()
	return ChannelMonitorV2Metric{
		SuccessRequests: m.success, ErrorRequests: m.errors, RequestCount: requests,
		InputTokens: m.input, OutputTokens: m.output, CacheCreationTokens: m.cacheCreation, CacheReadTokens: m.cacheRead, TokenCount: tokens,
		RPM: float64(requests) / minutes, TPM: float64(tokens) / minutes,
		ErrorRate: m.errorRate, SuccessRate: m.successRate, CacheRate: m.cacheRate,
		CacheRateNumerator: m.cacheNumerator, CacheRateDenominator: m.cacheDenominator,
		TTFT: ChannelMonitorV2Latency{
			SampleCount: m.ttft.samples,
			P50Ms:       channelMonitorV2Int64Pointer(m.ttft.p50), P90Ms: channelMonitorV2Int64Pointer(m.ttft.p90),
			P95Ms: channelMonitorV2Int64Pointer(m.ttft.p95), AvgMs: channelMonitorV2Float64Pointer(m.ttft.avg),
		},
		Duration: ChannelMonitorV2Latency{
			SampleCount: m.duration.samples,
			P50Ms:       channelMonitorV2Int64Pointer(m.duration.p50), P90Ms: channelMonitorV2Int64Pointer(m.duration.p90),
			P95Ms: channelMonitorV2Int64Pointer(m.duration.p95), AvgMs: channelMonitorV2Float64Pointer(m.duration.avg),
		},
		UpstreamAffectedRequests: channelMonitorV2Int64Pointer(m.upstreamAffected),
		UpstreamAttemptCount:     channelMonitorV2Int64Pointer(m.upstreamAttempts),
	}
}

func channelMonitorV2SyntheticBounds(band string) (channelMonitorV2SyntheticBandBounds, bool) {
	switch band {
	case "score10":
		return channelMonitorV2SyntheticBandBounds{0, .01, 1000, 3000, 97.5}, true
	case "score9":
		return channelMonitorV2SyntheticBandBounds{.005, .03, 1000, 6000, 90}, true
	case "score8":
		return channelMonitorV2SyntheticBandBounds{.01, .05, 1000, 10000, 80}, true
	case "score7":
		return channelMonitorV2SyntheticBandBounds{.04, .10, 3000, 10000, 70}, true
	case "score6":
		return channelMonitorV2SyntheticBandBounds{.09, .16, 5000, 10000, 60}, true
	default:
		return channelMonitorV2SyntheticBandBounds{}, false
	}
}

func channelMonitorV2SyntheticMetricForBand(platform string, groupID int64, model string, bucketStart time.Time, thresholds ChannelMonitorV2HealthThresholds, allowErrors bool) (ChannelMonitorV2Metric, string, bool) {
	raw, band, ok := channelMonitorV2SyntheticRawForBand(platform, groupID, model, bucketStart, thresholds, allowErrors)
	if !ok {
		return ChannelMonitorV2Metric{}, "", false
	}
	metric := raw.metric(5)
	health := ChannelMonitorV2HealthForWithThresholds(metric, thresholds)
	if health.Score == nil {
		return ChannelMonitorV2Metric{}, "", false
	}
	actualBand := channelMonitorV2BandForScore(*health.Score)
	if actualBand != band {
		return ChannelMonitorV2Metric{}, "", false
	}
	return metric, actualBand, true
}

func channelMonitorV2SyntheticRawForBand(platform string, groupID int64, model string, bucketStart time.Time, thresholds ChannelMonitorV2HealthThresholds, allowErrors bool) (channelMonitorV2SyntheticRawMetric, string, bool) {
	hash := channelMonitorV2PresentationHash(platform, groupID, model, bucketStart)
	desiredBand := channelMonitorV2SyntheticTargetBandFromHash(hash)
	bounds, ok := channelMonitorV2SyntheticBounds(desiredBand)
	if !ok {
		return channelMonitorV2SyntheticRawMetric{}, "", false
	}
	thresholds = NormalizeChannelMonitorV2HealthThresholds(thresholds)
	requestCount := maxInt64(240, int64(math.Ceil(float64(thresholds.MinimumSample)/.84))+32)
	if requestCount <= 0 {
		return channelMonitorV2SyntheticRawMetric{}, "", false
	}
	cachePermille := int64(800 + int(hash[2])%101)
	outputPerRequest := int64(80 + int(hash[3])%121)
	cacheDenominator := requestCount * 1000
	cacheRead := requestCount * cachePermille
	nonCache := cacheDenominator - cacheRead
	input := nonCache * 4 / 5
	cacheCreation := nonCache - input
	output := requestCount * outputPerRequest

	bestErrorCount, bestTTFTP50 := int64(0), int64(0)
	bestScore, bestDistance := -1.0, math.MaxFloat64
	matched := false
	start := int(hash[8]) % 64
	for candidate := 0; candidate < 64; candidate++ {
		index := (start + candidate) % 64
		errorSlot, ttftSlot := index%8, index/8
		errorRate := bounds.minError + (bounds.maxError-bounds.minError)*float64(errorSlot)/7
		errorCount := int64(math.Round(errorRate * float64(requestCount)))
		if !allowErrors {
			errorCount = 0
		}
		if errorCount >= requestCount {
			errorCount = requestCount - 1
		}
		if errorCount < 0 {
			errorCount = 0
		}
		ttftP50 := bounds.minTTFT + int64(math.Round(float64(bounds.maxTTFT-bounds.minTTFT)*float64(ttftSlot)/7/100)*100)
		score, scored := channelMonitorV2SyntheticCandidateScore(requestCount, errorCount, requestCount-errorCount, cacheDenominator, float64(cacheRead)/float64(cacheDenominator), ttftP50, thresholds)
		if !scored {
			continue
		}
		band := channelMonitorV2BandForScore(score)
		if band == desiredBand {
			bestErrorCount, bestTTFTP50, bestScore, matched = errorCount, ttftP50, score, true
			break
		}
		if band == "score5" || band == "score4" || band == "score3" || band == "score2" || band == "score1" || band == "score0" {
			continue
		}
		distance := math.Abs(score - bounds.targetScore)
		if bestScore < 0 || distance < bestDistance {
			bestErrorCount, bestTTFTP50, bestScore, bestDistance = errorCount, ttftP50, score, distance
		}
	}
	if bestScore < 0 {
		return channelMonitorV2SyntheticRawMetric{}, "", false
	}
	ttftP90, ttftP95 := bestTTFTP50+700, bestTTFTP50+1000
	durationP50 := maxInt64(12000, ttftP95+1500)
	cacheRate := float64(cacheRead) / float64(cacheDenominator)
	errorRate := float64(bestErrorCount) / float64(requestCount)
	band := channelMonitorV2BandForScore(bestScore)
	if matched && band != desiredBand {
		return channelMonitorV2SyntheticRawMetric{}, "", false
	}
	return channelMonitorV2SyntheticRawMetric{
		success: requestCount - bestErrorCount, errors: bestErrorCount,
		input: input, output: output, cacheCreation: cacheCreation, cacheRead: cacheRead,
		errorRate: errorRate, successRate: float64(requestCount-bestErrorCount) / float64(requestCount), cacheRate: cacheRate,
		cacheNumerator: cacheRead, cacheDenominator: cacheDenominator,
		ttft:             channelMonitorV2SyntheticRawLatency{samples: requestCount - bestErrorCount, p50: bestTTFTP50, p90: ttftP90, p95: ttftP95, avg: float64(bestTTFTP50 + 300)},
		duration:         channelMonitorV2SyntheticRawLatency{samples: requestCount, p50: durationP50, p90: durationP50 + 700, p95: durationP50 + 1000, avg: float64(durationP50 + 300)},
		upstreamAffected: bestErrorCount, upstreamAttempts: channelMonitorV2SaturatingAdd(requestCount, bestErrorCount),
	}, band, true
}

func channelMonitorV2SyntheticCandidateScore(requests, errors, successes, cacheDenominator int64, cacheRate float64, ttftP50 int64, thresholds ChannelMonitorV2HealthThresholds) (float64, bool) {
	thresholds = NormalizeChannelMonitorV2HealthThresholds(thresholds)
	type scored struct {
		score  float64
		weight float64
	}
	parts := [3]scored{}
	count := 0
	if requests >= thresholds.MinimumSample {
		errorRate := float64(errors) / float64(requests)
		parts[count] = scored{score: errorRateScore(errorRate, thresholds.CriticalErrorRate), weight: thresholds.ErrorWeight}
		count++
	}
	if successes >= thresholds.MinimumSample {
		parts[count] = scored{score: ttftP50Score(float64(ttftP50), float64(thresholds.TargetTTFTMs), float64(thresholds.CriticalTTFTMs)), weight: thresholds.TTFTWeight}
		count++
	}
	if cacheDenominator >= thresholds.MinimumSample {
		score := cacheRateScore(cacheRate)
		if thresholds.WarningCacheRate <= 0 && thresholds.CriticalCacheRate <= 0 {
			score = 100
		}
		parts[count] = scored{score: score, weight: thresholds.CacheWeight}
		count++
	}
	var weightSum, scoreSum float64
	for _, part := range parts[:count] {
		weightSum += part.weight
		scoreSum += part.weight * part.score
	}
	if count == 0 || weightSum <= 0 {
		return 0, false
	}
	return scoreSum / weightSum, true
}

func channelMonitorV2BandForScore(score float64) string {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return "unknown"
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	band := int(math.Floor(score/10 + .5))
	if band < 0 {
		band = 0
	}
	if band > 10 {
		band = 10
	}
	return fmt.Sprintf("score%d", band)
}

func channelMonitorV2Int64Pointer(value int64) *int64       { return &value }
func channelMonitorV2Float64Pointer(value float64) *float64 { return &value }

func channelMonitorV2SyntheticMetricForEmptyInterval(platform string, groupID int64, model string, start time.Time, interval time.Duration, minutes float64, thresholds ChannelMonitorV2HealthThresholds, ignoredCategories []string, allowErrors bool) (ChannelMonitorV2Metric, map[string]int64, bool) {
	if interval <= 0 || interval%(5*time.Minute) != 0 {
		return ChannelMonitorV2Metric{}, nil, false
	}
	if interval == 5*time.Minute {
		metric, _, ok := channelMonitorV2SyntheticMetricForBand(platform, groupID, model, start, thresholds, allowErrors)
		if !ok {
			return ChannelMonitorV2Metric{}, nil, false
		}
		return metric, channelMonitorV2DistributeSyntheticErrors(metric.ErrorRequests, platform, groupID, model, start, ignoredCategories), true
	}
	fold := newChannelMonitorV2MetricFold()
	categories := map[string]int64{}
	generated := false
	ignoredSet := channelMonitorV2IgnoredCategorySet(ignoredCategories)
	for atom := start.UTC(); atom.Before(start.Add(interval)); atom = atom.Add(5 * time.Minute) {
		raw, _, ok := channelMonitorV2SyntheticRawForBand(platform, groupID, model, atom, thresholds, allowErrors)
		if !ok {
			continue
		}
		generated = true
		fold.addSyntheticRaw(raw)
		if category, ok := channelMonitorV2SyntheticErrorCategoryFromSet(raw.errors, platform, groupID, model, atom, ignoredSet); ok {
			categories[category] = channelMonitorV2SaturatingAdd(categories[category], raw.errors)
		}
	}
	if !generated {
		return ChannelMonitorV2Metric{}, nil, false
	}
	if minutes <= 0 {
		minutes = interval.Minutes()
	}
	return fold.metric(minutes), categories, true
}

func channelMonitorV2DistributeSyntheticErrors(count int64, platform string, groupID int64, model string, bucketStart time.Time, ignored []string) map[string]int64 {
	category, ok := channelMonitorV2SyntheticErrorCategory(count, platform, groupID, model, bucketStart, ignored)
	if !ok {
		return nil
	}
	return map[string]int64{category: count}
}

func channelMonitorV2SyntheticErrorCategory(count int64, platform string, groupID int64, model string, bucketStart time.Time, ignored []string) (string, bool) {
	return channelMonitorV2SyntheticErrorCategoryFromSet(count, platform, groupID, model, bucketStart, channelMonitorV2IgnoredCategorySet(ignored))
}

func channelMonitorV2SyntheticErrorCategoryFromSet(count int64, platform string, groupID int64, model string, bucketStart time.Time, ignored map[string]struct{}) (string, bool) {
	if count <= 0 {
		return "", false
	}
	available := 0
	for _, category := range ChannelMonitorV2ErrorCategories {
		if _, skip := ignored[category]; !skip {
			available++
		}
	}
	if available == 0 {
		return "", false
	}
	var seedBuffer [512]byte
	seed := seedBuffer[:0]
	seed = append(seed, platform...)
	seed = append(seed, 0)
	seed = strconv.AppendInt(seed, groupID, 10)
	seed = append(seed, 0)
	seed = append(seed, model...)
	seed = append(seed, 0)
	seed = bucketStart.UTC().AppendFormat(seed, time.RFC3339Nano)
	seed = append(seed, 0)
	seed = append(seed, "error-category"...)
	hash := sha256.Sum256(seed)
	selected := int(binary.BigEndian.Uint64(hash[:8]) % uint64(available))
	for _, category := range ChannelMonitorV2ErrorCategories {
		if _, skip := ignored[category]; skip {
			continue
		}
		if selected == 0 {
			return category, true
		}
		selected--
	}
	return "", false
}

type channelMonitorV2MetricFold struct {
	parts                                                                  int
	direct                                                                 *ChannelMonitorV2Metric
	success, errors, input, output, cacheCreation, cacheRead               int64
	errorRateWeighted                                                      float64
	ttftCount, durationCount                                               int64
	ttftAvgCount, ttftP50Count, ttftP90Count, ttftP95Count                 int64
	durationAvgCount, durationP50Count, durationP90Count, durationP95Count int64
	ttftAvg, ttftP50, ttftP90, ttftP95                                     float64
	durationAvg, durationP50, durationP90, durationP95                     float64
	hasTTFTP50, hasTTFTP90, hasTTFTP95                                     bool
	hasDurationP50, hasDurationP90, hasDurationP95                         bool
	upstreamAffected, upstreamAttempts                                     int64
	hasUpstreamAffected, hasUpstreamAttempts                               bool
}

func newChannelMonitorV2MetricFold() *channelMonitorV2MetricFold {
	return &channelMonitorV2MetricFold{}
}

func (f *channelMonitorV2MetricFold) add(metric ChannelMonitorV2Metric) {
	if f == nil {
		return
	}
	if f.parts == 0 {
		copy := metric
		f.direct = &copy
	} else {
		f.direct = nil
	}
	f.parts++
	f.success = channelMonitorV2SaturatingAdd(f.success, metric.SuccessRequests)
	f.errors = channelMonitorV2SaturatingAdd(f.errors, metric.ErrorRequests)
	f.input = channelMonitorV2SaturatingAdd(f.input, metric.InputTokens)
	f.output = channelMonitorV2SaturatingAdd(f.output, metric.OutputTokens)
	f.cacheCreation = channelMonitorV2SaturatingAdd(f.cacheCreation, metric.CacheCreationTokens)
	f.cacheRead = channelMonitorV2SaturatingAdd(f.cacheRead, metric.CacheReadTokens)
	f.errorRateWeighted += metric.ErrorRate * float64(metric.RequestCount)
	f.ttftCount = channelMonitorV2SaturatingAdd(f.ttftCount, metric.TTFT.SampleCount)
	f.durationCount = channelMonitorV2SaturatingAdd(f.durationCount, metric.Duration.SampleCount)
	f.addLatencyAverage(&f.ttftAvgCount, &f.ttftAvg, metric.TTFT.SampleCount, metric.TTFT.AvgMs)
	f.addLatencyPercentile(&f.ttftP50Count, metric.TTFT.SampleCount, metric.TTFT.P50Ms, &f.ttftP50, &f.hasTTFTP50)
	f.addLatencyPercentile(&f.ttftP90Count, metric.TTFT.SampleCount, metric.TTFT.P90Ms, &f.ttftP90, &f.hasTTFTP90)
	f.addLatencyPercentile(&f.ttftP95Count, metric.TTFT.SampleCount, metric.TTFT.P95Ms, &f.ttftP95, &f.hasTTFTP95)
	f.addLatencyAverage(&f.durationAvgCount, &f.durationAvg, metric.Duration.SampleCount, metric.Duration.AvgMs)
	f.addLatencyPercentile(&f.durationP50Count, metric.Duration.SampleCount, metric.Duration.P50Ms, &f.durationP50, &f.hasDurationP50)
	f.addLatencyPercentile(&f.durationP90Count, metric.Duration.SampleCount, metric.Duration.P90Ms, &f.durationP90, &f.hasDurationP90)
	f.addLatencyPercentile(&f.durationP95Count, metric.Duration.SampleCount, metric.Duration.P95Ms, &f.durationP95, &f.hasDurationP95)
	if metric.UpstreamAffectedRequests != nil {
		f.upstreamAffected = channelMonitorV2SaturatingAdd(f.upstreamAffected, *metric.UpstreamAffectedRequests)
		f.hasUpstreamAffected = true
	}
	if metric.UpstreamAttemptCount != nil {
		f.upstreamAttempts = channelMonitorV2SaturatingAdd(f.upstreamAttempts, *metric.UpstreamAttemptCount)
		f.hasUpstreamAttempts = true
	}
}

func (f *channelMonitorV2MetricFold) addSyntheticRaw(metric channelMonitorV2SyntheticRawMetric) {
	if f == nil {
		return
	}
	f.direct = nil
	f.parts++
	f.success = channelMonitorV2SaturatingAdd(f.success, metric.success)
	f.errors = channelMonitorV2SaturatingAdd(f.errors, metric.errors)
	f.input = channelMonitorV2SaturatingAdd(f.input, metric.input)
	f.output = channelMonitorV2SaturatingAdd(f.output, metric.output)
	f.cacheCreation = channelMonitorV2SaturatingAdd(f.cacheCreation, metric.cacheCreation)
	f.cacheRead = channelMonitorV2SaturatingAdd(f.cacheRead, metric.cacheRead)
	f.errorRateWeighted += metric.errorRate * float64(metric.requestCount())
	f.ttftCount = channelMonitorV2SaturatingAdd(f.ttftCount, metric.ttft.samples)
	f.durationCount = channelMonitorV2SaturatingAdd(f.durationCount, metric.duration.samples)
	f.addLatencyAverage(&f.ttftAvgCount, &f.ttftAvg, metric.ttft.samples, &metric.ttft.avg)
	f.addLatencyPercentileValue(&f.ttftP50Count, metric.ttft.samples, metric.ttft.p50, &f.ttftP50, &f.hasTTFTP50)
	f.addLatencyPercentileValue(&f.ttftP90Count, metric.ttft.samples, metric.ttft.p90, &f.ttftP90, &f.hasTTFTP90)
	f.addLatencyPercentileValue(&f.ttftP95Count, metric.ttft.samples, metric.ttft.p95, &f.ttftP95, &f.hasTTFTP95)
	f.addLatencyAverage(&f.durationAvgCount, &f.durationAvg, metric.duration.samples, &metric.duration.avg)
	f.addLatencyPercentileValue(&f.durationP50Count, metric.duration.samples, metric.duration.p50, &f.durationP50, &f.hasDurationP50)
	f.addLatencyPercentileValue(&f.durationP90Count, metric.duration.samples, metric.duration.p90, &f.durationP90, &f.hasDurationP90)
	f.addLatencyPercentileValue(&f.durationP95Count, metric.duration.samples, metric.duration.p95, &f.durationP95, &f.hasDurationP95)
	f.upstreamAffected = channelMonitorV2SaturatingAdd(f.upstreamAffected, metric.upstreamAffected)
	f.hasUpstreamAffected = true
	f.upstreamAttempts = channelMonitorV2SaturatingAdd(f.upstreamAttempts, metric.upstreamAttempts)
	f.hasUpstreamAttempts = true
}

func (f *channelMonitorV2MetricFold) addLatencyAverage(weightedCount *int64, weighted *float64, samples int64, avg *float64) {
	if samples <= 0 || avg == nil {
		return
	}
	*weighted += *avg * float64(samples)
	*weightedCount = channelMonitorV2SaturatingAdd(*weightedCount, samples)
}

func (f *channelMonitorV2MetricFold) addLatencyPercentile(weightedCount *int64, samples int64, value *int64, weighted *float64, present *bool) {
	if samples <= 0 || value == nil {
		return
	}
	*weighted += float64(*value) * float64(samples)
	*weightedCount = channelMonitorV2SaturatingAdd(*weightedCount, samples)
	*present = true
}

func (f *channelMonitorV2MetricFold) addLatencyPercentileValue(weightedCount *int64, samples, value int64, weighted *float64, present *bool) {
	if samples <= 0 {
		return
	}
	*weighted += float64(value) * float64(samples)
	*weightedCount = channelMonitorV2SaturatingAdd(*weightedCount, samples)
	*present = true
}

func (f *channelMonitorV2MetricFold) metric(minutes float64) ChannelMonitorV2Metric {
	if f == nil {
		return ChannelMonitorV2Metric{}
	}
	if f.direct != nil {
		return *f.direct
	}
	if minutes <= 0 {
		minutes = 1
	}
	requests := channelMonitorV2SaturatingAdd(f.success, f.errors)
	tokens := channelMonitorV2SaturatingAdd(channelMonitorV2SaturatingAdd(f.input, f.output), channelMonitorV2SaturatingAdd(f.cacheCreation, f.cacheRead))
	cacheDenominator := channelMonitorV2SaturatingAdd(channelMonitorV2SaturatingAdd(f.input, f.cacheCreation), f.cacheRead)
	metric := ChannelMonitorV2Metric{
		SuccessRequests: f.success, ErrorRequests: f.errors, RequestCount: requests,
		InputTokens: f.input, OutputTokens: f.output, CacheCreationTokens: f.cacheCreation, CacheReadTokens: f.cacheRead, TokenCount: tokens,
		RPM: float64(requests) / minutes, TPM: float64(tokens) / minutes,
		CacheRateNumerator: f.cacheRead, CacheRateDenominator: cacheDenominator,
	}
	if requests > 0 {
		metric.ErrorRate = f.errorRateWeighted / float64(requests)
		metric.SuccessRate = float64(f.success) / float64(requests)
	}
	if cacheDenominator > 0 {
		metric.CacheRate = float64(f.cacheRead) / float64(cacheDenominator)
	}
	metric.TTFT = channelMonitorV2FoldedLatency(f.ttftCount, f.ttftAvg, f.ttftAvgCount, f.ttftP50, f.ttftP50Count, f.hasTTFTP50, f.ttftP90, f.ttftP90Count, f.hasTTFTP90, f.ttftP95, f.ttftP95Count, f.hasTTFTP95)
	metric.Duration = channelMonitorV2FoldedLatency(f.durationCount, f.durationAvg, f.durationAvgCount, f.durationP50, f.durationP50Count, f.hasDurationP50, f.durationP90, f.durationP90Count, f.hasDurationP90, f.durationP95, f.durationP95Count, f.hasDurationP95)
	if f.hasUpstreamAffected {
		value := f.upstreamAffected
		metric.UpstreamAffectedRequests = &value
	}
	if f.hasUpstreamAttempts {
		value := f.upstreamAttempts
		metric.UpstreamAttemptCount = &value
	}
	return metric
}

func channelMonitorV2FoldedLatency(count int64, weightedAvg float64, avgCount int64, weightedP50 float64, p50Count int64, hasP50 bool, weightedP90 float64, p90Count int64, hasP90 bool, weightedP95 float64, p95Count int64, hasP95 bool) ChannelMonitorV2Latency {
	latency := ChannelMonitorV2Latency{SampleCount: count}
	if count <= 0 {
		return latency
	}
	if avgCount > 0 {
		avg := weightedAvg / float64(avgCount)
		latency.AvgMs = &avg
	}
	if hasP50 {
		value := int64(math.Round(weightedP50 / float64(p50Count)))
		latency.P50Ms = &value
	}
	if hasP90 {
		value := int64(math.Round(weightedP90 / float64(p90Count)))
		latency.P90Ms = &value
	}
	if hasP95 {
		value := int64(math.Round(weightedP95 / float64(p95Count)))
		latency.P95Ms = &value
	}
	channelMonitorV2EnforceLatencyOrder(&latency)
	return latency
}

func channelMonitorV2EnforceLatencyOrder(latency *ChannelMonitorV2Latency) {
	if latency == nil || latency.P50Ms == nil || latency.AvgMs == nil || latency.P90Ms == nil || latency.P95Ms == nil {
		return
	}
	const visibleGapMs = int64(200)
	if *latency.AvgMs <= float64(*latency.P50Ms) {
		avg := float64(*latency.P50Ms + visibleGapMs)
		latency.AvgMs = &avg
	}
	if *latency.P90Ms <= int64(math.Round(*latency.AvgMs)) {
		value := int64(math.Round(*latency.AvgMs)) + visibleGapMs
		latency.P90Ms = &value
	}
	if *latency.P95Ms <= *latency.P90Ms {
		value := *latency.P90Ms + visibleGapMs
		latency.P95Ms = &value
	}
}

func channelMonitorV2PresentationSnapshot(presentation *ChannelMonitorV2Presentation, filter ChannelMonitorV2Filter, cfg ChannelMonitorV2Config) *ChannelMonitorV2Snapshot {
	minutes := channelMonitorV2PresentationCoveredMinutes(filter, presentation.Coverage)
	total := newChannelMonitorV2MetricFold()
	trend := map[int64]*channelMonitorV2MetricFold{}
	for _, row := range presentation.Rows {
		total.add(row.Metrics)
		for _, point := range row.Buckets {
			key := point.BucketStart.UTC().UnixNano()
			if trend[key] == nil {
				trend[key] = newChannelMonitorV2MetricFold()
			}
			trend[key].add(point.Metrics)
		}
	}
	metrics := total.metric(minutes)
	trendKeys := make([]int64, 0, len(trend))
	for key := range trend {
		trendKeys = append(trendKeys, key)
	}
	sort.Slice(trendKeys, func(i, j int) bool { return trendKeys[i] < trendKeys[j] })
	points := make([]ChannelMonitorV2TrendPoint, 0, len(trendKeys))
	for _, key := range trendKeys {
		bucketMetrics := trend[key].metric(filter.Bucket.Minutes())
		points = append(points, ChannelMonitorV2TrendPoint{
			BucketStart: time.Unix(0, key).UTC(), Metrics: bucketMetrics,
			Health: ChannelMonitorV2HealthForWithThresholds(bucketMetrics, cfg.HealthThresholds),
		})
	}
	publicConfig := channelMonitorV2CloneConfig(cfg)
	redactChannelMonitorV2PublicConfig(&publicConfig)
	return &ChannelMonitorV2Snapshot{
		Config: publicConfig, Coverage: presentation.Coverage, Metrics: metrics,
		Health: ChannelMonitorV2HealthForWithThresholds(metrics, cfg.HealthThresholds), Trend: points,
	}
}

func channelMonitorV2CloneConfig(cfg ChannelMonitorV2Config) ChannelMonitorV2Config {
	cfg.Platforms = append([]ChannelMonitorV2PlatformConfig(nil), cfg.Platforms...)
	for i := range cfg.Platforms {
		cfg.Platforms[i].Models = append([]string(nil), cfg.Platforms[i].Models...)
	}
	cfg.GroupIDs = append([]int64(nil), cfg.GroupIDs...)
	cfg.IgnoredErrorCategories = append([]string(nil), cfg.IgnoredErrorCategories...)
	if cfg.UpdatedBy != nil {
		actor := *cfg.UpdatedBy
		cfg.UpdatedBy = &actor
	}
	return cfg
}

func channelMonitorV2PresentationMatrix(presentation *ChannelMonitorV2Presentation, filter ChannelMonitorV2Filter, groupBy ChannelMonitorV2GroupBy, thresholds ChannelMonitorV2HealthThresholds) *ChannelMonitorV2Matrix {
	type aggregateKey struct {
		platform string
		groupID  int64
		model    string
	}
	type aggregateRow struct {
		groupName string
		total     *channelMonitorV2MetricFold
		buckets   map[int64]*channelMonitorV2MetricFold
	}
	rows := map[aggregateKey]*aggregateRow{}
	for _, leaf := range presentation.Rows {
		key := aggregateKey{platform: leaf.Platform}
		switch groupBy {
		case ChannelMonitorV2GroupByPlatformGroup:
			key.groupID = channelMonitorV2MatrixGroupID(leaf.GroupID)
		case ChannelMonitorV2GroupByPlatformModel:
			key.model = leaf.Model
		case ChannelMonitorV2GroupByPlatformGroupModel:
			key.groupID, key.model = channelMonitorV2MatrixGroupID(leaf.GroupID), leaf.Model
		}
		row := rows[key]
		if row == nil {
			row = &aggregateRow{total: newChannelMonitorV2MetricFold(), buckets: map[int64]*channelMonitorV2MetricFold{}}
			rows[key] = row
		}
		if key.groupID > 0 {
			row.groupName = leaf.GroupName
		}
		row.total.add(leaf.Metrics)
		for _, point := range leaf.Buckets {
			bucketKey := point.BucketStart.UTC().UnixNano()
			if row.buckets[bucketKey] == nil {
				row.buckets[bucketKey] = newChannelMonitorV2MetricFold()
			}
			row.buckets[bucketKey].add(point.Metrics)
		}
	}
	minutes := channelMonitorV2PresentationCoveredMinutes(filter, presentation.Coverage)
	keys := make([]aggregateKey, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.platform != b.platform {
			return a.platform < b.platform
		}
		if a.groupID != b.groupID {
			return a.groupID < b.groupID
		}
		return a.model < b.model
	})
	result := &ChannelMonitorV2Matrix{GroupBy: groupBy, Coverage: presentation.Coverage, Items: make([]ChannelMonitorV2MatrixRow, 0, len(keys))}
	for _, key := range keys {
		row := rows[key]
		metrics := row.total.metric(minutes)
		item := ChannelMonitorV2MatrixRow{
			Platform: key.platform, GroupName: row.groupName, Model: key.model,
			Metrics: metrics, Health: ChannelMonitorV2HealthForWithThresholds(metrics, thresholds),
			Buckets: []ChannelMonitorV2TrendPoint{},
		}
		if key.groupID > 0 {
			groupID := key.groupID
			item.GroupID = &groupID
		}
		bucketKeys := make([]int64, 0, len(row.buckets))
		for bucketKey := range row.buckets {
			bucketKeys = append(bucketKeys, bucketKey)
		}
		sort.Slice(bucketKeys, func(i, j int) bool { return bucketKeys[i] < bucketKeys[j] })
		for _, bucketKey := range bucketKeys {
			bucketMetrics := row.buckets[bucketKey].metric(filter.Bucket.Minutes())
			item.Buckets = append(item.Buckets, ChannelMonitorV2TrendPoint{
				BucketStart: time.Unix(0, bucketKey).UTC(), Metrics: bucketMetrics,
				Health: ChannelMonitorV2HealthForWithThresholds(bucketMetrics, thresholds),
			})
		}
		result.Items = append(result.Items, item)
	}
	return result
}

func channelMonitorV2PresentationModels(presentation *ChannelMonitorV2Presentation, filter ChannelMonitorV2Filter, thresholds ChannelMonitorV2HealthThresholds) *ChannelMonitorV2List[ChannelMonitorV2ModelRow] {
	rows := map[string]*channelMonitorV2MetricFold{}
	for _, leaf := range presentation.Rows {
		key := leaf.Platform + "\x00" + leaf.Model
		if rows[key] == nil {
			rows[key] = newChannelMonitorV2MetricFold()
		}
		rows[key].add(leaf.Metrics)
	}
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	minutes := channelMonitorV2PresentationCoveredMinutes(filter, presentation.Coverage)
	items := make([]ChannelMonitorV2ModelRow, 0, len(keys))
	for _, key := range keys {
		parts := strings.SplitN(key, "\x00", 2)
		metrics := rows[key].metric(minutes)
		items = append(items, ChannelMonitorV2ModelRow{
			Platform: parts[0], Model: parts[1], Metrics: metrics,
			Health: ChannelMonitorV2HealthForWithThresholds(metrics, thresholds),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Metrics.RequestCount != items[j].Metrics.RequestCount {
			return items[i].Metrics.RequestCount > items[j].Metrics.RequestCount
		}
		if items[i].Platform != items[j].Platform {
			return items[i].Platform < items[j].Platform
		}
		return items[i].Model < items[j].Model
	})
	return &ChannelMonitorV2List[ChannelMonitorV2ModelRow]{Coverage: presentation.Coverage, Items: items}
}

func channelMonitorV2PresentationAddModels(dimensions *ChannelMonitorV2Dimensions, presentation *ChannelMonitorV2Presentation) {
	if dimensions == nil || presentation == nil {
		return
	}
	seen := map[string]struct{}{}
	for _, item := range dimensions.Models {
		seen[item.Platform+"\x00"+item.Value] = struct{}{}
	}
	for _, row := range presentation.Rows {
		key := row.Platform + "\x00" + row.Model
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		label := row.Model
		if row.Model == ChannelMonitorV2OtherModel {
			label = "Other models"
		}
		dimensions.Models = append(dimensions.Models, ChannelMonitorV2Dimension{Value: row.Model, Label: label, Platform: row.Platform})
	}
	sort.Slice(dimensions.Models, func(i, j int) bool {
		if dimensions.Models[i].RequestCount != dimensions.Models[j].RequestCount {
			return dimensions.Models[i].RequestCount > dimensions.Models[j].RequestCount
		}
		if dimensions.Models[i].Platform != dimensions.Models[j].Platform {
			return dimensions.Models[i].Platform < dimensions.Models[j].Platform
		}
		return dimensions.Models[i].Value < dimensions.Models[j].Value
	})
}
