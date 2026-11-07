package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAccountTestService_OpenAIImageOAuthHandlesOutputItemDoneFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"ig_123\",\"type\":\"image_generation_call\",\"result\":\"aGVsbG8=\",\"revised_prompt\":\"draw a cat\",\"output_format\":\"png\"}}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"created_at\":1710000006,\"tool_usage\":{\"image_gen\":{\"images\":1}},\"output\":[]}}\n\n" +
					"data: [DONE]\n\n",
			)),
		},
	}
	svc := &AccountTestService{httpUpstream: upstream}
	account := &Account{
		ID:       53,
		Name:     "openai-oauth",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "token-123",
		},
	}

	err := svc.testOpenAIImageOAuth(c, context.Background(), account, "gpt-image-1", "draw a cat")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Contains(t, rec.Body.String(), "Calling Codex /responses image tool")
	require.Contains(t, rec.Body.String(), "data:image/png;base64,aGVsbG8=")
	require.Contains(t, rec.Body.String(), "\"success\":true")
}

func TestAccountTestService_OpenAIImageAPIKeyUsesConfiguredV1BaseURL(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"aGVsbG8=","revised_prompt":"draw a cat"}]}`)),
		},
	}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{},
	}
	account := &Account{
		ID:       54,
		Name:     "openai-apikey",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "test-api-key",
			"base_url": "https://image-upstream.example/v1",
		},
	}

	err := svc.testOpenAIImageAPIKey(c, context.Background(), account, "gpt-image-2", "draw a cat")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	require.Equal(t, HTTPUpstreamProfileOpenAI, HTTPUpstreamProfileFromContext(upstream.lastReq.Context()))
	require.Equal(t, "https://image-upstream.example/v1/images/generations", upstream.lastReq.URL.String())
	require.Equal(t, "Bearer test-api-key", upstream.lastReq.Header.Get("Authorization"))
	require.Contains(t, rec.Body.String(), "data:image/png;base64,aGVsbG8=")
	require.Contains(t, rec.Body.String(), "\"success\":true")
}

func runOpenAIImageAPIKeyTest(t *testing.T, responseBody string) (string, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)
	upstream := &httpUpstreamRecorder{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(responseBody)),
		},
	}
	svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
	account := &Account{
		ID:       56,
		Name:     "openai-apikey-url",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":  "test-api-key",
			"base_url": "https://image-upstream.example",
		},
	}
	err := svc.testOpenAIImageAPIKey(c, context.Background(), account, "gpt-image-2", "draw a cat")
	return rec.Body.String(), err
}

// ChatGPT Image 类上游忽略 response_format，只返回 url；测试弹窗也要能预览。
func TestAccountTestService_OpenAIImageAPIKeyPreviewsURLOnlyResponse(t *testing.T) {
	body, err := runOpenAIImageAPIKeyTest(t, `{"data":[{"url":"https://cdn.example/img.png?X-Amz-Expires=86400&sig=abc"}]}`)
	require.NoError(t, err)
	require.Contains(t, body, `"type":"image"`)
	require.Contains(t, body, `https://cdn.example/img.png?X-Amz-Expires=86400`)
	require.Contains(t, body, "\"success\":true")
}

// 既无 b64_json 也无 url 时不能报测试成功。
func TestAccountTestService_OpenAIImageAPIKeyFailsWhenNoImageData(t *testing.T) {
	body, err := runOpenAIImageAPIKeyTest(t, `{"data":[{"revised_prompt":"draw a cat"}]}`)
	require.Error(t, err)
	require.NotContains(t, body, `"type":"image"`)
	require.NotContains(t, body, "\"success\":true")
	require.Contains(t, body, "No image data")
}

func TestAccountTestService_OpenAIImageAPIKeyOmitsUnsupportedResponseFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, model := range []string{
		"gpt-image-1",
		"gpt-image-1-mini",
		"gpt-image-1.5",
		"gpt-image-2",
		"gpt-image-2-2026-04-21",
		"gpt-image-2.5-flare",
	} {
		t.Run(model, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/1/test", nil)

			upstream := &httpUpstreamRecorder{
				resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"data":[{"b64_json":"aGVsbG8="}]}`)),
				},
			}
			svc := &AccountTestService{httpUpstream: upstream, cfg: &config.Config{}}
			account := &Account{
				ID: 54, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "test-api-key"},
			}

			err := svc.testOpenAIAccountConnection(c, account, model, "draw a cat", AccountTestModeDefault)

			require.NoError(t, err)
			require.NotNil(t, upstream.lastReq)
			require.Equal(t, "https://api.openai.com/v1/images/generations", upstream.lastReq.URL.String())
			var payload map[string]any
			require.NoError(t, json.Unmarshal(upstream.lastBody, &payload))
			require.Equal(t, model, payload["model"])
			require.Equal(t, "draw a cat", payload["prompt"])
			require.Equal(t, float64(1), payload["n"])
			// GPT Image models always return base64 images and reject this legacy parameter.
			require.NotContains(t, payload, "response_format")
			require.Contains(t, rec.Body.String(), "data:image/png;base64,aGVsbG8=")
			require.Contains(t, rec.Body.String(), `"success":true`)
		})
	}
}
