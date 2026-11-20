package service

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGatewayAdaptiveHealthPriorityFallbackAndRecovery(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	route := adaptiveRoute{operation: "POST /v1/messages", transport: "sse"}
	telemetry := &adaptiveTelemetry{}
	c, b, a := adaptiveTestAccount(1, PlatformAnthropic, 1), adaptiveTestAccount(2, PlatformAnthropic, 2), adaptiveTestAccount(3, PlatformAnthropic, 3)
	model := func(*Account) string { return "wire-model" }
	key := func(account *Account) adaptiveSampleKey { return adaptiveKey(route, account, model(account)) }
	for i := range 4 {
		at := now.Add(time.Duration(i) * time.Second)
		telemetry.observe(key(&a), at, time.Second, true)
		telemetry.observe(key(&b), at, 6*time.Second, true)
		telemetry.observe(key(&c), at, 9*time.Second, true)
	}
	want := func(now time.Time, ids ...int64) {
		t.Helper()
		pool := []*Account{&a, &b, &c}
		rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, adaptiveProbeOptions[*Account]{}, now, nil)
		for i, id := range ids {
			require.Equal(t, id, pool[i].ID)
		}
	}
	want(now.Add(4*time.Second), c.ID, b.ID, a.ID)
	for i := range 3 {
		telemetry.observe(key(&c), now.Add(time.Duration(i+4)*time.Second), 0, false)
	}
	failureAt := now.Add(6 * time.Second)
	want(failureAt, b.ID, a.ID, c.ID)
	retryAt := telemetry.series[key(&c)].health.RetryAt
	require.Equal(t, failureAt.Add(5*time.Minute), retryAt)
	require.False(t, telemetry.score(key(&c), retryAt.Add(-time.Millisecond)).canRetest(retryAt.Add(-time.Millisecond)))
	for i := range 3 {
		at := retryAt.Add(time.Duration(i) * time.Second)
		telemetry.observeAttempt(key(&c), at, 8*time.Second, true, true)
		if i < 2 {
			require.Equal(t, uint8(i+1), telemetry.series[key(&c)].health.Passed)
			want(at, b.ID, a.ID, c.ID)
		}
	}
	want(retryAt.Add(3*time.Second), c.ID, b.ID, a.ID)
	require.Equal(t, adaptiveHealthy, telemetry.series[key(&c)].health.Fault)
}

func TestGatewayAdaptiveHealthLatencyBudgetAndComparableCost(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		latency time.Duration
		points  int
	}{
		{10 * time.Second, 100}, {10*time.Second + 1, 80}, {20*time.Second - 1, 80},
		{20 * time.Second, 40}, {30 * time.Second, 40}, {30*time.Second + 1, 0},
	} {
		require.Equal(t, tc.points, adaptiveTTFTPoints(tc.latency))
	}
	for _, tc := range []struct {
		operation, transport string
		text                 bool
	}{
		{"POST /v1/chat/completions", "sse", true}, {"POST /v1/messages", "sse", true},
		{"POST /v1beta/models/*modelAction:streamGenerateContent", "sse", true},
		{"GET /v1/responses", "ws", true}, {"GET /v1/realtime", "ws", false},
		{"POST /v1/images/generations", "sse", false}, {"POST /v1/embeddings", "http", false},
		{"POST /v1/responses", "http", false},
	} {
		require.Equal(t, tc.text, adaptiveTextStream(adaptiveRoute{operation: tc.operation, transport: tc.transport}), tc.operation)
	}
	route := adaptiveRoute{operation: "POST /v1/responses", transport: "sse"}
	telemetry := &adaptiveTelemetry{}
	cheap, expensive := adaptiveTestAccount(1, PlatformOpenAI, 1), adaptiveTestAccount(2, PlatformOpenAI, 2)
	model := func(*Account) string { return "wire-model" }
	cheapKey, expensiveKey := adaptiveKey(route, &cheap, model(&cheap)), adaptiveKey(route, &expensive, model(&expensive))
	for i := range 10 {
		telemetry.observe(cheapKey, now.Add(time.Duration(i)*time.Second), 6*time.Second, true)
	}
	require.Equal(t, 6*time.Second, telemetry.series[cheapKey].health.Baseline)
	for i := range 5 {
		telemetry.observe(cheapKey, now.Add(time.Duration(10+i)*time.Second), 9*time.Second, true)
	}
	require.True(t, telemetry.score(cheapKey, now.Add(15*time.Second)).probe)
	require.Equal(t, adaptiveHealthy, telemetry.series[cheapKey].health.Fault)
	require.Equal(t, 6*time.Second, telemetry.series[cheapKey].health.Baseline)
	for i := range 6 {
		telemetry.observe(cheapKey, now.Add(time.Duration(20+i)*time.Second), 25*time.Second, true)
	}
	at := now.Add(30 * time.Second)
	require.Equal(t, adaptiveLatencyDegraded, telemetry.series[cheapKey].health.Fault)
	winner := func() int64 {
		pool := []*Account{&cheap, &expensive}
		rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, adaptiveProbeOptions[*Account]{}, at, nil)
		return pool[0].ID
	}
	require.Equal(t, cheap.ID, winner())
	for range 3 {
		telemetry.observe(expensiveKey, at, 20*time.Second, true)
	}
	require.Equal(t, cheap.ID, winner())
	for range 4 {
		telemetry.observe(expensiveKey, at, 12*time.Second, true)
	}
	require.Equal(t, expensive.ID, winner())
	pool := []*Account{&cheap, &expensive}
	rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, func(_, _ *Account) bool { return false }, adaptiveProbeOptions[*Account]{}, at, nil)
	require.Equal(t, cheap.ID, pool[0].ID)
}

