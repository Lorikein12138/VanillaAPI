//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

// lifecycleChannelPubSub 按订阅停止契约模拟渠道缓存订阅：收到通知时经 wrap 调用 handler；
// ctx 取消后若设置了 hold，则等待 hold 关闭才关闭 done，用来模拟卡住的订阅 loop。
type lifecycleChannelPubSub struct {
	notify chan struct{}
	wrap   func(handler func()) func()
	hold   <-chan struct{}
}

func (p *lifecycleChannelPubSub) NotifyUpdate(context.Context) error { return nil }

func (p *lifecycleChannelPubSub) SubscribeUpdates(ctx context.Context, handler func()) <-chan struct{} {
	if p.wrap != nil {
		handler = p.wrap(handler)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				if p.hold != nil {
					<-p.hold
				}
				return
			case <-p.notify:
				if ctx.Err() == nil {
					handler()
				}
			}
		}
	}()
	return done
}

// lifecycleTLSRepo 只实现 List。failOnCancel 为真时，ctx 取消后 List 返回 ctx 错误，模拟停机途中的数据库读取。
type lifecycleTLSRepo struct {
	failOnCancel bool
}

func (r *lifecycleTLSRepo) List(ctx context.Context) ([]*model.TLSFingerprintProfile, error) {
	if r.failOnCancel && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, nil
}

func (r *lifecycleTLSRepo) GetByID(context.Context, int64) (*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (r *lifecycleTLSRepo) Create(context.Context, *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (r *lifecycleTLSRepo) Update(context.Context, *model.TLSFingerprintProfile) (*model.TLSFingerprintProfile, error) {
	return nil, nil
}

func (r *lifecycleTLSRepo) Delete(context.Context, int64) error { return nil }

// lifecycleTLSCache 记录订阅 handler。Set 与真实缓存一样受 ctx 约束：ctx 已取消或超时则返回其错误。
type lifecycleTLSCache struct {
	handler func()
}

func (c *lifecycleTLSCache) Get(context.Context) ([]*model.TLSFingerprintProfile, bool) {
	return nil, false
}

func (c *lifecycleTLSCache) Set(ctx context.Context, _ []*model.TLSFingerprintProfile) error {
	return ctx.Err()
}

func (c *lifecycleTLSCache) Invalidate(context.Context) error { return nil }

func (c *lifecycleTLSCache) NotifyUpdate(context.Context) error { return nil }

func (c *lifecycleTLSCache) SubscribeUpdates(ctx context.Context, handler func()) <-chan struct{} {
	c.handler = handler
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
	}()
	return done
}

type lifecycleSlogRecord struct {
	Level  string `json:"level"`
	Msg    string `json:"msg"`
	Reason string `json:"reason"`
}

// captureCacheSubscriberSlog 把全局 slog 默认 handler 换成 DEBUG 级别的 JSON 缓冲，用例结束时恢复。
// 须在 captureStructuredLog 之后调用：后者的 logger.Init 会重设全局 slog 默认 handler。
// 返回的函数按消息名取出记录；调用方不得并行运行。
func captureCacheSubscriberSlog(t *testing.T) func(msg string) []lifecycleSlogRecord {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func(msg string) []lifecycleSlogRecord {
		var out []lifecycleSlogRecord
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec lifecycleSlogRecord
			require.NoError(t, json.Unmarshal([]byte(line), &rec))
			if rec.Msg == msg {
				out = append(out, rec)
			}
		}
		return out
	}
}

// lifecycleEventsContaining 返回 zap 结构化日志中消息包含 substr 的事件。
func lifecycleEventsContaining(sink *inMemoryLogSink, substr string) []*logger.LogEvent {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	var out []*logger.LogEvent
	for _, event := range sink.events {
		if strings.Contains(event.Message, substr) {
			out = append(out, event)
		}
	}
	return out
}

