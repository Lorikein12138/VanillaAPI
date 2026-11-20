package repository

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAdaptiveHistoryStoreMergesWritersIdempotentlyAndExpiresRecords(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	first, second := ProvideAdaptiveHistoryStore(rdb), ProvideAdaptiveHistoryStore(rdb)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	cutoff := now.Add(-service.AdaptiveHistoryRetention)
	firstBatch := []service.AdaptiveHistoryRecord{
		{At: cutoff, Payload: "expired"},
		{At: cutoff.Add(time.Microsecond), Payload: "oldest"},
		{At: now, Payload: "writer-one"},
	}
	secondBatch := []service.AdaptiveHistoryRecord{
		{At: now, Payload: "writer-two"},
		{At: now.Add(time.Microsecond), Payload: "future"},
	}
	// Seed an exact-boundary row before it expires. The retry must prune it.
	require.NoError(t, first.Merge(ctx, cutoff.Add(-time.Microsecond), firstBatch))
	require.NoError(t, second.Merge(ctx, cutoff, secondBatch))
	require.NoError(t, first.Merge(ctx, cutoff, firstBatch))
	require.EqualValues(t, 4, rdb.ZCard(ctx, adaptiveHistoryKey).Val())
	rows, err := ProvideAdaptiveHistoryStore(rdb).Read(ctx, cutoff, now)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	require.Equal(t, []string{"writer-two", "writer-one", "oldest"}, []string{rows[0].Payload, rows[1].Payload, rows[2].Payload})
	require.Equal(t, now.UnixMicro(), rows[0].At.UnixMicro())
	require.Equal(t, cutoff.Add(time.Microsecond).UnixMicro(), rows[2].At.UnixMicro())
	server.FastForward(time.Hour)
	require.NoError(t, first.Merge(ctx, cutoff, nil))
	require.Equal(t, service.AdaptiveHistoryRetention-time.Hour, server.TTL(adaptiveHistoryKey))
	server.FastForward(service.AdaptiveHistoryRetention - time.Hour)
	require.False(t, server.Exists(adaptiveHistoryKey))
}

func TestAdaptiveHistoryStoreBoundsReadAndKeepsNewestRows(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := ProvideAdaptiveHistoryStore(rdb)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	cutoff := now.Add(-service.AdaptiveHistoryRetention)
	seed := make([]redis.Z, adaptiveHistoryLimit+1)
	for i := range seed {
		seed[i] = redis.Z{Score: float64(now.Add(time.Duration(i) * time.Microsecond).UnixMicro()), Member: strconv.Itoa(i)}
	}
	require.NoError(t, rdb.ZAdd(ctx, adaptiveHistoryKey, seed...).Err())
	rows, err := store.Read(ctx, cutoff, now.Add(time.Second))
	require.NoError(t, err)
	require.Len(t, rows, adaptiveHistoryLimit)
	require.Equal(t, "1", rows[len(rows)-1].Payload)
	require.NoError(t, store.Merge(ctx, cutoff, []service.AdaptiveHistoryRecord{{At: now.Add(time.Second), Payload: "newest"}}))
	require.EqualValues(t, adaptiveHistoryLimit, rdb.ZCard(ctx, adaptiveHistoryKey).Val())
	rows, err = store.Read(ctx, cutoff, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, "newest", rows[0].Payload)
	require.Equal(t, "2", rows[len(rows)-1].Payload)
}

func TestAdaptiveHistoryStoreKeepsLatestHealthForSevenDays(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := ProvideAdaptiveHistoryStore(rdb)
	ctx := context.Background()
	now := time.Now().Truncate(time.Microsecond)
	cutoff := now.Add(-service.AdaptiveHistoryRetention)
	old := service.AdaptiveHistoryRecord{At: now.Add(-48 * time.Hour), StateKey: "request-class", Payload: "degraded"}
	current := service.AdaptiveHistoryRecord{At: now.Add(-36 * time.Hour), StateKey: old.StateKey, Payload: "retest-pass"}
	require.NoError(t, store.Merge(ctx, cutoff, []service.AdaptiveHistoryRecord{old, current, old}))
	rows, err := store.Read(ctx, cutoff, now)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, current.Payload, rows[0].Payload)
	require.Equal(t, current.At.UnixMicro(), rows[0].At.UnixMicro())

	server.FastForward(time.Hour)
	require.NoError(t, store.Merge(ctx, cutoff, []service.AdaptiveHistoryRecord{current}))
	require.Equal(t, adaptiveHealthTTL-time.Hour, server.TTL(adaptiveHealthIndex))
	require.Equal(t, adaptiveHealthTTL-time.Hour, server.TTL(adaptiveHealthData))

	through := current.At.Add(adaptiveHealthTTL)
	rows, err = store.Read(ctx, through.Add(-service.AdaptiveHistoryRetention), through)
	require.NoError(t, err)
	require.Empty(t, rows, "the latest observation sets the logical expiry")
	server.FastForward(adaptiveHealthTTL - time.Hour)
	require.False(t, server.Exists(adaptiveHealthIndex))
	require.False(t, server.Exists(adaptiveHealthData))
}
