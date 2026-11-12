//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	advisorMainModel   = "claude-sonnet-4"
	advisorClientModel = "claude-advisor-alias"
	advisorWireModel   = "claude-opus-4.8"
	advisorToolBlock   = `{"type":"server_tool_use", "id":"srvtoolu_test","name":"advisor","input":{"opaque":"cc_sess_keep"}}`
	advisorResultBlock = `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_test", "content":{"type":"advisor_result","opaque":"cc_ses_keep","large":9007199254740993}}`
	advisorRedacted    = `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_test","content":{"type":"advisor_redacted_result","data":"cc_sess_keep"}}`
	advisorError       = `{"type":"advisor_tool_result","tool_use_id":"srvtoolu_test","content":{"type":"advisor_tool_result_error","error_code":"max_uses_exceeded"}}`
	advisorFirst       = `{"type":"advisor_message", "model":"claude-opus-4.8","input_tokens":10,"output_tokens":3,"cache_read_input_tokens":4,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":3}}`
	advisorSecond      = `{"type":"advisor_message","model":"claude-opus-4.8","input_tokens":2,"output_tokens":1,"cache_read_input_tokens":2}`
	advisorOuterUsage  = `"input_tokens":100,"output_tokens":20`
)

func advisorTool(model string) string {
	return fmt.Sprintf(`{"type":"advisor_20260301","name":"advisor","model":%q,"max_uses":2,"max_tokens":2048,"caching":{"type":"ephemeral"},"cache_control":{"type":"ephemeral","ttl":"5m"}}`, model)
}

func forwardAdvisorFixture(t *testing.T, svc *GatewayService, account *Account, ctx context.Context, requestModel, tool, response string, stream, disconnect bool) (*ForwardResult, *anthropicHTTPUpstreamRecorder, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	c.Request.Header.Set("Anthropic-Beta", "context-1m-2025-08-07")
	if tool != "" {
		// Real requests may supply the beta in a separate header value.
		c.Request.Header.Add("Anthropic-Beta", anthropicAdvisorBeta)
	}
	if disconnect {
		c.Writer = &failWriteResponseWriter{ResponseWriter: c.Writer}
	}
	body := fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":128,"tools":[%s],"messages":[{"role":"user","content":"hello"}]}`, requestModel, stream, tool)
	parsed := &ParsedRequest{Body: NewRequestBodyRef([]byte(body)), Model: requestModel, Stream: stream}
	headers := http.Header{"X-Request-Id": {"advisor-upstream"}}
	if stream {
		headers.Set("Content-Type", "text/event-stream")
	}
	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(response)),
	}}
	svc.httpUpstream = upstream
	result, err := svc.Forward(ctx, c, account, parsed)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, upstream, rec.Body.String()
}

func TestAnthropicAdvisorForwarding(t *testing.T) {
	tests := []struct {
		name        string
		accountType string
		realClaude  bool
		passthrough bool
	}{
		{name: "api-key", accountType: AccountTypeAPIKey},
		{name: "oauth-real", accountType: AccountTypeOAuth, realClaude: true},
		{name: "oauth-mimic", accountType: AccountTypeOAuth},
		{name: "setup-token-mimic", accountType: AccountTypeSetupToken},
		{name: "api-key-passthrough", accountType: AccountTypeAPIKey, passthrough: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newMinimalGatewayService()
			account := newAnthropicAPIKeyAccountForTest()
			account.Type = tt.accountType
			account.Credentials["access_token"] = "oauth-fixture"
			account.Credentials["model_mapping"] = map[string]any{
				"claude-main-alias": advisorMainModel, advisorClientModel: "do-not-use",
			}
			account.Credentials[credKeyHeaderOverrideEnabled] = true
			account.Credentials[credKeyHeaderOverrides] = map[string]any{"anthropic-beta": "context-1m-2025-08-07"}
			account.Extra["anthropic_passthrough"] = tt.passthrough
			ctx := SetClaudeCodeClient(context.Background(), tt.realClaude)
			model := "claude-main-alias"
			if tt.accountType != AccountTypeAPIKey {
				model = advisorMainModel
			}
			tool := advisorTool(advisorClientModel)
			blocks := strings.Join([]string{advisorToolBlock, advisorResultBlock, advisorRedacted, advisorError}, ",")
			response := fmt.Sprintf(`{"model":%q,"content":[%s],"usage":{%s,"iterations":[%s]}}`, advisorMainModel, blocks, advisorOuterUsage, advisorFirst)
			result, upstream, output := forwardAdvisorFixture(t, svc, account, ctx, model, tool, response, false, false)
			require.True(t, hasAnthropicAdvisorBeta(upstream.lastReq.Header))
			require.Equal(t, tool, gjson.GetBytes(upstream.lastBody, "tools.0").Raw)
			require.Equal(t, advisorMainModel, gjson.GetBytes(upstream.lastBody, "model").String())
			if !tt.passthrough {
				require.Equal(t, model, gjson.Get(output, "model").String())
			}
			require.Contains(t, output, blocks)
			require.Equal(t, advisorClientModel, gjson.Get(output, "usage.iterations.0.model").String())
			require.Equal(t, advisorWireModel, result.advisor.iterations[0].model)
		})
	}
}

