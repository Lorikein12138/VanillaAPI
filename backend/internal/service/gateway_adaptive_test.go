package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type adaptiveTestSettings struct{ SettingRepository }

func (adaptiveTestSettings) GetValue(context.Context, string) (string, error) {
	return "true", nil
}

func adaptiveTestAccount(id int64, platform string, priority int) Account {
	return Account{ID: id, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: priority, Concurrency: 1}
}

func seedAdaptiveTestSamples(t *testing.T, ctx context.Context, model string, accounts ...Account) {
	t.Helper()
	route, ok := gatewayAdaptiveRoute(ctx)
	require.True(t, ok)
	for i := range accounts {
		key := adaptiveKey(route, &accounts[i], model)
		for range adaptiveMinimum {
			gatewayAdaptiveTelemetry.observe(key, time.Now(), time.Second/time.Duration(1+i*3), true)
		}
		t.Cleanup(func() {
			gatewayAdaptiveTelemetry.mu.Lock()
			defer gatewayAdaptiveTelemetry.mu.Unlock()
			delete(gatewayAdaptiveTelemetry.series, key)
		})
	}
}

func TestGatewayAdaptiveRanksActualGatewayPools(t *testing.T) {
	ctx := context.Background()
	settings := &SettingService{settingRepo: adaptiveTestSettings{}}

	t.Run("Anthropic ordinary and excluded fallback", func(t *testing.T) {
		accounts := []Account{adaptiveTestAccount(60201, PlatformAnthropic, 1), adaptiveTestAccount(60202, PlatformAnthropic, 9)}
		model := "claude-sonnet-4-6"
		enabled := WithGatewayAdaptiveIngress(ctx, settings, "test_anthropic", "http")
		seedAdaptiveTestSamples(t, enabled, model, accounts...)
		svc := &GatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}}
		choose := func(ctx context.Context, exclusions map[int64]struct{}) int64 {
			ctx = context.WithValue(ctx, ctxkey.ForcePlatform, PlatformAnthropic)
			account, err := svc.selectAccountForModelWithPlatform(ctx, nil, "", model, exclusions, PlatformAnthropic)
			require.NoError(t, err)
			return account.ID
		}
		require.Equal(t, accounts[0].ID, choose(ctx, nil))
		require.Equal(t, accounts[1].ID, choose(enabled, nil))
		require.Equal(t, accounts[0].ID, choose(enabled, map[int64]struct{}{accounts[1].ID: {}}))
	})

	t.Run("OpenAI legacy and advanced before Top-K", func(t *testing.T) {
		accounts := []Account{adaptiveTestAccount(60301, PlatformOpenAI, 1), adaptiveTestAccount(60302, PlatformOpenAI, 9)}
		model := "gpt-5.1"
		enabled := WithGatewayAdaptiveIngress(ctx, settings, "test_openai", "http")
		seedAdaptiveTestSamples(t, enabled, model, accounts...)
		svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: &config.Config{}}
		for _, tc := range []struct {
			ctx  context.Context
			want int64
		}{{ctx, accounts[0].ID}, {enabled, accounts[1].ID}} {
			account, err := svc.SelectAccountForModel(tc.ctx, nil, "", model)
			require.NoError(t, err)
			require.Equal(t, tc.want, account.ID)
			scheduler := &defaultOpenAIAccountScheduler{service: svc}
			pool := []openAIAccountCandidateScore{
				{account: &accounts[0], loadInfo: &AccountLoadInfo{}, score: 10},
				{account: &accounts[1], loadInfo: &AccountLoadInfo{}, score: 1},
			}
			req := OpenAIAccountScheduleRequest{RequestedModel: model}
			plan := openAIAccountLoadPlan{candidates: pool, topK: 1}
			require.Equal(t, tc.want, scheduler.buildOpenAISelectionOrder(tc.ctx, req, plan)[0].account.ID)
			req.PreviousResponseID = "resp_owner"
			require.Equal(t, accounts[0].ID, scheduler.buildOpenAISelectionOrder(tc.ctx, req, plan)[0].account.ID)
		}
	})

	t.Run("Gemini model pool", func(t *testing.T) {
		accounts := []Account{adaptiveTestAccount(60401, PlatformGemini, 1), adaptiveTestAccount(60402, PlatformGemini, 9)}
		model := "gemini-2.5-flash"
		enabled := WithGatewayAdaptiveIngress(ctx, settings, "test_gemini", "http")
		seedAdaptiveTestSamples(t, enabled, model, accounts...)
		svc := &GeminiMessagesCompatService{}
		require.Equal(t, accounts[0].ID, svc.selectBestGeminiAccount(ctx, accounts, model, nil, PlatformGemini, false).ID)
		require.Equal(t, accounts[1].ID, svc.selectBestGeminiAccount(enabled, accounts, model, nil, PlatformGemini, false).ID)
		accounts[1].Schedulable = false
		require.Equal(t, accounts[0].ID, svc.selectBestGeminiAccount(enabled, accounts, model, nil, PlatformGemini, false).ID)
	})
}

func TestGatewayAdaptiveExpiredAndIsolatedSamples(t *testing.T) {
	now := time.Unix(1700000000, 0)
	telemetry := &adaptiveTelemetry{}
	ctx := withGatewayAdaptiveRoute(context.Background(), "test_messages", "", []byte("{\"max_tokens\":1024}"))
	route, _ := gatewayAdaptiveRoute(ctx)
	one := adaptiveTestAccount(1, PlatformAnthropic, 1)
	two := adaptiveTestAccount(2, PlatformAnthropic, 2)
	model := adaptiveMappedModel(ctx, "test-model")
	for range adaptiveMinimum {
		telemetry.observe(adaptiveKey(route, &one, model(&one)), now, time.Second, true)
		telemetry.observe(adaptiveKey(route, &two, model(&two)), now, 200*time.Millisecond, true)
	}
	check := func(route adaptiveRoute, at time.Time, peer adaptivePeerMatch, want int64) {
		t.Helper()
		pool := []*Account{&one, &two}
		rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, peer, adaptiveProbeOptions[*Account]{}, at, nil)
		require.Equal(t, want, pool[0].ID)
	}
	check(route, now, adaptiveAnyPeer, 2)
	check(route, now.Add(3*time.Hour), adaptiveAnyPeer, 2)
	check(route, now.Add(adaptiveWindow), adaptiveAnyPeer, 1)
	check(route, now, func(a, b *Account) bool { return a.Priority == b.Priority }, 1)
	other := route
	other.transport = "ws"
	check(other, now, adaptiveAnyPeer, 1)
	other = route
	other.effort = "high"
	check(other, now, adaptiveAnyPeer, 1)
	other = route
	other.inputSize++
	check(other, now, adaptiveAnyPeer, 1)
	other = route
	other.operation = "metadata"
	check(other, now, adaptiveAnyPeer, 1)
	protected := withGatewayAdaptiveRoute(ctx, "test_messages", "", []byte("{\"previous_response_id\":\"resp_a\"}"))
	pool := []*Account{&one, &two}
	require.False(t, rankAdaptivePool(protected, pool, adaptiveAccount, model, adaptiveAnyPeer, true))
}

