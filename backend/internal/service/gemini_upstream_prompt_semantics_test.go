//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiChatCompat_UsesUpstreamSystemRoles(t *testing.T) {
	suffixMessages := []any{
		map[string]any{"role": "system", "content": "persona"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "partial"},
		map[string]any{"role": "system", "content": "post1"},
		map[string]any{"role": "developer", "content": "post2"},
	}

	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", accountType, stream), func(t *testing.T) {
				upstreamStream := stream || accountType == AccountTypeOAuth
				svc, account, upstream := newGeminiPromptSemanticsHarness(accountType, upstreamStream, http.StatusOK)
				body := geminiPromptChatBody(t, stream, suffixMessages)
				ctx, recorder := newGeminiPromptSemanticsContext(http.MethodPost, "/v1/chat/completions", body)

				result, err := svc.ForwardAsChatCompletions(context.Background(), ctx, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, 1, upstream.calls)

				outer, request := capturedGeminiPromptRequest(t, upstream)
				requireGeminiPromptAuth(t, accountType, "gemini-2.5-flash", upstream, outer)
				require.Equal(t, []string{"persona", "post1", "post2"}, strings.Fields(geminiPromptSystemText(t, request)))
				contents := geminiPromptContents(t, request)
				require.Equal(t, []string{"user", "model"}, geminiPromptRoles(contents))
				require.Equal(t, "hello", geminiPromptContentText(t, contents[0]))
				require.Equal(t, "partial", geminiPromptContentText(t, contents[1]))
			})
		}
	}

	t.Run("leading-and-middle-system-with-user-ending", func(t *testing.T) {
		svc, account, upstream := newGeminiPromptSemanticsHarness(AccountTypeAPIKey, false, http.StatusOK)
		messages := []any{
			map[string]any{"role": "system", "content": "leading"},
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "developer", "content": "middle"},
			map[string]any{"role": "user", "content": "last"},
		}
		body := geminiPromptChatBody(t, false, messages)
		ctx, recorder := newGeminiPromptSemanticsContext(http.MethodPost, "/v1/chat/completions", body)

		result, err := svc.ForwardAsChatCompletions(context.Background(), ctx, account, body)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, 1, upstream.calls)

		outer, request := capturedGeminiPromptRequest(t, upstream)
		requireGeminiPromptAuth(t, AccountTypeAPIKey, "gemini-2.5-flash", upstream, outer)
		require.Equal(t, []string{"leading", "middle"}, strings.Fields(geminiPromptSystemText(t, request)))
		contents := geminiPromptContents(t, request)
		require.Equal(t, []string{"user"}, geminiPromptRoles(contents))
		userText := geminiPromptContentText(t, contents[0])
		require.Contains(t, userText, "first")
		require.Contains(t, userText, "last")
		require.Less(t, strings.Index(userText, "first"), strings.Index(userText, "last"))
	})
}

func TestGeminiChatCompat_PreservesAssistantPrefill(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", accountType, stream), func(t *testing.T) {
				upstreamStream := stream || accountType == AccountTypeOAuth
				svc, account, upstream := newGeminiPromptSemanticsHarness(accountType, upstreamStream, http.StatusOK)
				messages := []any{
					map[string]any{"role": "system", "content": "persona"},
					map[string]any{"role": "user", "content": "hello"},
					map[string]any{"role": "assistant", "content": "Once upon a"},
				}
				body := geminiPromptChatBody(t, stream, messages)
				ctx, recorder := newGeminiPromptSemanticsContext(http.MethodPost, "/v1/chat/completions", body)

				result, err := svc.ForwardAsChatCompletions(context.Background(), ctx, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, 1, upstream.calls)

				outer, request := capturedGeminiPromptRequest(t, upstream)
				requireGeminiPromptAuth(t, accountType, "gemini-2.5-flash", upstream, outer)
				require.Equal(t, []string{"persona"}, strings.Fields(geminiPromptSystemText(t, request)))
				contents := geminiPromptContents(t, request)
				require.Equal(t, []string{"user", "model"}, geminiPromptRoles(contents))
				require.Equal(t, "hello", geminiPromptContentText(t, contents[0]))
				require.Equal(t, "Once upon a", geminiPromptContentText(t, contents[1]))
			})
		}
	}
}

