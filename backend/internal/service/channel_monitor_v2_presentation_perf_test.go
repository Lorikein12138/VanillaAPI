//go:build unit && !race

package service

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2Presentation30dPerformanceSmoke(t *testing.T) {
	svc, filter, cfg, dimensions, repo := channelMonitorV2Presentation30dFixture(t)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	presentation, err := svc.buildChannelMonitorV2Presentation(context.Background(), filter, cfg, dimensions)
	elapsed := time.Since(started)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	totalBuckets := 0
	for _, row := range presentation.Rows {
		totalBuckets += len(row.Buckets)
	}
	require.Len(t, presentation.Rows, 168)
	require.Equal(t, 5040, totalBuckets, "performance smoke test uses the same bounded response shape")
	require.Len(t, repo.matrixFilters, 3, "each disjoint retention partition is read exactly once")
	t.Logf("leaves=%d buckets=%d partitions=%d elapsed=%s allocated=%d bytes", len(presentation.Rows), totalBuckets, len(repo.matrixFilters), elapsed, after.TotalAlloc-before.TotalAlloc)
	require.Less(t, elapsed, 5*time.Second, "ordinary unit performance smoke test must finish within the 5s budget")
}