func TestGatewayAdaptiveHealthRapidFailuresAndBackoff(t *testing.T) {
	telemetry := &adaptiveTelemetry{}
	key := adaptiveSampleKey{accountID: 1, route: adaptiveRoute{operation: "POST /v1/embeddings", transport: "http"}}
	now := time.Unix(1_800_000_000, 0)
	for i := range 5 {
		telemetry.observe(key, now.Add(time.Duration(i)*time.Second), time.Second, i%2 != 0)
	}
	state := telemetry.series[key].health
	require.Equal(t, adaptiveUpstreamDegraded, state.Fault)
	require.Equal(t, 5*time.Minute, state.RetryAt.Sub(state.Last))
	for _, delay := range []time.Duration{10, 20, 30, 30} {
		telemetry.observeAttempt(key, state.RetryAt, 0, false, true)
		state = telemetry.series[key].health
		require.Equal(t, delay*time.Minute, state.RetryAt.Sub(state.Last))
	}
}

func TestGatewayAdaptiveRecoveryLowHighTrafficAndSingleFlight(t *testing.T) {
	recovery := &adaptiveRecovery{}
	scope := adaptiveProbeScope{groupID: 10, model: "model"}
	key := adaptiveSampleKey{accountID: 1}
	now := time.Unix(1_800_000_000, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ingress := &adaptiveIngress{}
	ctx = context.WithValue(ctx, adaptiveIngressKey{}, ingress)
	require.True(t, recovery.opportunity(ctx, ingress, scope, now)(key), "sparse pool may probe on the first request")
	_, _ = adaptiveTrackSlot(ctx, key.accountID, &AcquireResult{}, nil)
	require.Zero(t, recovery.inFlight, "no slot frees the reservation")

	var winners atomic.Int32
	var selected atomic.Pointer[adaptiveIngress]
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			request := &adaptiveIngress{}
			if recovery.opportunity(ctx, request, scope, now.Add(time.Second))(key) {
				winners.Add(1)
				selected.Store(request)
			}
		})
	}
	wg.Wait()
	require.Equal(t, int32(1), winners.Load())
	requestCtx := context.WithValue(ctx, adaptiveIngressKey{}, selected.Load())
	slot, err := adaptiveTrackSlot(requestCtx, key.accountID, &AcquireResult{Acquired: true, ReleaseFunc: func() {}}, nil)
	require.NoError(t, err)
	slot.ReleaseFunc()
	for range 99 {
		request := &adaptiveIngress{}
		require.False(t, recovery.opportunity(ctx, request, scope, now.Add(2*time.Second))(key))
	}
	next := &adaptiveIngress{}
	require.True(t, recovery.opportunity(ctx, next, scope, now.Add(2*time.Second))(key))
	next.recovery.Load().finish()
}