func TestGatewayAdaptiveDecayAndRecentOverride(t *testing.T) {
	now := time.Unix(1700000000, 0)
	route := adaptiveRoute{operation: "chat", transport: "http"}
	telemetry := &adaptiveTelemetry{}
	accounts := []Account{adaptiveTestAccount(1, PlatformOpenAI, 1), adaptiveTestAccount(2, PlatformOpenAI, 1)}
	keys := []adaptiveSampleKey{adaptiveKey(route, &accounts[0], "model"), adaptiveKey(route, &accounts[1], "model")}
	for range adaptiveMinimum - 1 {
		telemetry.observe(keys[0], now, time.Second, true)
	}
	require.False(t, telemetry.score(keys[0], now).known)
	for range adaptiveMinimum {
		telemetry.observe(keys[1], now.Add(-6*time.Hour-time.Second), time.Second, true)
	}
	require.False(t, telemetry.score(keys[1], now).known)
	require.True(t, telemetry.score(keys[1], now.Add(-time.Second)).known)

	weighted := adaptiveKey(route, &accounts[1], "weighted")
	for range 3 {
		telemetry.observe(weighted, now.Add(-18*time.Hour), 15*time.Second, true)
	}
	telemetry.observe(weighted, now, time.Second, true)
	telemetry.observe(weighted, now, time.Second, false)
	// Three 18-hour-old successes weigh 1/8 each, yielding a mean of about 4.8s.
	require.Equal(t, adaptiveScore{reliability: 2, latency: 22, known: true}, telemetry.score(weighted, now))

	telemetry = &adaptiveTelemetry{}
	for range 12 {
		telemetry.observe(keys[0], now.Add(-time.Hour), 100*time.Millisecond, true)
		telemetry.observe(keys[1], now.Add(-time.Hour), time.Second, true)
	}
	for range adaptiveMinimum {
		telemetry.observe(keys[0], now, 2*time.Second, true)
	}
	pool := []*Account{&accounts[0], &accounts[1]}
	rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, func(*Account) string { return "model" }, adaptiveAnyPeer, adaptiveProbeOptions[*Account]{}, now, nil)
	require.Equal(t, accounts[1].ID, pool[0].ID)
	for range adaptiveMinimum {
		telemetry.observe(keys[0], now.Add(time.Second), 0, false)
	}
	require.Less(t, telemetry.score(keys[0], now.Add(time.Second)).reliability, telemetry.score(keys[1], now.Add(time.Second)).reliability)

	tooSlow := adaptiveKey(route, &accounts[0], "too-slow")
	for range adaptiveMinimum {
		telemetry.observe(tooSlow, now, adaptiveMaxSuccessLatency+time.Nanosecond, true)
	}
	require.NotContains(t, telemetry.series, tooSlow)
}

func TestGatewayAdaptiveColdStartBudgetCooldownAndRotation(t *testing.T) {
	now := time.Unix(1700000000, 0)
	telemetry := &adaptiveTelemetry{}
	route := adaptiveRoute{operation: "chat", transport: "http"}
	baseline := adaptiveTestAccount(1, PlatformOpenAI, 1)
	first := adaptiveTestAccount(2, PlatformOpenAI, 1)
	second := adaptiveTestAccount(3, PlatformOpenAI, 1)
	otherTier := adaptiveTestAccount(4, PlatformOpenAI, 2)
	model := func(*Account) string { return "model" }
	probe := adaptiveProbeOptions[*Account]{opportunity: func() bool { return true }}
	for range adaptiveMinimum {
		telemetry.observe(adaptiveKey(route, &baseline, "model"), now, time.Second, true)
	}
	try := func(at time.Time, want ...*Account) {
		t.Helper()
		pool := []*Account{&baseline, &otherTier, &second, &first}
		rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, probe, at, nil)
		require.Equal(t, want, pool)
	}
	try(now, &first, &otherTier, &baseline, &second)
	otherRoute := route
	otherRoute.operation = "another_protocol"
	for range adaptiveMinimum {
		telemetry.observe(adaptiveKey(otherRoute, &baseline, "model"), now, time.Second, true)
	}
	otherCandidate := adaptiveTestAccount(5, PlatformGemini, 1)
	otherPool := []*Account{&baseline, &otherCandidate}
	changed, _ := rankAdaptiveWithTelemetry(telemetry, otherRoute, otherPool, adaptiveAccount, model, adaptiveAnyPeer, probe, now.Add(time.Second), nil)
	require.False(t, changed) // a separate pool still shares the process cap
	for range adaptiveProbeRequests - 1 {
		try(now.Add(time.Minute), &baseline, &otherTier, &second, &first)
	}
	try(now.Add(time.Minute), &second, &otherTier, &baseline, &first)
	for range adaptiveProbeRequests - 1 {
		try(now.Add(time.Minute*2), &baseline, &otherTier, &second, &first)
	}
	try(now.Add(29*time.Minute), &baseline, &otherTier, &second, &first)
	try(now.Add(30*time.Minute), &first, &otherTier, &baseline, &second)
}

func TestGatewayAdaptiveDoesNotPreferAllFailuresAtZeroReliability(t *testing.T) {
	now := time.Now()
	route := adaptiveRoute{operation: "chat", transport: "http"}
	telemetry := &adaptiveTelemetry{}
	failed := adaptiveTestAccount(1, PlatformOpenAI, 1)
	partial := adaptiveTestAccount(2, PlatformOpenAI, 1)
	model := func(*Account) string { return "model" }
	failedKey, partialKey := adaptiveKey(route, &failed, "model"), adaptiveKey(route, &partial, "model")
	for i := range adaptiveSamples {
		telemetry.observe(failedKey, now, 0, false)
		telemetry.observe(partialKey, now, 10*time.Second, i == 0)
	}
	require.Zero(t, telemetry.score(failedKey, now).reliability)
	require.Zero(t, telemetry.score(partialKey, now).reliability)
	pool := []*Account{&failed, &partial}
	changed, _ := rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, adaptiveProbeOptions[*Account]{}, now, nil)
	require.True(t, changed)
	require.Equal(t, &partial, pool[0])
}

