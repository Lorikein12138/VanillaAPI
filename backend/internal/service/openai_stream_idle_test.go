package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIPassthroughIdleReplayKeepsKeepaliveAccounting(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamDataIntervalTimeout: 2, StreamKeepaliveInterval: 1,
	}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	for _, id := range []int64{1, 2} {
		reader, writer := io.Pipe()
		t.Cleanup(func() { _ = writer.Close() })
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
		account := &Account{ID: id, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

		_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
		require.True(t, failover.SafeToFailoverAfterWrite)
		require.False(t, failover.RetryableOnSameAccount)
		require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
	}
	require.GreaterOrEqual(t, strings.Count(rec.Body.String(), ": keepalive\n\n"), 2)
	require.Empty(t, strings.ReplaceAll(rec.Body.String(), ": keepalive\n\n", ""))
}

func TestOpenAIRawChatIdleAfterUpstreamKeepalive(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamDataIntervalTimeout: 1,
	}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reader, writer := io.Pipe()
	defer func() { _ = writer.Close() }()
	go func() { _, _ = io.WriteString(writer, ": upstream keepalive\n\n") }()
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	_, err := svc.streamRawChatCompletions(c, resp, account, "model", "model", "model", nil, nil, time.Now(), 0)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.True(t, failover.SafeToFailoverAfterWrite)
	require.Equal(t, ": upstream keepalive\n\n", rec.Body.String())
	require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
}
