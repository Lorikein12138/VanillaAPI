//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeminiToolResultImagesPreserveContinuationRules(t *testing.T) {
	request := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"inspect","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[
			{"type":"text","text":"found"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"d29ybGQ="}}
		]}]}
	]}`)
	converted, err := convertClaudeMessagesToGeminiGenerateContent(request)
	require.NoError(t, err)
	var got struct {
		Contents []struct {
			Role  string           `json:"role"`
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	require.NoError(t, json.Unmarshal(converted, &got))
	require.Len(t, got.Contents, 2)
	require.Equal(t, "user", got.Contents[1].Role)
	require.Len(t, got.Contents[1].Parts, 3)
	require.Contains(t, got.Contents[1].Parts[0], "functionResponse")
	require.Equal(t, "found", got.Contents[1].Parts[0]["functionResponse"].(map[string]any)["response"].(map[string]any)["content"])
	require.Equal(t, "image/png", got.Contents[1].Parts[1]["inlineData"].(map[string]any)["mimeType"])
	require.Equal(t, "image/jpeg", got.Contents[1].Parts[2]["inlineData"].(map[string]any)["mimeType"])
	require.NotContains(t, string(converted), geminiContinuationPrompt, "tool results end in a user turn and must not get a continuation nudge")

	prefill := []any{map[string]any{"role": "model", "parts": []any{map[string]any{"text": "partial"}}}}
	continued := appendGeminiContinuationTurn(prefill)
	require.Len(t, continued, 2, "an assistant prefill gets one continuation turn")
	require.Equal(t, geminiContinuationPrompt, continued[1].(map[string]any)["parts"].([]any)[0].(map[string]any)["text"])

	functionCall := []any{map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "inspect"}}}}}
	require.Len(t, appendGeminiContinuationTurn(functionCall), 1, "a trailing function call must wait for its function response")
}

func TestVertexChatThoughtSeparation(t *testing.T) {
	response := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{
		"role": "model",
		"parts": []any{
			map[string]any{"text": "private reasoning", "thought": true},
			map[string]any{"text": "visible answer"},
		},
	}}}}
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	converted, _, err := geminiResponseToChatCompletions(response, "vertex-gemini-model", raw, nil)
	require.NoError(t, err)
	require.Len(t, converted.Choices, 1)
	message, err := json.Marshal(converted.Choices[0].Message)
	require.NoError(t, err)
	var got struct {
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
	}
	require.NoError(t, json.Unmarshal(message, &got))
	require.Equal(t, "visible answer", got.Content)
	require.Equal(t, "private reasoning", got.ReasoningContent)
}