func TestAnthropicAdvisorNativeCompatibleForwarding(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newAnthropicAPIKeyAccountForTest()
	account.Credentials[credKeyHeaderOverrideEnabled] = true
	account.Credentials[credKeyHeaderOverrides] = map[string]any{"anthropic-beta": "context-1m-2025-08-07"}
	c := newVertexBetaTestContext(t, anthropicAdvisorBeta)
	tool := advisorTool(advisorClientModel)
	body := []byte(fmt.Sprintf(`{"model":%q,"tools":[%s]}`, advisorMainModel, tool))
	req, wireBody, err := svc.buildNativeAnthropicUpstreamRequest(context.Background(), c, account, body, "upstream-token", claudeAPIURL)
	require.NoError(t, err)
	require.True(t, hasAnthropicAdvisorBeta(req.Header))
	require.Equal(t, tool, gjson.GetBytes(wireBody, "tools.0").Raw)
}

func TestAnthropicAdvisorForwardingHonorsBetaBlock(t *testing.T) {
	svc := newMinimalGatewayService()
	settings, err := json.Marshal(BetaPolicySettings{Rules: []BetaPolicyRule{
		{BetaToken: anthropicAdvisorBeta, Action: BetaPolicyActionBlock, Scope: BetaPolicyScopeAll},
	}})
	require.NoError(t, err)
	svc.settingService = NewSettingService(&betaPolicySettingRepoStub{
		values: map[string]string{SettingKeyBetaPolicySettings: string(settings)},
	}, svc.cfg)
	c := newVertexBetaTestContext(t, "context-1m-2025-08-07")
	c.Request.Header.Add("Anthropic-Beta", anthropicAdvisorBeta)
	account := newAnthropicAPIKeyAccountForTest()
	account.Extra["anthropic_passthrough"] = false
	body := []byte(fmt.Sprintf(`{"model":%q,"tools":[%s]}`, advisorMainModel, advisorTool(advisorWireModel)))
	result, err := svc.Forward(context.Background(), c, account, &ParsedRequest{Body: NewRequestBodyRef(body), Model: advisorMainModel})
	var blocked *BetaBlockedError
	require.ErrorAs(t, err, &blocked)
	require.Nil(t, result)
}