func TestGatewayAdaptiveProbeBudgetSurvivesCandidateAndBaselineChanges(t *testing.T) {
	now := time.Now()
	telemetry := &adaptiveTelemetry{}
	settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
	model := func(*Account) string { return "model" }
	firstBaseline := adaptiveTestAccount(1, PlatformOpenAI, 1)
	secondBaseline := adaptiveTestAccount(2, PlatformOpenAI, 1)
	firstUnknown := adaptiveTestAccount(3, PlatformOpenAI, 1)
	secondUnknown := adaptiveTestAccount(4, PlatformOpenAI, 1)
	groupID := int64(10)
	newContext := func(group int64) context.Context {
		ctx := WithGatewayAdaptiveIngress(context.Background(), settings, "test_budget", "http")
		ctx = SetGatewayAdaptiveInput(ctx, []byte(`{"model":"request-model"}`))
		return WithGatewayAdaptiveProbeGroup(ctx, &group)
	}
	ctx := newContext(groupID)
	route, _ := gatewayAdaptiveRoute(ctx)
	for range adaptiveMinimum {
		telemetry.observe(adaptiveKey(route, &firstBaseline, "model"), now, time.Second, true)
		telemetry.observe(adaptiveKey(route, &secondBaseline, "model"), now, time.Second, true)
	}
	pool := []*Account{&firstBaseline, &firstUnknown}
	require.True(t, rankAdaptivePool(ctx, pool, adaptiveAccount, model, adaptiveAnyPeer, true))
	// Advance the process limiter without changing any cooldowns or pool budgets.
	telemetry.exploration.lastProbe = now.Add(-adaptiveProbeInterval)
	tryChangedPool := func(ctx context.Context) bool {
		pool := []*Account{&secondBaseline, &secondUnknown}
		return rankAdaptivePool(ctx, pool, adaptiveAccount, model, adaptiveAnyPeer, true)
	}
	request := newContext(groupID)
	for range adaptiveProbeRequests {
		require.False(t, tryChangedPool(request))
	}
	for range adaptiveProbeRequests - 2 {
		require.False(t, tryChangedPool(newContext(groupID)))
	}
	require.True(t, tryChangedPool(newContext(groupID)))
	telemetry.exploration.lastProbe = now.Add(-adaptiveProbeInterval)
	otherUnknown := adaptiveTestAccount(5, PlatformOpenAI, 1)
	pool = []*Account{&secondBaseline, &otherUnknown}
	require.True(t, rankAdaptivePool(newContext(groupID+1), pool, adaptiveAccount, model, adaptiveAnyPeer, true))
}

func TestGatewayAdaptiveProbeBudgetUsesParsedRequestModel(t *testing.T) {
	for _, tc := range []struct {
		name, operation, firstModel, secondModel string
		parse                                    func(*testing.T, context.Context, string) context.Context
	}{
		{
			name: "Gemini URL", operation: "POST /v1beta/models/*modelAction:generateContent", firstModel: "gemini-one", secondModel: "gemini-two",
			parse: func(_ *testing.T, ctx context.Context, model string) context.Context {
				return WithGatewayAdaptiveRequestModel(SetGatewayAdaptiveInput(ctx, []byte(`{"contents":[]}`)), model)
			},
		},
		{
			name: "image multipart", operation: "POST /v1/images/edits", firstModel: "gpt-image-1", secondModel: "gpt-image-2",
			parse: func(t *testing.T, ctx context.Context, model string) context.Context {
				var body bytes.Buffer
				writer := multipart.NewWriter(&body)
				require.NoError(t, writer.WriteField("model", model))
				require.NoError(t, writer.WriteField("prompt", "edit"))
				part, err := writer.CreateFormFile("image", "source.png")
				require.NoError(t, err)
				_, err = part.Write([]byte("image"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body.Bytes()))
				c.Request.Header.Set("Content-Type", writer.FormDataContentType())
				svc := &OpenAIGatewayService{}
				input, err := svc.ParseOpenAIImagesRequest(c, body.Bytes())
				require.NoError(t, err)
				return WithAdaptiveImageInput(ctx, body.Bytes(), input)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			telemetry := &adaptiveTelemetry{}
			settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
			newIngress := func() context.Context {
				return WithGatewayAdaptiveIngress(context.Background(), settings, tc.operation, "http")
			}
			firstIngress := newIngress()
			first := tc.parse(t, firstIngress, tc.firstModel)
			second := tc.parse(t, newIngress(), tc.secondModel)
			baseline, firstUnknown, secondUnknown := adaptiveTestAccount(1, PlatformOpenAI, 1), adaptiveTestAccount(2, PlatformOpenAI, 1), adaptiveTestAccount(3, PlatformOpenAI, 1)
			mapped := func(account *Account) string { return fmt.Sprintf("mapped-model-%d", account.ID) }
			for _, ctx := range []context.Context{first, second} {
				route, _ := gatewayAdaptiveRoute(ctx)
				for range adaptiveMinimum {
					telemetry.observe(adaptiveKey(route, &baseline, mapped(&baseline)), now, time.Second, true)
				}
			}
			try := func(ctx context.Context, unknown *Account) bool {
				return rankAdaptivePool(ctx, []*Account{&baseline, unknown}, adaptiveAccount, mapped, adaptiveAnyPeer, true)
			}
			require.True(t, try(first, &firstUnknown))
			telemetry.exploration.lastProbe = now.Add(-adaptiveProbeInterval)
			require.False(t, try(tc.parse(t, firstIngress, tc.secondModel), &secondUnknown))
			require.True(t, try(second, &secondUnknown))
		})
	}
}