func TestGatewayAdaptiveHealthRestorePreservesObservationAndRetest(t *testing.T) {
	now := time.Now().Truncate(time.Microsecond)
	var stored []AdaptiveHistoryRecord
	store := adaptiveHistoryTestStore{
		read: func(context.Context, time.Time, time.Time) ([]AdaptiveHistoryRecord, error) {
			return slices.Clone(stored), nil
		},
		merge: func(_ context.Context, _ time.Time, records []AdaptiveHistoryRecord) error {
			stored = append(stored, records...)
			return nil
		},
	}
	telemetry := &adaptiveTelemetry{}
	ctx := telemetry.attachAdaptiveHistory(store)
	defer telemetry.history.stop()
	key := adaptiveSampleKey{groupID: 10, accountID: 1, route: adaptiveRoute{operation: "POST /v1/chat/completions", transport: "sse"}}
	for i := range 3 {
		telemetry.observe(key, now.Add(time.Duration(i-10)*time.Minute), 0, false)
	}
	telemetry.observeAttempt(key, now.Add(-time.Minute), 9*time.Second, true, true)
	require.NoError(t, telemetry.history.flush(ctx, telemetry, true))
	restart := &adaptiveTelemetry{}
	ctx2 := restart.attachAdaptiveHistory(store)
	defer restart.history.stop()
	require.NoError(t, restart.history.restore(ctx2, restart, now.Add(25*time.Hour)))
	require.Equal(t, uint8(1), restart.series[key].health.Passed)
	require.True(t, restart.series[key].health.Last.Equal(now.Add(-time.Minute)))
	require.True(t, restart.score(key, now.Add(25*time.Hour)).canRetest(now.Add(25*time.Hour)))
	require.False(t, restart.score(key, now.Add(25*time.Hour)).known)
	require.Empty(t, restart.history.queue)
}

func TestGatewayAdaptiveSoftAndHardContinuity(t *testing.T) {
	now := time.Now()
	telemetry := &adaptiveTelemetry{}
	settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
	a := adaptiveTestAccount(1, PlatformOpenAI, 1)
	for _, tc := range []struct {
		body, transport string
		escape          bool
	}{
		{`{"model":"model","stream":true,"input":[{"role":"user","content":"question"},{"role":"assistant","content":"answer"},{"role":"user","content":"new question"}]}`, "", true},
		{`{"model":"model","stream":true,"input":"hello","previous_response_id":"resp_1"}`, "", false},
		{`{"model":"model","stream":true,"input":"hello","conversation":"conv_1"}`, "", false},
		{`{"model":"model","stream":true,"input":[{"type":"function_call_output","output":"result"}]}`, "", false},
		{`{"model":"model","input":"hello"}`, "ws", false},
	} {
		ctx := WithGatewayAdaptiveIngress(context.Background(), settings, "POST /v1/responses", tc.transport)
		ctx = SetGatewayAdaptiveInput(ctx, []byte(tc.body))
		route, _ := gatewayAdaptiveRoute(ctx)
		for range 3 {
			telemetry.observe(adaptiveContextKey(ctx, route, &a, "model"), now, 0, false)
		}
		require.Equal(t, tc.escape, adaptiveStickyEscape(ctx, &a, "model"), tc.body)
		require.False(t, adaptiveStickyEscape(context.Background(), &a, "model"))
		header := make(http.Header)
		header.Set(grokConversationIDHeader, "native-continuation")
		require.False(t, adaptiveStickyEscape(WithGatewayAdaptiveUpstreamSession(ctx, header), &a, "model"))
	}
}

func TestGatewayAdaptiveRecoveryNoSlotKeepsWaitOrder(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	telemetry := &adaptiveTelemetry{}
	c, b := adaptiveTestAccount(21, PlatformAnthropic, 1), adaptiveTestAccount(22, PlatformAnthropic, 2)
	route := adaptiveRoute{operation: "POST /v1/messages", transport: "sse"}
	model := func(*Account) string { return "wire-model" }
	cKey := adaptiveKey(route, &c, model(&c))
	for range 3 {
		telemetry.observe(cKey, now, 0, false)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ingress := &adaptiveIngress{telemetry: telemetry}
	ctx = context.WithValue(ctx, adaptiveIngressKey{}, ingress)
	var waitOrder []*Account
	pool := []*Account{&c, &b}
	due := now.Add(5 * time.Minute)
	options := adaptiveProbeOptions[*Account]{
		waitOrder: &waitOrder,
		reserve:   telemetry.recovery.opportunity(ctx, ingress, adaptiveProbeScope{route: route}, due),
	}
	changed, normal := rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, options, due, nil)
	require.True(t, changed)
	require.Equal(t, &c, pool[0])
	require.Equal(t, &b, pool[normal])
	require.Equal(t, []*Account{&b, &c}, waitOrder)
	_, _ = adaptiveTrackSlot(ctx, c.ID, &AcquireResult{}, nil)
	require.Nil(t, ingress.recovery.Load())
	require.Zero(t, telemetry.series[cKey].health.Passed)
}

