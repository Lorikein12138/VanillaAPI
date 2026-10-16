//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestConfiguredMinOutputTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want int
	}{
		{"nil", nil, 128},
		{"zero", &config.Config{}, 128},
		{"invalid", &config.Config{Gateway: config.GatewayConfig{MinOutputTokens: 15}}, 128},
		{"default", &config.Config{Gateway: config.GatewayConfig{MinOutputTokens: 128}}, 128},
		{"boundary", &config.Config{Gateway: config.GatewayConfig{MinOutputTokens: 16}}, 16},
		{"custom", &config.Config{Gateway: config.GatewayConfig{MinOutputTokens: 256}}, 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, configuredMinOutputTokens(tc.cfg))
			requested := 1
			chat, err := chatCompletionsToResponses(tc.cfg, &apicompat.ChatCompletionsRequest{Model: "gpt-4o", MaxTokens: &requested})
			require.NoError(t, err)
			anthropic, err := anthropicToResponses(tc.cfg, &apicompat.AnthropicRequest{Model: "gpt-4o", MaxTokens: requested})
			require.NoError(t, err)
			for _, request := range []*apicompat.ResponsesRequest{chat, anthropic} {
				body, err := json.Marshal(request)
				require.NoError(t, err)
				require.Equal(t, int64(tc.want), gjson.GetBytes(body, "max_output_tokens").Int())
			}
		})
	}
}

func TestAPICompatConfigPreservesLargerLimitsAndLegacyDefaults(t *testing.T) {
	cfg := &config.Config{Gateway: config.GatewayConfig{MinOutputTokens: 256}}
	requested := 512
	chat, err := chatCompletionsToResponses(cfg, &apicompat.ChatCompletionsRequest{Model: "gpt-4o", MaxTokens: &requested})
	require.NoError(t, err)
	require.Equal(t, requested, *chat.MaxOutputTokens)
	anthropic, err := anthropicToResponses(cfg, &apicompat.AnthropicRequest{Model: "gpt-4o", MaxTokens: requested})
	require.NoError(t, err)
	require.Equal(t, requested, *anthropic.MaxOutputTokens)
	requested = 1
	chat, err = apicompat.ChatCompletionsToResponses(&apicompat.ChatCompletionsRequest{Model: "gpt-4o", MaxTokens: &requested})
	require.NoError(t, err)
	require.Equal(t, 128, *chat.MaxOutputTokens)
	anthropic, err = apicompat.AnthropicToResponses(&apicompat.AnthropicRequest{Model: "gpt-4o", MaxTokens: requested})
	require.NoError(t, err)
	require.Equal(t, 128, *anthropic.MaxOutputTokens)
	require.Nil(t, settingServiceConfig(nil))
	require.Same(t, cfg, settingServiceConfig(&SettingService{cfg: cfg}))
}
