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

func TestOpenAIForwardFirstOutputTimeoutIncludesResponseHeaderWait(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := &blockingOpenAIResponseHeaderUpstream{canceled: make(chan struct{})}
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			OpenAIFirstOutputTimeoutSeconds: 1,
			MaxLineSize:                     defaultMaxLineSize,
		}},
		httpUpstream: upstream,
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

func firstOutputTimeoutTestIncrementCounts(counter *firstOutputTimeoutTestCounterCache) []int64 {
	counter.mu.Lock()
	defer counter.mu.Unlock()
	counts := make([]int64, len(counter.increments))
	for i, increment := range counter.increments {
		counts[i] = increment.count
	}
	return counts
}

func TestOpenAIFirstOutputTimeoutRestoresStreamPenalty(t *testing.T) {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 30
	svc, _, repo, counter := newFirstOutputTimeoutTestGateway(cfg, map[string]string{
		"smart_scheduling_enabled": "true", "openai_first_output_timeout_seconds": "1",
	})
	account := &Account{ID: 88001, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.Equal(t, 30*time.Second, svc.openAIFirstOutputTimeout(""))
	for i := 0; i < 3; i++ {
		err := svc.newOpenAIFirstOutputTimeoutError(context.Background(), newFirstOutputTimeoutTestRequestContext(), account, nil, "direct", time.Now(), "gpt-test", "", 30*time.Second, "headers", http.Header{})
		require.Equal(t, http.StatusGatewayTimeout, err.StatusCode)
		require.True(t, err.SafeToFailoverAfterWrite)
	}
	require.Equal(t, []int64{1, 2, 3}, firstOutputTimeoutTestIncrementCounts(counter))
	require.Len(t, repo.tempUnschedCalls, 1)
	require.Equal(t, account.ID, repo.tempUnschedCalls[0].accountID)
	require.WithinDuration(t, time.Now().Add(5*time.Minute), repo.tempUnschedCalls[0].until, time.Second)
}

func TestOpenAIProductionFactoryKeepsOAuthRetryAndPublicFields(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 99401, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	public := http.Header{"Retry-After": []string{"2"}, "X-Request-Id": []string{"public-request"}}
	public.Set("x-codex-primary-used-percent", "100")
	body := []byte(`{"error":{"code":"insufficient_quota","message":"quota"}}`)
	before := time.Now()
	fo := svc.newOpenAIAccountFailoverErrorWithClassificationHeaders(account, 429, public, nil, body, "", false, false)
	require.True(t, fo.RetryableOnSameAccount)
	require.False(t, fo.RequestScopedTransient)
	require.WithinDuration(t, before.Add(openAIOAuth429RetryWindow), fo.SameAccountRetryDeadline, time.Second)
	require.InDelta(t, 2*time.Second, fo.SameAccountRetryDelay, float64(time.Second))
	require.Equal(t, public, fo.ResponseHeaders)
	require.Equal(t, body, fo.ResponseBody)
	require.Empty(t, fo.Scope)
	require.Zero(t, fo.ClientStatusCode)
	require.Empty(t, fo.ClientMessage)
}