func TestGatewayAdaptiveColdStartRequiresAnEligibleUnboundRequest(t *testing.T) {
	telemetry := &adaptiveTelemetry{}
	settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
	baseline := adaptiveTestAccount(1, PlatformOpenAI, 1)
	unknown := adaptiveTestAccount(2, PlatformOpenAI, 1)
	model := func(*Account) string { return "model" }
	newContext := func() context.Context {
		return WithGatewayAdaptiveIngress(context.Background(), settings, "test_cold_start", "http")
	}
	check := func(ctx context.Context, peers adaptivePeerMatch, explore, want bool) {
		t.Helper()
		pool := []*Account{&baseline, &unknown}
		require.Equal(t, want, rankAdaptivePool(ctx, pool, adaptiveAccount, model, peers, explore))
	}
	ctx := newContext()
	check(ctx, adaptiveAnyPeer, true, false) // no confidence yet
	route, _ := gatewayAdaptiveRoute(ctx)
	for range adaptiveMinimum {
		telemetry.observe(adaptiveKey(route, &baseline, "model"), time.Now(), time.Second, true)
	}
	check(protectAdaptiveSelection(ctx), adaptiveAnyPeer, true, false)
	check(withoutAdaptiveExploration(ctx), adaptiveAnyPeer, true, false) // internal retry
	check(ctx, adaptiveAnyPeer, false, false)                            // excluded or waiting path
	check(ctx, func(*Account, *Account) bool { return false }, true, false)
	check(ctx, adaptiveAnyPeer, true, true)
	check(ctx, adaptiveAnyPeer, true, false)
}

type adaptiveTopKConcurrencyCache struct {
	upstreamCostTrackingConcurrencyCache
	busy       int64
	attempted  []int64
	allBusy    bool
	refreshErr error
}

func (c *adaptiveTopKConcurrencyCache) AcquireAccountSlot(_ context.Context, accountID int64, _ int, _ string) (bool, error) {
	c.attempted = append(c.attempted, accountID)
	return !c.allBusy && accountID != c.busy, nil
}

func (c *adaptiveTopKConcurrencyCache) GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if c.refreshErr != nil {
		return nil, c.refreshErr
	}
	return c.upstreamCostTrackingConcurrencyCache.GetAccountsLoadBatch(ctx, accounts)
}

func TestGatewayAdaptiveOpenAIAdvancedWaitUsesNormalTopSevenAfterRefreshFailure(t *testing.T) {
	accounts := make([]Account, 8)
	filtered := make([]*Account, 0, len(accounts))
	loads := make(map[int64]*AccountLoadInfo, len(accounts))
	for i := range accounts {
		accounts[i] = adaptiveTestAccount(int64(i+1), PlatformOpenAI, 1)
		filtered = append(filtered, &accounts[i])
		loads[accounts[i].ID] = &AccountLoadInfo{AccountID: accounts[i].ID, LoadRate: 14 * i}
	}
	cache := &adaptiveTopKConcurrencyCache{allBusy: true, refreshErr: errors.New("load refresh failed")}
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(cache),
	}}
	telemetry := &adaptiveTelemetry{}
	settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
	ctx := WithGatewayAdaptiveIngress(context.Background(), settings, "test_advanced_probe_wait", "http")
	const model = "gpt-5.1"
	route, _ := gatewayAdaptiveRoute(ctx)
	for i := range 7 {
		for range adaptiveMinimum {
			telemetry.observe(adaptiveKey(route, &accounts[i], model), time.Now(), time.Second, true)
		}
	}
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: model, SessionHash: "stable-weighted-order"}
	normal := scheduler.buildOpenAIAccountLoadPlan(withoutAdaptiveExploration(ctx), req, filtered, loads)
	require.Equal(t, 7, normal.topK)
	require.Len(t, normal.selectionOrder, 7)
	budget := newOpenAISelectionProbeBudget()
	attempt := scheduler.trySelectByLoadBalancePool(ctx, req, filtered, loads, budget)
	require.NoError(t, attempt.err)
	require.Nil(t, attempt.result)
	require.Len(t, cache.attempted, 8)
	require.Equal(t, accounts[7].ID, cache.attempted[0])
	for i, candidate := range normal.selectionOrder {
		require.Equal(t, candidate.account.ID, cache.attempted[i+1])
	}
	selection, _, _, _, err := scheduler.finishLoadBalanceSelectionFallback(ctx, req, attempt, budget, openAISelectionFilterStats{})
	require.NoError(t, err)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, normal.selectionOrder[0].account.ID, selection.WaitPlan.AccountID)
}

func TestGatewayAdaptiveOpenAILegacyWithoutBatchWaitsOnNormalFirst(t *testing.T) {
	accounts := []Account{adaptiveTestAccount(1, PlatformOpenAI, 1), adaptiveTestAccount(2, PlatformOpenAI, 1)}
	cache := &adaptiveTopKConcurrencyCache{allBusy: true}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(cache)}
	telemetry := &adaptiveTelemetry{}
	settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
	ctx := WithGatewayAdaptiveIngress(context.Background(), settings, "test_legacy_probe_wait", "http")
	route, _ := gatewayAdaptiveRoute(ctx)
	const model = "gpt-5.1"
	for range adaptiveMinimum {
		telemetry.observe(adaptiveKey(route, &accounts[0], model), time.Now(), time.Second, true)
	}
	selection, err := svc.selectAccountWithLoadAwareness(ctx, nil, PlatformOpenAI, "", model, nil, false, OpenAIEndpointCapability(""), false)
	require.NoError(t, err)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, []int64{accounts[1].ID, accounts[0].ID}, cache.attempted)
	require.Equal(t, accounts[0].ID, selection.WaitPlan.AccountID)
}

