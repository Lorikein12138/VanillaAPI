package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type smartSchedulingStallSettingsRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *smartSchedulingStallSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *smartSchedulingStallSettingsRepo) SetMultiple(_ context.Context, updates map[string]string) error {
	if r.values == nil {
		r.values = make(map[string]string, len(updates))
	}
	for key, value := range updates {
		r.values[key] = value
	}
	return nil
}

func (r *smartSchedulingStallSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	return r.values[key], nil
}

type smartSchedulingStallSessionCache struct {
	groupID   int64
	accountID int64
}

func (c *smartSchedulingStallSessionCache) GetSessionAccountID(_ context.Context, groupID int64, sessionHash string) (int64, error) {
	if groupID == c.groupID && sessionHash != "" {
		return c.accountID, nil
	}
	return 0, service.ErrStickySessionNotFound
}

func (c *smartSchedulingStallSessionCache) SetSessionAccountID(context.Context, int64, string, int64, time.Duration) error {
	return nil
}

func (c *smartSchedulingStallSessionCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (c *smartSchedulingStallSessionCache) DeleteSessionAccountID(context.Context, int64, string) error {
	return nil
}

func (c *smartSchedulingStallSessionCache) SetGrokVideoPendingBilling(context.Context, string, []byte, time.Duration) error {
	return nil
}

func (c *smartSchedulingStallSessionCache) GetGrokVideoPendingBilling(context.Context, string) ([]byte, error) {
	return nil, nil
}

func (c *smartSchedulingStallSessionCache) ClaimGrokVideoBilled(context.Context, string, time.Duration) (bool, error) {
	return false, nil
}

func (c *smartSchedulingStallSessionCache) ReleaseGrokVideoBilled(context.Context, string) error {
	return nil
}

func (c *smartSchedulingStallSessionCache) SetReasoningContent(context.Context, string, string, time.Duration) error {
	return nil
}

func (c *smartSchedulingStallSessionCache) GetReasoningContent(context.Context, string) (string, error) {
	return "", service.ErrReasoningContentNotFound
}

var _ service.GatewayCache = (*smartSchedulingStallSessionCache)(nil)

type smartSchedulingStallHTTPUpstream struct {
	service.HTTPUpstream
	calls          []int64
	timeoutAccount map[int64]bool
	do             func(*http.Request, int64) (*http.Response, error)
}

func (u *smartSchedulingStallHTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, accountID)
	if u.do != nil {
		return u.do(req, accountID)
	}
	if accountID == 46 || u.timeoutAccount[accountID] {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if accountID != 44 {
		return nil, fmt.Errorf("unexpected account selection")
	}
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"ready"}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
	}, "\n\n")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(stream)),
	}, nil
}

func newSmartSchedulingStallHandler(t *testing.T, adaptive bool, usageRepo service.UsageLogRepository) (
	*OpenAIGatewayHandler, *smartSchedulingStallHTTPUpstream, *service.APIKey, *service.SettingService, *openAIWSFailoverHandlerAccountRepoStub,
) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	const groupID int64 = 30
	accounts := []service.Account{
		{
			ID: 46, Name: "unknown-sticky", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID},
			Credentials: map[string]any{"api_key": "test-token"},
			Extra:       map[string]any{"openai_responses_supported": true},
		},
		{
			ID: 45, Name: "unsupported-model", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID},
			Credentials: map[string]any{
				"api_key": "test-token", "model_mapping": map[string]any{"other-model": "other-model"},
			},
			Extra: map[string]any{"openai_responses_supported": true},
		},
		{
			ID: 44, Name: "compatible-fallback", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
			Status: service.StatusActive, Schedulable: true, Concurrency: 1, Priority: 2, GroupIDs: []int64{groupID},
			Credentials: map[string]any{"api_key": "test-token"},
			Extra:       map[string]any{"openai_responses_supported": true},
		},
	}
	accountRepo := &openAIWSFailoverHandlerAccountRepoStub{accounts: accounts}
	settingsRepo := &smartSchedulingStallSettingsRepo{values: map[string]string{}}
	if adaptive {
		settingsRepo.values[service.SettingKeyGatewayAdaptiveSchedulingEnabled] = "true"
	}
	settingService := service.NewSettingService(settingsRepo, &config.Config{})
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	cfg.Gateway.MaxAccountSwitches = 3
	rateLimitSvc := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	rateLimitSvc.SetSettingService(settingService)
	upstream := &smartSchedulingStallHTTPUpstream{}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo, usageRepo, nil, nil, nil, nil,
		&smartSchedulingStallSessionCache{groupID: groupID, accountID: 46},
		cfg, nil, nil, service.NewBillingService(cfg, nil), rateLimitSvc, billingCacheSvc,
		upstream, &service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil,
	)
	cache := &concurrencyCacheMock{
		acquireUserSlotFn:    func(context.Context, int64, int, string) (bool, error) { return true, nil },
		acquireAccountSlotFn: func(context.Context, int64, int, string) (bool, error) { return true, nil },
	}
	handler := &OpenAIGatewayHandler{
		gatewayService:      gatewaySvc,
		billingCacheService: billingCacheSvc,
		apiKeyService:       &service.APIKeyService{},
		concurrencyHelper:   NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatNone, time.Second),
		maxAccountSwitches:  3,
	}

	apiKey := &service.APIKey{
		ID: 1813, GroupID: ptrForSmartSchedulingStall(groupID),
		User:  &service.User{ID: 1713, Status: service.StatusActive, Balance: 1000},
		Group: &service.Group{ID: groupID, Platform: service.PlatformOpenAI, Status: service.StatusActive},
	}
	return handler, upstream, apiKey, settingService, accountRepo
}

