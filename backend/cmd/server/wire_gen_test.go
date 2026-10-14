package main

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestProvideServiceBuildInfo(t *testing.T) {
	in := handler.BuildInfo{
		Version:   "v-test",
		BuildType: "release",
	}
	out := provideServiceBuildInfo(in)
	require.Equal(t, in.Version, out.Version)
	require.Equal(t, in.BuildType, out.BuildType)
}

// cleanupFixture 收拢 newMinimalCleanup 的结果：cleanup 为待测的关闭函数，moderation 用于验证内容审核已被 cleanup 回收。
type cleanupFixture struct {
	cleanup    func()
	moderation *service.ContentModerationService
}

// newMinimalCleanup 以最小依赖构造 provideCleanup。rdb 与两个缓存订阅服务由调用方指定，其余依赖均为空实现。
func newMinimalCleanup(rdb *redis.Client, channelSvc *service.ChannelService, tlsSvc *service.TLSFingerprintProfileService) cleanupFixture {
	cfg := &config.Config{}

	oauthSvc := service.NewOAuthService(nil, nil)
	openAIOAuthSvc := service.NewOpenAIOAuthService(nil, nil)
	geminiOAuthSvc := service.NewGeminiOAuthService(nil, nil, nil, nil, cfg)
	antigravityOAuthSvc := service.NewAntigravityOAuthService(nil)

	tokenRefreshSvc := service.NewTokenRefreshService(
		nil,
		oauthSvc,
		openAIOAuthSvc,
		geminiOAuthSvc,
		antigravityOAuthSvc,
		nil,
		nil,
		cfg,
		nil,
	)
	accountExpirySvc := service.NewAccountExpiryService(nil, time.Second)
	codexVersionSyncSvc := service.NewOpenAICodexVersionSyncService(nil, nil, nil, time.Second)
	claudeCodeVersionSyncSvc := service.NewClaudeCodeVersionSyncService(nil, nil, nil, time.Second)
	proxyExpirySvc := service.NewProxyExpiryService(nil, time.Second)
	subscriptionExpirySvc := service.NewSubscriptionExpiryService(nil, time.Second)
	pricingSvc := service.NewPricingService(cfg, nil)
	emailQueueSvc := service.NewEmailQueueService(nil, 1)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	idempotencyCleanupSvc := service.NewIdempotencyCleanupService(nil, cfg)
	schedulerSnapshotSvc := service.NewSchedulerSnapshotService(nil, nil, nil, nil, cfg)
	opsSystemLogSinkSvc := service.NewOpsSystemLogSink(nil)

	moderationSvc := service.NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	cleanup := provideCleanup(
		nil, // entClient
		rdb, // redis
		&service.OpsMetricsCollector{},
		&service.OpsAggregationService{},
		&service.OpsAlertEvaluatorService{},
		&service.OpsCleanupService{},
		&service.OpsScheduledReportService{},
		opsSystemLogSinkSvc,
		nil, // opsService
		nil, // opsIngressRejectAggregator
		nil, // apiKeyService
		nil, // authCacheInvalidationWorker
		schedulerSnapshotSvc,
		tokenRefreshSvc,
		accountExpirySvc,
		nil, // cnProviderBalanceCheck
		codexVersionSyncSvc,
		claudeCodeVersionSyncSvc,
		proxyExpirySvc,
		subscriptionExpirySvc,
		&service.UsageCleanupService{},
		idempotencyCleanupSvc,
		service.NewGeminiCachedContentService(nil, nil, cfg),
		&service.BatchImageCleanupService{},
		nil, // batchImageWorker
		pricingSvc,
		emailQueueSvc,
		billingCacheSvc,
		&service.UsageRecordWorkerPool{},
		&service.SubscriptionService{},
		oauthSvc,
		openAIOAuthSvc,
		geminiOAuthSvc,
		antigravityOAuthSvc,
		nil,           // grokOAuth
		nil,           // openAIGateway
		nil,           // scheduledTestRunner
		nil,           // backupSvc
		nil,           // paymentOrderExpiry
		nil,           // channelMonitorRunner
		nil,           // channelMonitorV2Aggregator
		nil,           // quotaFlusher
		nil,           // upstreamBillingProbe
		nil,           // ollamaCloudUsage
		nil,           // opencodeGoUsage
		nil,           // auditLog
		nil,           // openAIAutoReset
		nil,           // promptAudit
		nil,           // pluginManager
		moderationSvc, // contentModeration
		channelSvc,
		tlsSvc,
	)
	return cleanupFixture{cleanup: cleanup, moderation: moderationSvc}
}

