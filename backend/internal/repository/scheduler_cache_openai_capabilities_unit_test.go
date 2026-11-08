//go:build unit

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// roundTripSchedulerMetadata 走一遍真实的 sched:meta 序列化/反序列化路径。
func roundTripSchedulerMetadata(t *testing.T, account service.Account) service.Account {
	t.Helper()
	payload, err := json.Marshal(buildSchedulerMetadataAccount(account))
	require.NoError(t, err)
	var restored service.Account
	require.NoError(t, json.Unmarshal(payload, &restored))
	return restored
}

func seedanceSchedulerTestAccount() service.Account {
	return service.Account{
		ID:          95,
		Platform:    service.PlatformOpenAI,
		Type:        service.AccountTypeAPIKey,
		Status:      service.StatusActive,
		Schedulable: true,
		Concurrency: 5,
		GroupIDs:    []int64{24},
		Credentials: map[string]any{
			"api_key":  "test-key",
			"base_url": "https://ark.example.test/api/v3",
			service.OpenAIEndpointCapabilitiesCredentialKey: []any{"seedance"},
		},
	}
}

// 候选过滤读的是调度投影：投影裁掉 openai_capabilities 时，须显式声明的 seedance
// 永远判不支持，网关报 503 seedance_no_eligible_account（capability_mismatch）。
func TestBuildSchedulerMetadataAccount_KeepsOpenAIEndpointCapabilities(t *testing.T) {
	account := seedanceSchedulerTestAccount()
	require.True(t, account.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilitySeedance), "前置条件：完整账号支持 seedance")

	restored := roundTripSchedulerMetadata(t, account)

	require.Equal(t, []any{"seedance"}, restored.Credentials[service.OpenAIEndpointCapabilitiesCredentialKey])
	require.True(t, restored.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilitySeedance),
		"投影后的账号必须仍支持 seedance")
}

// 同一根因的另一面：显式限定能力的账号在投影上不能退化成「不限能力」。
func TestBuildSchedulerMetadataAccount_KeepsExplicitEndpointCapabilityRestriction(t *testing.T) {
	account := service.Account{
		ID:       96,
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key": "test-key",
			service.OpenAIEndpointCapabilitiesCredentialKey: []any{"embeddings"},
		},
	}
	require.False(t, account.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilityChatCompletions), "前置条件")

	restored := roundTripSchedulerMetadata(t, account)

	require.True(t, restored.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilityEmbeddings))
	require.False(t, restored.SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilityChatCompletions),
		"只声明 embeddings 的账号在投影上不得被当作支持 chat_completions")
}

// compact_unsupported 过滤与 openAICompactSupportTier 排序都读投影。
func TestBuildSchedulerMetadataAccount_KeepsOpenAICompactFlags(t *testing.T) {
	cases := []struct {
		name          string
		extra         map[string]any
		wantSupported bool
	}{
		{name: "force_off", extra: map[string]any{"openai_compact_mode": service.OpenAICompactModeForceOff}, wantSupported: false},
		{name: "probed_supported", extra: map[string]any{"openai_compact_supported": true}, wantSupported: true},
		{name: "probed_unsupported", extra: map[string]any{"openai_compact_supported": false}, wantSupported: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := service.Account{ID: 97, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Extra: tc.extra}
			restored := roundTripSchedulerMetadata(t, account)
			supported, known := restored.OpenAICompactSupportKnown()
			require.True(t, known, "投影丢了 compact 键会退回「未知」")
			require.Equal(t, tc.wantSupported, supported)
		})
	}
}

// Grok 配额自动暂停在候选过滤阶段读投影上的 grok_usage_snapshot。
func TestBuildSchedulerMetadataAccount_KeepsGrokUsageSnapshot(t *testing.T) {
	snapshot := map[string]any{"fetched_at": "2026-11-08T00:00:00Z", "windows": []any{map[string]any{"name": "5h", "utilization": 0.97}}}
	account := service.Account{ID: 98, Platform: service.PlatformGrok, Type: service.AccountTypeOAuth, Extra: map[string]any{"grok_usage_snapshot": snapshot}}

	restored := roundTripSchedulerMetadata(t, account)

	require.Equal(t, snapshot, restored.Extra["grok_usage_snapshot"])
}

type seedanceSchedulerAccountRepo struct {
	service.AccountRepository
	account service.Account
}

func (r seedanceSchedulerAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if id != r.account.ID {
		return nil, service.ErrAccountNotFound
	}
	account := r.account
	return &account, nil
}

// 端到端：账号只存在于 Redis 调度快照（投影）里，POST /contents/generations/tasks
// 走的 seedance 能力选号必须选中它，而不是报 capability_mismatch。
func TestSchedulerSnapshotSelectsSeedanceAccountFromProjection(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	account := seedanceSchedulerTestAccount()

	bucket := service.SchedulerBucket{GroupID: 24, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle}
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{account}))

	cfg := &config.Config{RunMode: config.RunModeStandard}
	// 关掉 DB 回源：快照未命中直接报错，保证选号读的就是投影。
	cfg.Gateway.Scheduling.DbFallbackEnabled = false
	repo := seedanceSchedulerAccountRepo{account: account}
	snapshot := service.NewSchedulerSnapshotService(cache, nil, repo, nil, cfg)
	gateway := service.NewOpenAIGatewayService(repo, nil, nil, nil, nil, nil, nil, cfg, snapshot, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	listed, _, err := snapshot.ListSchedulableAccounts(ctx, &bucket.GroupID, service.PlatformOpenAI, false)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.True(t, listed[0].SupportsOpenAIEndpointCapability(service.OpenAIEndpointCapabilitySeedance),
		"快照投影上的账号必须支持 seedance")

	groupID := int64(24)
	selection, _, err := gateway.SelectAccountWithSchedulerForCapability(ctx, &groupID, "", "", "doubao-seedance-2.5", nil,
		service.OpenAIUpstreamTransportAny, service.OpenAIEndpointCapabilitySeedance, false, false, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, account.ID, selection.Account.ID)
}