func TestConvertClaudeMessagesToGeminiGenerateContent_PreservesUpstreamTurns(t *testing.T) {
	t.Run("assistant text prefill", func(t *testing.T) {
		request := convertPromptSemanticsClaudeBody(t, []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "Once upon a"},
		})
		contents := geminiPromptContents(t, request)
		require.Equal(t, []string{"user", "model"}, geminiPromptRoles(contents))
		require.Equal(t, "Once upon a", geminiPromptContentText(t, contents[1]))
	})

	t.Run("assistant function call remains a model ending", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "look up Paris"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "I will look that up."},
				map[string]any{
					"type":      "tool_use",
					"id":        "toolu_lookup_1",
					"name":      "lookup",
					"input":     map[string]any{"city": "Paris"},
					"signature": "sig-lookup-1",
				},
			}},
		}}
		converted, err := convertClaudeMessagesToGeminiGenerateContent(geminiPromptJSON(t, body))
		require.NoError(t, err)
		require.NotContains(t, string(converted), "toolu_lookup_1")
		var request map[string]any
		require.NoError(t, json.Unmarshal(converted, &request))
		contents := geminiPromptContents(t, request)
		require.Equal(t, []string{"user", "model"}, geminiPromptRoles(contents))
		parts := geminiPromptParts(t, contents[1])
		require.Len(t, parts, 2)
		require.Equal(t, "I will look that up.", parts[0]["text"])
		functionCall, ok := parts[1]["functionCall"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "lookup", functionCall["name"])
		require.Equal(t, map[string]any{"city": "Paris"}, functionCall["args"])
		require.Equal(t, "sig-lookup-1", parts[1]["thoughtSignature"])
		require.NotContains(t, functionCall, "id")
	})

	t.Run("tool response keeps mapped name and removes call id", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_lookup_2", "name": "lookup", "input": map[string]any{"city": "Paris"}, "signature": "sig-lookup-2"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_lookup_2", "content": []any{map[string]any{"type": "text", "text": "found"}}},
			}},
		}}
		converted, err := convertClaudeMessagesToGeminiGenerateContent(geminiPromptJSON(t, body))
		require.NoError(t, err)
		require.NotContains(t, string(converted), "toolu_lookup_2")
		var request map[string]any
		require.NoError(t, json.Unmarshal(converted, &request))
		contents := geminiPromptContents(t, request)
		require.Equal(t, []string{"model", "user"}, geminiPromptRoles(contents))
		functionResponse, ok := geminiPromptParts(t, contents[1])[0]["functionResponse"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "lookup", functionResponse["name"])
		require.Equal(t, map[string]any{"content": "found"}, functionResponse["response"])
		require.NotContains(t, functionResponse, "id")
	})

	t.Run("ordinary user ending", func(t *testing.T) {
		request := convertPromptSemanticsClaudeBody(t, []any{map[string]any{"role": "user", "content": "done"}})
		contents := geminiPromptContents(t, request)
		require.Equal(t, []string{"user"}, geminiPromptRoles(contents))
		require.Equal(t, "done", geminiPromptContentText(t, contents[0]))
	})

	t.Run("empty messages stay empty", func(t *testing.T) {
		request := convertPromptSemanticsClaudeBody(t, []any{})
		require.Empty(t, geminiPromptContents(t, request))
	})
}