// gatedSubscription 模拟缓存订阅 loop：ctx 取消后先关闭 cancelled，再等待 open 放行才关闭 done。
// 与真实订阅一样，done 只在 loop 结束之后关闭，用来验证 cleanup 会等订阅结束才关闭 Redis。
type gatedSubscription struct {
	cancelled chan struct{}
	release   chan struct{}
	openOnce  sync.Once
}

func newGatedSubscription() *gatedSubscription {
	return &gatedSubscription{
		cancelled: make(chan struct{}),
		release:   make(chan struct{}),
	}
}

// start 启动订阅 loop 并返回其 done 通道。
func (g *gatedSubscription) start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		close(g.cancelled)
		<-g.release
	}()
	return done
}

// open 放行订阅 loop 的结束，可重复调用。
func (g *gatedSubscription) open() {
	g.openOnce.Do(func() { close(g.release) })
}

// gatedChannelPubSub 以 gatedSubscription 实现渠道缓存订阅，NotifyUpdate 为空操作。
type gatedChannelPubSub struct {
	sub *gatedSubscription
}

func (p *gatedChannelPubSub) NotifyUpdate(context.Context) error { return nil }

func (p *gatedChannelPubSub) SubscribeUpdates(ctx context.Context, _ func()) <-chan struct{} {
	return p.sub.start(ctx)
}

// gatedTLSCache 以 gatedSubscription 实现 TLS 指纹缓存订阅；Set 直接成功，启动时的重载不会报错。
type gatedTLSCache struct {
	sub *gatedSubscription
}

func (c *gatedTLSCache) Get(context.Context) ([]*model.TLSFingerprintProfile, bool) {
	return nil, false
}

func (c *gatedTLSCache) Set(context.Context, []*model.TLSFingerprintProfile) error { return nil }

func (c *gatedTLSCache) Invalidate(context.Context) error { return nil }

func (c *gatedTLSCache) NotifyUpdate(context.Context) error { return nil }

func (c *gatedTLSCache) SubscribeUpdates(ctx context.Context, _ func()) <-chan struct{} {
	return c.sub.start(ctx)
}

// emptyTLSProfileRepo 让启动时的 reloadFromDB 读到空列表。
type emptyTLSProfileRepo struct{}