func TestOpenAIFirstOutputTimeoutFailoverBudget(t *testing.T) {
	handler, upstream, apiKey, _, _ := newSmartSchedulingStallHandler(t, false, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-6-sol","stream":true,"input":"hello","prompt_cache_key":"stall-failover"}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})

	handler.Responses(c)

	if len(upstream.calls) == 0 {
		selectedAccount, _ := c.Get("ops_account_id")
		t.Fatalf("request did not reach upstream: status=%d selected_account=%v endpoint=%q body=%q", rec.Code, selectedAccount, service.GetActualOpenAIUpstreamEndpoint(c), rec.Body.String())
	}
	require.Equal(t, []int64{46, 44}, upstream.calls, "only the sticky first account and compatible fallback are attempted")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "response.output_text.delta")
	require.Contains(t, rec.Body.String(), "response.completed")
	require.NotContains(t, rec.Body.String(), "first_output_timeout")

	// The same shared upstream can stall both candidates. Only the first timeout
	// gets one account switch; the second must end the response without trying 45.
	upstream.calls = nil
	upstream.timeoutAccount = map[int64]bool{44: true}
	rec = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(
		`{"model":"gpt-6-sol","stream":true,"input":"hello","prompt_cache_key":"stall-failover"}`,
	))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(string(middleware.ContextKeyAPIKey), apiKey)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
	started := time.Now()
	handler.Responses(c)
	require.Less(t, time.Since(started), 2500*time.Millisecond, "both shared-upstream attempts have bounded one-second deadlines")
	require.Equal(t, []int64{46, 44}, upstream.calls, "first-output timeout budget permits only one account switch")
	require.Equal(t, http.StatusBadGateway, rec.Code, "the final 504 attempt maps to the established client error contract")
	require.NotContains(t, rec.Body.String(), "response.output_text.delta")
}

