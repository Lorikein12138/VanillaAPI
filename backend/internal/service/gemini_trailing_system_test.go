//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func chatMessagesForTest(t *testing.T, raw string) []apicompat.ChatMessage {
	t.Helper()
	var messages []apicompat.ChatMessage
	require.NoError(t, json.Unmarshal([]byte(raw), &messages))
	return messages
}

func chatMessageRoles(messages []apicompat.ChatMessage) []string {
	roles := make([]string, 0, len(messages))
	for _, m := range messages {
		roles = append(roles, m.Role)
	}
	return roles
}

func TestDemoteTrailingSystemMessagesAfterAssistant(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "trailing system after assistant becomes user",
			raw:  `[{"role":"system","content":"s"},{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"system","content":"p1"},{"role":"developer","content":"p2"}]`,
			want: []string{"system", "user", "assistant", "user", "user"},
		},
		{
			name: "trailing system after user is kept",
			raw:  `[{"role":"user","content":"u"},{"role":"system","content":"p"}]`,
			want: []string{"user", "system"},
		},
		{
			name: "system in the middle is kept",
			raw:  `[{"role":"user","content":"u"},{"role":"system","content":"m"},{"role":"assistant","content":"a"}]`,
			want: []string{"user", "system", "assistant"},
		},
		{
			name: "only system messages are kept",
			raw:  `[{"role":"system","content":"s"}]`,
			want: []string{"system"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := chatMessagesForTest(t, tt.raw)
			demoteTrailingSystemMessagesAfterAssistant(messages)
			require.Equal(t, tt.want, chatMessageRoles(messages))
		})
	}
}

// Mirrors the SillyTavern request that Gemini rejected with "Requests ending
// with a model turn are not supported": post-history instructions sent as
// system messages after an assistant turn.
func TestGeminiChatCompat_TrailingSystemAfterAssistantEndsWithUserTurn(t *testing.T) {
	ccReq := apicompat.ChatCompletionsRequest{
		Model: "gemini-3.8-flash",
		Messages: chatMessagesForTest(t, `[
			{"role":"system","content":"persona"},
			{"role":"user","content":"hello"},
			{"role":"system","content":"ROLEPLAY INSTRUCTION"},
			{"role":"assistant","content":"明白了。"},
			{"role":"system","content":"post-history one"},
			{"role":"system","content":"post-history two"}
		]`),
	}

	demoteTrailingSystemMessagesAfterAssistant(ccReq.Messages)
	responsesReq, err := apicompat.ChatCompletionsToResponses(&ccReq)
	require.NoError(t, err)
	anthropicReq, err := apicompat.ResponsesToAnthropicRequest(responsesReq)
	require.NoError(t, err)
	claudeBody, err := json.Marshal(anthropicReq)
	require.NoError(t, err)
	geminiBody, err := convertClaudeMessagesToGeminiGenerateContent(claudeBody)
	require.NoError(t, err)

	var geminiReq struct {
		SystemInstruction struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	require.NoError(t, json.Unmarshal(geminiBody, &geminiReq))
	require.NotEmpty(t, geminiReq.Contents)

	lastContent := geminiReq.Contents[len(geminiReq.Contents)-1]
	require.Equal(t, "user", lastContent.Role)
	var lastTexts []string
	for _, p := range lastContent.Parts {
		lastTexts = append(lastTexts, p.Text)
	}
	require.Contains(t, lastTexts, "post-history two")

	require.Len(t, geminiReq.SystemInstruction.Parts, 1)
	systemText := geminiReq.SystemInstruction.Parts[0].Text
	require.Contains(t, systemText, "persona")
	require.Contains(t, systemText, "ROLEPLAY INSTRUCTION")
	require.NotContains(t, systemText, "post-history")
}

func geminiContentRolesForTest(t *testing.T, claudeBody string) ([]string, string) {
	t.Helper()
	geminiBody, err := convertClaudeMessagesToGeminiGenerateContent([]byte(claudeBody))
	require.NoError(t, err)
	var geminiReq struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	require.NoError(t, json.Unmarshal(geminiBody, &geminiReq))
	roles := make([]string, 0, len(geminiReq.Contents))
	for _, c := range geminiReq.Contents {
		roles = append(roles, c.Role)
	}
	lastText := ""
	if n := len(geminiReq.Contents); n > 0 && len(geminiReq.Contents[n-1].Parts) > 0 {
		lastText = geminiReq.Contents[n-1].Parts[0].Text
	}
	return roles, lastText
}

// Assistant prefill / "continue" requests end with an assistant message; Gemini
// rejects a trailing model turn, so a continuation user turn is appended.
func TestConvertClaudeMessagesToGemini_AssistantPrefillAppendsContinuationTurn(t *testing.T) {
	roles, lastText := geminiContentRolesForTest(t, `{"messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"Once upon a"}
	]}`)
	require.Equal(t, []string{"user", "model", "user"}, roles)
	require.Equal(t, geminiContinuationPrompt, lastText)
}

func TestConvertClaudeMessagesToGemini_NoContinuationTurnWhenNotNeeded(t *testing.T) {
	roles, lastText := geminiContentRolesForTest(t, `{"messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"hi"},
		{"role":"user","content":"again"}
	]}`)
	require.Equal(t, []string{"user", "model", "user"}, roles)
	require.Equal(t, "again", lastText)

	roles, _ = geminiContentRolesForTest(t, `{"messages":[
		{"role":"user","content":"weather?"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{}}]}
	]}`)
	require.Equal(t, []string{"user", "model"}, roles)
}