func TestGatewayAdaptiveOpenAITopOneAttemptsBaselineAfterProbe(t *testing.T) {
	accounts := []Account{adaptiveTestAccount(1, PlatformOpenAI, 1), adaptiveTestAccount(2, PlatformOpenAI, 1)}
	cache := &adaptiveTopKConcurrencyCache{busy: accounts[1].ID}
	scheduler := &defaultOpenAIAccountScheduler{service: &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, cfg: &config.Config{}, concurrencyService: NewConcurrencyService(cache),
	}}
	telemetry := &adaptiveTelemetry{}
	settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
	ctx := WithGatewayAdaptiveIngress(context.Background(), settings, "test_topk_probe", "http")
	route, _ := gatewayAdaptiveRoute(ctx)
	req := OpenAIAccountScheduleRequest{Platform: PlatformOpenAI, RequestedModel: "gpt-5.1"}
	for range adaptiveMinimum {
		telemetry.observe(adaptiveKey(route, &accounts[0], req.RequestedModel), time.Now(), time.Second, true)
	}
	plan := openAIAccountLoadPlan{topK: 1, candidates: []openAIAccountCandidateScore{
		{account: &accounts[0], score: 2, loadInfo: &AccountLoadInfo{}},
		{account: &accounts[1], score: 1, loadInfo: &AccountLoadInfo{}, loadKnown: true},
	}}
	req.ExcludedIDs = map[int64]struct{}{3: {}}
	require.Len(t, scheduler.buildOpenAISelectionOrder(ctx, req, plan), 1)
	req.ExcludedIDs = nil
	req.StickyAccountID = accounts[0].ID
	require.Len(t, scheduler.buildOpenAISelectionOrder(ctx, req, plan), 1)
	req.StickyAccountID = 0
	plan.candidates[1].loadInfo.CurrentConcurrency = 1
	require.Len(t, scheduler.buildOpenAISelectionOrder(ctx, req, plan), 1)
	plan.candidates[1].loadInfo.CurrentConcurrency = 0
	order := scheduler.buildOpenAISelectionOrder(ctx, req, plan)
	require.Len(t, order, 2)
	require.True(t, order[0].adaptiveProbeOnly)
	selection, _, err := scheduler.tryAcquireOpenAISelectionOrder(ctx, req, order)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, accounts[0].ID, selection.Account.ID)
	require.Equal(t, []int64{accounts[1].ID, accounts[0].ID}, cache.attempted)
	selection.ReleaseFunc()
	require.Len(t, scheduler.buildOpenAISelectionOrder(ctx, req, plan), 1)
}

func TestGatewayAdaptiveObservationRequiresOneUpstreamAndLiveClient(t *testing.T) {
	account := adaptiveTestAccount(60491, PlatformOpenAI, 0)
	ctx := withGatewayAdaptiveRoute(context.Background(), "test_upstream_observation", "http", nil)
	defer func() {
		route, _ := gatewayAdaptiveRoute(ctx)
		gatewayAdaptiveTelemetry.mu.Lock()
		defer gatewayAdaptiveTelemetry.mu.Unlock()
		delete(gatewayAdaptiveTelemetry.series, adaptiveKey(route, &account, "upstream"))
	}()
	route, _ := gatewayAdaptiveRoute(ctx)
	key := adaptiveKey(route, &account, "upstream")
	result := &OpenAIForwardResult{Model: "alias", UpstreamModel: "upstream"}
	send := func() (*http.Response, error) { return &http.Response{StatusCode: http.StatusOK}, nil }

	work, obs := beginAdaptiveObservation(ctx)
	_, nested := beginAdaptiveObservation(work)
	require.Nil(t, nested)
	_, err := adaptiveHTTP(work, send)
	require.NoError(t, err)
	obs.first = time.Now().Add(-time.Second)
	observeAdaptiveOpenAIResult(ctx, obs, &account, result, nil)

	work, obs = beginAdaptiveObservation(ctx)
	_, _ = adaptiveHTTP(work, send)
	_, _ = adaptiveHTTP(work, send)
	observeAdaptiveOpenAIResult(ctx, obs, &account, result, nil)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	observeAdaptiveOpenAIResult(canceled, &adaptiveObservation{first: time.Now().Add(-time.Second), calls: 1, status: 200}, &account, result, nil)

	gatewayAdaptiveTelemetry.mu.Lock()
	defer gatewayAdaptiveTelemetry.mu.Unlock()
	require.NotNil(t, gatewayAdaptiveTelemetry.series[key])
	require.Equal(t, 1, gatewayAdaptiveTelemetry.series[key].next)
}

func TestGatewayAdaptiveDetachedImageSetting(t *testing.T) {
	svc := &OpenAIGatewayService{settingService: &SettingService{settingRepo: adaptiveTestSettings{}}}
	ctx := svc.WithAdaptiveImageTask(context.Background(), "/v1/images/edits")
	route, ok := gatewayAdaptiveRoute(ctx)
	require.True(t, ok)
	require.Equal(t, "async_image_edit", route.operation)
	svc.settingService = nil
	ctx = svc.WithAdaptiveImageTask(ctx, "/v1/images/edits")
	_, ok = gatewayAdaptiveRoute(ctx)
	require.False(t, ok)
}

func TestGatewayAdaptiveProtectsGrokContinuationsAndBucketsVideoDuration(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
		`{"input":{"type":"reasoning","encrypted_content":"opaque"}}`,
		`{"input":{"type":"function_call_output","call_id":"opaque"}}`,
	} {
		ctx := withGatewayAdaptiveRoute(context.Background(), "POST /v1/responses", "", []byte(body))
		route, ok := gatewayAdaptiveRoute(ctx)
		require.True(t, ok)
		require.True(t, route.protected, body)
	}
	for _, op := range []string{"POST /v1/videos", "POST /v3/contents/generations/tasks"} {
		duration := func(body string) string {
			ctx := withGatewayAdaptiveRoute(context.Background(), op, "", []byte(body))
			route, ok := gatewayAdaptiveRoute(ctx)
			require.True(t, ok)
			return route.duration
		}
		require.Equal(t, duration(`{"duration":4}`), duration(`{"duration":7}`))
		require.NotEqual(t, duration(`{"duration":4}`), duration(`{"duration":8}`))
		require.Equal(t, duration(`{"duration":-1}`), duration(`{"duration":"auto"}`))
		require.NotEqual(t, duration(`{"duration":-1}`), duration(`{"duration":4}`))
	}
}

