package service

import (
	"testing"
	"time"
)

// newTestChannelServiceForStats creates a ChannelService with a single channel
// mapped to the given groupID, suitable for resolveAccountStatsCost tests.
// It lives in an untagged test file so unit-tagged and untagged tests can share it.
func newTestChannelServiceForStats(t *testing.T, channel *Channel, groupID int64, platform string) *ChannelService {
	t.Helper()
	cache := newEmptyChannelCache()
	cache.channelByGroupID[groupID] = channel
	cache.groupPlatform[groupID] = platform
	expandPricingToCache(cache, channel, groupID, platform)
	cs := &ChannelService{}
	cache.loadedAt = time.Now()
	cs.cache.Store(cache)
	return cs
}
