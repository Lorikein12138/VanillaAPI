//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type gatewayAdaptiveTestRepo struct {
	settingUpdateRepoStub
	getValue func(context.Context, string) (string, error)
}

func (r *gatewayAdaptiveTestRepo) GetValue(ctx context.Context, key string) (string, error) {
	return r.getValue(ctx, key)
}

func TestGatewayAdaptiveSchedulingCache(t *testing.T) {
	ctx := context.Background()
	var nilService *SettingService
	require.False(t, nilService.IsGatewayAdaptiveSchedulingEnabled(ctx))
	require.False(t, (&SettingService{}).IsGatewayAdaptiveSchedulingEnabled(ctx))

	for _, tc := range []struct {
		name string
		raw  string
		err  error
		want bool
	}{
		{name: "enabled", raw: "true", want: true},
		{name: "missing", err: ErrSettingNotFound},
		{name: "error", raw: "true", err: errors.New("read failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			repo := &gatewayAdaptiveTestRepo{getValue: func(_ context.Context, key string) (string, error) {
				require.Equal(t, SettingKeyGatewayAdaptiveSchedulingEnabled, key)
				reads++
				return tc.raw, tc.err
			}}
			svc := NewSettingService(repo, &config.Config{})
			require.Equal(t, tc.want, svc.IsGatewayAdaptiveSchedulingEnabled(ctx))
			require.Equal(t, tc.want, svc.IsGatewayAdaptiveSchedulingEnabled(ctx))
			require.Equal(t, 1, reads)
		})
	}
}

func TestGatewayAdaptiveSchedulingCacheIgnoresStaleReadAfterWrite(t *testing.T) {
	ctx := context.Background()
	started, finished := make(chan struct{}), make(chan struct{})
	repo := &gatewayAdaptiveTestRepo{getValue: func(_ context.Context, _ string) (string, error) {
		close(started)
		<-finished
		return "false", nil
	}}
	svc := NewSettingService(repo, &config.Config{})
	result := make(chan bool, 1)
	go func() {
		result <- svc.IsGatewayAdaptiveSchedulingEnabled(ctx)
	}()
	defer close(finished)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("cache lookup did not start")
	}

	settings := &SystemSettings{GatewayAdaptiveSchedulingEnabled: true}
	require.NoError(t, svc.UpdateSettings(ctx, settings))
	finished <- struct{}{}
	select {
	case enabled := <-result:
		require.True(t, enabled)
	case <-time.After(5 * time.Second):
		t.Fatal("cache lookup did not return")
	}

	settings.GatewayAdaptiveSchedulingEnabled = false
	repo.setMultipleErr = errors.New("write failed")
	require.ErrorIs(t, svc.UpdateSettings(ctx, settings), repo.setMultipleErr)
	require.True(t, svc.IsGatewayAdaptiveSchedulingEnabled(ctx))
}