func TestGatewayAdaptiveOpenAIContinuationCannotMove(t *testing.T) {
	const model, response = "gpt-5.1", "resp_adaptive_owner"
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			groupID := int64(37)
			c, b := adaptiveTestAccount(7101, PlatformOpenAI, 1), adaptiveTestAccount(7102, PlatformOpenAI, 2)
			for _, account := range []*Account{&c, &b} {
				account.GroupIDs = []int64{groupID}
				account.Extra = map[string]any{"openai_apikey_responses_websockets_v2_enabled": true}
			}
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			svc := &OpenAIGatewayService{
				accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{c, b}},
				cache:              &schedulerTestGatewayCache{},
				cfg:                cfg,
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			ctx := withGatewayAdaptiveRoute(context.Background(), "GET /v1/responses", "ws", nil)
			require.NoError(t, svc.getOpenAIWSStateStore().BindResponseAccount(ctx, groupID, response, b.ID, time.Hour))
			selectAccount := func(excluded map[int64]struct{}, canRebuild bool) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
				return svc.SelectAccountWithSchedulerForCapability(ctx, &groupID, response, "", model, excluded,
					OpenAIUpstreamTransportAny, "", false, canRebuild, false)
			}
			selection, decision, err := selectAccount(nil, true)
			require.NoError(t, err)
			require.True(t, decision.StickyPreviousHit)
			require.Equal(t, b.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			selection, _, err = selectAccount(map[int64]struct{}{b.ID: {}}, true)
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)

			require.NoError(t, svc.getOpenAIWSStateStore().DeleteResponseAccount(ctx, groupID, response))
			selection, _, err = selectAccount(nil, false)
			require.ErrorIs(t, err, ErrNoAvailableAccounts)
			require.Nil(t, selection)
			selection, decision, err = selectAccount(map[int64]struct{}{b.ID: {}}, true)
			require.NoError(t, err)
			require.False(t, decision.StickyPreviousHit, "handler must receive the cache miss to rebuild the first request")
			require.Equal(t, c.ID, selection.Account.ID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestGatewayAdaptiveOpenAISoftStickyMovesOnce(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			const model, session = "gpt-5.1", "adaptive-soft-session"
			groupID := int64(39)
			c, b := adaptiveTestAccount(7201, PlatformOpenAI, 1), adaptiveTestAccount(7202, PlatformOpenAI, 2)
			c.GroupIDs, b.GroupIDs = []int64{groupID}, []int64{groupID}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:" + session: c.ID}}
			svc := &OpenAIGatewayService{
				accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{c, b}},
				cache:              cache,
				cfg:                &config.Config{},
				rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced),
				concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
			}
			telemetry := &adaptiveTelemetry{}
			settings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
			ctx := WithGatewayAdaptiveIngress(context.Background(), settings, "POST /v1/responses", "")
			ctx = WithGatewayAdaptiveProbeGroup(ctx, &groupID)
			ctx = SetGatewayAdaptiveInput(ctx, []byte(`{"model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"next"}]}`))
			route, _ := gatewayAdaptiveRoute(ctx)
			route.tier = adaptiveCandidateTier(ctx, route, &c, model)
			key := adaptiveContextKey(ctx, route, &c, model)
			now := time.Now().Add(-10 * time.Minute)
			for range 3 {
				telemetry.observe(key, now, 0, false)
			}
			choose := func(ctx context.Context) int64 {
				t.Helper()
				selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", session, model, nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, selection)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				return selection.Account.ID
			}
			require.Equal(t, c.ID, choose(context.Background()))
			hard := SetGatewayAdaptiveInput(ctx, []byte(`{"model":"gpt-5.1","stream":true,"conversation":"conv_test","input":"next"}`))
			require.Equal(t, c.ID, choose(hard))
			require.Equal(t, b.ID, choose(ctx))
			for i := range 3 {
				telemetry.observeAttempt(key, now.Add(5*time.Minute+time.Duration(i)*time.Second), 8*time.Second, true, true)
			}
			require.Equal(t, adaptiveHealthy, telemetry.series[key].health.Fault)
			require.Equal(t, b.ID, choose(ctx), "recovered C must not reclaim B's existing session")
		})
	}
}