func TestGatewayAdaptiveReadsObservedWireModel(t *testing.T) {
	for _, tc := range []struct {
		name, operation, body, model, channelModel, wireModel, platform, accountType string
	}{
		{"Claude OAuth dated model", "POST /v1/messages", `{}`, "claude-sonnet-4-5", "", "claude-sonnet-4-5-20250929", PlatformAnthropic, AccountTypeOAuth},
		{"Antigravity thinking variant", "POST /v1beta/models/*modelAction:generateContent", `{"generationConfig":{"thinkingConfig":{"thinkingBudget":1000}}}`, "gemini-3.8-flash", "", "gemini-3.8-flash-low", PlatformAntigravity, AccountTypeOAuth},
		{"native Messages channel rewrite", "POST /v1/messages", `{}`, "channel-alias", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929", PlatformAnthropic, AccountTypeOAuth},
		{"compat Responses channel rewrite", "POST /v1/responses", `{}`, "channel-alias", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929", PlatformAnthropic, AccountTypeOAuth},
		{"compat Chat channel rewrite", "POST /v1/chat/completions", `{}`, "channel-alias", "claude-sonnet-4-5", "claude-sonnet-4-5-20250929", PlatformAnthropic, AccountTypeOAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := withGatewayAdaptiveRoute(context.Background(), tc.operation, "", []byte(tc.body))
			ctx = WithAdaptiveChannelModel(ctx, tc.channelModel)
			route, _ := gatewayAdaptiveRoute(ctx)
			slow := adaptiveTestAccount(61401, tc.platform, 1)
			fast := adaptiveTestAccount(61402, tc.platform, 9)
			slow.Type, fast.Type = tc.accountType, tc.accountType
			if tc.platform == PlatformAntigravity {
				account := newAntigravityAccountWithMapping(map[string]string{
					"gemini-3.8-flash-low": tc.wireModel, "gemini-3.8-flash-high": "gemini-3.8-flash-high",
				})
				slow.Credentials, fast.Credentials = account.Credentials, account.Credentials
			}
			for i, account := range []*Account{&slow, &fast} {
				key := adaptiveKey(route, account, tc.wireModel)
				t.Cleanup(func() {
					gatewayAdaptiveTelemetry.mu.Lock()
					defer gatewayAdaptiveTelemetry.mu.Unlock()
					delete(gatewayAdaptiveTelemetry.series, key)
				})
				for range adaptiveMinimum {
					obs := &adaptiveObservation{first: time.Now().Add(-time.Second / time.Duration(i*3+1)), calls: 1, status: 200}
					observeAdaptiveGatewayResult(ctx, obs, account, &ForwardResult{Model: tc.model, UpstreamModel: tc.wireModel}, nil)
				}
			}
			model := adaptiveMappedModel(ctx, tc.model)
			require.Equal(t, tc.wireModel, model(&fast))
			pool := []*Account{&slow, &fast}
			require.True(t, rankAdaptivePool(ctx, pool, adaptiveAccount, model, adaptiveAnyPeer, false))
			require.Equal(t, fast.ID, pool[0].ID)
			if tc.platform == PlatformAntigravity {
				high := withGatewayAdaptiveRoute(context.Background(), tc.operation, "", []byte(`{"generationConfig":{"thinkingConfig":{"thinkingBudget":-1}}}`))
				pool = []*Account{&slow, &fast}
				require.Equal(t, "gemini-3.8-flash-high", adaptiveMappedModel(high, tc.model)(&fast))
				require.False(t, rankAdaptivePool(high, pool, adaptiveAccount, adaptiveMappedModel(high, tc.model), adaptiveAnyPeer, false))
			}
		})
	}
}

func TestGatewayAdaptiveBucketsParsedGrokMultipartVideoDuration(t *testing.T) {
	build := func(seconds string) adaptiveRoute {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		require.NoError(t, writer.WriteField("model", "grok-imagine-video"))
		require.NoError(t, writer.WriteField("duration", seconds))
		require.NoError(t, writer.Close())
		info := ParseGrokMediaRequest(writer.FormDataContentType(), body.Bytes())
		ctx := withGatewayAdaptiveRoute(context.Background(), "POST /v1/videos", "", body.Bytes())
		ctx = WithAdaptiveGrokVideoDuration(ctx, info.DurationSeconds)
		route, ok := gatewayAdaptiveRoute(ctx)
		require.True(t, ok)
		return route
	}
	require.Equal(t, build("4"), build("7"))
	require.NotEqual(t, build("4"), build("8"))
}

func TestGatewayAdaptiveKeepsServiceTiersSeparate(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	operation := "POST /v1/responses"
	for _, tier := range []string{"scale", "ultrafast"} {
		ctx := withGatewayAdaptiveRoute(context.Background(), operation, "", []byte(`{"service_tier":"`+tier+`"}`))
		route, ok := gatewayAdaptiveRoute(ctx)
		require.True(t, ok)
		require.Equal(t, tier, route.tier)
		_, ok = adaptiveObservationRoute(ctx, route, account, "gpt-6.2-sol", &tier, tier)
		require.True(t, ok)
		standard := "default"
		_, ok = adaptiveObservationRoute(ctx, route, account, "gpt-6.2-sol", &standard, "")
		require.False(t, ok)
		_, ok = adaptiveObservationRoute(ctx, route, account, "gpt-6.2-sol", &tier, standard)
		require.False(t, ok)
	}

	ctx := withGatewayAdaptiveRoute(context.Background(), operation, "", nil)
	route, _ := gatewayAdaptiveRoute(ctx)
	omitted, ok := adaptiveObservationRoute(ctx, route, account, "gpt-6.2-sol", nil, "default")
	require.True(t, ok)
	defaultTier := "default"
	explicitCtx := withGatewayAdaptiveRoute(context.Background(), operation, "", []byte(`{"service_tier":"default"}`))
	explicitRoute, _ := gatewayAdaptiveRoute(explicitCtx)
	explicit, ok := adaptiveObservationRoute(explicitCtx, explicitRoute, account, "gpt-6.2-sol", &defaultTier, "default")
	require.True(t, ok)
	require.Equal(t, omitted, explicit)
}

func TestGatewayAdaptiveMessagesUsesWireFastMode(t *testing.T) {
	ctx := withGatewayAdaptiveRoute(context.Background(), "POST /v1/messages", "", nil)
	ctx = WithAdaptiveMessagesFastMode(ctx, claude.BetaFastMode)
	route, _ := gatewayAdaptiveRoute(ctx)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	model := "gpt-6.2-sol"
	require.Equal(t, "priority", adaptiveCandidateTier(ctx, route, account, model))
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
	require.Empty(t, adaptiveCandidateTier(ctx, route, account, model))
}

func TestGatewayAdaptiveRanksForceFastAndForceUltrafast(t *testing.T) {
	for i, tc := range []struct {
		name, forcedTier, plan string
		fast, ultra            bool
	}{
		{name: "ForceFast", fast: true, plan: "plus", forcedTier: "priority"},
		{name: "ForceUltrafast", ultra: true, plan: "pro", forcedTier: "ultrafast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := &Group{ID: 62001, Platform: PlatformComposite, Status: StatusActive, Hydrated: true, ForceOpenAIFast: tc.fast, ForceOpenAIUltrafast: tc.ultra}
			groupCtx := context.WithValue(context.Background(), ctxkey.Group, group)
			body := []byte(`{"model":"gpt-6.2-sol"}`)
			ctx := withGatewayAdaptiveRoute(groupCtx, "POST /v1/responses", "", body)
			model := "gpt-6.2-sol"
			baseID := int64(62002 + i*10)
			slow := adaptiveTestAccount(baseID, PlatformOpenAI, 1)
			fast := adaptiveTestAccount(baseID+1, PlatformOpenAI, 9)
			for _, account := range []*Account{&slow, &fast} {
				account.Type = AccountTypeOAuth
				account.Credentials = map[string]any{"plan_type": tc.plan}
			}
			svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{slow, fast}}, cfg: &config.Config{}}
			for j, account := range []*Account{&slow, &fast} {
				wireBody, err := svc.applyOpenAIFastPolicyToBody(ctx, account, model, body)
				require.NoError(t, err)
				wireTier := extractOpenAIServiceTierFromBody(wireBody)
				require.NotNil(t, wireTier)
				require.Equal(t, tc.forcedTier, *wireTier)
				route, _ := gatewayAdaptiveRoute(ctx)
				route.tier = tc.forcedTier
				key := adaptiveKey(route, account, model)
				t.Cleanup(func() {
					gatewayAdaptiveTelemetry.mu.Lock()
					defer gatewayAdaptiveTelemetry.mu.Unlock()
					delete(gatewayAdaptiveTelemetry.series, key)
				})
				for range adaptiveMinimum {
					observation := &adaptiveObservation{first: time.Now().Add(-time.Second / time.Duration(1+j*3)), calls: 1, status: http.StatusOK}
					observeAdaptiveOpenAIResult(ctx, observation, account, &OpenAIForwardResult{
						Model: model, UpstreamModel: model, ServiceTier: wireTier, UpstreamResponseServiceTier: "default",
					}, nil)
				}
			}
			baseline, err := svc.SelectAccountForModel(groupCtx, nil, "", model)
			require.NoError(t, err)
			require.Equal(t, slow.ID, baseline.ID)
			winner, err := svc.SelectAccountForModel(ctx, nil, "", model)
			require.NoError(t, err)
			require.Equal(t, fast.ID, winner.ID)
			scheduler := &defaultOpenAIAccountScheduler{service: svc}
			plan := openAIAccountLoadPlan{candidates: []openAIAccountCandidateScore{
				{account: &slow, loadInfo: &AccountLoadInfo{}, score: 10},
				{account: &fast, loadInfo: &AccountLoadInfo{}, score: 1},
			}, topK: 1}
			order := scheduler.buildOpenAISelectionOrder(ctx, OpenAIAccountScheduleRequest{RequestedModel: model}, plan)
			require.Equal(t, fast.ID, order[0].account.ID)

			// A Composite group keeps other platforms and non-entitled OAuth candidates usable.
			route, _ := gatewayAdaptiveRoute(ctx)
			other := adaptiveTestAccount(baseID+2, PlatformAnthropic, 5)
			require.Empty(t, adaptiveCandidateTier(ctx, route, &other, model))
			seedAdaptiveTestSamples(t, ctx, model, other)
			unknown := adaptiveTestAccount(baseID+4, PlatformOpenAI, 6)
			pool := []*Account{&slow, &other, &unknown, &fast}
			if tc.ultra {
				unentitled := slow
				unentitled.ID = baseID + 3
				unentitled.Credentials = map[string]any{"plan_type": "plus"}
				require.Empty(t, adaptiveCandidateTier(ctx, route, &unentitled, model))
				seedAdaptiveTestSamples(t, ctx, model, unentitled)
				pool = append(pool, &unentitled)
			}
			modelFor := adaptiveOpenAIModel(ctx, model, false)
			require.True(t, rankAdaptivePool(ctx, pool, adaptiveAccount, func(candidate *Account) string {
				if candidate.ID == unknown.ID {
					return ""
				}
				return modelFor(candidate)
			}, adaptiveAnyPeer, false))
			require.Equal(t, fast.ID, pool[0].ID)
			require.Equal(t, other.ID, pool[1].ID)
			require.Equal(t, unknown.ID, pool[2].ID)
		})
	}
}

