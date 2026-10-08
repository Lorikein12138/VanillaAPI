//go:build unit

package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type tlsCacheSlogRecord struct {
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
}

// captureTLSCacheSlog 把全局 slog 默认 handler 换成 DEBUG 级别的 JSON 缓冲，用例结束时恢复。
// 返回的函数按消息名取出记录；调用方不得并行运行。
func captureTLSCacheSlog(t *testing.T) func(msg string) []tlsCacheSlogRecord {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func(msg string) []tlsCacheSlogRecord {
		var out []tlsCacheSlogRecord
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec tlsCacheSlogRecord
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			if rec.Msg == msg {
				out = append(out, rec)
			}
		}
		return out
	}
}

func TestTLSFingerprintProfileCachePubSubClearsLocalCacheBeforeHandler(t *testing.T) {
	redisServer := miniredis.RunT(t)
	publisherRedis := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	subscriberRedis := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() {
		_ = publisherRedis.Close()
		_ = subscriberRedis.Close()
	})

	publisher := &tlsFingerprintProfileCache{rdb: publisherRedis}
	subscriber := &tlsFingerprintProfileCache{rdb: subscriberRedis}
	subscriber.localCache = []*model.TLSFingerprintProfile{{ID: 1}}

	cleared := make(chan bool, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := subscriber.SubscribeUpdates(ctx, func() {
		subscriber.localMu.RLock()
		isCleared := subscriber.localCache == nil
		subscriber.localMu.RUnlock()
		select {
		case cleared <- isCleared:
		default:
		}
	})

	require.Eventually(t, func() bool {
		return redisServer.PubSubNumSub(tlsFPProfilePubSubKey)[tlsFPProfilePubSubKey] == 1
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, publisher.NotifyUpdate(context.Background()))

	select {
	case isCleared := <-cleared:
		require.True(t, isCleared, "local cache must be cleared before the handler runs")
	case <-time.After(time.Second):
		t.Fatal("tls fingerprint cache invalidation notification was not received")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("tls fingerprint cache subscriber did not stop after cancel")
	}
}

func TestTLSFingerprintProfileCacheGetStoppedByCancelLogsDebug(t *testing.T) {
	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	cache := &tlsFingerprintProfileCache{rdb: rdb}

	records := captureTLSCacheSlog(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	profiles, ok := cache.Get(ctx)

	require.Nil(t, profiles)
	require.False(t, ok)
	stopped := records("tls_fp_profile_cache_get_failed")
	require.Len(t, stopped, 1)
	require.Equal(t, "DEBUG", stopped[0].Level)
	require.Equal(t, "context_done", stopped[0].Reason)
}

func TestTLSFingerprintProfileCacheGetClosedClientStillWarns(t *testing.T) {
	redisServer := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	require.NoError(t, rdb.Close())
	cache := &tlsFingerprintProfileCache{rdb: rdb}

	records := captureTLSCacheSlog(t)

	profiles, ok := cache.Get(context.Background())

	require.Nil(t, profiles)
	require.False(t, ok)
	failed := records("tls_fp_profile_cache_get_failed")
	require.Len(t, failed, 1)
	require.Equal(t, "WARN", failed[0].Level)
}
