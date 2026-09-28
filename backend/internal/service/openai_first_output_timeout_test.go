package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type blockingOpenAIResponseHeaderUpstream struct {
	canceled chan struct{}
	once     sync.Once
}

type firstOutputCloseTrackingBody struct {
	io.ReadCloser
	closed chan struct{}
	once   sync.Once
}

func (b *firstOutputCloseTrackingBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.ReadCloser.Close()
}

func (u *blockingOpenAIResponseHeaderUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	select {
	case <-req.Context().Done():
		u.once.Do(func() { close(u.canceled) })
		return nil, req.Context().Err()
	case <-time.After(1500 * time.Millisecond):
		return nil, errors.New("test upstream was not canceled before response headers")
	}
}

func (u *blockingOpenAIResponseHeaderUpstream) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, "", 0, 0)
}

func TestSmartSchedulingStall_HTTP(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	repo := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
		SettingKeySmartSchedulingEnabled:          "true",
		SettingKeyOpenAIFirstOutputTimeoutSeconds: "1",
	}}
	upstream := &blockingOpenAIResponseHeaderUpstream{canceled: make(chan struct{})}
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 0,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{settingService: NewSettingService(repo, cfg)},
	}
	body := []byte(`{"model":"gpt-5.5","stream":true,"reasoning":{"effort":"low"},"input":"hello"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	account := &Account{
		ID: 1, Name: "oauth-test", Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "test-token", "chatgpt_account_id": "test-account"},
	}

	started := time.Now()
	_, err := svc.Forward(context.Background(), c, account, body)

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Contains(t, string(failoverErr.ResponseBody), "first_output_timeout")
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Less(t, time.Since(started), 1300*time.Millisecond)
	require.Empty(t, rec.Body.String())
	select {
	case <-upstream.canceled:
	default:
		t.Fatal("response-header timeout did not cancel the upstream request context")
	}
}

func TestOpenAINativeFirstOutputTimeoutDisabledPreservesSynchronousStream(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 0,
		MaxLineSize:                     defaultMaxLineSize,
	}}}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_disabled"}}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_disabled","usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
	}, "\n")))}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "response.completed")
}

func TestOpenAINativeFirstOutputTimeoutIgnoresPreambleAndCleansReader(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 1,
		MaxLineSize:                     defaultMaxLineSize,
	}}}
	pr, pw := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_slow\"}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_slow\"}}\n\n"))
		time.Sleep(200 * time.Millisecond)
	}()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	body := &firstOutputCloseTrackingBody{ReadCloser: pr, closed: make(chan struct{})}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now().Add(-2*time.Second), "model", "model")

	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Contains(t, string(failoverErr.ResponseBody), "first_output_timeout")
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Empty(t, rec.Body.String())
	select {
	case <-body.closed:
	default:
		t.Fatal("first-output timeout did not close the upstream response body")
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("stream reader/writer goroutine did not exit after first-output timeout")
	}
}

func TestNewOpenAIFirstOutputTimeoutErrorRecordsCallerProxyAttribution(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	svc := &OpenAIGatewayService{}
	proxyID := int64(10060)
	account := &Account{ID: 1, Name: "acc", Platform: PlatformOpenAI, ProxyID: &proxyID, Proxy: &Proxy{ID: proxyID, Name: "ws-proxy"}}

	// WebSocket callers pass the WS attribution; HTTP callers pass the HTTP one.
	wsID, wsName := opsUpstreamWSProxyAttribution(&Account{ID: 2, Platform: PlatformOpenAI})
	_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), c, &Account{ID: 2, Name: "ws", Platform: PlatformOpenAI},
		wsID, wsName, time.Now(), "gpt-5.5", "", time.Second, "websocket_first_semantic_output", nil)
	_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), c, account,
		opsUpstreamProxyID(account), opsUpstreamProxyName(account), time.Now(), "gpt-5.5", "", time.Second, "semantic_output", nil)

	raw, ok := c.Get(OpsUpstreamErrorsKey)
	require.True(t, ok)
	events, ok := raw.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	require.Len(t, events, 2)
	require.Equal(t, "first_output_timeout", events[0].Kind)
	require.Nil(t, events[0].ProxyID)
	require.Equal(t, opsProxyNameUnknown, events[0].ProxyName)
	require.NotNil(t, events[1].ProxyID)
	require.Equal(t, proxyID, *events[1].ProxyID)
	require.Equal(t, "ws-proxy", events[1].ProxyName)
}

func TestOpenAIFirstOutputTimeoutForReasoningEffort(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds:           120,
		OpenAIHighEffortFirstOutputTimeoutSeconds: 300,
	}}}

	require.Equal(t, 120*time.Second, svc.openAIFirstOutputTimeout("low"))
	require.Equal(t, 300*time.Second, svc.openAIFirstOutputTimeout("high"))
	require.Equal(t, 300*time.Second, svc.openAIFirstOutputTimeout("xhigh"))
	require.Equal(t, 300*time.Second, svc.openAIFirstOutputTimeout("max"))
}

func TestSmartSchedulingStall_TimeoutModes(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

	newContext := func() *gin.Context {
		t.Helper()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		return c
	}
	newGateway := func(values map[string]string, static, high int) (*OpenAIGatewayService, *openAIAdvancedSchedulerSettingRepoStub) {
		repo := &openAIAdvancedSchedulerSettingRepoStub{values: values}
		cfg := &config.Config{Gateway: config.GatewayConfig{
			OpenAIFirstOutputTimeoutSeconds:           static,
			OpenAIHighEffortFirstOutputTimeoutSeconds: high,
		}}
		gateway := &OpenAIGatewayService{
			cfg:              cfg,
			rateLimitService: &RateLimitService{settingService: NewSettingService(repo, cfg)},
		}
		return gateway, repo
	}

	t.Run("smart backend values include absent default and explicit boundaries", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			value   string
			want    time.Duration
			present bool
		}{
			{name: "missing defaults to thirty", want: 30 * time.Second},
			{name: "zero disables", value: "0", present: true},
			{name: "one second", value: "1", want: time.Second, present: true},
			{name: "thirty seconds", value: "30", want: 30 * time.Second, present: true},
			{name: "three hundred seconds", value: "300", want: 300 * time.Second, present: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetOpenAIAdvancedSchedulerSettingCacheForTest()
				values := map[string]string{SettingKeySmartSchedulingEnabled: "true"}
				if tc.present {
					values[SettingKeyOpenAIFirstOutputTimeoutSeconds] = tc.value
				}
				gateway, _ := newGateway(values, 0, 900)
				require.Equal(t, tc.want, gateway.openAIFirstOutputTimeoutForRequest(context.Background(), newContext(), "max"))
			})
		}
	})

	t.Run("legacy config and high effort override remain isolated to smart off", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		gateway, _ := newGateway(map[string]string{
			SettingKeySmartSchedulingEnabled:          "false",
			SettingKeyOpenAIFirstOutputTimeoutSeconds: "0",
		}, 30, 300)
		c := newContext()
		require.Equal(t, 30*time.Second, gateway.openAIFirstOutputTimeoutForRequest(context.Background(), c, "low"))
		require.Equal(t, 300*time.Second, gateway.openAIFirstOutputTimeoutForRequest(context.Background(), c, "xhigh"))
	})

	t.Run("in flight context retains its setting generation", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		gateway, repo := newGateway(map[string]string{
			SettingKeySmartSchedulingEnabled:          "true",
			SettingKeyOpenAIFirstOutputTimeoutSeconds: "1",
		}, 30, 300)
		inFlight := newContext()
		require.Equal(t, time.Second, gateway.openAIFirstOutputTimeoutForRequest(context.Background(), inFlight, "high"))

		repo.values[SettingKeySmartSchedulingEnabled] = "false"
		repo.values[SettingKeyOpenAIFirstOutputTimeoutSeconds] = "300"
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		require.Equal(t, time.Second, gateway.openAIFirstOutputTimeoutForRequest(context.Background(), inFlight, "high"))
		fresh := newContext()
		require.Equal(t, 300*time.Second, gateway.openAIFirstOutputTimeoutForRequest(context.Background(), fresh, "high"))
	})
}

func TestSmartSchedulingStall_Passthrough(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	upstream := &blockingOpenAIResponseHeaderUpstream{canceled: make(chan struct{})}
	repo := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
		SettingKeySmartSchedulingEnabled:          "true",
		SettingKeyOpenAIFirstOutputTimeoutSeconds: "1",
	}}
	cfg := &config.Config{Gateway: config.GatewayConfig{OpenAIFirstOutputTimeoutSeconds: 0, MaxLineSize: defaultMaxLineSize}}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		httpUpstream:     upstream,
		rateLimitService: &RateLimitService{settingService: NewSettingService(repo, cfg)},
	}
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":"hello"}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive,
		Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "test-token"}}

	started := time.Now()
	_, err := svc.forwardOpenAIPassthrough(context.Background(), c, account, body, body, "gpt-5.5", false, nil, true, started)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Less(t, time.Since(started), 1300*time.Millisecond)
	select {
	case <-upstream.canceled:
	default:
		t.Fatal("passthrough response-header timeout did not cancel the upstream request")
	}
}

func TestSmartSchedulingStall_PassthroughSemanticTimeout(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pr, pw := io.Pipe()
	body := &firstOutputCloseTrackingBody{ReadCloser: pr, closed: make(chan struct{})}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = io.WriteString(pw, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ready\"}\n\n")
		// A short test deadline models long generation after semantic output.
		time.Sleep(250 * time.Millisecond)
		_, _ = io.WriteString(pw, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		_ = pw.Close()
	}()

	result, err := svc.handleStreamingResponsePassthrough(ctx, resp, c, &Account{ID: 2, Platform: PlatformOpenAI}, time.Now(), "model", "model",
		openAIFirstOutputRequestOptions{timeout: 100 * time.Millisecond})
	require.NoError(t, err, "the first semantic event disarms the watchdog for long generation")
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "response.output_text.delta")
	require.Contains(t, rec.Body.String(), "response.completed")
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("passthrough stream writer did not finish")
	}
}

func TestSmartSchedulingStall_PassthroughTimeout(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc, _, accountRepo, timeoutCounter := newFirstOutputTimeoutTestGateway(cfg, nil)
	pr, pw := io.Pipe()
	body := &firstOutputCloseTrackingBody{ReadCloser: pr, closed: make(chan struct{})}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(openAIFirstOutputSettingsContextKey, openAIAdvancedSchedulerRuntimeSettings{smartEnabled: true})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = io.WriteString(pw, ": keepalive\n\n")
		time.Sleep(250 * time.Millisecond)
		_, _ = io.WriteString(pw, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")
		_ = pw.Close()
	}()

	started := time.Now()
	_, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, &Account{ID: 3, Platform: PlatformOpenAI}, started, "model", "model",
		openAIFirstOutputRequestOptions{timeout: 100 * time.Millisecond})
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Empty(t, rec.Body.String(), "a heartbeat does not become semantic output or restart the clock")
	require.Empty(t, timeoutCounter.increments, "a smart-mode first-output timeout is not a stream-idle penalty")
	require.Empty(t, accountRepo.tempUnschedCalls)
	select {
	case <-body.closed:
	case <-time.After(time.Second):
		t.Fatal("passthrough semantic-output timeout did not close the upstream body")
	}
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("passthrough blocked reader did not stop after timeout")
	}
}

func TestSmartSchedulingStall_PassthroughStagingLimit(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
	upstreamPayload := "data: " + strings.Repeat("x", int(openAIFirstOutputStageMaxBytes+1024))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstreamPayload))}

	_, err := svc.handleStreamingResponsePassthrough(context.Background(), resp, c, &Account{ID: 4, Platform: PlatformOpenAI}, time.Now(), "model", "model")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Empty(t, rec.Body.String(), "overflowing pre-output data is not exposed before failover")
}

func TestOpenAIFirstOutputStageDefaultLimitIsIndependentFromScannerLimit(t *testing.T) {
	stage := newDefaultOpenAIFirstOutputStage()
	defer func() { require.NoError(t, stage.Close()) }()

	require.EqualValues(t, 8*1024*1024, stage.limit)
	require.Greater(t, stage.limit, int64(68106))
	require.Less(t, stage.limit, int64(defaultMaxLineSize))
}

func TestOpenAIFirstOutputEventQueueSizeBackpressuresGuardedStreams(t *testing.T) {
	require.Equal(t, 1, openAIFirstOutputEventQueueSize(true))
	require.Equal(t, 16, openAIFirstOutputEventQueueSize(false))
}

func TestOpenAIFirstOutputDynamicScannerLimitsOnlyWhileGuardIsActive(t *testing.T) {
	var guardActive atomic.Bool
	guardActive.Store(true)
	split := openAIFirstOutputDynamicScanLines(&guardActive)
	guardLimit := openAIFirstOutputStageMaxBytes + openAIFirstOutputScannerFramingAllowance
	undelimited := bytes.Repeat([]byte("x"), guardLimit)

	_, _, err := split(undelimited, false)
	require.ErrorIs(t, err, errOpenAIFirstOutputScannerLimit)

	guardActive.Store(false)
	advance, token, err := split(undelimited, false)
	require.NoError(t, err)
	require.Zero(t, advance)
	require.Nil(t, token)
}

func TestOpenAIFirstOutputStageOverflowIsAtomicAndCleanupRemovesSpool(t *testing.T) {
	stage := newOpenAIFirstOutputStage(70 * 1024)
	payload := bytes.Repeat([]byte("x"), 68*1024)
	n, err := stage.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	if runtime.GOOS == "windows" {
		require.Nil(t, stage.tempFile)
		require.Empty(t, stage.tempPath)
	} else {
		require.NotNil(t, stage.tempFile)
		require.NotEmpty(t, stage.tempPath)
		_, err = os.Stat(stage.tempPath)
		require.ErrorIs(t, err, os.ErrNotExist)
		stat, statErr := stage.tempFile.Stat()
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())
	}

	n, err = stage.Write(bytes.Repeat([]byte("y"), 3*1024))
	require.Zero(t, n)
	require.ErrorIs(t, err, errOpenAIFirstOutputStageLimit)
	require.EqualValues(t, len(payload), stage.Buffered())
	path := stage.tempPath
	require.NoError(t, stage.Close())
	require.True(t, stage.closed)
	require.Nil(t, stage.tempFile)
	require.Empty(t, stage.tempPath)
	if path != "" {
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestOpenAIFirstOutputStageCommitCopiesSpoolAndRemovesTemp(t *testing.T) {
	stage := newOpenAIFirstOutputStage(80 * 1024)
	payload := bytes.Repeat([]byte("z"), 68*1024)
	_, err := stage.Write(payload)
	require.NoError(t, err)
	path := stage.tempPath
	if runtime.GOOS == "windows" {
		require.Empty(t, path)
		require.Nil(t, stage.tempFile)
	} else {
		require.NotEmpty(t, path)
		require.NotNil(t, stage.tempFile)
		_, statErr := os.Stat(path)
		require.ErrorIs(t, statErr, os.ErrNotExist)
	}

	var downstream bytes.Buffer
	require.NoError(t, stage.CommitTo(&downstream))
	require.Equal(t, payload, downstream.Bytes())
	require.Zero(t, stage.Buffered())
	if path != "" {
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.NoError(t, stage.Close())
}

func TestOpenAIFirstOutputStageUnlinkFailurePermanentlyFallsBackToMemoryAndRetriesCleanup(t *testing.T) {
	stage := newDefaultOpenAIFirstOutputStage()
	stage.memoryOnly = false
	t.Cleanup(func() {
		stage.removeFile = os.Remove
		_ = stage.Close()
	})
	createCalls := 0
	stage.createTemp = func() (*os.File, error) {
		createCalls++
		return os.CreateTemp("", "sub2api-openai-first-output-fallback-*")
	}
	removeCalls := 0
	stage.removeFile = func(path string) error {
		removeCalls++
		if removeCalls <= 2 {
			return errors.New("forced remove failure")
		}
		return os.Remove(path)
	}

	payload := bytes.Repeat([]byte("m"), 68*1024)
	_, err := stage.Write(payload)
	require.NoError(t, err)
	require.True(t, stage.memoryOnly)
	require.Nil(t, stage.tempFile)
	require.NotEmpty(t, stage.tempPath)
	require.Equal(t, 1, createCalls)
	stat, statErr := os.Stat(stage.tempPath)
	require.NoError(t, statErr)
	require.Zero(t, stat.Size(), "failed-unlink fallback must never write plaintext to the named file")

	_, err = stage.WriteString("more")
	require.NoError(t, err)
	require.Equal(t, 1, createCalls, "memory-only fallback must not retry CreateTemp")
	path := stage.tempPath
	cleanupErr := stage.Close()
	require.ErrorContains(t, cleanupErr, "forced remove failure")
	require.Empty(t, stage.tempPath)
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, stage.Close())
}

func TestOpenAINativeFirstOutputTimeoutDisarmsAfterSemanticOutput(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 1,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_ok\"}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
		time.Sleep(1100 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ok\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"))
	}()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"X-Request-Id":                   []string{"request-winning"},
		"X-Ratelimit-Remaining-Requests": []string{"42"},
	}, Body: pr}

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.firstTokenMs)
	require.Contains(t, rec.Body.String(), "response.output_text.delta")
	require.Contains(t, rec.Body.String(), "response.completed")
	require.Equal(t, "request-winning", rec.Result().Header.Get("X-Request-Id"))
	require.Equal(t, "42", rec.Result().Header.Get("X-Ratelimit-Remaining-Requests"))
}

func TestOpenAINativeFirstOutputTimeoutWaitsForCompleteSemanticEvent(t *testing.T) {
	const lineSize = 68106
	prefix := `data: {"type":"response.output_text.delta","delta":"`
	suffix := `"}`
	line := prefix + strings.Repeat("x", lineSize-len(prefix)-len(suffix)) + suffix
	require.Len(t, line, lineSize)
	assertOpenAINativeLargeOpenEventTimesOutWithoutLeak(t, line)
}

func TestOpenAINativeFirstOutputTimeoutDoesNotLeakLargePreambleEvent(t *testing.T) {
	const lineSize = 68106
	prefix := `data: {"type":"response.created","response":{"id":"resp_partial","padding":"`
	suffix := `"}}`
	line := prefix + strings.Repeat("x", lineSize-len(prefix)-len(suffix)) + suffix
	require.Len(t, line, lineSize)
	assertOpenAINativeLargeOpenEventTimesOutWithoutLeak(t, line)
}

func assertOpenAINativeLargeOpenEventTimesOutWithoutLeak(t *testing.T, line string) {
	t.Helper()
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 1,
		StreamKeepaliveInterval:         1,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	pr, pw := io.Pipe()
	body := &firstOutputCloseTrackingBody{ReadCloser: pr, closed: make(chan struct{})}
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte(line + "\n"))
		select {
		case <-body.closed:
		case <-time.After(2 * time.Second):
		}
	}()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{
		"X-Request-Id":                   []string{"request-partial"},
		"X-Ratelimit-Remaining-Requests": []string{"1"},
	}, Body: body}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.Contains(t, string(failoverErr.ResponseBody), "first_output_timeout")
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.NotContains(t, rec.Body.String(), "data:", "attempt JSON must remain private before the SSE boundary")
	require.NotContains(t, rec.Body.String(), `"type"`, "attempt JSON must remain private before the SSE boundary")
	for _, outputLine := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		if outputLine != "" {
			require.True(t, strings.HasPrefix(outputLine, ":"), "only keepalive comments may precede failover: %q", outputLine)
		}
	}
	require.Empty(t, rec.Header().Values("X-Request-Id"))
	require.Empty(t, rec.Header().Values("X-Ratelimit-Remaining-Requests"))
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("partial-event writer did not exit after timeout closed the body")
	}
}