func TestGeminiMessagesCompat_PreservesModelEndingRequest(t *testing.T) {
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeOAuth} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", accountType, stream), func(t *testing.T) {
				upstreamStream := stream || accountType == AccountTypeOAuth
				svc, account, upstream := newGeminiPromptSemanticsHarness(accountType, upstreamStream, http.StatusOK)
				body := geminiPromptMessagesBody(t, stream, []any{
					map[string]any{"role": "user", "content": "hello"},
					map[string]any{"role": "assistant", "content": "Once upon a"},
				})
				ctx, recorder := newGeminiPromptSemanticsContext(http.MethodPost, "/v1/messages", body)

				result, err := svc.Forward(context.Background(), ctx, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Equal(t, 1, upstream.calls)

				outer, request := capturedGeminiPromptRequest(t, upstream)
				requireGeminiPromptAuth(t, accountType, "claude-sonnet-4-20250514", upstream, outer)
				require.Equal(t, []string{"messages", "system"}, strings.Fields(geminiPromptSystemText(t, request)))
				contents := geminiPromptContents(t, request)
				require.Equal(t, []string{"user", "model"}, geminiPromptRoles(contents))
				require.Equal(t, "hello", geminiPromptContentText(t, contents[0]))
				require.Equal(t, "Once upon a", geminiPromptContentText(t, contents[1]))
			})
		}
	}
}

func TestAntigravityCompatGemini_PreservesModelEndingRequest(t *testing.T) {
	svc := &AntigravityGatewayService{}
	tests := []struct {
		name     string
		messages []any
		wantTail string
	}{
		{
			name: "assistant text prefill",
			messages: []any{
				map[string]any{"role": "user", "content": "hello"},
				map[string]any{"role": "assistant", "content": "Once upon a"},
			},
			wantTail: "Once upon a",
		},
		{
			name: "assistant function call",
			messages: []any{
				map[string]any{"role": "assistant", "content": []any{
					map[string]any{"type": "tool_use", "id": "ant-tool-call", "name": "lookup", "input": map[string]any{"city": "Paris"}, "signature": "ant-sig"},
				}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claudeBody := map[string]any{
				"system":   "client system",
				"messages": tt.messages,
				"tools": []any{
					map[string]any{"name": "lookup", "description": "Look up a city", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}}},
					map[string]any{"type": "web_search_20250305", "name": "web_search"},
				},
			}
			body, err := svc.buildAntigravityCompatGeminiBody(context.Background(), geminiPromptJSON(t, claudeBody), nil, "project-prompt-test", "gemini-3.1-pro-high")
			require.NoError(t, err)
			var wrapped map[string]any
			require.NoError(t, json.Unmarshal(body, &wrapped))
			require.Equal(t, "project-prompt-test", wrapped["project"])
			require.Equal(t, "gemini-3.1-pro-high", wrapped["model"])
			require.Equal(t, "antigravity", wrapped["userAgent"])
			request, ok := wrapped["request"].(map[string]any)
			require.True(t, ok)
			require.Contains(t, geminiPromptSystemText(t, request), "You are Antigravity")
			require.Contains(t, geminiPromptSystemText(t, request), "client system")

			contents := geminiPromptContents(t, request)
			roles := geminiPromptRoles(contents)
			if tt.wantTail != "" {
				require.Equal(t, []string{"user", "model"}, roles)
				require.Equal(t, tt.wantTail, geminiPromptContentText(t, contents[1]))
			} else {
				require.Equal(t, []string{"model"}, roles)
				parts := geminiPromptParts(t, contents[0])
				call, ok := parts[len(parts)-1]["functionCall"].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "lookup", call["name"])
				require.Equal(t, map[string]any{"city": "Paris"}, call["args"])
			}

			tools, ok := request["tools"].([]any)
			require.True(t, ok)
			require.NotEmpty(t, tools)
			hasFunctionDeclarations, hasGoogleSearch := false, false
			for _, rawTool := range tools {
				tool, ok := rawTool.(map[string]any)
				if !ok {
					continue
				}
				if declarations, ok := tool["functionDeclarations"].([]any); ok && len(declarations) > 0 {
					hasFunctionDeclarations = true
				}
				if _, ok := tool["googleSearch"]; ok {
					hasGoogleSearch = true
				}
			}
			require.True(t, hasFunctionDeclarations)
			require.False(t, hasGoogleSearch)
		})
	}
}

