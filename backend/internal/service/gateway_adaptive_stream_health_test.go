package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

const streamHealthPublicModel = "deepseek-v4.1-flash"
const streamHealthEconomyModel = "deepseek-v4.1-flash-economy"
const streamHealthBody = `{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"hello"}]}`

func streamHealthFixture(group int64) (c, b, a Account) {
	zero, paid := 0.0, 0.3
	c, b, a = adaptiveTestAccount(301, PlatformDeepseek, 1), adaptiveTestAccount(302, PlatformDeepseek, 2), adaptiveTestAccount(303, PlatformDeepseek, 3)
	for _, acc := range []*Account{&c, &b, &a} {
		acc.GroupIDs = []int64{group}
		acc.Credentials = map[string]any{"api_protocol": APIProtocolChatCompletions, "api_key": "test-key", "base_url": "https://upstream.example"}
		acc.Credentials["model_mapping"] = map[string]any{streamHealthPublicModel: streamHealthPublicModel}
		acc.RateMultiplier = &zero
	}
	c.Credentials["model_mapping"] = map[string]any{streamHealthPublicModel: streamHealthEconomyModel}
	a.RateMultiplier = &paid
	return
}

func streamHealthContext(ctx context.Context, telemetry *adaptiveTelemetry, group int64, body string) context.Context {
	ctx = WithGatewayAdaptiveIngress(ctx, &SettingService{settingRepo: adaptiveTestSettings{}, adaptiveTelemetry: telemetry}, "POST /v1/chat/completions", "")
	return SetGatewayAdaptiveInput(WithGatewayAdaptiveProbeGroup(ctx, &group), []byte(body))
}

func streamHealthKey(t *testing.T, ctx context.Context, acc *Account) adaptiveStreamKey {
	t.Helper()
	route, ok := gatewayAdaptiveRoute(ctx)
	require.True(t, ok)
	key, ok := adaptiveStreamCandidateKey(ctx, route, acc, streamHealthPublicModel)
	require.True(t, ok)
	return key
}

type streamHealthBindingCache struct {
	schedulerTestGatewayCache
	failKey string
	reads   []string
	ttls    map[string]time.Duration
}

func (c *streamHealthBindingCache) GetSessionAccountID(ctx context.Context, group int64, key string) (int64, error) {
	c.reads = append(c.reads, key)
	if key == c.failKey {
		return 0, errors.New("sticky cache unavailable")
	}
	return c.schedulerTestGatewayCache.GetSessionAccountID(ctx, group, key)
}

func (c *streamHealthBindingCache) SetSessionAccountID(ctx context.Context, group int64, key string, id int64, ttl time.Duration) error {
	if c.ttls == nil {
		c.ttls = make(map[string]time.Duration)
	}
	c.ttls[key] = ttl
	return c.schedulerTestGatewayCache.SetSessionAccountID(ctx, group, key, id, ttl)
}

type streamHealthConcurrency struct {
	upstreamCostTrackingConcurrencyCache
	deny     int64
	attempts []int64
}

func (c *streamHealthConcurrency) AcquireAccountSlot(ctx context.Context, id int64, limit int, request string) (bool, error) {
	c.attempts = append(c.attempts, id)
	if id == c.deny {
		return false, nil
	}
	return c.upstreamCostTrackingConcurrencyCache.AcquireAccountSlot(ctx, id, limit, request)
}

type streamHealthAccountRepo struct {
	schedulerTestOpenAIAccountRepo
	stale int64
}

func (r streamHealthAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	if id == r.stale {
		return nil, errors.New("candidate removed")
	}
	return r.schedulerTestOpenAIAccountRepo.GetByID(ctx, id)
}