func TestGatewayAdaptiveComparisonCollectsRealEvidenceBeforeSwitching(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		seed                  []time.Duration
		backup                time.Duration
		wantProbe, wantSwitch bool
	}{
		{"absolute", []time.Duration{25, 25, 25, 25, 25, 25}, 10, true, true},
		{"relative", []time.Duration{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 15, 15, 15, 15, 15, 15}, 8, true, true},
		{"occasional", []time.Duration{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 15}, 8, false, false},
		{"backup not faster", []time.Duration{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 15, 15, 15, 15, 15, 15}, 16, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			telemetry := &adaptiveTelemetry{}
			route := adaptiveRoute{operation: "POST /v1/chat/completions", transport: "sse"}
			c, b := adaptiveTestAccount(8101, PlatformOpenAI, 1), adaptiveTestAccount(8102, PlatformOpenAI, 2)
			model := func(*Account) string { return "wire-model" }
			cKey, bKey := adaptiveKey(route, &c, model(&c)), adaptiveKey(route, &b, model(&b))
			normalWinner := func(at time.Time) *Account {
				pool := []*Account{&b, &c}
				rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, adaptiveProbeOptions[*Account]{}, at, nil)
				return pool[0]
			}
			require.Equal(t, &c, normalWinner(now))
			for i, latency := range tc.seed {
				telemetry.observe(cKey, now.Add(time.Duration(i-len(tc.seed))*time.Second), latency*time.Second, true)
			}
			require.NotContains(t, telemetry.series, bKey)
			for i := range 3 {
				at := now.Add(time.Duration(i) * 2 * time.Second)
				require.Equal(t, &c, normalWinner(at), "ordinary traffic must stay on C until B has three samples")
				ingress := &adaptiveIngress{telemetry: telemetry}
				ctx := context.WithValue(context.Background(), adaptiveIngressKey{}, ingress)
				pool := []*Account{&c, &b}
				var wait []*Account
				options := adaptiveProbeOptions[*Account]{
					waitOrder: &wait,
					reserve:   telemetry.recovery.opportunity(ctx, ingress, adaptiveProbeScope{route: route}, at),
				}
				_, baseline := rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, options, at, nil)
				if !tc.wantProbe {
					require.Zero(t, baseline)
					require.Equal(t, &c, pool[0])
					continue
				}
				require.Equal(t, 1, baseline)
				require.Equal(t, &c, wait[0])
				require.Equal(t, &b, pool[0])
				slot, err := adaptiveTrackSlot(ctx, b.ID, &AcquireResult{Acquired: true}, nil)
				require.NoError(t, err)
				observeAdaptiveAttempt(ctx, route, &b, model(&b), at, tc.backup*time.Second, true)
				slot.ReleaseFunc()
			}
			require.Equal(t, tc.wantSwitch, normalWinner(now.Add(time.Minute)) == &b)
		})
	}
}

