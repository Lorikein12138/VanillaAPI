package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
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
				"compat-healthy", "compat-probe", "compat-plain", "compat-other-tool-stream", "compat-no-instructions",
				"compat-cache", "compat-opaque", "compat-tool-history", "compat-owner", "compat-unknown-binding",
				"compat-duplicate-content", "compat-duplicate-messages", "compat-duplicate-tool-stream", "compat-duplicate-tool-stream-probe",
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
					body := streamHealthBody
					if strings.HasPrefix(scenario, "compat-") {
						body = "{\"instructions\":\"concise\",\"tool_stream\":false," + body[1:]
					}
					if strings.HasPrefix(scenario, "compat-duplicate-tool-stream") {
						body = strings.Replace(body, "\"tool_stream\":false", "\"tool_stream\":true", 1)
						body = "{\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"search\"}}]," + body[1:]
					}
					ctx := streamHealthContext(parent, telemetry, group, body)
					cKey, bKey, aKey := streamHealthKey(t, ctx, &c), streamHealthKey(t, ctx, &b), streamHealthKey(t, ctx, &a)
					switch scenario {
					case "compat-plain":
						ctx = streamHealthContext(parent, telemetry, group, streamHealthBody)
					case "compat-other-tool-stream":
						ctx = streamHealthContext(parent, telemetry, group, strings.Replace(body, "\"tool_stream\":false,", "", 1))
					case "compat-no-instructions":
						ctx = streamHealthContext(parent, telemetry, group, strings.Replace(body, "\"instructions\":\"concise\",", "", 1))
					case "compat-duplicate-content":
						ctx = streamHealthContext(parent, telemetry, group, strings.Replace(body,
							`"content":"hello"`, `"content":"hello","content":[{"type":"image_url"}]`, 1))
					case "compat-duplicate-messages":
						ctx = streamHealthContext(parent, telemetry, group,
							body[:len(body)-1]+`,"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`)
					case "compat-duplicate-tool-stream", "compat-duplicate-tool-stream-probe":
						ctx = streamHealthContext(parent, telemetry, group, body[:len(body)-1]+`,"tool_stream":false}`)
					case "compat-cache":
						body = "{\"prompt_cache_options\":{\"ttl\":\"5m\"}," + body[1:]
					case "compat-opaque":
						body = strings.Replace(body, "[{\"role\":\"user\"", "[{\"role\":\"assistant\",\"content\":\"earlier\",\"reasoning_opaque\":\"\"},{\"role\":\"user\"", 1)
					case "compat-tool-history":
						body = strings.Replace(body, "\"role\":\"user\"", "\"role\":\"tool\",\"tool_call_id\":\"fixture\"", 1)
					}
					switch scenario {
					case "compat-cache", "compat-opaque", "compat-tool-history":
						ctx = streamHealthContext(parent, telemetry, group, body)
						cKey, bKey = streamHealthKey(t, ctx, &c), streamHealthKey(t, ctx, &b)
					}
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
					case "missing-samples", "compat-probe", "compat-duplicate-tool-stream-probe", "cooldown", "only-higher-rate-is-healthy", "probe-no-slot", "probe-recheck-failure":
					default:
						seed(bKey, 12*time.Second, 13*time.Second, 14*time.Second)
					}

					want := c.ID
					hash, previous := "new-hash", ""
					var exclusions map[int64]struct{}
					switch scenario {
					case "healthy-alternative", "compat-healthy", "missing-samples", "compat-probe", "two-recovery-votes":
						want = b.ID
					case "no-slot", "probe-no-slot":
						concurrency.deny = b.ID
					case "failed-recheck", "probe-recheck-failure":
						repo.stale = b.ID
					case "cooldown":
						telemetry.recovery.streamCooldown = map[adaptiveStreamKey]time.Time{bKey: now.Add(time.Minute)}
					case "only-higher-rate-is-healthy":
						ctx = withoutAdaptiveExploration(ctx)
					case "sticky-owner", "compat-owner":
						cache.sessionBindings["openai:"+hash] = c.ID
						ctx = protectAdaptiveSelection(ctx)
					case "legacy-owner":
						cache.sessionBindings["openai:old-hash"] = c.ID
					case "primary-read-error", "compat-unknown-binding":
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
					} else if strings.HasPrefix(scenario, "compat-") {
						require.Zero(t, telemetry.streamHealth.counters[streamDemoted].Load())
						require.Zero(t, telemetry.streamHealth.counters[streamProbeReserved].Load())
						if strings.HasPrefix(scenario, "compat-duplicate-") {
							route, _ := gatewayAdaptiveRoute(ctx)
							_, ok := adaptiveStreamCandidateKey(ctx, route, &c, streamHealthPublicModel)
							require.False(t, ok)
							ingress, ok := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
							require.True(t, ok)
							require.Nil(t, ingress.recovery.Load())
						}
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
					if scenario == "missing-samples" || scenario == "compat-probe" {
						ingress, ok := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
						require.True(t, ok)
						lease := ingress.recovery.Load()
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
		ingress, ok := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
		require.True(t, ok)
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

func TestGatewayAdaptiveStreamHealthWorkloadShapes(t *testing.T) {
	const user = `{"role":"user","content":"hello"}`
	const tools = `,"tools":[{"type":"function","function":{"name":"search","parameters":{"properties":{"variable":{"type":"string"}}}}}]`
	const text = adaptiveWorkText
	const declared = adaptiveWorkDeclaredTools
	const stream = adaptiveWorkToolStream
	const cache = adaptiveWorkCache | adaptiveWorkState
	for _, tc := range []struct {
		name, message, root string
		work                adaptiveWorkload
		nonText             bool
	}{
		{"text", user, "", text, false},
		{"tools", user, tools, text | declared, false},
		{"legacy-function", user, `,"functions":[{"name":"search"}]`, text | declared, false},
		{"tool-history", `{"role":"tool","content":"result","tool_call_id":"fixture"}`, "", text | adaptiveWorkToolHistory, false},
		{"instructions", user, `,"instructions":""`, text | adaptiveWorkInstructions, false},
		{"instructions-type", user, `,"instructions":{}`, 0, false},
		{"instructions-null", user, `,"instructions":null`, 0, false},
		{"native-messages-required", "", `,"instructions":"guide"`, 0, false},
		{"input-excluded", user, `,"instructions":"guide","input":null`, 0, false},
		{"stream-false", user, `,"tool_stream":false`, text | stream, false},
		{"tools-stream-false", user, tools + `,"tool_stream":false`, text | declared | stream, false},
		{"tools-stream-true", user, tools + `,"tool_stream":true`, text | declared | stream | adaptiveWorkToolStreamTrue, false},
		{"stream-unquoted", user, `,"tool_stream":1`, 0, false},
		{"stream-string", user, `,"tool_stream":"false"`, 0, false},
		{"stream-null", user, `,"tool_stream":null`, 0, false},
		{"stream-no-tools", user, `,"tool_stream":true`, 0, false},
		{"stream-empty-tools", user, `,"tools":[],"tool_stream":true`, 0, false},
		{"stream-legacy-functions", user, `,"functions":[{"name":"search"}],"tool_stream":true`, 0, false},
		{"tool-wrapper", user, `,"instructions":"guide","tools":[{"type":"function","function":{"name":"search"},"wrapper":{}}]`, 0, false},
		{"tool-definition", user, `,"tools":[{"type":"function","function":{}}],"tool_stream":true`, 0, false},
		{"tool-custom", user, `,"tools":[{"type":"custom","custom":{"name":"search"}}],"tool_stream":true`, 0, true},
		{"tool-hosted", user, `,"tools":[{"type":"web_search"}],"instructions":"guide"`, 0, true},
		{"reasoning-text", `{"role":"assistant","content":"text","reasoning_text":"plan"}`, "", text | adaptiveWorkReasoningText, false},
		{"reasoning-role", `{"role":"user","content":"text","reasoning_text":"plan"}`, "", 0, false},
		{"reasoning-null", `{"role":"assistant","content":"text","reasoning_text":null}`, "", 0, false},
		{"reasoning-type", `{"role":"assistant","content":"text","reasoning_text":true}`, "", 0, false},
		{"reasoning-tool-history", `{"role":"assistant","content":null,"reasoning_text":"plan","tool_calls":[{"type":"function","function":{"name":"search","arguments":"[]"}}]}`, tools, text | declared | adaptiveWorkReasoningText | adaptiveWorkToolHistory, false},
		{"opaque", `{"role":"assistant","content":"text","reasoning_opaque":""}`, "", text | adaptiveWorkReasoningOpaque | adaptiveWorkState, false},
		{"opaque-role", `{"role":"tool","content":"text","reasoning_opaque":"cipher"}`, "", 0, false},
		{"opaque-null", `{"role":"assistant","content":"text","reasoning_opaque":null}`, "", 0, false},
		{"opaque-type", `{"role":"assistant","content":"text","reasoning_opaque":[]}`, "", 0, false},
		{"cache-empty-options", user, `,"prompt_cache_options":{}`, text | cache, false},
		{"cache-options", user, `,"prompt_cache_options":{"ttl":"5m","mode":"explicit"}`, text | cache, false},
		{"cache-options-ttl", user, `,"prompt_cache_options":{"ttl":null}`, 0, false},
		{"cache-options-mode", user, `,"prompt_cache_options":{"mode":"other"}`, 0, false},
		{"cache-options-duplicate", user, `,"prompt_cache_options":{"mode":"explicit","mode":false}`, 0, false},
		{"cache-options-type", user, `,"prompt_cache_options":[]`, 0, false},
		{"cache-options-null", user, `,"prompt_cache_options":null`, 0, false},
		{"cache-options-unknown", user, `,"prompt_cache_options":{"fixture":true}`, 0, false},
		{"text-array", `{"role":"user","content":[{"type":"text","text":"hello"}]}`, `,"instructions":"guide"`, text | adaptiveWorkInstructions, false},
		{"text-breakpoint", `{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"}}]}`, "", text | cache, false},
		{"breakpoint-null", `{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":null}]}`, "", 0, false},
		{"breakpoint-unknown", `{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit","ttl":"5m"}}]}`, "", 0, false},
		{"breakpoint-nontext", `{"role":"user","content":[{"type":"image_url","prompt_cache_breakpoint":{"mode":"explicit"}}]}`, "", 0, true},
		{"fake-text", `{"role":"user","content":[{"type":"text","text":"hello","image_url":{}}]}`, `,"instructions":"guide"`, 0, true},
		{"wrapped-text", `{"role":"user","content":[{"type":"text","text":{"wrapper":"hello"}}]}`, `,"instructions":"guide"`, 0, false},
		{"file", `{"role":"user","content":[{"type":"file","file":{}}]}`, "", 0, true},
		{"video", `{"role":"user","content":[{"type":"video_url","video_url":{}}]}`, "", 0, true},
		{"media-tool-result", `{"role":"tool","content":[{"type":"tool_result","content":[{"type":"text","text":"ok","input_audio":{}}]}]}`, "", 0, true},
		{"unknown-root", user, `,"fixture":true`, 0, false},
		{"unknown-message", `{"role":"assistant","content":"text","fixture":true}`, "", 0, false},
		{"unknown-before-media", `{"role":"user","fixture":true,"content":[{"type":"unfamiliar"},{"type":"input_audio"}]}`, `,"fixture":true`, 0, true},
		{"duplicate-messages-text", user, `,"messages":[{"role":"user","content":"second"}]`, 0, false},
		{"duplicate-messages-media", user, `,"messages":[{"role":"user","content":[{"type":"input_audio"}]}]`, 0, true},
		{"duplicate-content-text", `{"role":"user","content":"a","content":"b"}`, `,"instructions":"guide"`, 0, false},
		{"duplicate-content-media", `{"role":"user","content":"a","content":[{"type":"image_url"}]}`, "", 0, true},
		{"duplicate-escaped-content-media", `{"role":"user","content":[{"type":"image_url"}],"co\u006etent":"a"}`, "", 0, true},
		{"duplicate-tool-stream", user, tools + `,"tool_stream":true,"tool_stream":false`, 0, false},
		{"duplicate-tool-stream-identical", user, `,"tool_stream":false,"tool_stream":false`, 0, false},
		{"duplicate-role", `{"role":"assistant","role":"user","reasoning_text":"plan","content":"answer"}`, "", 0, false},
		{"duplicate-reasoning-text", `{"role":"assistant","reasoning_text":"a","reasoning_text":"b","content":"answer"}`, "", 0, false},
		{"duplicate-part-type", `{"role":"user","content":[{"type":"text","type":"text","text":"hello"}]}`, "", 0, false},
		{"duplicate-part-type-media", `{"role":"user","content":[{"type":"text","type":"input_audio","text":"hello"}]}`, "", 0, true},
		{"text-tool-result-compatible", `{"role":"tool","content":[{"type":"tool_result","content":"result"}]}`, `,"instructions":"guide"`, text | adaptiveWorkInstructions | adaptiveWorkToolHistory, false},
		{"missing-tool-result-content-compatible", `{"role":"tool","content":[{"type":"tool_result"}]}`, `,"instructions":"guide"`, 0, false},

		{"duplicate-tool-result-content-media", `{"role":"tool","content":[{"type":"tool_result","content":"a","content":[{"type":"image_url"}]}]}`, "", 0, true},
		{"duplicate-part-type-tool-result", `{"role":"user","content":[{"type":"text","type":"tool_result","content":[{"type":"image_url"}],"text":"hello"}]}`, "", 0, true},
		{"duplicate-tools-media", user, tools + `,"tools":[{"type":"custom"}]`, 0, true},
		{"duplicate-tool-type", user, `,"tools":[{"type":"function","type":"function","function":{"name":"search"}}]`, 0, false},
		{"duplicate-tool-type-media", user, `,"tools":[{"type":"function","type":"custom","function":{"name":"search"}}]`, 0, true},
		{"duplicate-tool-function", user, `,"tools":[{"type":"function","function":{"name":"search"},"function":{"name":"other"}}]`, 0, false},
		{"duplicate-tool-name", user, `,"tools":[{"type":"function","function":{"name":"search","name":"other"}}]`, 0, false},
		{"duplicate-tool-choice-type-media", user, `,"tool_choice":{"type":"function","type":"custom","function":{"name":"search"}}`, 0, true},
		{"duplicate-tool-history-type", `{"role":"assistant","tool_calls":[{"type":"function","type":"custom"}]}`, "", 0, true},
		{"duplicate-modality-media", user, `,"modalities":["text"],"modalities":["video"]`, 0, true},
		{"duplicate-cache-ttl", user, `,"prompt_cache_options":{"ttl":"5m","ttl":"1h"}`, 0, false},
		{"duplicate-breakpoint-mode", `{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit","mode":"explicit"}}]}`, "", 0, false},
		{"duplicate-breakpoint", `{"role":"user","content":[{"type":"text","text":"hello","prompt_cache_breakpoint":{"mode":"explicit"},"prompt_cache_breakpoint":null}]}`, "", 0, false},
		{"duplicate-effort", user, `,"reasoning":{"effort":"low","effort":"high"}`, 0, false},
		{"duplicate-usage-option", user, `,"stream_options":{"include_usage":true,"include_usage":false}`, 0, false},
		{"combined", `{"role":"assistant","content":"text","reasoning_text":"plan","reasoning_opaque":"state"}`,
			tools + `,"instructions":"guide","tool_stream":true,"prompt_cache_options":{"ttl":"1h"}`,
			text | declared | stream | adaptiveWorkToolStreamTrue | adaptiveWorkInstructions | adaptiveWorkReasoningText | adaptiveWorkReasoningOpaque | cache, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"messages":[` + tc.message + "]" + tc.root + "}"
			shape := adaptiveChatWorkload(gjson.Parse(body), false)
			require.Equal(t, adaptiveChatShape{work: tc.work, nonText: tc.nonText}, shape)
		})
	}
}

func TestGatewayAdaptiveStreamHealthUnsupportedSummary(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var health adaptiveStreamHealth
	health.count(streamUnsupportedNonText)
	health.count(streamUnsupportedIngress)
	health.count(streamUnsupportedIngress)
	health.count(streamUnsupportedOutbound)
	health.count(streamCanceled)
	health.summarize()
	health.summarize()

	var summary map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &summary))
	delete(summary, "time")
	delete(summary, "level")
	require.Equal(t, map[string]any{
		"msg":                           "gateway.adaptive_stream_health",
		"unsupported_workload":          float64(4),
		"unsupported_workload_non_text": float64(1),
		"unsupported_workload_ingress":  float64(2),
		"unsupported_workload_outbound": float64(1),
		"cancel_or_disconnect":          float64(1),
	}, summary)
}