func TestGatewayAdaptiveStreamHealthSelection(t *testing.T) {
	for _, mode := range []struct {
		name, advanced string
		batch          bool
	}{{"advanced-top-k-1", "true", true}, {"legacy-batch", "false", true}, {"legacy-non-batch", "false", false}} {
		t.Run(mode.name, func(t *testing.T) {
			for _, scenario := range []string{
				"healthy-alternative", "single-spike", "no-slot", "failed-recheck", "missing-samples", "cooldown",
				"probe-no-slot", "probe-recheck-failure", "profit-terminal-veto",
				"only-higher-rate-is-healthy", "two-recovery-votes", "three-recovery-votes",
				"sticky-owner", "legacy-owner", "primary-read-error", "legacy-read-error", "no-hash",
				"excluded-failover", "previous-response", "guarded", "not-ready", "disabled",
			} {
				t.Run(scenario, func(t *testing.T) {
					resetOpenAIAdvancedSchedulerSettingCacheForTest()
					t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
					group := int64(72)
					c, b, a := streamHealthFixture(group)
					cfg := &config.Config{}
					cfg.Gateway.Scheduling.LoadBatchEnabled = mode.batch
					cfg.Gateway.OpenAIWS.LBTopK = 1
					cfg.Gateway.OpenAIWS.StickySessionTTLSeconds = 600
					cfg.Gateway.OpenAIWS.SessionHashReadOldFallback = true
					settings := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{openAIAdvancedSchedulerSettingKey: mode.advanced}}
					cache := &streamHealthBindingCache{schedulerTestGatewayCache: schedulerTestGatewayCache{sessionBindings: map[string]int64{}}}
					concurrency := &streamHealthConcurrency{}
					repo := streamHealthAccountRepo{schedulerTestOpenAIAccountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{a, b, c}}}
					now := time.Unix(1_800_000_000, 0)
					telemetry := &adaptiveTelemetry{}
					telemetry.streamHealth.ready.Store(scenario != "not-ready")
					telemetry.streamHealth.clock = func() time.Time { return now }
					parent, cancel := context.WithCancel(context.Background())
					defer cancel()
					parent = withOpenAILegacySessionHash(parent, "old-hash")
					ctx := streamHealthContext(parent, telemetry, group, streamHealthBody)
					cKey, bKey, aKey := streamHealthKey(t, ctx, &c), streamHealthKey(t, ctx, &b), streamHealthKey(t, ctx, &a)
					require.Equal(t, streamHealthEconomyModel, cKey.sample.model)
					require.Equal(t, streamHealthPublicModel, bKey.sample.model)
					seed := func(key adaptiveStreamKey, values ...time.Duration) {
						for i, latency := range values {
							telemetry.streamHealth.observe(key, now.Add(-time.Minute+time.Duration(i)*time.Second), latency)
						}
					}
					slow := []time.Duration{39800 * time.Millisecond, 48400 * time.Millisecond, 32200 * time.Millisecond}
					switch scenario {
					case "single-spike":
						slow = slow[:1]
					case "two-recovery-votes":
						slow = append(slow, time.Second, 19*time.Second)
					case "three-recovery-votes":
						slow = append(slow, time.Second, 19*time.Second, 18*time.Second)
					}
					seed(cKey, slow...)
					seed(aKey, time.Second, time.Second, time.Second)
					switch scenario {
					case "missing-samples", "cooldown", "only-higher-rate-is-healthy", "probe-no-slot", "probe-recheck-failure":
					default:
						seed(bKey, 12*time.Second, 13*time.Second, 14*time.Second)
					}

					want := c.ID
					hash, previous := "new-hash", ""
					var exclusions map[int64]struct{}
					switch scenario {
					case "healthy-alternative", "missing-samples", "two-recovery-votes":
						want = b.ID
					case "no-slot", "probe-no-slot":
						concurrency.deny = b.ID
					case "failed-recheck", "probe-recheck-failure":
						repo.stale = b.ID
					case "cooldown":
						telemetry.recovery.streamCooldown = map[adaptiveStreamKey]time.Time{bKey: now.Add(time.Minute)}
					case "only-higher-rate-is-healthy":
						ctx = withoutAdaptiveExploration(ctx)
					case "sticky-owner":
						cache.sessionBindings["openai:"+hash] = c.ID
						ctx = protectAdaptiveSelection(ctx)
					case "legacy-owner":
						cache.sessionBindings["openai:old-hash"] = c.ID
					case "primary-read-error":
						cache.failKey = "openai:" + hash
					case "legacy-read-error":
						cache.failKey = "openai:old-hash"
					case "profit-terminal-veto":
						ctx = context.WithValue(ctx, openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{groupID: group, platform: PlatformDeepseek, threshold: 0.1})
						want = b.ID
					case "no-hash":
						hash, want = "", b.ID
					case "excluded-failover":
						exclusions = map[int64]struct{}{999: {}}
					case "previous-response":
						previous = "resp_owner"
					case "guarded":
						ctx = context.WithValue(ctx, openAIGuardianParentAffinityContextKey{}, openAIGuardianParentAffinity{currentSessionHash: "parent"})
					case "disabled":
						ctx = parent
					}

					svc := &OpenAIGatewayService{accountRepo: repo, cache: cache, cfg: cfg,
						rateLimitService:   &RateLimitService{settingService: NewSettingService(settings, cfg)},
						concurrencyService: NewConcurrencyService(concurrency)}
					if scenario == "failed-recheck" || scenario == "probe-recheck-failure" || scenario == "profit-terminal-veto" {
						snapshot := &openAISnapshotCacheStub{snapshotAccounts: []*Account{&a, &b, &c},
							accountsByID: map[int64]*Account{a.ID: &a, b.ID: &b, c.ID: &c}}
						svc.schedulerSnapshot = NewSchedulerSnapshotService(snapshot, nil, repo, nil, cfg)
					}
					result, decision, err := svc.SelectAccountWithSchedulerForCapability(ctx, &group, previous, hash, streamHealthPublicModel,
						exclusions, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, PlatformDeepseek)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.True(t, result.Acquired)
					defer result.ReleaseFunc()
					if mode.advanced == "true" && scenario == "healthy-alternative" {
						require.Equal(t, 1, decision.TopK)
					}
					require.Equal(t, want, result.Account.ID, "binding miss=%d, binding unknown=%d, no alternative=%d, demoted=%d",
						telemetry.streamHealth.counters[streamBindingMiss].Load(), telemetry.streamHealth.counters[streamBindingUnknown].Load(),
						telemetry.streamHealth.counters[streamNoAlternate].Load(), telemetry.streamHealth.counters[streamDemoted].Load())
					if want == b.ID {
						require.Equal(t, []int64{b.ID}, concurrency.attempts)
					}
					if scenario == "no-slot" || scenario == "probe-no-slot" {
						require.Equal(t, []int64{b.ID, c.ID}, concurrency.attempts)
						require.Equal(t, c.ID, cache.sessionBindings["openai:"+hash])
					}
					if mode.advanced == "false" {
						switch scenario {
						case "single-spike", "healthy-alternative", "no-slot":
							require.Equal(t, want, cache.sessionBindings["openai:"+hash])
							require.Equal(t, time.Hour, cache.ttls["openai:"+hash])
						}
					}
					if (scenario == "failed-recheck" || scenario == "probe-recheck-failure") && mode.batch {
						require.Equal(t, []int64{b.ID, c.ID}, concurrency.attempts)
					}
					if scenario == "missing-samples" {
						lease := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress).recovery.Load()
						require.NotNil(t, lease)
						require.Equal(t, bKey, lease.streamKey)
						require.True(t, lease.acquired.Load())
					}
					if scenario == "profit-terminal-veto" {
						require.Empty(t, cache.sessionBindings)
						require.Empty(t, cache.ttls)
						newRate := 0.5
						b.RateMultiplier = &newRate
						recheckCtx := ContextWithSelectionProfitGate(ctx, result)
						_, vetoed, _ := svc.ProfitControlVetoLatest(recheckCtx, result.Account)
						require.True(t, vetoed)
						fallback, _, err := svc.SelectAccountWithSchedulerForCapability(ctx, &group, "", hash, streamHealthPublicModel,
							map[int64]struct{}{b.ID: {}}, OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions, false, false, true, PlatformDeepseek)
						require.NoError(t, err)
						require.True(t, fallback.Acquired)
						defer fallback.ReleaseFunc()
						require.Equal(t, c.ID, fallback.Account.ID)
						require.NoError(t, svc.BindStickySessionAfterProfitAdmission(ctx, &group, hash, fallback.Account.ID))
						require.Equal(t, c.ID, cache.sessionBindings["openai:"+hash])
					}
					if scenario == "legacy-read-error" {
						wantReads := 2
						if mode.name != "legacy-batch" {
							wantReads = 4
						}
						require.Len(t, cache.reads, wantReads)
					}
				})
			}
		})
	}
}

