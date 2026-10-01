package repository

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

var admissionCacheBackend = "miniredis"
var admissionCacheClient = func(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, rdb.Close()) })
	return rdb
}

func TestAdmissionInflightBalanceCacheContract(t *testing.T) {
	rdb := admissionCacheClient(t)
	t.Logf("backend=%s", admissionCacheBackend)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cache := &billingCache{rdb: rdb}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := cache.ReserveInflightBalance(ctx, 731, fmt.Sprint(i), 1, 8, time.Minute)
			require.NoError(t, err)
			if ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int64(8), admitted.Load(), "atomic Lua must admit only covered requests")
	zkey, hkey := billingInflightKeys(731)
	members, err := rdb.ZRange(ctx, zkey, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, members, 8)
	hashLen, err := rdb.HLen(ctx, hkey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(8), hashLen)
	t.Log("stage=admitted")
	member := members[0]
	oldScore := rdb.ZScore(ctx, zkey, member).Val()
	renewed, err := cache.RenewInflightBalance(ctx, 731, member, 2*time.Minute)
	require.NoError(t, err)
	require.True(t, renewed)
	require.Greater(t, rdb.ZScore(ctx, zkey, member).Val(), oldScore)
	require.Greater(t, rdb.PTTL(ctx, zkey).Val(), time.Minute)
	require.Greater(t, rdb.PTTL(ctx, hkey).Val(), time.Minute)
	t.Log("stage=renewed")
	// Explicit server score makes expiration deterministic on both backends.
	require.NoError(t, rdb.ZAdd(ctx, zkey, redis.Z{Score: float64(time.Now().Add(-time.Second).UnixMilli()), Member: member}).Err())
	renewed, err = cache.RenewInflightBalance(ctx, 731, member, time.Minute)
	require.NoError(t, err)
	require.False(t, renewed, "expired member must not be resurrected")
	exists, err := rdb.HExists(ctx, hkey, member).Result()
	require.NoError(t, err)
	require.False(t, exists)
	require.ErrorIs(t, rdb.ZScore(ctx, zkey, member).Err(), redis.Nil)
	t.Log("stage=expired")
	for _, member := range members {
		require.NoError(t, cache.ReleaseInflightBalance(ctx, 731, member))
		require.NoError(t, cache.ReleaseInflightBalance(ctx, 731, member))
	}
	require.Zero(t, rdb.ZCard(ctx, zkey).Val())
	hashLen, err = rdb.HLen(ctx, hkey).Result()
	require.NoError(t, err)
	require.Zero(t, hashLen)
	t.Log("stage=released")
	// Reservation itself removes both fields of expired members before summing.
	ok, _, err := cache.ReserveInflightBalance(ctx, 731, "expired", 100, 8, time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "first request retains upstream admission semantics")
	require.NoError(t, rdb.ZAdd(ctx, zkey, redis.Z{Score: 0, Member: "expired"}).Err())
	ok, sum, err := cache.ReserveInflightBalance(ctx, 731, "new", 1, 8, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	require.Zero(t, sum)
	exists, err = rdb.HExists(ctx, hkey, "expired").Result()
	require.NoError(t, err)
	require.False(t, exists)
	require.Equal(t, int64(1), rdb.ZCard(ctx, zkey).Val())
	hashLen, err = rdb.HLen(ctx, hkey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), hashLen)
	t.Log("stage=expired-reservation-cleaned")
}

func TestAdmissionAPIKeyCreateCountContract(t *testing.T) {
	rdb := admissionCacheClient(t)
	t.Logf("backend=%s", admissionCacheBackend)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cache := &apiKeyCache{rdb: rdb}
	count, err := cache.IncrementCreateCount(ctx, 732, time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	key := apiKeyCreateCountKey(732)
	require.Greater(t, rdb.PTTL(ctx, key).Val(), 59*time.Minute)
	require.NoError(t, rdb.PExpire(ctx, key, 10*time.Second).Err())
	count, err = cache.IncrementCreateCount(ctx, 732, time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(2), count)
	require.LessOrEqual(t, rdb.PTTL(ctx, key).Val(), 10*time.Second, "later INCR cannot extend TTL")
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.IncrementCreateCount(ctx, 732, time.Hour)
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	require.Equal(t, "34", rdb.Get(ctx, key).Val())
	count, err = cache.IncrementCreateCount(ctx, 733, time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "users are isolated")
	// PEXPIRE with a past deadline is server-side expiry, not a mock clock.
	require.NoError(t, rdb.PExpire(ctx, key, -time.Second).Err())
	require.Zero(t, rdb.Exists(ctx, key).Val())
	count, err = cache.IncrementCreateCount(ctx, 732, time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), count)
	require.Greater(t, rdb.PTTL(ctx, key).Val(), 59*time.Minute)
}