func (emptyTLSProfileRepo) List(context.Context) ([]*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (emptyTLSProfileRepo) GetByID(context.Context, int64) (*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (emptyTLSProfileRepo) Create(context.Context, *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (emptyTLSProfileRepo) Update(context.Context, *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (emptyTLSProfileRepo) Delete(context.Context, int64) error { return nil }

// newCleanupRedis 返回由 miniredis 支撑的客户端，便于断言 cleanup 是否真的关闭了它。
func newCleanupRedis(t *testing.T) *redis.Client {
	t.Helper()
	server := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

// captureCleanupLog 把标准库 log 重定向到缓冲区，用例结束时恢复。
// cleanup 的并行步骤会并发写日志，log.Logger 内部加锁；读取缓冲区须在 cleanup 返回之后。
func captureCleanupLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// waitForSignal 等待 ch 关闭；超时直接让用例失败，避免卡住整个测试包。
func waitForSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen within 5s", what)
	}
}

func TestProvideCleanup_WithMinimalDependencies_NoPanic(t *testing.T) {
	fixture := newMinimalCleanup(nil, nil, nil)

	require.NotPanics(t, func() {
		fixture.cleanup()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, fixture.moderation.Shutdown(ctx), "cleanup must already have joined content moderation")
}

func TestProvideCleanup_CacheSubscribersStopBeforeRedisClose(t *testing.T) {
	rdb := newCleanupRedis(t)
	channelSub := newGatedSubscription()
	tlsSub := newGatedSubscription()
	t.Cleanup(channelSub.open)
	t.Cleanup(tlsSub.open)
	channelSvc := service.NewChannelService(nil, nil, nil, nil, &gatedChannelPubSub{sub: channelSub})
	tlsSvc := service.NewTLSFingerprintProfileService(emptyTLSProfileRepo{}, &gatedTLSCache{sub: tlsSub})
	logs := captureCleanupLog(t)

	fixture := newMinimalCleanup(rdb, channelSvc, tlsSvc)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		fixture.cleanup()
	}()

	waitForSignal(t, channelSub.cancelled, "channel subscription observing cancel")
	waitForSignal(t, tlsSub.cancelled, "tls subscription observing cancel")
	select {
	case <-finished:
		t.Fatal("cleanup returned while cache subscriptions were still stopping")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, rdb.Ping(context.Background()).Err(), "redis must stay open until cache subscriptions stop")

	channelSub.open()
	tlsSub.open()
	waitForSignal(t, finished, "cleanup")

	require.Error(t, rdb.Ping(context.Background()).Err(), "cleanup must close redis")
	out := logs.String()
	require.Contains(t, out, "[Cleanup] ChannelService succeeded")
	require.Contains(t, out, "[Cleanup] TLSFingerprintProfileService succeeded")
	require.Contains(t, out, "[Cleanup] Redis succeeded")
	redisAt := strings.Index(out, "[Cleanup] Redis succeeded")
	require.Greater(t, redisAt, strings.Index(out, "[Cleanup] ChannelService succeeded"), "redis must close after the channel cache subscriber stops")
	require.Greater(t, redisAt, strings.Index(out, "[Cleanup] TLSFingerprintProfileService succeeded"), "redis must close after the tls cache subscriber stops")
}

func TestProvideCleanup_CacheSubscriberStopEarlyAndRepeated(t *testing.T) {
	rdb := newCleanupRedis(t)
	channelSub := newGatedSubscription()
	channelSub.open() // 已放行：订阅 loop 在 ctx 取消时立即结束
	tlsSub := newGatedSubscription()
	tlsSub.open()
	channelSvc := service.NewChannelService(nil, nil, nil, nil, &gatedChannelPubSub{sub: channelSub})
	tlsSvc := service.NewTLSFingerprintProfileService(emptyTLSProfileRepo{}, &gatedTLSCache{sub: tlsSub})
	logs := captureCleanupLog(t)

	// 服务先被提前 Stop，cleanup 随后再次 Stop，两次都应直接返回 nil。
	require.NoError(t, channelSvc.Stop(context.Background()))
	require.NoError(t, tlsSvc.Stop(context.Background()))
	newMinimalCleanup(rdb, channelSvc, tlsSvc).cleanup()

	out := logs.String()
	require.NotContains(t, out, "[Cleanup] ChannelService failed")
	require.NotContains(t, out, "[Cleanup] TLSFingerprintProfileService failed")
	require.Contains(t, out, "[Cleanup] ChannelService succeeded")
	require.Contains(t, out, "[Cleanup] TLSFingerprintProfileService succeeded")
	require.Contains(t, out, "[Cleanup] Redis succeeded")
	require.Contains(t, out, "[Cleanup] All cleanup steps completed")
	require.Error(t, rdb.Ping(context.Background()).Err())
}

func TestProvideCleanup_CacheSubscriberTimeoutIsReportedAsFailure(t *testing.T) {
	rdb := newCleanupRedis(t)
	channelSub := newGatedSubscription()
	t.Cleanup(channelSub.open) // 订阅卡住时 Stop 只能等到 10s 预算耗尽；用例结束时放行，避免 goroutine 泄漏
	channelSvc := service.NewChannelService(nil, nil, nil, nil, &gatedChannelPubSub{sub: channelSub})
	logs := captureCleanupLog(t)

	start := time.Now()
	newMinimalCleanup(rdb, channelSvc, nil).cleanup()
	elapsed := time.Since(start)

	require.True(t, elapsed >= 9*time.Second, "cleanup must wait for the stuck subscriber until its budget expires, elapsed=%s", elapsed)
	out := logs.String()
	require.Contains(t, out, "[Cleanup] ChannelService failed: context deadline exceeded")
	require.Contains(t, out, "[Cleanup] Redis succeeded")
	require.Contains(t, out, "[Cleanup] Warning: cleanup timed out after 10 seconds")
	require.Error(t, rdb.Ping(context.Background()).Err())
}

func TestProvideCleanup_NilCacheSubscribersStillCloseRedis(t *testing.T) {
	rdb := newCleanupRedis(t)
	logs := captureCleanupLog(t)

	newMinimalCleanup(rdb, nil, nil).cleanup()

	out := logs.String()
	require.Contains(t, out, "[Cleanup] ChannelService succeeded")
	require.Contains(t, out, "[Cleanup] TLSFingerprintProfileService succeeded")
	require.Contains(t, out, "[Cleanup] Redis succeeded")
	require.Contains(t, out, "[Cleanup] All cleanup steps completed")
	require.Error(t, rdb.Ping(context.Background()).Err())
}