func TestGatewayAdaptiveStreamHealthEvidenceProbe(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	telemetry := &adaptiveTelemetry{}
	telemetry.streamHealth.ready.Store(true)
	c, b, a := streamHealthFixture(72)
	pool := []*Account{&c, &b, &a}
	sampleContext := streamHealthContext(context.Background(), telemetry, 72, streamHealthBody)
	cKey, bKey := streamHealthKey(t, sampleContext, &c), streamHealthKey(t, sampleContext, &b)
	for i := range 3 {
		telemetry.streamHealth.observe(cKey, now.Add(time.Duration(i-3)*time.Second), 39*time.Second)
	}
	peer := func(*Account, *Account) bool { return true }
	choose := func(ctx context.Context, at time.Time) (adaptiveStreamChoice, func(adaptiveSampleKey, adaptiveStreamKey) bool) {
		ctx = withAdaptiveStreamPermission(ctx, adaptiveBindingMiss, true)
		ingress := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
		route, _ := gatewayAdaptiveRoute(ctx)
		reserve := telemetry.recovery.opportunityWithStream(ctx, ingress, adaptiveProbeScope{groupID: 72, model: streamHealthPublicModel, route: route}, at)
		return chooseAdaptiveStreamAccount(ctx, pool, adaptiveAccount, peer, nil, streamHealthPublicModel, at, func(key adaptiveStreamKey) bool { return reserve(key.sample, key) }), reserve
	}
	for i := range 3 {
		ctx, cancel := context.WithCancel(streamHealthContext(context.Background(), telemetry, 72, streamHealthBody))
		at := now.Add(time.Duration(i) * time.Minute)
		choice, reserve := choose(ctx, at)
		require.Equal(t, bKey, choice.key)
		require.True(t, choice.evidence)
		require.False(t, reserve(bKey.sample, bKey))
		slot, err := adaptiveTrackSlot(ctx, b.ID, &AcquireResult{Acquired: true}, nil)
		require.NoError(t, err)
		observeAdaptiveAttempt(ctx, bKey.sample.route, &b, bKey.sample.model, at, 12*time.Second, true)
		require.Zero(t, telemetry.score(bKey.sample, at).health.Passed)
		slot.ReleaseFunc()
		telemetry.streamHealth.observe(bKey, at, 12*time.Second)
		cancel()
		blocked, cancelBlocked := context.WithCancel(streamHealthContext(context.Background(), telemetry, 72, streamHealthBody))
		choice, _ = choose(blocked, at.Add(time.Minute-time.Second))
		cancelBlocked()
		if i < 2 {
			require.Zero(t, choice.accountID)
		} else {
			require.Equal(t, b.ID, choice.accountID)
			require.False(t, choice.evidence)
		}
	}
}