func TestAdaptiveResponsesFirstOutputReplayAndBilling(t *testing.T) {
	const safeBody = `{"model":"gpt-6-sol","stream":true,"input":"hello","store":false}`
	for _, tc := range []struct {
		name         string
		body         string
		firstWire    string
		secondCode   int
		second       bool
		incompatible bool
		stallHead    bool
		cancel       bool
		cancelBody   bool
		keepalive    bool
		allowance    time.Duration
		wantCode     int
		wantTokens   int
	}{
		{name: "CDN headers and recovered stream", body: safeBody, second: true, firstWire: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_private\",\"store\":false}}\n\n", wantCode: 200, wantTokens: 9},
		{name: "preamble event header without line ending", body: safeBody, firstWire: "event: response.created", wantCode: 504},
		{name: "preamble data without frame separator", body: safeBody, firstWire: "data: {\"type\":\"response.created\",\"response\":{\"store\":false}}\n", wantCode: 504},
		{name: "replacement fails without third POST", body: safeBody, second: true, secondCode: 503, stallHead: true, wantCode: 502},
		{name: "incompatible replacement stops before POST", body: safeBody, incompatible: true, stallHead: true, wantCode: 504},
		{name: "stored request times out at headers", body: `{"model":"gpt-6-sol","stream":true,"input":"hello"}`, stallHead: true, wantCode: 504},
		{name: "partial usage bills initial account", body: safeBody, firstWire: "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":2}}}}", wantCode: 504, wantTokens: 7},
		{name: "started keepalive uses SSE failure", body: `{"model":"gpt-6-sol","stream":true,"input":"hello","store":true}`, keepalive: true, stallHead: true, wantCode: 200},
		{name: "header cancellation", body: safeBody, stallHead: true, cancel: true, wantCode: 200},
		{name: "body cancellation bills observed usage", body: safeBody, firstWire: "data: {\"type\":\"response.in_progress\",\"response\":{\"usage\":{\"input_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":2}}}}", cancelBody: true, wantCode: 200, wantTokens: 7},
		{name: "header and body share deadline", body: `{"model":"gpt-6-sol","stream":true,"input":"hello"}`, allowance: 700 * time.Millisecond, wantCode: 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usageRepo := &openAIWSUsageHandlerUsageLogRepoStub{created: make(chan *service.UsageLog, 4)}
			h, upstream, apiKey, settings, accountRepo := newSmartSchedulingStallHandler(t, true, usageRepo)
			if tc.incompatible {
				accountRepo.accounts[2].Extra["openai_passthrough"] = true
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = service.WithGatewayAdaptiveIngress(ctx, settings, "POST /openai/v1/responses", "sse")
			c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", strings.NewReader(tc.body)).WithContext(ctx)
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("CF-Ray", "test-hkg")
			c.Request.Header.Set("CF-Connecting-IP", "192.0.2.15")
			c.Request.Header.Set("X-ESA-Request-ID", "edge-request")
			c.Request.Header.Set("Via", "proxy")
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.User.ID, Concurrency: 1})
			if tc.keepalive {
				_, err := c.Writer.WriteString(":\n\n")
				require.NoError(t, err)
				c.Writer.Flush()
			}
			bodyClosed := make(chan struct{})
			upstream.do = func(req *http.Request, accountID int64) (*http.Response, error) {
				require.Empty(t, req.Header.Get("CF-Ray"))
				require.Empty(t, req.Header.Get("X-ESA-Request-ID"))
				if len(upstream.calls) > 1 {
					if !tc.stallHead {
						select {
						case <-bodyClosed:
						default:
							t.Fatal("replacement started before the original response closed")
						}
					}
					if tc.secondCode != 0 {
						return &http.Response{StatusCode: tc.secondCode, Header: http.Header{},
							Body: io.NopCloser(strings.NewReader(`{"error":{"message":"unavailable"}}`))}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"req_winner"}},
						Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":9,\"output_tokens\":3}}}\n\n"))}, nil
				}
				if tc.cancel {
					cancel()
				}
				if tc.stallHead {
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				if tc.allowance > 0 {
					time.Sleep(tc.allowance)
				}
				pr, pw := io.Pipe()
				t.Cleanup(func() { _ = pw.Close() })
				go func() {
					_, _ = io.WriteString(pw, tc.firstWire)
					if tc.cancelBody {
						cancel()
					}
				}()
				return &http.Response{StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"req_private"}},
					Body:   &adaptiveResponsesTestBody{ReadCloser: pr, done: bodyClosed}}, nil
			}
			start := time.Now()
			h.Responses(c)
			require.NotEmpty(t, upstream.calls, rec.Body.String())
			wantCalls := []int64{46}
			if tc.second {
				wantCalls = append(wantCalls, 44)
			}
			require.Equal(t, wantCalls, upstream.calls, rec.Body.String())
			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "resp_private")
			require.NotEqual(t, "req_private", rec.Header().Get("X-Request-Id"))
			events, _ := c.Get(service.OpsUpstreamErrorsKey)
			if tc.cancel || tc.cancelBody {
				require.Empty(t, events)
				require.Empty(t, rec.Body.String())
			}
			if !tc.cancel && !tc.cancelBody {
				attempts, ok := events.([]*service.OpsUpstreamErrorEvent)
				require.True(t, ok)
				require.NotEmpty(t, attempts)
				require.Equal(t, "first_output_timeout", attempts[0].Kind)
				require.Equal(t, upstream.calls[0], attempts[0].AccountID)
				if tc.stallHead {
					require.Contains(t, attempts[0].Detail, "phase=response_headers")
				} else {
					require.Contains(t, attempts[0].Detail, "phase=semantic_output")
					require.Equal(t, "req_private", attempts[0].UpstreamRequestID)
				}
			}
			if tc.allowance > 0 {
				require.Less(t, time.Since(start), 1500*time.Millisecond)
			}
			if tc.keepalive {
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: response.failed"))
				streamErr, recorded := service.GetOpsStreamError(c)
				require.True(t, recorded)
				require.True(t, streamErr.CountTowardsSLA)
			}
			if tc.wantCode == 504 {
				require.Contains(t, rec.Body.String(), "first_output_timeout")
				require.Contains(t, rec.Header().Get("Content-Type"), "application/json")
			}
			if tc.wantTokens > 0 {
				require.Len(t, usageRepo.created, 1, rec.Body.String())
				record := <-usageRepo.created
				require.Equal(t, tc.wantTokens, record.InputTokens)
				if tc.second {
					require.Equal(t, upstream.calls[1], record.AccountID)
					require.Equal(t, "req_winner", record.RequestID)
					require.NotContains(t, rec.Body.String(), "first_output_timeout")
					_, finalStreamFailure := service.GetOpsStreamError(c)
					require.False(t, finalStreamFailure)
				} else {
					require.Equal(t, 2, record.CacheReadTokens)
					require.Equal(t, upstream.calls[0], record.AccountID)
					require.Equal(t, "req_private", record.RequestID)
				}
			} else {
				require.Empty(t, usageRepo.created)
			}
		})
	}
}

