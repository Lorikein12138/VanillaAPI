//go:build unit

package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Upstream SSE that returns thought summaries ("thought": true) before the answer,
// as antigravity does even when includeThoughts was not requested.
const geminiThoughtSSE = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Thinking A. ","thought":true}]}}]}` + "\n\n" +
	`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Thinking B.","thought":true}]}}]}` + "\n\n" +
	`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}]}` + "\n\n" +
	`data: {"candidates":[{"content":{"role":"model","parts":[{"text":" world","thoughtSignature":"sig"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2}}` + "\n\n" +
	"data: [DONE]\n\n"

func newGeminiSSEResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestCollectGeminiSSE_KeepsThoughtSeparateFromText(t *testing.T) {
	collected, _, err := collectGeminiSSE(strings.NewReader(geminiThoughtSSE), false)
	require.NoError(t, err)

	parts := extractGeminiParts(collected)
	require.Len(t, parts, 2)
	require.Equal(t, "Thinking A. Thinking B.", parts[0]["text"])
	require.Equal(t, true, parts[0]["thought"])
	require.Equal(t, "Hello world", parts[1]["text"])
	require.Nil(t, parts[1]["thought"])
	require.Equal(t, "sig", parts[1]["thoughtSignature"])
}

func TestMergeCollectedTextParts_ThoughtOnly(t *testing.T) {
	resp := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{
		"parts": []any{map[string]any{"text": "b", "thought": true}},
	}}}}
	merged := mergeCollectedTextParts(resp, []string{"a", "b"}, nil)
	parts := extractGeminiParts(merged)
	require.Len(t, parts, 1)
	require.Equal(t, "ab", parts[0]["text"])
	require.Equal(t, true, parts[0]["thought"])
}

func TestGeminiResponseToChatCompletions_ThoughtBecomesReasoningContent(t *testing.T) {
	collected, _, err := collectGeminiSSE(strings.NewReader(geminiThoughtSSE), false)
	require.NoError(t, err)
	raw, err := json.Marshal(collected)
	require.NoError(t, err)

	chatResp, _, err := geminiResponseToChatCompletions(collected, "gemini-3-pro", raw, nil)
	require.NoError(t, err)
	require.Len(t, chatResp.Choices, 1)

	msg, err := json.Marshal(chatResp.Choices[0].Message)
	require.NoError(t, err)
	var got struct {
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
	}
	require.NoError(t, json.Unmarshal(msg, &got))
	require.Equal(t, "Hello world", got.Content)
	require.Equal(t, "Thinking A. Thinking B.", got.ReasoningContent)
}

func TestConvertGeminiToClaudeMessage_ThoughtBecomesThinkingBlock(t *testing.T) {
	resp := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{
		"parts": []any{
			map[string]any{"text": "plan", "thought": true},
			map[string]any{"text": "answer"},
		},
	}}}}
	claudeResp, _ := convertGeminiToClaudeMessage(resp, "gemini-3-pro", nil, false)
	content, ok := claudeResp["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 2)
	require.Equal(t, map[string]any{"type": "thinking", "thinking": "plan"}, content[0])
	require.Equal(t, map[string]any{"type": "text", "text": "answer"}, content[1])
}

func TestChatCompletionsStreamFromGemini_ThoughtStreamsAsReasoningContent(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	svc := &GeminiMessagesCompatService{}
	_, err := svc.handleChatCompletionsStreamingResponseFromGemini(c, newGeminiSSEResponse(geminiThoughtSSE), time.Now(), "gemini-3-pro", false, false)
	require.NoError(t, err)

	var reasoning, content strings.Builder
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		payload = strings.TrimSpace(payload)
		if !ok || payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          *string `json:"content"`
					ReasoningContent *string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk), payload)
		for _, ch := range chunk.Choices {
			if ch.Delta.ReasoningContent != nil {
				reasoning.WriteString(*ch.Delta.ReasoningContent)
			}
			if ch.Delta.Content != nil {
				content.WriteString(*ch.Delta.Content)
			}
		}
	}
	require.Equal(t, "Thinking A. Thinking B.", reasoning.String())
	require.Equal(t, "Hello world", content.String())
}

func TestGeminiMessagesHandleStreamingResponse_ThoughtStreamsAsThinkingBlock(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)

	svc := &GeminiMessagesCompatService{}
	_, err := svc.handleStreamingResponse(c, newGeminiSSEResponse(geminiThoughtSSE), time.Now(), "claude-sonnet-4-5")
	require.NoError(t, err)

	body := rec.Body.String()
	events := parseAnthropicContentBlockEvents(t, body)
	var starts []string
	for _, ev := range events {
		if ev.event == "content_block_start" {
			starts = append(starts, ev.blockType)
		}
	}
	require.Equal(t, []string{"thinking", "text"}, starts)
	require.Contains(t, body, `"thinking":"Thinking A. "`)
	require.Contains(t, body, `"type":"thinking_delta"`)
	require.Contains(t, body, `"text":"Hello"`)
	require.NotContains(t, body, `"text":"Thinking`)
}

func TestConvertClaudeMessagesToGeminiContents_SkipsThinkingBlocks(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "secret plan", "signature": ""},
			map[string]any{"type": "text", "text": "hello"},
		}},
		map[string]any{"role": "user", "content": "again"},
	}
	raw, err := json.Marshal(messages)
	require.NoError(t, err)

	contents, err := convertClaudeMessagesToGeminiContents(messages, map[string]string{})
	require.NoError(t, err, string(raw))
	out, err := json.Marshal(contents)
	require.NoError(t, err)
	require.NotContains(t, string(out), "secret plan")
	require.Contains(t, string(out), "hello")
}

func TestProcessGeminiStream_SkipsThoughtParts(t *testing.T) {
	ctx, recorder := newTestContext()
	svc := &AccountTestService{}

	err := svc.processGeminiStream(ctx, strings.NewReader(geminiThoughtSSE))
	require.NoError(t, err)

	body := recorder.Body.String()
	require.NotContains(t, body, "Thinking")
	require.Contains(t, body, `"text":"Hello"`)
	require.Contains(t, body, `"text":" world"`)
}