func TestGatewayAdaptiveStreamHealthRecovery(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, tc := range []struct {
		name         string
		durations    []time.Duration
		slow, normal bool
	}{
		{"single-peak", []time.Duration{39, 7, 12}, false, true},
		{"two-severe", []time.Duration{39, 48}, true, false},
		{"nonconsecutive-severe", []time.Duration{39, 30, 39}, false, false},
		{"five-slow", []time.Duration{20, 9, 20, 9, 20, 9, 20, 9, 20}, true, false},
		{"two-recoveries", []time.Duration{39, 48, 7, 19}, true, false},
		{"three-recoveries", []time.Duration{39, 48, 7, 19, 10}, false, true},
		{"20s-breaks-recovery", []time.Duration{39, 48, 7, 19, 20, 10}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := adaptiveStreamHealth{}
			key := adaptiveStreamKey{wire: APIProtocolChatCompletions, work: adaptiveWorkText}
			for i, latency := range tc.durations {
				health.observe(key, now.Add(time.Duration(i-12)*time.Second), latency*time.Second)
			}
			actual := adaptiveStreamStatus(health.series[key], now)
			require.Equal(t, tc.slow, actual.slow)
			require.Equal(t, tc.normal, actual.normal)
			require.Equal(t, adaptiveStreamState{}, adaptiveStreamStatus(health.series[key], now.Add(adaptiveStreamWindow)))
		})
	}
}

