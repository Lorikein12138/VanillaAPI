package repository

import (
	"context"
	"errors"
	"log/slog"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const channelCachePubSubKey = "channel_cache_updated"

// Exit reasons of a cache subscriber loop, shared by the channel and TLS fingerprint caches.
const (
	pubSubStopContextDone   = "context_done"
	pubSubStopChannelClosed = "channel_closed"
)

type channelCache struct {
	rdb *redis.Client
}

// NewChannelCache creates the Redis-backed channel cache invalidation bus.
func NewChannelCache(rdb *redis.Client) service.ChannelCachePubSub {
	return &channelCache{rdb: rdb}
}

// NotifyUpdate notifies all instances to invalidate their local channel cache.
func (c *channelCache) NotifyUpdate(ctx context.Context) error {
	return c.rdb.Publish(ctx, channelCachePubSubKey, "refresh").Err()
}

// SubscribeUpdates subscribes to channel cache invalidation notifications.
// The returned channel is closed only after the PubSub has been closed and the handler
// has returned, so a caller waiting on it observes a fully stopped subscriber.
func (c *channelCache) SubscribeUpdates(ctx context.Context, handler func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		pubsub := c.rdb.Subscribe(ctx, channelCachePubSubKey)
		defer closePubSubSubscription(ctx, pubsub, "channel cache subscriber")

		reason := consumePubSubMessages(ctx, pubsub.Channel(), handler)
		logPubSubStopped("channel cache subscriber stopped", reason)
	}()
	return done
}

// consumePubSubMessages delivers notifications to handle until ctx ends or the channel closes.
// nil messages on an open channel are ignored, and ctx is checked before every handler call,
// so no new callback starts once cancellation has been observed.
func consumePubSubMessages(ctx context.Context, messages <-chan *redis.Message, handle func()) string {
	for {
		select {
		case <-ctx.Done():
			return pubSubStopContextDone
		case message, ok := <-messages:
			if !ok {
				if ctx.Err() != nil {
					return pubSubStopContextDone
				}
				return pubSubStopChannelClosed
			}
			if message == nil {
				continue
			}
			if ctx.Err() != nil {
				return pubSubStopContextDone
			}
			handle()
		}
	}
}

// logPubSubStopped logs the loop exit: a context-driven stop is routine (DEBUG),
// while a channel closed under a live context is an unexpected disconnect (WARN).
func logPubSubStopped(msg, reason string) {
	if reason == pubSubStopContextDone {
		slog.Debug(msg, "reason", reason)
		return
	}
	slog.Warn(msg, "reason", reason)
}

// closePubSubSubscription closes the subscription. Errors after a context-driven stop are
// expected duplicates and are logged at DEBUG only; errors while the context is live stay WARN.
func closePubSubSubscription(ctx context.Context, pubsub *redis.PubSub, name string) {
	err := pubsub.Close()
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		slog.Debug(name+" closed after cancel", "error", err)
		return
	}
	slog.Warn("failed to close "+name, "error", err)
}

// isStoppedByCancel reports whether err was caused by ctx being explicitly cancelled, as Stop does.
// A deadline expiry is not treated as a stop, so timed-out CRUD refreshes keep their WARN level.
func isStoppedByCancel(ctx context.Context, err error) bool {
	return errors.Is(ctx.Err(), context.Canceled) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
