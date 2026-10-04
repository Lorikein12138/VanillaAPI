package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func presentationSnapshotRow(platform string, groupID int64, model string, total int64, buckets map[time.Time]int64) ChannelMonitorV2MatrixRow {
	gid := groupID
	row := ChannelMonitorV2MatrixRow{Platform: platform, GroupID: &gid, Model: model}
	row.Metrics = ChannelMonitorV2Metric{
		InputTokens: total, TokenCount: total, TPM: float64(total) / 90,
		SuccessRequests: total / 100, RequestCount: total / 100, RPM: float64(total/100) / 90,
	}
	for start, tokens := range buckets {
		row.Buckets = append(row.Buckets, ChannelMonitorV2TrendPoint{
			BucketStart: start,
			Metrics: ChannelMonitorV2Metric{
				InputTokens: tokens, TokenCount: tokens, TPM: float64(tokens) / 5,
				SuccessRequests: tokens / 100, RequestCount: tokens / 100, RPM: float64(tokens/100) / 5,
			},
		})
	}
	return row
}

func TestChannelMonitorV2PresentationSnapshotAveragesThroughputPerPlatformGroup(t *testing.T) {
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	t0, t1 := start, start.Add(5*time.Minute)
	filter := ChannelMonitorV2Filter{Range: "90m", Start: start, End: end, AsOf: end, Bucket: 5 * time.Minute}
	coverage := ChannelMonitorV2Coverage{RequestedStart: start, RequestedEnd: end, CoverageStart: start, DataThrough: end, BucketSeconds: 300}
	presentation := &ChannelMonitorV2Presentation{Coverage: coverage, Rows: []ChannelMonitorV2MatrixRow{
		presentationSnapshotRow("openai", 1, "m1", 54000, map[time.Time]int64{t0: 3000, t1: 3000}),
		presentationSnapshotRow("openai", 1, "m2", 27000, map[time.Time]int64{t0: 1500}),
		presentationSnapshotRow("openai", 2, "m1", 108000, map[time.Time]int64{t0: 6000}),
		presentationSnapshotRow("anthropic", 3, "m1", 0, map[time.Time]int64{t0: 0}),
	}}

	snapshot := channelMonitorV2PresentationSnapshot(presentation, filter, ChannelMonitorV2Config{HealthThresholds: DefaultChannelMonitorV2HealthThresholds()})

	// Two (platform, group) pairs carry throughput; the idle third pair is not averaged in.
	require.InDelta(t, (600.0+300.0+1200.0)/2, snapshot.Metrics.TPM, 1e-9)
	require.Equal(t, int64(54000+27000+108000), snapshot.Metrics.TokenCount, "token totals stay the sum")
	require.Equal(t, int64(1890), snapshot.Metrics.RequestCount, "request totals stay the sum")
	require.InDelta(t, 1890.0/90, snapshot.Metrics.RPM, 1e-9, "RPM stays the sum of the leaves")

	require.Len(t, snapshot.Trend, 2)
	require.Equal(t, t0, snapshot.Trend[0].BucketStart)
	require.InDelta(t, (3000.0+1500.0+6000.0)/5/2, snapshot.Trend[0].Metrics.TPM, 1e-9, "bucket averages over the pairs present in that bucket")
	require.Equal(t, t1, snapshot.Trend[1].BucketStart)
	require.InDelta(t, 3000.0/5, snapshot.Trend[1].Metrics.TPM, 1e-9, "a bucket with a single pair is unchanged")
}

func TestChannelMonitorV2PresentationSnapshotKeepsSinglePairThroughput(t *testing.T) {
	start := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	filter := ChannelMonitorV2Filter{Range: "90m", Start: start, End: end, AsOf: end, Bucket: 5 * time.Minute}
	coverage := ChannelMonitorV2Coverage{RequestedStart: start, RequestedEnd: end, CoverageStart: start, DataThrough: end, BucketSeconds: 300}
	presentation := &ChannelMonitorV2Presentation{Coverage: coverage, Rows: []ChannelMonitorV2MatrixRow{
		presentationSnapshotRow("openai", 1, "m1", 54000, map[time.Time]int64{start: 3000}),
		presentationSnapshotRow("openai", 1, "m2", 27000, map[time.Time]int64{start: 1500}),
	}}

	snapshot := channelMonitorV2PresentationSnapshot(presentation, filter, ChannelMonitorV2Config{HealthThresholds: DefaultChannelMonitorV2HealthThresholds()})

	require.InDelta(t, 900.0, snapshot.Metrics.TPM, 1e-9, "one pair: the mean is the pair's own throughput")
	require.InDelta(t, 900.0, snapshot.Trend[0].Metrics.TPM, 1e-9)

	empty := channelMonitorV2PresentationSnapshot(&ChannelMonitorV2Presentation{Coverage: coverage}, filter, ChannelMonitorV2Config{HealthThresholds: DefaultChannelMonitorV2HealthThresholds()})
	require.Zero(t, empty.Metrics.TPM)
	require.Empty(t, empty.Trend)
}

func TestChannelMonitorV2PresentationSnapshotThroughputEqualsMeanOfPlatformGroupRows(t *testing.T) {
	svc, _, _, _, repo := channelMonitorV2Presentation30dFixture(t)
	repo.config.Enabled = true
	asOf := time.Date(2026, 9, 29, 12, 34, 56, 0, time.UTC)
	filter, err := svc.ParseFilterAt("90m", nil, nil, nil, asOf)
	require.NoError(t, err)

	snapshot, err := svc.PresentationSnapshot(context.Background(), filter)
	require.NoError(t, err)
	matrix, err := svc.PresentationMatrix(context.Background(), filter, ChannelMonitorV2GroupByPlatformGroup)
	require.NoError(t, err)
	require.Len(t, matrix.Items, 21, "one platform-group row per group")

	sumTPM, sumRPM, sumTokens := 0.0, 0.0, int64(0)
	bucketSums := map[time.Time]float64{}
	bucketRows := map[time.Time]int{}
	for _, row := range matrix.Items {
		sumTPM += row.Metrics.TPM
		sumRPM += row.Metrics.RPM
		sumTokens += row.Metrics.TokenCount
		for _, point := range row.Buckets {
			bucketSums[point.BucketStart] += point.Metrics.TPM
			bucketRows[point.BucketStart]++
		}
	}
	require.InDelta(t, sumTPM/21, snapshot.Metrics.TPM, 1e-6, "top card throughput is the mean of the platform-group rows")
	require.GreaterOrEqual(t, snapshot.Metrics.TPM/60, 10.0-1)
	require.LessOrEqual(t, snapshot.Metrics.TPM/60, 50.0+1)
	require.InDelta(t, sumRPM, snapshot.Metrics.RPM, 1e-6, "request rate stays the sum of the rows")
	require.Equal(t, sumTokens, snapshot.Metrics.TokenCount, "token total stays the sum of the rows")
	require.NotEmpty(t, snapshot.Trend)
	for _, point := range snapshot.Trend {
		require.InDelta(t, bucketSums[point.BucketStart]/float64(bucketRows[point.BucketStart]), point.Metrics.TPM, 1e-6)
	}
}