func TestAnthropicAdvisorAntigravityUpstreamRelay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			svc, store, input := newAdvisorBillingFixture()
			input.Account.Platform = PlatformAntigravity
			input.Account.Type = AccountTypeUpstream
			tool := advisorTool(advisorClientModel)
			body := fmt.Sprintf(`{"model":%q,"stream":%t,"tools":[%s]}`, advisorMainModel, stream, tool)
			response := fmt.Sprintf(`{"model":%q,"content":[%s,%s],"usage":{%s,"iterations":[%s]}}`,
				advisorMainModel, advisorToolBlock, advisorResultBlock, advisorOuterUsage, advisorFirst)
			if stream {
				response = "data: " + `{"type":"content_block_start","index":0,"content_block":` + advisorToolBlock + "}\n\n" +
					"data: " + `{"type":"content_block_start","index":1,"content_block":` + advisorResultBlock + "}\n\n" +
					"data: " + `{"type":"message_delta","usage":{"iterations":[{"type":"advisor_message","model":"claude-opus-4.8","input_tokens":999}]}}` + "\n\n" +
					"data: " + fmt.Sprintf(`{"type":"message_delta","usage":{%s,"iterations":[%s]}}`, advisorOuterUsage, advisorFirst) + "\n\n" +
					"data: " + `{"type":"message_stop"}` + "\n\n"
			}
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(response)),
			}}
			relay := &AntigravityGatewayService{httpUpstream: upstream, settingService: &SettingService{cfg: svc.cfg}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set("Anthropic-Beta", "context-1m-2025-08-07")
			c.Request.Header.Add("Anthropic-Beta", anthropicAdvisorBeta)
			ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "advisor-relay")
			result, err := relay.Forward(ctx, c, input.Account, []byte(body), false)
			require.NoError(t, err)
			require.True(t, hasAnthropicAdvisorBeta(upstream.lastReq.Header))
			require.Equal(t, body, string(upstream.lastBody))
			require.Equal(t, strings.ReplaceAll(response, advisorWireModel, advisorClientModel), rec.Body.String())
			input.Result = result
			require.NoError(t, svc.RecordUsage(ctx, input))
			require.Len(t, store.logs, 2)
			require.Equal(t, "client:advisor-relay:advisor:1", store.logs[1].RequestID)
			require.Equal(t, advisorWireModel, store.logs[1].Model)
			require.InDelta(t, 200*2.5e-6, store.logs[1].ActualCost, 1e-12)
		})
	}
}

type advisorTestUsageStore struct {
	UsageLogRepository
	UsageBillingRepository
	logs     []*UsageLog
	charges  map[string]UsageBillingCommand
	balance  float64
	failOnce string
}

func (s *advisorTestUsageStore) Create(_ context.Context, log *UsageLog) (bool, error) {
	for _, prior := range s.logs {
		if prior.RequestID == log.RequestID {
			return false, nil
		}
	}
	s.logs = append(s.logs, log)
	return true, nil
}

func (s *advisorTestUsageStore) Apply(_ context.Context, cmd *UsageBillingCommand) (*UsageBillingApplyResult, error) {
	if cmd.RequestID == s.failOnce {
		s.failOnce = ""
		return nil, errors.New("transient settlement failure")
	}
	if previous, exists := s.charges[cmd.RequestID]; exists {
		if previous.RequestFingerprint != cmd.RequestFingerprint {
			return nil, errors.New("billing fingerprint changed on replay")
		}
		return &UsageBillingApplyResult{}, nil
	}
	s.charges[cmd.RequestID] = *cmd
	s.balance += cmd.BalanceCost
	return &UsageBillingApplyResult{Applied: true}, nil
}

func newAdvisorBillingFixture() (*GatewayService, *advisorTestUsageStore, *RecordUsageInput) {
	store := &advisorTestUsageStore{charges: make(map[string]UsageBillingCommand)}
	svc := newGatewayRecordUsageServiceWithBillingRepoForTest(store, store, &openAIRecordUsageUserRepoStub{}, nil)
	svc.rateLimitService = &RateLimitService{}
	svc.resolver = NewModelPricingResolver(nil, svc.billingService)
	userRate := 2.5
	svc.userGroupRateResolver = newUserGroupRateResolver(&openAIUserGroupRateRepoStub{rate: &userRate}, nil, 0, nil, "advisor-test")
	price := func(n float64) *float64 {
		p := n * 1e-6
		return &p
	}
	group := &Group{ID: 8, RateMultiplier: 1.75, Platform: PlatformAnthropic, ModelPricing: []ChannelModelPricing{
		{Models: []string{advisorMainModel}, InputPrice: price(2), OutputPrice: price(3), CacheWritePrice: price(4), CacheWrite1hPrice: price(6), CacheReadPrice: price(8)},
		{Models: []string{advisorWireModel}, InputPrice: price(5), OutputPrice: price(7), CacheWritePrice: price(11), CacheWrite1hPrice: price(13), CacheReadPrice: price(17)},
		{Models: []string{advisorClientModel}, InputPrice: price(23), OutputPrice: price(29), CacheWritePrice: price(31), CacheWrite1hPrice: price(37), CacheReadPrice: price(41)},
	}}
	account := newAnthropicAPIKeyAccountForTest()
	account.Extra = map[string]any{"quota_limit": 100.0}
	accountRate := 3.0
	account.RateMultiplier = &accountRate
	return svc, store, &RecordUsageInput{
		APIKey: &APIKey{ID: 1, Group: group, GroupID: &group.ID}, User: &User{ID: 2}, Account: account,
		ChannelUsageFields: ChannelUsageFields{ChannelID: 9, OriginalModel: advisorMainModel, ChannelMappedModel: advisorMainModel},
	}
}