func TestOpenAINativeFirstOutputEOFDispatchesTerminalEventWithoutBlankLine(t *testing.T) {
	gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{
		openAITTFTMode: OpenAITTFTModeVisible,
		expiresAt:      time.Now().Add(time.Minute).UnixNano(),
	})
	t.Cleanup(func() {
		gatewayForwardingCache.Store(&cachedGatewayForwardingSettings{
			openAITTFTMode: OpenAITTFTModeSemantic,
			expiresAt:      time.Now().Add(time.Minute).UnixNano(),
		})
	})
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 1,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	payload := `data: {"type":"response.completed","response":{"id":"resp_eof","usage":{"input_tokens":3,"output_tokens":2}}}`
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"X-Request-Id":                   []string{"request-eof"},
			"X-Ratelimit-Remaining-Requests": []string{"17"},
		},
		Body: io.NopCloser(strings.NewReader(payload)),
	}

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Nil(t, result.firstTokenMs, "usage-only terminal event is not visible output")
	require.Equal(t, "resp_eof", result.responseID)
	require.Equal(t, 3, result.usage.InputTokens)
	require.Equal(t, 2, result.usage.OutputTokens)
	require.Contains(t, rec.Body.String(), `"type":"response.completed"`)
	require.Contains(t, rec.Body.String(), `"id":"resp_eof"`)
	require.True(t, strings.HasSuffix(rec.Body.String(), "\n"))
	require.False(t, strings.HasSuffix(rec.Body.String(), "\n\n"), "EOF dispatch must not synthesize a blank line")
	require.Equal(t, "request-eof", rec.Result().Header.Get("X-Request-Id"))
	require.Equal(t, "17", rec.Result().Header.Get("X-Ratelimit-Remaining-Requests"))
}