func TestGatewayAdaptiveHTTPRejectsMismatchedUltrafastEcho(t *testing.T) {
	const model = "gpt-5.5"
	const path = "/v1/responses"
	body := []byte(`{"model":"gpt-5.5","service_tier":"priority","input":"hello","stream":false}`)
	ctx := withGatewayAdaptiveRoute(context.Background(), "POST "+path, "", body)
	account := adaptiveTestAccount(62120, PlatformOpenAI, 1)
	account.Credentials = map[string]any{"api_key": "sk-test"}
	account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: true}
	route, _ := gatewayAdaptiveRoute(ctx)
	priorityKey := adaptiveKey(route, &account, model)
	route.tier = "ultrafast"
	ultrafastKey := adaptiveKey(route, &account, model)
	t.Cleanup(func() {
		gatewayAdaptiveTelemetry.mu.Lock()
		defer gatewayAdaptiveTelemetry.mu.Unlock()
		delete(gatewayAdaptiveTelemetry.series, priorityKey)
		delete(gatewayAdaptiveTelemetry.series, ultrafastKey)
	})
	for _, tc := range []struct {
		echo, billingObservation string
	}{{"priority", "priority"}, {"ultrafast", ""}} {
		t.Run(tc.echo, func(t *testing.T) {
			payload := fmt.Sprintf(`{"id":"resp_1","object":"response","created_at":1795072425,"status":"completed","model":"%s","service_tier":"%s","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, model, tc.echo)
			upstream := &codexModelsHTTPUpstreamStub{do: func(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
				wire, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, "priority", optionalStringValue(extractOpenAIServiceTierFromBody(wire)))
				time.Sleep(time.Millisecond)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			result, err := svc.Forward(ctx, c, &account, body)
			require.NoError(t, err)
			require.Equal(t, tc.billingObservation, result.UpstreamResponseServiceTier)
			resolution := ApplyOpenAIServiceTierBillingResolution(&account, result)
			require.Equal(t, "priority", resolution.Billing)
			require.False(t, resolution.Downgraded)
			require.JSONEq(t, payload, recorder.Body.String())

			gatewayAdaptiveTelemetry.mu.Lock()
			defer gatewayAdaptiveTelemetry.mu.Unlock()
			require.NotNil(t, gatewayAdaptiveTelemetry.series[priorityKey])
			require.Equal(t, 1, gatewayAdaptiveTelemetry.series[priorityKey].next)
			require.NotContains(t, gatewayAdaptiveTelemetry.series, ultrafastKey)
		})
	}
}

func TestGatewayAdaptiveSamplesDirectChatAndOAuthImageFailures(t *testing.T) {
	for i, tc := range []struct {
		name, path, body, model string
		image                   bool
	}{
		{"raw Chat", "/v1/chat/completions", `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`, "gpt-5.4", false},
		{"OAuth Images", "/v1/images/generations", `{"model":"gpt-image-1","prompt":"draw a cat"}`, "gpt-image-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			ctx := withGatewayAdaptiveRoute(context.Background(), "POST "+tc.path, "", body)
			fixture := adaptiveTestAccount(int64(62100+i), PlatformOpenAI, 1)
			account := &fixture
			account.Credentials = map[string]any{"api_key": "sk-test", "base_url": "https://upstream.example"}
			account.Extra = map[string]any{openai_compat.ExtraKeyResponsesSupported: false}
			if tc.image {
				account.Type = AccountTypeOAuth
				account.Credentials = map[string]any{"access_token": "test"}
			}
			route, _ := gatewayAdaptiveRoute(ctx)
			key := adaptiveKey(route, account, tc.model)
			t.Cleanup(func() {
				gatewayAdaptiveTelemetry.mu.Lock()
				defer gatewayAdaptiveTelemetry.mu.Unlock()
				delete(gatewayAdaptiveTelemetry.series, key)
			})
			for _, status := range []int{400, 502, 503, 504} {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body)).WithContext(ctx)
				c.Request.Header.Set("Content-Type", "application/json")
				calls := 0
				upstream := &codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
					calls++
					time.Sleep(time.Millisecond)
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"message":"upstream returned %d"}}`, status)))}, nil
				}}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				var err error
				if tc.image {
					parsed, parseErr := svc.ParseOpenAIImagesRequest(c, body)
					require.NoError(t, parseErr)
					_, err = svc.ForwardImages(ctx, c, account, body, parsed, "")
				} else {
					_, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
				}
				require.Error(t, err)
				require.Equal(t, 1, calls)
			}
			gatewayAdaptiveTelemetry.mu.Lock()
			series := gatewayAdaptiveTelemetry.series[key]
			gatewayAdaptiveTelemetry.mu.Unlock()
			require.NotNil(t, series)
			require.Equal(t, 3, series.next)
		})
	}
}