func TestAnthropicAdvisorBufferedBilling(t *testing.T) {
	for _, tt := range []struct {
		name       string
		iterations string
		amounts    []float64
	}{
		{name: "one", iterations: advisorFirst, amounts: []float64{200}},
		{name: "two", iterations: advisorFirst + "," + advisorSecond, amounts: []float64{200, 51}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc, store, input := newAdvisorBillingFixture()
			ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, strings.Repeat("r", 64))
			outer := fmt.Sprintf(`{"model":%q,"content":[],"usage":{%s}}`, advisorMainModel, advisorOuterUsage)
			body := strings.TrimSuffix(outer, "}}") + `,"iterations":[{"type":"message","input_tokens":1000000},{"type":"compaction","input_tokens":999},` + tt.iterations + "]}}"
			result, _, output := forwardAdvisorFixture(t, svc, input.Account, ctx, advisorMainModel, advisorTool(advisorWireModel), body, false, false)
			require.Equal(t, body, output)
			input.Result = result
			require.NoError(t, svc.RecordUsage(ctx, input))
			require.Len(t, store.logs, len(tt.amounts)+1)
			parent := store.logs[0]
			require.Equal(t, "client:"+strings.Repeat("r", 64), parent.RequestID)
			require.InDelta(t, 260*2.5e-6, parent.ActualCost, 1e-12)
			for i, amount := range tt.amounts {
				log := store.logs[i+1]
				require.Equal(t, fmt.Sprintf("%s:advisor:%d", parent.RequestID, i+1), log.RequestID)
				require.Equal(t, advisorWireModel, log.Model)
				require.Equal(t, advisorWireModel, log.RequestedModel)
				require.InDelta(t, amount*1e-6, log.TotalCost, 1e-12)
				require.InDelta(t, amount*2.5e-6, log.ActualCost, 1e-12)
				require.Equal(t, 3.0, *log.AccountRateMultiplier)
				require.InDelta(t, amount*3e-6, store.charges[log.RequestID].AccountQuotaCost, 1e-12)
			}
			require.Equal(t, 2, store.logs[1].CacheCreation5mTokens)
			require.Equal(t, 3, store.logs[1].CacheCreation1hTokens)

			ordinarySvc, ordinaryStore, ordinaryInput := newAdvisorBillingFixture()
			ordinary, _, ordinaryOutput := forwardAdvisorFixture(t, ordinarySvc, ordinaryInput.Account, ctx, advisorMainModel, "", outer, false, false)
			require.Equal(t, outer, ordinaryOutput)
			ordinaryInput.Result = ordinary
			require.NoError(t, ordinarySvc.RecordUsage(ctx, ordinaryInput))
			require.Len(t, ordinaryStore.logs, 1)
			ordinaryLog := *ordinaryStore.logs[0]
			ordinaryLog.CreatedAt, ordinaryLog.DurationMs = parent.CreatedAt, parent.DurationMs
			require.Equal(t, *parent, ordinaryLog)
			require.Equal(t, store.charges[parent.RequestID], ordinaryStore.charges[parent.RequestID])
		})
	}
}