func TestCacheSubscriberLifecycleChannelStopWaitsForInFlightCallback(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	pubsub := &lifecycleChannelPubSub{
		notify: make(chan struct{}, 1),
		wrap: func(handler func()) func() {
			return func() {
				close(entered)
				<-release
				handler()
			}
		},
	}
	svc := NewChannelService(nil, nil, nil, nil, pubsub)
	pubsub.notify <- struct{}{}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("subscription callback did not start")
	}

	stopped := make(chan error, 1)
	go func() { stopped <- svc.Stop(context.Background()) }()

	select {
	case err := <-stopped:
		t.Fatalf("Stop returned %v while a callback was still running", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after the callback finished")
	}
}

func TestCacheSubscriberLifecycleRepeatedStopAndNilReceivers(t *testing.T) {
	svc := NewChannelService(nil, nil, nil, nil, &lifecycleChannelPubSub{notify: make(chan struct{}, 1)})
	require.NoError(t, svc.Stop(context.Background()))
	require.NoError(t, svc.Stop(context.Background()))

	require.NoError(t, NewChannelService(nil, nil, nil, nil, nil).Stop(context.Background()))

	var nilChannel *ChannelService
	require.NoError(t, nilChannel.Stop(context.Background()))
	var nilTLS *TLSFingerprintProfileService
	require.NoError(t, nilTLS.Stop(context.Background()))
}

func TestCacheSubscriberLifecycleStopTimeoutCanWaitAgain(t *testing.T) {
	hold := make(chan struct{})
	svc := NewChannelService(nil, nil, nil, nil, &lifecycleChannelPubSub{notify: make(chan struct{}, 1), hold: hold})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, svc.Stop(ctx), context.DeadlineExceeded)

	close(hold)
	require.NoError(t, svc.Stop(context.Background()))
}

func TestCacheSubscriberLifecycleStopWithCancelledContextReturnsCtxErr(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	svc := NewChannelService(nil, nil, nil, nil, &lifecycleChannelPubSub{notify: make(chan struct{}, 1), hold: hold})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, svc.Stop(ctx), context.Canceled)
}

func TestCacheSubscriberLifecycleTLSWithoutCacheStopsImmediately(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&lifecycleTLSRepo{}, nil)
	require.NoError(t, svc.Stop(context.Background()))
	require.NoError(t, svc.Stop(context.Background()))
}

func TestCacheSubscriberLifecycleTLSRefreshAfterStopLogsDebug(t *testing.T) {
	cache := &lifecycleTLSCache{}
	svc := NewTLSFingerprintProfileService(&lifecycleTLSRepo{failOnCancel: true}, cache)
	require.NoError(t, svc.Stop(context.Background()))

	sink, restore := captureStructuredLog(t)
	t.Cleanup(restore)
	records := captureCacheSubscriberSlog(t)

	cache.handler()

	stopped := records("tls_fp_profile_refresh_stopped")
	require.Len(t, stopped, 1)
	require.Equal(t, "DEBUG", stopped[0].Level)
	require.Equal(t, "context_done", stopped[0].Reason)
	require.Empty(t, lifecycleEventsContaining(sink, "Failed to refresh cache on notification"))
}

func TestCacheSubscriberLifecycleTLSCacheSetAfterStopLogsDebug(t *testing.T) {
	cache := &lifecycleTLSCache{}
	svc := NewTLSFingerprintProfileService(&lifecycleTLSRepo{}, cache)
	require.NoError(t, svc.Stop(context.Background()))

	sink, restore := captureStructuredLog(t)
	t.Cleanup(restore)
	records := captureCacheSubscriberSlog(t)

	cache.handler()

	setStopped := records("tls_fp_profile_cache_set_stopped")
	require.Len(t, setStopped, 1)
	require.Equal(t, "DEBUG", setStopped[0].Level)
	require.Empty(t, lifecycleEventsContaining(sink, "Failed to set cache"))
}

func TestCacheSubscriberLifecycleCRUDDeadlineKeepsFailureLog(t *testing.T) {
	svc := NewTLSFingerprintProfileService(&lifecycleTLSRepo{}, &lifecycleTLSCache{})
	t.Cleanup(func() { _ = svc.Stop(context.Background()) })

	sink, restore := captureStructuredLog(t)
	t.Cleanup(restore)
	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()

	require.NoError(t, svc.reloadFromDB(ctx))

	require.NotEmpty(t, lifecycleEventsContaining(sink, "Failed to set cache"))
}