type adaptiveResponsesTestBody struct {
	io.ReadCloser
	done   chan struct{}
	closed bool
}

func (b *adaptiveResponsesTestBody) Close() error {
	if !b.closed {
		b.closed = true
		close(b.done)
	}
	return b.ReadCloser.Close()
}

func ptrForSmartSchedulingStall(value int64) *int64 { return &value }

func TestOpenAIFirstOutputTimeoutUsesBaselineSchedulerFailureAndBudget(t *testing.T) {
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:               http.StatusGatewayTimeout,
		ResponseBody:             []byte(`{"error":{"type":"first_output_timeout"}}`),
		SafeToFailoverAfterWrite: true,
	}
	count := 0
	require.True(t, failoverErr.ShouldReportAccountScheduleFailure())
	require.False(t, openAIFirstOutputFailoverExhausted(failoverErr, &count))
	require.Equal(t, 1, count, "the first timeout allows exactly one account switch")
	require.True(t, openAIFirstOutputFailoverExhausted(failoverErr, &count))
	require.Equal(t, 1, count, "additional timeout cannot grant another switch")
}

func TestOpenAIForwardMayFailoverOnlyAfterNonSemanticWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	before := service.OpenAICompactKeepaliveAdjustedWrittenSize(c)

	_, err := fmt.Fprint(c.Writer, ":\n\n")
	require.NoError(t, err)
	c.Writer.Flush()

	require.True(t, openAIForwardMayFailover(c, before, &service.UpstreamFailoverError{
		SafeToFailoverAfterWrite: true,
	}))
	require.False(t, openAIForwardMayFailover(c, before, &service.UpstreamFailoverError{}))
}

func TestOpenAIFirstOutputFailoverStopsAfterOneAccountSwitch(t *testing.T) {
	failoverErr := &service.UpstreamFailoverError{SafeToFailoverAfterWrite: true}
	count := 0

	require.False(t, openAIFirstOutputFailoverExhausted(failoverErr, &count))
	require.Equal(t, 1, count)
	require.True(t, openAIFirstOutputFailoverExhausted(failoverErr, &count))
	require.Equal(t, 1, count)
}

func TestOpenAIRequestAllowsFailoverReplayStopsCanceledClient(t *testing.T) {
	require.False(t, openAIRequestAllowsFailoverReplay(nil))

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	requestCtx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(requestCtx)

	require.True(t, openAIRequestAllowsFailoverReplay(c))
	cancel()
	require.False(t, openAIRequestAllowsFailoverReplay(c))
}