func TestAnthropicAdvisorStreamingBilling(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			svc, store, input := newAdvisorBillingFixture()
			input.Account.Extra["anthropic_passthrough"] = passthrough
			input.Account.Credentials["model_mapping"] = map[string]any{"claude-main-alias": advisorMainModel}
			input.BillingModelSource = BillingModelSourceChannelMapped
			firstArray := `[{"type":"advisor_message","model":"claude-opus-4.8","input_tokens":999}]`
			finalArray := "[" + advisorFirst + `,{"type":"fallback_message","model":"cc_sess_keep","marker":9007199254740993},` + advisorSecond + "]"
			start := `{"type":"message_start","message":{"model":"claude-sonnet-4","usage":{"input_tokens":100}}}`
			stream := "event: message_start\ndata: " + start + "\n\n" +
				"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":` + advisorToolBlock + "}\n\n" +
				"event: content_block_start\ndata: " + `{"type":"content_block_start","index":1,"content_block":` + advisorResultBlock + "}\n\n" +
				"event: message_delta\ndata: " + `{"type":"message_delta","usage":{"output_tokens":10,"iterations":` + firstArray + "}}\n\n" +
				"event: message_delta\ndata: " + `{"type":"message_delta","usage":{"output_tokens":20,"cached_tokens":1,"iterations":` + finalArray + "}}\n\n" +
				"event: message_delta\ndata: " + `{"type":"message_delta","usage":{"output_tokens":20}}` + "\n\n" +
				"event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
			result, _, output := forwardAdvisorFixture(t, svc, input.Account, context.Background(), "claude-main-alias", advisorTool(advisorWireModel), stream, true, false)
			for _, raw := range []string{advisorToolBlock, advisorResultBlock, firstArray, finalArray} {
				require.Contains(t, output, raw)
			}
			if !passthrough {
				require.Contains(t, output, `"model":"claude-main-alias"`)
			}
			input.Result = result
			require.NoError(t, svc.RecordUsage(context.Background(), input))
			require.Len(t, store.logs, 3)
			require.InDelta(t, 268*2.5e-6, store.logs[0].ActualCost, 1e-12)
			require.InDelta(t, 200*2.5e-6, store.logs[1].ActualCost, 1e-12)
			require.InDelta(t, 51*2.5e-6, store.logs[2].ActualCost, 1e-12)
		})
	}
}

func TestAnthropicAdvisorBillingSourcesAndFallback(t *testing.T) {
	for _, tt := range []struct {
		name, source, model string
		want                float64
	}{
		{name: "upstream", source: BillingModelSourceUpstream, model: advisorWireModel, want: 200},
		{name: "requested", source: BillingModelSourceRequested, model: advisorWireModel, want: 654},
		{name: "channel-mapped", source: BillingModelSourceChannelMapped, model: advisorWireModel, want: 654},
		{name: "response", source: BillingModelSourceResponse, model: advisorWireModel, want: 200},
		{name: "missing", source: BillingModelSourceUpstream, model: "claude-haiku-unknown-99", want: 87},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			if tt.name == "missing" {
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(previous) })
			}
			svc, store, input := newAdvisorBillingFixture()
			input.BillingModelSource = tt.source
			iteration := strings.ReplaceAll(advisorFirst, advisorWireModel, tt.model)
			body := fmt.Sprintf(`{"model":%q,"usage":{%s,"iterations":[%s]}}`, advisorMainModel, advisorOuterUsage, iteration)
			result, _, output := forwardAdvisorFixture(t, svc, input.Account, context.Background(), advisorMainModel, advisorTool(advisorClientModel), body, false, false)
			require.Equal(t, advisorClientModel, gjson.Get(output, "usage.iterations.0.model").String())
			input.Result = result
			require.NoError(t, svc.RecordUsage(context.Background(), input))
			require.Len(t, store.logs, 2)
			require.Equal(t, tt.model, store.logs[1].Model)
			require.InDelta(t, tt.want*2.5e-6, store.logs[1].ActualCost, 1e-12)
			if tt.name == "missing" {
				require.Equal(t, 1, strings.Count(logs.String(), "billing.advisor_price_fallback"))
				require.Contains(t, logs.String(), "level=WARN msg=billing.advisor_price_fallback advisor_model="+tt.model+" main_model="+advisorMainModel)
			}
		})
	}
}

func TestAnthropicAdvisorBillingReplay(t *testing.T) {
	svc, store, input := newAdvisorBillingFixture()
	ctx := context.WithValue(context.Background(), ctxkey.ClientRequestID, "replay")
	body := fmt.Sprintf(`{"model":%q,"usage":{%s,"iterations":[%s,%s]}}`, advisorMainModel, advisorOuterUsage, advisorFirst, advisorSecond)
	result, _, _ := forwardAdvisorFixture(t, svc, input.Account, ctx, advisorMainModel, advisorTool(advisorWireModel), body, false, false)
	input.Result = result
	store.failOnce = "client:replay:advisor:1"
	require.ErrorContains(t, svc.RecordUsage(ctx, input), "transient settlement failure")
	require.Len(t, store.logs, 2)
	require.NoError(t, svc.RecordUsage(ctx, input))
	require.NoError(t, svc.RecordUsage(ctx, input))
	require.Len(t, store.logs, 3)
	require.Len(t, store.charges, 3)
	require.InDelta(t, (260+200+51)*2.5e-6, store.balance, 1e-12)
}

func TestAnthropicAdvisorBillingWithoutRequestID(t *testing.T) {
	svc, store, input := newAdvisorBillingFixture()
	ctx := context.Background()
	body := fmt.Sprintf(`{"model":%q,"usage":{%s,"iterations":[%s]}}`, advisorMainModel, advisorOuterUsage, advisorFirst)
	result, _, _ := forwardAdvisorFixture(t, svc, input.Account, ctx, advisorMainModel, advisorTool(advisorWireModel), body, false, false)
	result.RequestID = ""
	input.Result = result
	require.NoError(t, svc.RecordUsage(ctx, input))
	require.NoError(t, svc.RecordUsage(ctx, input))
	require.Len(t, store.logs, 2)
	require.True(t, strings.HasPrefix(store.logs[0].RequestID, "generated:"))
	require.Equal(t, store.logs[0].RequestID+":advisor:1", store.logs[1].RequestID)
	require.InDelta(t, (260+200)*2.5e-6, store.balance, 1e-12)
}

func TestAnthropicAdvisorMissingParentPriceReturnsError(t *testing.T) {
	svc, store, input := newAdvisorBillingFixture()
	ctx := context.Background()
	mainModel, advisorModel := "unknown-main", "unknown-advisor"
	input.OriginalModel, input.ChannelMappedModel = mainModel, mainModel
	iteration := strings.ReplaceAll(advisorFirst, advisorWireModel, advisorModel)
	body := fmt.Sprintf(`{"model":%q,"usage":{%s,"iterations":[%s]}}`, mainModel, advisorOuterUsage, iteration)
	result, _, _ := forwardAdvisorFixture(t, svc, input.Account, ctx, mainModel, advisorTool(advisorModel), body, false, false)
	input.Result = result
	require.ErrorIs(t, svc.RecordUsage(ctx, input), ErrModelPricingUnavailable)
	require.Len(t, store.logs, 1)
	require.Len(t, store.charges, 1)
}

func TestAnthropicAdvisorDisconnectWithoutIterations(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			svc, store, input := newAdvisorBillingFixture()
			input.Account.Extra["anthropic_passthrough"] = passthrough
			stream := "event: message_start\ndata: " + `{"type":"message_start","message":{"usage":{"input_tokens":100}}}` + "\n\n" +
				"event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":` + advisorToolBlock + "}\n\n"
			result, _, _ := forwardAdvisorFixture(t, svc, input.Account, context.Background(), advisorMainModel, advisorTool(advisorWireModel), stream, true, true)
			require.True(t, result.ClientDisconnect)
			input.Result = result
			require.NoError(t, svc.RecordUsage(context.Background(), input))
			require.Len(t, store.logs, 1)
			require.InDelta(t, 200*2.5e-6, store.logs[0].ActualCost, 1e-12)
		})
	}
}
