//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeminiToolResultImagesPreserveTurnAndPartOrder(t *testing.T) {
	request := []byte(`{"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"inspect","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[
			{"type":"text","text":"found"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}},
			{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"d29ybGQ="}}
		]}]}
	]}`)
	converted, err := convertClaudeMessagesToGeminiGenerateContent(request, "gemini-2.5-flash")
	require.NoError(t, err)
	var got struct {
		Contents []struct {
			Role  string           `json:"role"`
			Parts []map[string]any `json:"parts"`
		} `json:"contents"`
	}
	require.NoError(t, json.Unmarshal(converted, &got))
	require.Len(t, got.Contents, 3)
	require.Equal(t, "model", got.Contents[0].Role)
	require.Len(t, got.Contents[0].Parts, 1)
	require.Contains(t, got.Contents[0].Parts[0], "functionCall")
	require.Equal(t, "user", got.Contents[1].Role)
	require.Equal(t, []map[string]any{{"functionResponse": map[string]any{
		"name": "inspect", "response": map[string]any{"content": "found"},
	}}}, got.Contents[1].Parts)
	require.Equal(t, "user", got.Contents[2].Role)
	require.Equal(t, []map[string]any{
		{"inlineData": map[string]any{"mimeType": "image/png", "data": "aGVsbG8="}},
		{"inlineData": map[string]any{"mimeType": "image/jpeg", "data": "d29ybGQ="}},
	}, got.Contents[2].Parts)
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