func TestGatewayAdaptiveSingleConfirmedHTTPFailures(t *testing.T) {
	for i, operation := range []string{"POST /v1/chat/completions", "POST /v1/messages", "POST /v1/images/edits"} {
		ctx := withGatewayAdaptiveRoute(context.Background(), operation, "", nil)
		route, _ := gatewayAdaptiveRoute(ctx)
		account := adaptiveTestAccount(int64(61500+i), PlatformOpenAI, 0)
		key := adaptiveKey(route, &account, "wire-model")
		t.Cleanup(func() {
			gatewayAdaptiveTelemetry.mu.Lock()
			defer gatewayAdaptiveTelemetry.mu.Unlock()
			delete(gatewayAdaptiveTelemetry.series, key)
		})
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		for _, status := range []int{400, 429, 502, 503, 504} {
			obs := &adaptiveObservation{first: time.Now().Add(-time.Second), calls: 1, status: status}
			observeAdaptiveHTTPFailure(ctx, obs, &account, "wire-model", status)
			observeAdaptiveHTTPFailure(canceled, obs, &account, "wire-model", status)
			obs.calls++
			observeAdaptiveHTTPFailure(ctx, obs, &account, "wire-model", status)
		}
		gatewayAdaptiveTelemetry.mu.Lock()
		series := gatewayAdaptiveTelemetry.series[key]
		gatewayAdaptiveTelemetry.mu.Unlock()
		require.NotNil(t, series)
		require.Equal(t, 3, series.next)
	}
}

type adaptiveMetadataRepo struct{ schedulerTestOpenAIAccountRepo }

func (r adaptiveMetadataRepo) ListByGroup(context.Context, int64) ([]Account, error) {
	return r.accounts, nil
}

func TestGatewayAdaptiveStandardModelsRanksOnlySchedulerFallback(t *testing.T) {
	ctx := withGatewayAdaptiveRoute(context.Background(), "openai_standard_models_fallback", "http", nil)
	group := &Group{ID: 61540, Platform: PlatformOpenAI, CodexModelsManifestConfig: GroupCodexModelsManifestConfig{
		Enabled: true, FallbackToScheduler: true, AccountIDs: []int64{61541},
	}}
	slow := newCodexModelsAPIKeyTestAccount("https://slow.example/v1")
	fast := newCodexModelsAPIKeyTestAccount("https://fast.example/v1")
	slow.ID, slow.Priority = 61541, 0
	fast.ID, fast.Priority = 61542, 10
	for _, account := range []*Account{slow, fast} {
		account.Status, account.Schedulable, account.GroupIDs = StatusActive, true, []int64{group.ID}
	}
	seedAdaptiveTestSamples(t, ctx, "", *slow, *fast)
	svc := newCodexModelsAPIKeyTestService(&codexModelsHTTPUpstreamStub{do: func(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
		// Give the upstream observation nonzero elapsed time on Windows.
		time.Sleep(time.Millisecond)
		return ordinaryModelsUpstreamResponse(`{"data":[{"id":"wire-model"}]}`), nil
	}})
	svc.accountRepo = adaptiveMetadataRepo{schedulerTestOpenAIAccountRepo{accounts: []Account{*slow, *fast}}}
	_, account, err := svc.FetchPinnedOpenAIModelsList(ctx, group, 1, "")
	require.NoError(t, err)
	require.Equal(t, slow.ID, account.ID)

	group.CodexModelsManifestConfig.AccountIDs = []int64{99999}
	_, account, err = svc.FetchPinnedOpenAIModelsList(ctx, group, 1, "")
	require.NoError(t, err)
	require.Equal(t, fast.ID, account.ID)

	route, _ := gatewayAdaptiveRoute(ctx)
	gatewayAdaptiveTelemetry.mu.Lock()
	defer gatewayAdaptiveTelemetry.mu.Unlock()
	require.Equal(t, adaptiveMinimum, gatewayAdaptiveTelemetry.series[adaptiveKey(route, slow, "")].next)
	require.Equal(t, adaptiveMinimum+1, gatewayAdaptiveTelemetry.series[adaptiveKey(route, fast, "")].next)
}
