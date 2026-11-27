//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type tempProtectionAdaptiveSetting struct{ service.SettingRepository }

func (tempProtectionAdaptiveSetting) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyGatewayAdaptiveSchedulingEnabled {
		return "true", nil
	}
	return "", service.ErrSettingNotFound
}

func TestSchedulerCacheProtectedProjectionRejectsDelayedWritersAndSelection(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxkey.ForcePlatform, service.PlatformAnthropic)
	cache, mr := newSchedulerCacheUnitWithRedis(t)
	// A different client represents the delayed OAuth or rebuild process.
	delayedClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = delayedClient.Close() })
	delayed := newSchedulerCacheWithChunkSizes(delayedClient, defaultSchedulerSnapshotMGetChunkSize, defaultSchedulerSnapshotWriteChunkSize)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mr.SetTime(now)
	groups := []int64{14, 27}
	stale := service.Account{
		ID: 1, Name: "stale", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, GroupIDs: groups,
		UpdatedAt: now.Add(-time.Second), Priority: 1,
	}
	alternative := stale
	alternative.ID, alternative.Name, alternative.Priority = 2, "alternative", 100
	protected := stale
	hold, effective := now.Add(24*time.Hour), now.Add(48*time.Hour)
	protected.Name, protected.UpdatedAt = "protected", now
	protected.TempUnschedulableUntil, protected.TempUnschedulableProtectedUntil = &effective, &hold
	bucketFor := func(id int64) service.SchedulerBucket {
		return service.SchedulerBucket{GroupID: id, Platform: service.PlatformAnthropic, Mode: service.SchedulerModeForced}
	}
	publish := func(groupID int64, account service.Account) {
		t.Helper()
		bucket := bucketFor(groupID)
		token, err := cache.CaptureBucketWriteToken(ctx, bucket)
		require.NoError(t, err)
		require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account, alternative}))
	}
	for _, groupID := range groups {
		publish(groupID, stale)
	}

	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Gateway.Scheduling.DbFallbackEnabled = false
	snapshot := service.NewSchedulerSnapshotService(cache, nil, nil, nil, cfg)
	sticky := NewGatewayCache(cache.rdb)
	gateway := service.NewGatewayService(
		nil, nil, nil, nil, nil, nil, nil, sticky, cfg, snapshot, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	adaptive := service.WithGatewayAdaptiveIngress(ctx, service.NewSettingService(tempProtectionAdaptiveSetting{}, cfg), "POST /v1/messages", "http")
	for _, id := range groups {
		require.NoError(t, sticky.SetSessionAccountID(ctx, id, "pinned", stale.ID, time.Hour))
		selected, err := gateway.SelectAccount(ctx, &id, "pinned")
		require.NoError(t, err)
		require.Equal(t, stale.ID, selected.ID)
	}

	permitLateWrite := make(chan struct{})
	finished := make(chan error, 1)
	go func(old service.Account) {
		<-permitLateWrite
		finished <- delayed.SetAccount(ctx, &old)
	}(stale)
	// Publish the confirmed account and then rebuild both group buckets without it,
	// as the existing account-changed outbox consumer does.
	require.NoError(t, cache.SetAccount(ctx, &protected))
	for _, groupID := range groups {
		bucket := bucketFor(groupID)
		token, err := cache.CaptureBucketWriteToken(ctx, bucket)
		require.NoError(t, err)
		require.NoError(t, cache.SetSnapshotByAccountIDs(ctx, bucket, token, []int64{alternative.ID}))
	}
	close(permitLateWrite)
	require.NoError(t, <-finished)
	full, err := cache.GetAccount(ctx, stale.ID)
	require.NoError(t, err)
	require.Equal(t, protected.Name, full.Name)
	require.Equal(t, effective, *full.TempUnschedulableUntil)
	require.Equal(t, hold, *full.TempUnschedulableProtectedUntil)

	// A late bucket rebuild can republish membership, but cannot replace either
	// protected projection or shorten a pre-existing longer suspension.
	shortened := protected
	shortened.TempUnschedulableUntil = &hold
	for _, id := range groups {
		publish(id, shortened)
		accounts, ok, err := cache.GetSnapshot(ctx, bucketFor(id))
		require.NoError(t, err)
		require.True(t, ok)
		require.Len(t, accounts, 2, "include the stale member so selection must filter its protected metadata")
		require.Equal(t, effective, *accounts[0].TempUnschedulableUntil)
		for _, selectionCtx := range []context.Context{ctx, adaptive} {
			for _, session := range []string{"", "pinned"} {
				if session != "" {
					require.NoError(t, sticky.SetSessionAccountID(ctx, id, session, stale.ID, time.Hour))
				}
				selected, err := gateway.SelectAccount(selectionCtx, &id, session)
				require.NoError(t, err)
				require.Equal(t, alternative.ID, selected.ID)
			}
		}
	}

	// Once protection expires, a normal projection can replace it with the
	// account's authoritative remaining state. Unprotected writes never fence.
	mr.SetTime(hold.Add(time.Second))
	remaining := protected
	remaining.TempUnschedulableProtectedUntil = nil
	require.NoError(t, cache.SetAccount(ctx, &remaining))
	require.NoError(t, delayed.SetAccount(ctx, &stale))
	full, err = cache.GetAccount(ctx, stale.ID)
	require.NoError(t, err)
	require.Equal(t, stale.Name, full.Name)
	require.Nil(t, full.TempUnschedulableUntil)
	require.Nil(t, full.TempUnschedulableProtectedUntil)
}