func TestGeminiCompat_ModelEnding400UsesExistingErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		stream bool
		chat   bool
	}{
		{name: "chat/nonstream", chat: true},
		{name: "chat/stream", stream: true, chat: true},
		{name: "messages/nonstream"},
		{name: "messages/stream", stream: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			svc, account, upstream := newGeminiPromptSemanticsHarness(AccountTypeAPIKey, tt.stream, http.StatusBadRequest)
			if tt.chat {
				body := geminiPromptChatBody(t, tt.stream, []any{
					map[string]any{"role": "user", "content": "hello"},
					map[string]any{"role": "assistant", "content": "Once upon a"},
				})
				ctx, recorder := newGeminiPromptSemanticsContext(http.MethodPost, "/v1/chat/completions", body)
				result, err := svc.ForwardAsChatCompletions(context.Background(), ctx, account, body)
				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Contains(t, recorder.Body.String(), "Requests ending with a model turn are not supported")
			} else {
				body := geminiPromptMessagesBody(t, tt.stream, []any{
					map[string]any{"role": "user", "content": "hello"},
					map[string]any{"role": "assistant", "content": "Once upon a"},
				})
				ctx, recorder := newGeminiPromptSemanticsContext(http.MethodPost, "/v1/messages", body)
				result, err := svc.Forward(context.Background(), ctx, account, body)
				require.Error(t, err)
				require.Nil(t, result)
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Contains(t, recorder.Body.String(), "Requests ending with a model turn are not supported")
			}

			require.Equal(t, 1, upstream.calls)
			outer, request := capturedGeminiPromptRequest(t, upstream)
			requireGeminiPromptAuth(t, AccountTypeAPIKey, "", upstream, outer)
			contents := geminiPromptContents(t, request)
			require.Equal(t, []string{"user", "model"}, geminiPromptRoles(contents))
			require.Equal(t, "Once upon a", geminiPromptContentText(t, contents[1]))
			if tt.stream {
				require.Contains(t, upstream.lastReq.URL.String(), ":streamGenerateContent")
			} else {
				require.Contains(t, upstream.lastReq.URL.String(), ":generateContent")
				require.NotContains(t, upstream.lastReq.URL.String(), ":streamGenerateContent")
			}
		})
	}
}

func newGeminiPromptSemanticsHarness(accountType string, upstreamStream bool, status int) (*GeminiMessagesCompatService, *Account, *geminiCompatHTTPUpstreamStub) {
	credentials := map[string]any{}
	if accountType == AccountTypeOAuth {
		credentials["access_token"] = "prompt-semantics-access-token"
		credentials["project_id"] = "project-prompt-test"
	} else {
		credentials["api_key"] = "prompt-semantics-api-key"
	}
	account := &Account{
		ID:          401,
		Platform:    PlatformGemini,
		Type:        accountType,
		Credentials: credentials,
		Concurrency: 1,
	}
	upstream := &geminiCompatHTTPUpstreamStub{response: newGeminiPromptSemanticsResponse(status, upstreamStream, accountType == AccountTypeOAuth)}
	service := &GeminiMessagesCompatService{
		tokenProvider: &GeminiTokenProvider{},
		httpUpstream:  upstream,
		cfg:           &config.Config{},
	}
	return service, account, upstream
}