func TestGatewayAdaptiveOpenAICostPreferenceHealthExceptions(t *testing.T) {
	const model = "gpt-5.1"
	for _, mode := range []struct {
		name, advanced string
		batch          bool
	}{
		{"legacy", "false", false},
		{"legacy batched", "false", true},
		{"advanced", "true", true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, scenario := range []string{"healthy", "fault", "faster", "no improvement", "different actual model", "compact tier", "profit veto"} {
				t.Run(scenario, func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
					group := int64(47)
					c, b := adaptiveTestAccount(901, PlatformOpenAI, 1), adaptiveTestAccount(902, PlatformOpenAI, 2)
					cheapRate, expensiveRate := 1.0, 1.5
					c.GroupIDs, b.GroupIDs = []int64{group}, []int64{group}
					c.RateMultiplier, b.RateMultiplier = &cheapRate, &expensiveRate
					compact := scenario == "compact tier"
					if compact {
						c.Extra = map[string]any{"openai_compact_supported": true}
						require.NotEqual(t, openAICompactSupportTier(&c), openAICompactSupportTier(&b))
					}
					if scenario == "different actual model" {
						b.Credentials = map[string]any{"model_mapping": map[string]any{model: "gpt-5.2"}}
					}
					cfg := &config.Config{}
					cfg.Gateway.Scheduling.LoadBatchEnabled = mode.batch
					cfg.Gateway.OpenAIWS.LBTopK = 1
					cfg.Gateway.OpenAIWS.SchedulerScoreWeights.UpstreamCost = 1
					settings := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
						openAIAdvancedSchedulerSettingKey:              mode.advanced,
						SettingKeyOpenAILowUpstreamRatePriorityEnabled: "true",
					}}
					cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:stateful": c.ID}}
					concurrency := &upstreamCostTrackingConcurrencyCache{}
					svc := &OpenAIGatewayService{
						accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{c, b}},
						cache:              cache,
						cfg:                cfg,
						rateLimitService:   &RateLimitService{settingService: NewSettingService(settings, cfg)},
						concurrencyService: NewConcurrencyService(concurrency),
					}
					telemetry := &adaptiveTelemetry{}
					adaptiveSettings := &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}
					body := []byte(`{"model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"next"}]}`)
					newRequest := func(parent context.Context) context.Context {
						ctx := WithGatewayAdaptiveIngress(parent, adaptiveSettings, "POST /v1/responses", "")
						ctx = WithGatewayAdaptiveProbeGroup(ctx, &group)
						return SetGatewayAdaptiveInput(ctx, body)
					}
					parent := context.Background()
					if scenario == "profit veto" {
						parent = context.WithValue(parent, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: group, platform: PlatformOpenAI, threshold: 1.2})
					}
					ctx := newRequest(parent)
					route, enabled := gatewayAdaptiveRoute(ctx)
					require.True(t, enabled)
					actualModel := adaptiveOpenAIModel(ctx, model, compact)
					key := func(a *Account) adaptiveSampleKey {
						upstreamModel := actualModel(a)
						accountRoute := route
						accountRoute.tier = adaptiveCandidateTier(ctx, route, a, upstreamModel)
						require.NotEqual(t, "unknown", accountRoute.tier)
						return adaptiveContextKey(ctx, accountRoute, a, upstreamModel)
					}
					cKey, bKey := key(&c), key(&b)
					if scenario == "different actual model" {
						require.NotEqual(t, cKey.model, bKey.model)
					}
					selectAccount := func(ctx context.Context, session string, id int64, sample time.Duration) {
						t.Helper()
						result, _, err := svc.SelectAccountWithScheduler(ctx, &group, "", session, model, nil, OpenAIUpstreamTransportAny, compact)
						require.NoError(t, err)
						require.NotNil(t, result)
						require.True(t, result.Acquired)
						defer result.ReleaseFunc()
						require.Equal(t, id, result.Account.ID)
						if sample > 0 {
							ingress, ok := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
							require.True(t, ok, "adaptive ingress must exist for a recovered probe")
							lease := ingress.recovery.Load()
							require.NotNil(t, lease)
							require.Equal(t, bKey, lease.key)
							require.True(t, lease.acquired.Load())
							observedRoute := route
							observedRoute.tier = bKey.route.tier
							observeAdaptiveAttempt(ctx, observedRoute, result.Account, bKey.model, time.Now(), sample, true)
						}
					}

					selectAccount(ctx, "", c.ID, 0)
					require.Empty(t, concurrency.limits(b.ID))
					seedAt := time.Now().Add(-time.Minute)
					switch scenario {
					case "healthy":
						for range 6 {
							telemetry.observe(cKey, seedAt, 9*time.Second, true)
							telemetry.observe(bKey, seedAt, 4*time.Second, true)
						}
						selectAccount(newRequest(parent), "", c.ID, 0)
						require.Empty(t, concurrency.limits(b.ID))
					case "fault":
						for range 3 {
							telemetry.observe(cKey, seedAt, 0, false)
						}
						selectAccount(newRequest(parent), "", b.ID, 0)
					default:
						for range 6 {
							telemetry.observe(cKey, seedAt, 25*time.Second, true)
						}
						require.Equal(t, adaptiveLatencyDegraded, telemetry.score(cKey, time.Now()).health.Fault)
						require.NotContains(t, telemetry.series, bKey)
						if scenario == "different actual model" || compact || scenario == "profit veto" {
							selectAccount(newRequest(parent), "", c.ID, 0)
							require.Empty(t, concurrency.limits(b.ID))
							return
						}
						backupTTFT := 8 * time.Second
						if scenario == "no improvement" {
							backupTTFT = 25 * time.Second
						}
						for i := range 3 {
							selectAccount(withoutAdaptiveExploration(newRequest(parent)), "", c.ID, 0)
							selectAccount(newRequest(parent), "", b.ID, backupTTFT)
							require.Equal(t, i+1, telemetry.score(bKey, time.Now()).comparable)
							// Advance the process probe interval without adding wall-clock sleeps.
							telemetry.recovery.mu.Lock()
							telemetry.recovery.lastProbe = time.Now().Add(-2 * time.Second)
							telemetry.recovery.mu.Unlock()
						}
						if scenario == "no improvement" {
							selectAccount(newRequest(parent), "", c.ID, 0)
							require.Len(t, concurrency.limits(b.ID), 3)
							return
						}
						selectAccount(newRequest(parent), "", b.ID, 0)
						stateful := SetGatewayAdaptiveInput(newRequest(parent), []byte(`{"model":"gpt-5.1","stream":true,"conversation":"conv_pinned","input":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"next"}]}`))
						require.True(t, adaptiveHardContinuity(stateful))
						selectAccount(stateful, "stateful", c.ID, 0)
						require.Equal(t, c.ID, cache.sessionBindings["openai:stateful"])
					}
				})
			}
		})
	}
}

