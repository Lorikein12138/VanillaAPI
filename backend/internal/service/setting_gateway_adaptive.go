package service

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

const (
	gatewayAdaptiveSchedulingCacheTTL  = 60 * time.Second
	gatewayAdaptiveSchedulingErrorTTL  = 5 * time.Second
	gatewayAdaptiveSchedulingDBTimeout = 5 * time.Second
)

type cachedGatewayAdaptiveScheduling struct {
	enabled   bool
	expiresAt int64
}

func newGatewayAdaptiveSchedulingCache(enabled bool, ttl time.Duration) *cachedGatewayAdaptiveScheduling {
	return &cachedGatewayAdaptiveScheduling{
		enabled:   enabled,
		expiresAt: time.Now().Add(ttl).UnixNano(),
	}
}

// IsGatewayAdaptiveSchedulingEnabled reports whether adaptive candidate scoring is enabled.
// The hot path reads a per-service cache, with concurrent refreshes sharing one lookup.
func (s *SettingService) IsGatewayAdaptiveSchedulingEnabled(ctx context.Context) bool {
	if s == nil || s.settingRepo == nil {
		return false
	}
	if cached := s.gatewayAdaptiveSchedulingCache.Load(); cached != nil && time.Now().UnixNano() < cached.expiresAt {
		return cached.enabled
	}

	result, _, _ := s.gatewayAdaptiveSchedulingSF.Do(SettingKeyGatewayAdaptiveSchedulingEnabled, func() (any, error) {
		previous := s.gatewayAdaptiveSchedulingCache.Load()
		if previous != nil && time.Now().UnixNano() < previous.expiresAt {
			return previous.enabled, nil
		}

		dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gatewayAdaptiveSchedulingDBTimeout)
		defer cancel()
		value, err := s.settingRepo.GetValue(dbCtx, SettingKeyGatewayAdaptiveSchedulingEnabled)
		ttl := gatewayAdaptiveSchedulingCacheTTL
		if err != nil && !errors.Is(err, ErrSettingNotFound) {
			slog.Warn("failed to read adaptive gateway scheduling setting", "error", err)
			ttl = gatewayAdaptiveSchedulingErrorTTL
		}
		refreshed := newGatewayAdaptiveSchedulingCache(err == nil && value == "true", ttl)
		// A successful settings write may have replaced the cache during this lookup.
		s.gatewayAdaptiveSchedulingCache.CompareAndSwap(previous, refreshed)
		return s.gatewayAdaptiveSchedulingCache.Load().enabled, nil
	})
	enabled, _ := result.(bool)
	return enabled
}

// refreshGatewayAdaptiveSchedulingAfterWrite publishes the committed value while
// the settings write lock is still held. Omitted keys leave the cache untouched.
func (s *SettingService) refreshGatewayAdaptiveSchedulingAfterWrite(updates map[string]string) {
	if value, written := updates[SettingKeyGatewayAdaptiveSchedulingEnabled]; written {
		s.gatewayAdaptiveSchedulingCache.Store(newGatewayAdaptiveSchedulingCache(value == "true", gatewayAdaptiveSchedulingCacheTTL))
	}
}