func TestGatewayAdaptiveStreamHealthWorkloadIsolation(t *testing.T) {
	c, _, _ := streamHealthFixture(72)
	telemetry := &adaptiveTelemetry{}
	ctx := streamHealthContext(context.Background(), telemetry, 72, streamHealthBody)
	baseline := streamHealthKey(t, ctx, &c)
	for _, scenario := range []struct {
		name, field string
	}{
		{"declared-tools", `"tools":[{"type":"function","function":{"name":"search"}}],`},
		{"state", `"prompt_cache_key":"opaque",`},
		{"thinking", `"reasoning_effort":"high",`},
		{"tier", `"service_tier":"priority",`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			other := streamHealthContext(context.Background(), telemetry, 72, "{"+scenario.field+streamHealthBody[1:])
			require.NotEqual(t, baseline, streamHealthKey(t, other, &c))
		})
	}
	longBody := strings.Replace(streamHealthBody, "hello", strings.Repeat("x", 1024), 1)
	long := streamHealthContext(context.Background(), telemetry, 72, longBody)
	require.NotEqual(t, baseline.sample.route.inputSize, streamHealthKey(t, long, &c).sample.route.inputSize)
	toolHistory := strings.Replace(streamHealthBody, "\"role\":\"user\"", "\"role\":\"tool\",\"tool_call_id\":\"opaque\"", 1)
	toolCtx := streamHealthContext(context.Background(), telemetry, 72, toolHistory)
	toolKey := streamHealthKey(t, toolCtx, &c)
	require.Equal(t, adaptiveWorkText|adaptiveWorkToolHistory, toolKey.work)
	combined := streamHealthContext(context.Background(), telemetry, 72, "{\"prompt_cache_key\":\"opaque\","+toolHistory[1:])
	require.Equal(t, toolKey.work|adaptiveWorkState, streamHealthKey(t, combined, &c).work)
	c.Credentials["api_protocol"] = APIProtocolResponses
	rawRoute, _ := gatewayAdaptiveRoute(ctx)
	_, allowed := adaptiveStreamCandidateKey(ctx, rawRoute, &c, streamHealthPublicModel)
	require.False(t, allowed)
	c.Credentials["api_protocol"] = APIProtocolChatCompletions
	require.NotEqual(t, baseline, streamHealthKey(t, streamHealthContext(context.Background(), telemetry, 73, streamHealthBody), &c))
}

func TestGatewayAdaptiveStreamHealthEvidenceProbeSharedBudget(t *testing.T) {
	t.Run("eight-global-and-one-target-key", func(t *testing.T) {
		var recovery adaptiveRecovery
		now := time.Unix(1_800_000_000, 0)
		var leases []*adaptiveRecoveryLease
		defer func() {
			for _, lease := range leases {
				lease.finish()
			}
		}()
		base := adaptiveSampleKey{accountID: 1}
		first := &adaptiveIngress{}
		require.True(t, recovery.opportunity(context.Background(), first, adaptiveProbeScope{groupID: 1}, now)(base))
		leases = append(leases, first.recovery.Load())
		reserve := func(id int64, at time.Time) bool {
			ingress := &adaptiveIngress{}
			healthKey := adaptiveSampleKey{accountID: id}
			streamKey := adaptiveStreamKey{sample: healthKey, wire: APIProtocolChatCompletions, work: adaptiveWorkText}
			allowed := recovery.opportunityWithStream(context.Background(), ingress, adaptiveProbeScope{groupID: id + 1}, at)(healthKey, streamKey)
			if allowed {
				leases = append(leases, ingress.recovery.Load())
			}
			return allowed
		}
		require.False(t, reserve(1, now.Add(time.Second)))
		for i := int64(2); i <= 8; i++ {
			require.True(t, reserve(i, now.Add(time.Duration(i)*time.Second)))
		}
		require.False(t, reserve(9, now.Add(9*time.Second)))
		leases[0].finish()
		require.True(t, reserve(9, now.Add(10*time.Second)))
	})
	t.Run("busy-pool-shares-arrivals", func(t *testing.T) {
		var recovery adaptiveRecovery
		scope := adaptiveProbeScope{groupID: 1}
		now := time.Unix(1_800_000_000, 0)
		target := adaptiveSampleKey{accountID: 5}
		for range 99 {
			recovery.opportunity(context.Background(), &adaptiveIngress{}, scope, now)
		}
		request := &adaptiveIngress{}
		require.True(t, recovery.opportunity(context.Background(), request, scope, now)(target))
		slot, err := adaptiveTrackSlot(context.WithValue(context.Background(), adaptiveIngressKey{}, request), target.accountID, &AcquireResult{Acquired: true}, nil)
		require.NoError(t, err)
		slot.ReleaseFunc()
		target.accountID++
		key := adaptiveStreamKey{sample: target, wire: APIProtocolChatCompletions, work: adaptiveWorkText}
		for range 99 {
			reserve := recovery.opportunityWithStream(context.Background(), &adaptiveIngress{}, scope, now.Add(time.Second))
			require.False(t, reserve(target, key))
		}
		next := &adaptiveIngress{}
		reserve := recovery.opportunityWithStream(context.Background(), next, scope, now.Add(time.Second))
		require.True(t, reserve(target, key))
		next.recovery.Load().finish()
	})
}