func TestGatewayAdaptiveStaleFaultKeepsPriorityAndRemainsDue(t *testing.T) {
	base := time.Now().Add(-25 * time.Hour)
	at := base.Add(25 * time.Hour)
	telemetry := &adaptiveTelemetry{}
	route := adaptiveRoute{operation: "POST /v1/messages", transport: "sse"}
	c, b := adaptiveTestAccount(91, PlatformAnthropic, 1), adaptiveTestAccount(92, PlatformAnthropic, 2)
	model := func(*Account) string { return "wire-model" }
	key := adaptiveKey(route, &c, model(&c))
	for range 3 {
		telemetry.observe(key, base, 0, false)
	}
	require.Equal(t, adaptiveUpstreamDegraded, telemetry.series[key].health.Fault)
	state := telemetry.score(key, at)
	require.Equal(t, adaptiveHealthy, state.health.Fault)
	require.True(t, state.canRetest(at))
	require.Equal(t, base.Add(5*time.Minute), state.health.RetryAt)
	pool := []*Account{&c, &b}
	rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, adaptiveAnyPeer, adaptiveProbeOptions[*Account]{}, at, nil)
	require.Equal(t, &c, pool[0])
	telemetry.observe(key, at, 6*time.Second, true)
	require.True(t, telemetry.score(key, at).canRetest(at), "ordinary traffic must leave an expired conclusion due for validation")
}

func TestGatewayAdaptiveRecoveryAliasesAndSlidingTraffic(t *testing.T) {
	recovery := &adaptiveRecovery{}
	now := time.Unix(1_800_000_000, 0)
	scopes := []adaptiveProbeScope{{model: "alias-one"}, {model: "alias-two"}}
	key := adaptiveSampleKey{accountID: 9, model: "wire-model"}
	ctx := context.Background()
	first := &adaptiveIngress{}
	require.True(t, recovery.opportunity(ctx, first, scopes[0], now)(key))
	require.False(t, recovery.opportunity(ctx, &adaptiveIngress{}, scopes[1], now.Add(2*time.Second))(key))
	cancelUnacquiredAdaptiveProbe(context.WithValue(ctx, adaptiveIngressKey{}, first))
	require.Nil(t, first.recovery.Load())
	second := &adaptiveIngress{}
	require.True(t, recovery.opportunity(ctx, second, scopes[1], now.Add(3*time.Second))(key))
	cancelUnacquiredAdaptiveProbe(context.WithValue(ctx, adaptiveIngressKey{}, second))
	require.Nil(t, second.recovery.Load())

	scope := adaptiveProbeScope{groupID: 37, model: "busy"}
	for i := range 120 {
		recovery.opportunity(ctx, &adaptiveIngress{}, scope, now.Add(time.Duration(i)*500*time.Millisecond))
	}
	first = &adaptiveIngress{}
	ctx = context.WithValue(ctx, adaptiveIngressKey{}, first)
	now = now.Add(time.Minute)
	require.True(t, recovery.opportunity(ctx, first, scope, now)(key))
	slot, err := adaptiveTrackSlot(ctx, key.accountID, &AcquireResult{Acquired: true}, nil)
	require.NoError(t, err)
	slot.ReleaseFunc()
	for i := 1; i < 100; i++ {
		require.False(t, recovery.opportunity(ctx, &adaptiveIngress{}, scope, now.Add(time.Duration(i)*500*time.Millisecond))(key),
			"continuous 120/min must not open the low-traffic gate at a 30-second boundary")
	}
	next := &adaptiveIngress{}
	require.True(t, recovery.opportunity(ctx, next, scope, now.Add(50*time.Second))(key))
	next.recovery.Load().finish()
}