func TestOpenAINativeFirstOutputStageOverflowFailsOverWithoutAttemptBytes(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 30,
		MaxLineSize:                     2 * 1024 * 1024,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	const lineSize = 1024*1024 - 256
	prefix := `data: {"type":"response.output_text.delta","delta":"`
	suffix := `"}`
	line := prefix + strings.Repeat("x", lineSize-len(prefix)-len(suffix)) + suffix
	body := strings.Repeat(line+"\n", 9)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"X-Request-Id":                   []string{"request-overflow"},
			"X-Ratelimit-Remaining-Requests": []string{"1"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Contains(t, string(failoverErr.ResponseBody), "staging limit exceeded")
	require.Empty(t, rec.Body.String())
	require.Empty(t, rec.Header().Values("X-Request-Id"))
	require.Empty(t, rec.Header().Values("X-Ratelimit-Remaining-Requests"))
}

func TestOpenAINativeFirstOutputScannerRejectsOversizedLineWithoutLeak(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 30,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	oversizedLine := "data: " + strings.Repeat("x", openAIFirstOutputStageMaxBytes+openAIFirstOutputScannerFramingAllowance+1024)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_private\"}}\n\n" + oversizedLine + "\n"
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"X-Request-Id":                   []string{"request-too-large"},
			"X-Ratelimit-Remaining-Requests": []string{"1"},
		},
		Body: io.NopCloser(strings.NewReader(body)),
	}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusBadGateway, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Contains(t, string(failoverErr.ResponseBody), "line exceeds guarded first-output limit")
	require.Empty(t, rec.Body.String())
	require.Empty(t, rec.Header().Values("X-Request-Id"))
	require.Empty(t, rec.Header().Values("X-Ratelimit-Remaining-Requests"))
}

