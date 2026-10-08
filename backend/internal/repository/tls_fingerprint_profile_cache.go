package repository

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	tlsFPProfileCacheKey  = "tls_fingerprint_profiles"
	tlsFPProfilePubSubKey = "tls_fingerprint_profiles_updated"
	tlsFPProfileCacheTTL  = 24 * time.Hour
)

type tlsFingerprintProfileCache struct {
	rdb        *redis.Client
	localCache []*model.TLSFingerprintProfile
	localMu    sync.RWMutex
}

// NewTLSFingerprintProfileCache 创建 TLS 指纹模板缓存
func NewTLSFingerprintProfileCache(rdb *redis.Client) service.TLSFingerprintProfileCache {
	return &tlsFingerprintProfileCache{
		rdb: rdb,
	}
}

// Get 从缓存获取模板列表
func (c *tlsFingerprintProfileCache) Get(ctx context.Context) ([]*model.TLSFingerprintProfile, bool) {
	c.localMu.RLock()
	if c.localCache != nil {
		profiles := c.localCache
		c.localMu.RUnlock()
		return profiles, true
	}
	c.localMu.RUnlock()

	data, err := c.rdb.Get(ctx, tlsFPProfileCacheKey).Bytes()
	if err != nil {
		if err != redis.Nil {
			// 订阅被 Stop 取消时的读取中断属于正常停止，不作为告警
			if isStoppedByCancel(ctx, err) {
				slog.Debug("tls_fp_profile_cache_get_failed", "reason", "context_done", "error", err)
			} else {
				slog.Warn("tls_fp_profile_cache_get_failed", "error", err)
			}
		}
		return nil, false
	}

	var profiles []*model.TLSFingerprintProfile
	if err := json.Unmarshal(data, &profiles); err != nil {
		slog.Warn("tls_fp_profile_cache_unmarshal_failed", "error", err)
		return nil, false
	}

	c.localMu.Lock()
	c.localCache = profiles
	c.localMu.Unlock()

	return profiles, true
}

// Set 设置缓存
func (c *tlsFingerprintProfileCache) Set(ctx context.Context, profiles []*model.TLSFingerprintProfile) error {
	data, err := json.Marshal(profiles)
	if err != nil {
		return err
	}

	if err := c.rdb.Set(ctx, tlsFPProfileCacheKey, data, tlsFPProfileCacheTTL).Err(); err != nil {
		return err
	}

	c.localMu.Lock()
	c.localCache = profiles
	c.localMu.Unlock()

	return nil
}

// Invalidate 使缓存失效
func (c *tlsFingerprintProfileCache) Invalidate(ctx context.Context) error {
	c.localMu.Lock()
	c.localCache = nil
	c.localMu.Unlock()

	return c.rdb.Del(ctx, tlsFPProfileCacheKey).Err()
}

// NotifyUpdate 通知其他实例刷新缓存
func (c *tlsFingerprintProfileCache) NotifyUpdate(ctx context.Context) error {
	return c.rdb.Publish(ctx, tlsFPProfilePubSubKey, "refresh").Err()
}

// SubscribeUpdates 订阅缓存更新通知
// 返回的 channel 只在 PubSub 已关闭且 handler 已返回后关闭，供 Stop 等待订阅 loop 完全退出
func (c *tlsFingerprintProfileCache) SubscribeUpdates(ctx context.Context, handler func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		sub := c.rdb.Subscribe(ctx, tlsFPProfilePubSubKey)
		defer closePubSubSubscription(ctx, sub, "tls fingerprint profile cache subscriber")

		reason := consumePubSubMessages(ctx, sub.Channel(), func() {
			// 先清空本地缓存再通知上层，保证 handler 触发的刷新不会读到旧数据
			c.localMu.Lock()
			c.localCache = nil
			c.localMu.Unlock()

			handler()
		})
		logPubSubStopped("tls_fp_profile_cache_subscriber_stopped", reason)
	}()
	return done
}
