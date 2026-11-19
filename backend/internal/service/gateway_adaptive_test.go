package service

import (
	"bytes"
	"context"
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
		rankAdaptiveWithTelemetry(telemetry, route, pool, adaptiveAccount, model, peer, false, at, func(int) int { return 0 }, nil)
		require.Equal(t, want, pool[0].ID)
	}
	check(route, now, adaptiveAnyPeer, 2)
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