func TestOpenAINativeFirstOutputScannerAllowsLargeEventAfterSemanticBoundary(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 30,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg)}
	largeDelta := strings.Repeat("i", openAIFirstOutputStageMaxBytes+openAIFirstOutputScannerFramingAllowance+1024)
	body := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"ready"}`,
		"",
		`data: {"type":"response.output_text.delta","delta":"` + largeDelta + `"}`,
		"",
		`data: {"type":"response.completed","response":{"id":"resp_large_image","usage":{"input_tokens":4,"output_tokens":3}}}`,
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"X-Request-Id": []string{"request-large-image"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.firstTokenMs)
	require.Equal(t, "resp_large_image", result.responseID)
	require.Equal(t, 4, result.usage.InputTokens)
	require.Equal(t, 3, result.usage.OutputTokens)
	require.Contains(t, rec.Body.String(), `"delta":"ready"`)
	require.Contains(t, rec.Body.String(), `"id":"resp_large_image"`)
	require.Contains(t, rec.Body.String(), strings.Repeat("i", 1024))
	require.Equal(t, "request-large-image", rec.Result().Header.Get("X-Request-Id"))
}

func TestOpenAINativeFirstOutputTimeoutDisabledKeepsPreamblePrivateAcrossKeepalive(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamKeepaliveInterval: 1,
		MaxLineSize:             defaultMaxLineSize,
	}}}
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = pw.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stalled\"}}\n\n"))
		_, _ = pw.Write([]byte("data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_stalled\"}}\n\n"))
		time.Sleep(2100 * time.Millisecond)
	}()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: pr}

	_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Contains(t, rec.Body.String(), ":\n\n")
	require.NotContains(t, rec.Body.String(), "response.created")
	require.NotContains(t, rec.Body.String(), "response.in_progress")
}

func TestOpenAINativeFirstOutputFailoverKeepsAttemptHeadersPrivateAfterKeepaliveCommit(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{
		OpenAIFirstOutputTimeoutSeconds: 2,
		StreamKeepaliveInterval:         1,
		MaxLineSize:                     defaultMaxLineSize,
	}}
	svc := &OpenAIGatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	firstBody, firstWriter := io.Pipe()
	trackedFirstBody := &firstOutputCloseTrackingBody{ReadCloser: firstBody, closed: make(chan struct{})}
	firstWriterDone := make(chan struct{})
	go func() {
		defer close(firstWriterDone)
		defer func() { _ = firstWriter.Close() }()
		_, _ = firstWriter.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_first\"}}\n\n"))
		select {
		case <-trackedFirstBody.closed:
		case <-time.After(4 * time.Second):
		}
	}()
	firstResp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":                   []string{"text/event-stream"},
			"X-Request-Id":                   []string{"request-first"},
			"X-Ratelimit-Remaining-Requests": []string{"1"},
		},
		Body: trackedFirstBody,
	}

	_, firstErr := svc.handleStreamingResponse(c.Request.Context(), firstResp, c, &Account{ID: 1, Platform: PlatformOpenAI}, time.Now(), "model", "model")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, firstErr, &failoverErr)
	require.Contains(t, rec.Body.String(), ":\n\n", "first attempt should have committed only a stable keepalive")
	require.NotContains(t, rec.Body.String(), "resp_first")

	secondResp := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type":                   []string{"text/event-stream"},
			"X-Request-Id":                   []string{"request-second"},
			"X-Ratelimit-Remaining-Requests": []string{"99"},
		},
		Body: io.NopCloser(strings.NewReader(strings.Join([]string{
			`data: {"type":"response.output_text.delta","delta":"hello"}`,
			"",
			`data: {"type":"response.completed","response":{"id":"resp_second","usage":{"input_tokens":1,"output_tokens":1}}}`,
			"",
		}, "\n"))),
	}
	result, secondErr := svc.handleStreamingResponse(c.Request.Context(), secondResp, c, &Account{ID: 2, Platform: PlatformOpenAI}, time.Now(), "model", "model")

	require.NoError(t, secondErr)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "resp_second")
	wireHeaders := rec.Result().Header
	require.Empty(t, wireHeaders.Values("X-Request-Id"))
	require.Empty(t, wireHeaders.Values("X-Ratelimit-Remaining-Requests"))
	require.Empty(t, rec.Header().Values("X-Request-Id"))
	require.Empty(t, rec.Header().Values("X-Ratelimit-Remaining-Requests"))
	select {
	case <-firstWriterDone:
	case <-time.After(time.Second):
		t.Fatal("first account writer did not exit after timeout")
	}
}

type firstOutputTimeoutTestSettingRepo struct {
	SettingRepository
	mu     sync.RWMutex
	values map[string]string
}

func (r *firstOutputTimeoutTestSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.RLock()
	value, ok := r.values[key]
	r.mu.RUnlock()
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *firstOutputTimeoutTestSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *firstOutputTimeoutTestSettingRepo) setValue(key, value string) {
	r.mu.Lock()
	r.values[key] = value
	r.mu.Unlock()
}

var _ SettingRepository = (*firstOutputTimeoutTestSettingRepo)(nil)

type firstOutputTimeoutTestTempUnschedCall struct {
	accountID  int64
	until      time.Time
	reason     string
	recordedAt time.Time
}

type firstOutputTimeoutTestAccountRepo struct {
	AccountRepository
	tempUnschedCalls []firstOutputTimeoutTestTempUnschedCall
}

func (r *firstOutputTimeoutTestAccountRepo) SetTempUnschedulable(_ context.Context, accountID int64, until time.Time, reason string) error {
	r.tempUnschedCalls = append(r.tempUnschedCalls, firstOutputTimeoutTestTempUnschedCall{
		accountID: accountID, until: until, reason: reason, recordedAt: time.Now(),
	})
	return nil
}

type firstOutputTimeoutTestCounterIncrement struct {
	accountID     int64
	windowMinutes int
	count         int64
}

type firstOutputTimeoutTestCounterCache struct {
	mu         sync.Mutex
	counts     map[int64]int64
	increments []firstOutputTimeoutTestCounterIncrement
	resets     []int64
}

func (c *firstOutputTimeoutTestCounterCache) IncrementTimeoutCount(_ context.Context, accountID int64, windowMinutes int) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[int64]int64)
	}
	c.counts[accountID]++
	count := c.counts[accountID]
	c.increments = append(c.increments, firstOutputTimeoutTestCounterIncrement{
		accountID: accountID, windowMinutes: windowMinutes, count: count,
	})
	return count, nil
}

func (c *firstOutputTimeoutTestCounterCache) GetTimeoutCount(_ context.Context, accountID int64) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[accountID], nil
}

func (c *firstOutputTimeoutTestCounterCache) ResetTimeoutCount(_ context.Context, accountID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.counts, accountID)
	c.resets = append(c.resets, accountID)
	return nil
}

func (c *firstOutputTimeoutTestCounterCache) GetTimeoutCountTTL(context.Context, int64) (time.Duration, error) {
	return 0, nil
}

var _ TimeoutCounterCache = (*firstOutputTimeoutTestCounterCache)(nil)

const firstOutputTimeoutTestStreamSettings = `{"enabled":true,"action":"temp_unsched","temp_unsched_minutes":5,"threshold_count":3,"threshold_window_minutes":10}`

func newFirstOutputTimeoutTestGateway(
	cfg *config.Config,
	extraSettings map[string]string,
) (*OpenAIGatewayService, *firstOutputTimeoutTestSettingRepo, *firstOutputTimeoutTestAccountRepo, *firstOutputTimeoutTestCounterCache) {
	values := map[string]string{SettingKeyStreamTimeoutSettings: firstOutputTimeoutTestStreamSettings}
	for key, value := range extraSettings {
		values[key] = value
	}
	settingRepo := &firstOutputTimeoutTestSettingRepo{values: values}
	accountRepo := &firstOutputTimeoutTestAccountRepo{}
	counter := &firstOutputTimeoutTestCounterCache{}
	rateLimitService := &RateLimitService{
		accountRepo:    accountRepo,
		cfg:            cfg,
		settingService: NewSettingService(settingRepo, cfg),
	}
	rateLimitService.SetTimeoutCounterCache(counter)
	return &OpenAIGatewayService{cfg: cfg, rateLimitService: rateLimitService}, settingRepo, accountRepo, counter
}

func newFirstOutputTimeoutTestRequestContext() *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func TestSmartFirstOutputTimeoutSkipsStreamTimeoutPenalty(t *testing.T) {
	cfg := &config.Config{}
	svc, _, accountRepo, counter := newFirstOutputTimeoutTestGateway(cfg, map[string]string{
		SettingKeySmartSchedulingEnabled: "true",
	})
	account := &Account{ID: 731, Name: "fixture-account", Platform: PlatformOpenAI}
	attempts := []struct {
		name   string
		effort string
		phase  string
	}{
		{name: "high_response_headers_one", effort: "high", phase: "response_headers"},
		{name: "xhigh_response_headers_two", effort: "xhigh", phase: "response_headers"},
		{name: "high_response_headers_three", effort: "high", phase: "response_headers"},
		{name: "xhigh_semantic_output", effort: "xhigh", phase: "semantic_output"},
	}
	for _, attempt := range attempts {
		t.Run(attempt.name, func(t *testing.T) {
			c := newFirstOutputTimeoutTestRequestContext()
			c.Set(openAIFirstOutputSettingsContextKey, openAIAdvancedSchedulerRuntimeSettings{
				smartEnabled: true, smartEnabledKnown: true, firstOutputTimeoutSeconds: 30,
			})
			headers := http.Header{"X-Fixture-Header": []string{"kept"}}
			failoverErr := svc.newOpenAIFirstOutputTimeoutError(
				context.Background(), c, account, nil, "", time.Now(), "fixture-model",
				attempt.effort, 30*time.Second, attempt.phase, headers,
			)

			require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
			require.Contains(t, string(failoverErr.ResponseBody), "first_output_timeout")
			require.True(t, failoverErr.SafeToFailoverAfterWrite)
			require.Equal(t, headers, failoverErr.ResponseHeaders)
			raw, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			events, ok := raw.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, events, 1)
			require.Equal(t, "first_output_timeout", events[0].Kind)
			require.Equal(t, account.ID, events[0].AccountID)
			require.Equal(t, http.StatusGatewayTimeout, events[0].UpstreamStatusCode)
			require.Contains(t, events[0].Detail, "phase="+attempt.phase)
		})
	}
	require.Empty(t, counter.increments, "smart first-output failures must not touch the stream timeout counter")
	require.Empty(t, accountRepo.tempUnschedCalls, "smart first-output failures must not trigger stream timeout quarantine")
}

func TestSmartFirstOutputTimeoutSnapshotAndLegacyPenalty(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

	t.Run("smart_disabled_missing_and_invalid_snapshots_keep_legacy_accounting", func(t *testing.T) {
		cfg := &config.Config{Gateway: config.GatewayConfig{
			OpenAIFirstOutputTimeoutSeconds:           30,
			OpenAIHighEffortFirstOutputTimeoutSeconds: 300,
		}}
		svc, _, accountRepo, counter := newFirstOutputTimeoutTestGateway(cfg, map[string]string{
			SettingKeySmartSchedulingEnabled:          "false",
			SettingKeyOpenAIFirstOutputTimeoutSeconds: "0",
		})
		account := &Account{ID: 732, Name: "fixture-account", Platform: PlatformOpenAI}

		disabled := newFirstOutputTimeoutTestRequestContext()
		disabled.Set(openAIFirstOutputSettingsContextKey, openAIAdvancedSchedulerRuntimeSettings{smartEnabledKnown: true})
		require.Equal(t, 300*time.Second, svc.openAIFirstOutputTimeoutForRequest(context.Background(), disabled, "xhigh"),
			"smart-off keeps the explicit high-effort YAML override")
		_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), disabled, account, nil, "", time.Now(), "fixture-model", "xhigh", 300*time.Second, "response_headers", nil)

		missing := newFirstOutputTimeoutTestRequestContext()
		_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), missing, account, nil, "", time.Now(), "fixture-model", "high", 30*time.Second, "response_headers", nil)

		invalid := newFirstOutputTimeoutTestRequestContext()
		invalid.Set(openAIFirstOutputSettingsContextKey, "invalid-snapshot")
		_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), invalid, account, nil, "", time.Now(), "fixture-model", "high", 30*time.Second, "response_headers", nil)

		require.Equal(t, []int64{1, 2, 3}, firstOutputTimeoutTestIncrementCounts(counter))
		require.Len(t, accountRepo.tempUnschedCalls, 1, "the third legacy timeout should retain the configured threshold action")
		require.Contains(t, accountRepo.tempUnschedCalls[0].reason, "Stream data interval timeout for model: fixture-model")
	})

	t.Run("request_snapshot_survives_a_scheduler_cache_flip", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		cfg := &config.Config{Gateway: config.GatewayConfig{
			OpenAIFirstOutputTimeoutSeconds:           30,
			OpenAIHighEffortFirstOutputTimeoutSeconds: 300,
		}}
		svc, settingRepo, accountRepo, counter := newFirstOutputTimeoutTestGateway(cfg, map[string]string{
			SettingKeySmartSchedulingEnabled:          "true",
			SettingKeyOpenAIFirstOutputTimeoutSeconds: "30",
		})
		account := &Account{ID: 733, Name: "fixture-account", Platform: PlatformOpenAI}
		inFlight := newFirstOutputTimeoutTestRequestContext()
		require.Equal(t, 30*time.Second, svc.openAIFirstOutputTimeoutForRequest(context.Background(), inFlight, "xhigh"))

		settingRepo.setValue(SettingKeySmartSchedulingEnabled, "false")
		settingRepo.setValue(SettingKeyOpenAIFirstOutputTimeoutSeconds, "0")
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), inFlight, account, nil, "", time.Now(), "fixture-model", "xhigh", 30*time.Second, "response_headers", nil)
		require.Empty(t, counter.increments, "the in-flight request keeps its smart-enabled snapshot")
		require.Empty(t, accountRepo.tempUnschedCalls)

		fresh := newFirstOutputTimeoutTestRequestContext()
		require.Equal(t, 300*time.Second, svc.openAIFirstOutputTimeoutForRequest(context.Background(), fresh, "xhigh"),
			"a fresh smart-off request uses the current high-effort YAML override")
		_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), fresh, account, nil, "", time.Now(), "fixture-model", "xhigh", 300*time.Second, "response_headers", nil)
		require.Equal(t, []int64{1}, firstOutputTimeoutTestIncrementCounts(counter))
	})
}

func TestSmartFirstOutputTimeoutKeepsStreamIdlePenalty(t *testing.T) {
	for _, smartEnabled := range []bool{true, false} {
		mode := "smart_disabled"
		if smartEnabled {
			mode = "smart_enabled"
		}
		t.Run(mode, func(t *testing.T) {
			extraSettings := map[string]string{
				SettingKeySmartSchedulingEnabled: "false",
			}
			if smartEnabled {
				extraSettings[SettingKeySmartSchedulingEnabled] = "true"
			}
			svc, _, accountRepo, counter := newFirstOutputTimeoutTestGateway(&config.Config{}, extraSettings)
			account := &Account{ID: 734, Name: "fixture-account", Platform: PlatformOpenAI}

			if smartEnabled {
				for range 3 {
					c := newFirstOutputTimeoutTestRequestContext()
					c.Set(openAIFirstOutputSettingsContextKey, openAIAdvancedSchedulerRuntimeSettings{smartEnabled: true})
					_ = svc.newOpenAIFirstOutputTimeoutError(context.Background(), c, account, nil, "", time.Now(), "fixture-model", "xhigh", 30*time.Second, "response_headers", nil)
				}
				require.Empty(t, counter.increments, "smart first-output events do not seed the real idle threshold")
			}

			before := time.Now()
			for i := 0; i < 3; i++ {
				stopped := svc.rateLimitService.HandleStreamTimeout(context.Background(), account, "fixture-model")
				require.Equal(t, i == 2, stopped, "only the third genuine idle event reaches the threshold")
			}
			require.Equal(t, []int64{1, 2, 3}, firstOutputTimeoutTestIncrementCounts(counter))
			require.Len(t, counter.resets, 1)
			require.Len(t, accountRepo.tempUnschedCalls, 1)
			call := accountRepo.tempUnschedCalls[0]
			require.Equal(t, account.ID, call.accountID)
			require.Contains(t, call.reason, "Stream data interval timeout for model: fixture-model")
			require.WithinDuration(t, call.recordedAt.Add(5*time.Minute), call.until, time.Second)
			require.GreaterOrEqual(t, call.recordedAt, before)
		})
	}
}

func firstOutputTimeoutTestIncrementCounts(counter *firstOutputTimeoutTestCounterCache) []int64 {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counts := make([]int64, len(counter.increments))
	for i, increment := range counter.increments {
		counts[i] = increment.count
	}
	return counts
}
