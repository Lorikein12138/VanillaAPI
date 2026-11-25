package repository

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAdaptiveHistoryStoreStreamHealthIsolatedIdempotentAndExpiring(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	store := ProvideAdaptiveHistoryStore(rdb)
	stream := store.(service.AdaptiveStreamHistoryStore)
	now := time.Unix(1_800_000_000, 0)
	cutoff := now.Add(-15 * time.Minute)
	require.NoError(t, store.Merge(ctx, now.Add(-service.AdaptiveHistoryRetention), []service.AdaptiveHistoryRecord{{At: now, Payload: "legacy"}}))
	records := []service.AdaptiveHistoryRecord{
		{At: cutoff, Payload: "excluded-boundary"},
		{At: cutoff.Add(time.Microsecond), Payload: "oldest"},
		{At: now, Payload: "seen"},
		{At: now, Payload: strings.Repeat("x", service.AdaptiveHistoryMaxMember+1)},
	}
	for range 2 {
		trimmed, err := stream.MergeStreamHealth(ctx, cutoff, records)
		require.NoError(t, err)
		require.Zero(t, trimmed)
	}
	rows, err := stream.ReadStreamHealth(ctx, cutoff, now)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, "seen", rows[0].Payload)
	require.Equal(t, "oldest", rows[1].Payload)
	legacy, err := store.Read(ctx, cutoff, now)
	require.NoError(t, err)
	require.Len(t, legacy, 1)
	require.Equal(t, "legacy", legacy[0].Payload)
	server.FastForward(15 * time.Minute)
	require.False(t, server.Exists(adaptiveStreamHistoryKey))
	require.True(t, server.Exists(adaptiveHistoryKey))
}

func TestAdaptiveHistoryStoreStreamHealthCapsNewestMembers(t *testing.T) {
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	stream := ProvideAdaptiveHistoryStore(rdb).(service.AdaptiveStreamHistoryStore)
	now := time.Unix(1_800_000_000, 0)
	cutoff := now.Add(-15 * time.Minute)
	rows := make([]redis.Z, adaptiveHistoryLimit)
	for i := range rows {
		rows[i] = redis.Z{Score: float64(cutoff.Add(time.Duration(i+1) * time.Microsecond).UnixMicro()), Member: strconv.Itoa(i)}
	}
	require.NoError(t, rdb.ZAdd(ctx, adaptiveStreamHistoryKey, rows...).Err())
	trimmed, err := stream.MergeStreamHealth(ctx, cutoff, []service.AdaptiveHistoryRecord{{At: now, Payload: "newest"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, trimmed)
	records, err := stream.ReadStreamHealth(ctx, cutoff, now)
	require.NoError(t, err)
	require.Len(t, records, adaptiveHistoryLimit)
	require.Equal(t, "newest", records[0].Payload)
	require.Equal(t, "1", records[len(records)-1].Payload)
}
