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
}

func (u *smartSchedulingStallHTTPUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	u.calls = append(u.calls, accountID)
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

func TestSmartSchedulingStall_FailoverBudget(t *testing.T) {
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
	settingService := service.NewSettingService(settingsRepo, &config.Config{})
	require.NoError(t, settingService.UpdateSettings(context.Background(), &service.SystemSettings{
		SmartSchedulingEnabled:          true,
		OpenAIFirstOutputTimeoutSeconds: 1,
	}))
	cfg := &config.Config{RunMode: config.RunModeSimple}
	cfg.Default.RateMultiplier = 1
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 0
	cfg.Gateway.MaxAccountSwitches = 3
	rateLimitSvc := service.NewRateLimitService(accountRepo, nil, cfg, nil, nil)
	rateLimitSvc.SetSettingService(settingService)
	upstream := &smartSchedulingStallHTTPUpstream{}
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billingCacheSvc.Stop)
	gatewaySvc := service.NewOpenAIGatewayService(
		accountRepo, nil, nil, nil, nil, nil,
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

func ptrForSmartSchedulingStall(value int64) *int64 { return &value }

func TestSmartSchedulingStall_TimeoutAttribution(t *testing.T) {
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