func TestGatewayAdaptiveSlowStickyNeedsProvenFreeAlternative(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, tc := range []struct {
			name        string
			samples     int
			ttft        time.Duration
			free, moves bool
		}{
			{"unknown", 0, 0, true, false},
			{"slow backup", 3, 22, true, false},
			{"proved but busy", 3, 9, false, false},
			{"proved and free", 3, 9, true, true},
		} {
			t.Run(advanced+"/"+tc.name, func(t *testing.T) {
				const model, session = "gpt-5.1", "adaptive-wait-owner"
				group := int64(43)
				c, b := adaptiveTestAccount(8111, PlatformOpenAI, 1), adaptiveTestAccount(8112, PlatformOpenAI, 2)
				c.GroupIDs, b.GroupIDs = []int64{group}, []int64{group}
				cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:" + session: c.ID}}
				cfg := &config.Config{}
				cfg.Gateway.Scheduling.LoadBatchEnabled = true
				cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
				cfg.Gateway.Scheduling.StickySessionWaitTimeout = 45 * time.Second
				service := &OpenAIGatewayService{
					accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{c, b}},
					cache:       cache, cfg: cfg, rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced),
					concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
						acquireResults: map[int64]bool{c.ID: false, b.ID: tc.free},
						loadMap: map[int64]*AccountLoadInfo{
							c.ID: {AccountID: c.ID, LoadRate: 100, CurrentConcurrency: 1},
							b.ID: {AccountID: b.ID, LoadRate: 0},
						},
					}),
				}
				telemetry := &adaptiveTelemetry{}
				ctx := WithGatewayAdaptiveProbeGroup(WithGatewayAdaptiveIngress(context.Background(), &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}, "POST /v1/responses", ""), &group)
				ctx = SetGatewayAdaptiveInput(ctx, []byte(`{"model":"gpt-5.1","stream":true,"input":[{"role":"user","content":"q"},{"role":"assistant","content":"a"},{"role":"user","content":"next"}]}`))
				route, _ := gatewayAdaptiveRoute(ctx)
				route.tier = adaptiveCandidateTier(ctx, route, &c, model)
				now := time.Now()
				for range 5 {
					telemetry.observe(adaptiveContextKey(ctx, route, &c, model), now, 25*time.Second, true)
				}
				for range tc.samples {
					telemetry.observe(adaptiveContextKey(ctx, route, &b, model), now, tc.ttft*time.Second, true)
				}
				got, _, err := service.SelectAccountWithScheduler(ctx, &group, "", session, model, nil, OpenAIUpstreamTransportAny, false)
				require.NoError(t, err)
				require.NotNil(t, got)
				if tc.moves {
					require.True(t, got.Acquired)
					require.Equal(t, b.ID, got.Account.ID)
					require.Equal(t, b.ID, cache.sessionBindings["openai:"+session])
					got.ReleaseFunc()
				} else {
					require.NotNil(t, got.WaitPlan)
					require.Equal(t, c.ID, got.WaitPlan.AccountID)
					require.Equal(t, 45*time.Second, got.WaitPlan.Timeout)
					require.Equal(t, c.ID, cache.sessionBindings["openai:"+session])
				}
			})
		}
	}
}
