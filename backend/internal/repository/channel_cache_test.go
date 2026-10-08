package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestChannelCachePubSub(t *testing.T) {
	redisServer := miniredis.RunT(t)
	publisherRedis := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	subscriberRedis := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	t.Cleanup(func() {
		_ = publisherRedis.Close()
		_ = subscriberRedis.Close()
	})

	publisher := NewChannelCache(publisherRedis)
	subscriber := NewChannelCache(subscriberRedis)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	received := make(chan struct{}, 1)
	done := subscriber.SubscribeUpdates(ctx, func() {
		select {
		case received <- struct{}{}:
		default:
		}
	})

	require.Eventually(t, func() bool {
		return redisServer.PubSubNumSub(channelCachePubSubKey)[channelCachePubSubKey] == 1
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, publisher.NotifyUpdate(context.Background()))

	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("channel cache invalidation notification was not received")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("channel cache subscriber did not stop after cancel")
	}
}

func TestChannelCacheConsumeMessagesIgnoresNilAndStopsOnClose(t *testing.T) {
	messages := make(chan *redis.Message, 2)
	messages <- nil
	messages <- &redis.Message{Channel: channelCachePubSubKey}
	close(messages)

	calls := 0
	reason := consumePubSubMessages(context.Background(), messages, func() { calls++ })

	require.Equal(t, pubSubStopChannelClosed, reason)
	require.Equal(t, 1, calls)
}

func TestChannelCacheConsumeMessagesDoesNotHandleAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	messages := make(chan *redis.Message, 1)
	messages <- &redis.Message{Channel: channelCachePubSubKey}

	calls := 0
	reason := consumePubSubMessages(ctx, messages, func() { calls++ })

	require.Equal(t, pubSubStopContextDone, reason)
	require.Zero(t, calls)
}

func TestChannelCacheIsStoppedByCancelOnlyForExplicitCancel(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	require.True(t, isStoppedByCancel(canceled, context.Canceled))

	expired, cancelExpired := context.WithTimeout(context.Background(), 0)
	defer cancelExpired()
	require.False(t, isStoppedByCancel(expired, context.DeadlineExceeded))
	require.False(t, isStoppedByCancel(canceled, errors.New("boom")))
}