func newGeminiPromptSemanticsResponse(status int, upstreamStream, oauth bool) *http.Response {
	header := http.Header{"Content-Type": []string{"application/json"}}
	body := `{"error":{"code":400,"message":"Requests ending with a model turn are not supported","status":"INVALID_ARGUMENT"}}`
	if status < http.StatusBadRequest {
		payload := `{"candidates":[{"content":{"role":"model","parts":[{"text":"upstream-ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1}}`
		if upstreamStream {
			if oauth {
				payload = `{"response":` + payload + `}`
			}
			body = "data: " + payload + "\n\n" + "data: [DONE]\n\n"
			header.Set("Content-Type", "text/event-stream")
		} else {
			body = payload
		}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func geminiPromptChatBody(t *testing.T, stream bool, messages []any) []byte {
	t.Helper()
	return geminiPromptJSON(t, map[string]any{
		"model":    "gemini-2.5-flash",
		"stream":   stream,
		"messages": messages,
	})
}

func geminiPromptMessagesBody(t *testing.T, stream bool, messages []any) []byte {
	t.Helper()
	return geminiPromptJSON(t, map[string]any{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 64,
		"stream":     stream,
		"system":     "messages system",
		"messages":   messages,
	})
}

func geminiPromptJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	require.NoError(t, err)
	return body
}

func convertPromptSemanticsClaudeBody(t *testing.T, messages []any) map[string]any {
	t.Helper()
	converted, err := convertClaudeMessagesToGeminiGenerateContent(geminiPromptJSON(t, map[string]any{"messages": messages}))
	require.NoError(t, err)
	var request map[string]any
	require.NoError(t, json.Unmarshal(converted, &request))
	return request
}

func newGeminiPromptSemanticsContext(method, path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	return ctx, recorder
}

func capturedGeminiPromptRequest(t *testing.T, upstream *geminiCompatHTTPUpstreamStub) (map[string]any, map[string]any) {
	t.Helper()
	require.NotNil(t, upstream.lastReq)
	body, err := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, err)
	var outer map[string]any
	require.NoError(t, json.Unmarshal(body, &outer))
	if request, ok := outer["request"].(map[string]any); ok {
		return outer, request
	}
	return outer, outer
}

func requireGeminiPromptAuth(t *testing.T, accountType, expectedModel string, upstream *geminiCompatHTTPUpstreamStub, outer map[string]any) {
	t.Helper()
	require.NotNil(t, upstream.lastReq)
	if accountType == AccountTypeOAuth {
		require.Equal(t, "Bearer prompt-semantics-access-token", upstream.lastReq.Header.Get("Authorization"))
		require.Empty(t, upstream.lastReq.Header.Get("x-goog-api-key"))
		require.Equal(t, "project-prompt-test", outer["project"])
		if expectedModel != "" {
			require.Equal(t, expectedModel, outer["model"])
		}
		return
	}
	require.Equal(t, "prompt-semantics-api-key", upstream.lastReq.Header.Get("x-goog-api-key"))
	require.Empty(t, upstream.lastReq.Header.Get("Authorization"))
}

func geminiPromptContents(t *testing.T, request map[string]any) []map[string]any {
	t.Helper()
	raw, ok := request["contents"].([]any)
	require.True(t, ok)
	contents := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		content, ok := item.(map[string]any)
		require.True(t, ok)
		contents = append(contents, content)
	}
	return contents
}

func geminiPromptRoles(contents []map[string]any) []string {
	roles := make([]string, 0, len(contents))
	for _, content := range contents {
		role, _ := content["role"].(string)
		roles = append(roles, role)
	}
	return roles
}

func geminiPromptParts(t *testing.T, content map[string]any) []map[string]any {
	t.Helper()
	raw, ok := content["parts"].([]any)
	require.True(t, ok)
	parts := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		part, ok := item.(map[string]any)
		require.True(t, ok)
		parts = append(parts, part)
	}
	return parts
}

func geminiPromptContentText(t *testing.T, content map[string]any) string {
	t.Helper()
	var text strings.Builder
	for _, part := range geminiPromptParts(t, content) {
		if value, ok := part["text"].(string); ok {
			text.WriteString(value)
		}
	}
	return text.String()
}

func geminiPromptSystemText(t *testing.T, request map[string]any) string {
	t.Helper()
	system, ok := request["systemInstruction"].(map[string]any)
	require.True(t, ok)
	parts := geminiPromptParts(t, system)
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if text, ok := part["text"].(string); ok {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}
