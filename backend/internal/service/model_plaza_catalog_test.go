//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
)

func TestPlazaCatalogDisplayMatching(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		model    string
		want     bool
	}{
		{name: "exact name matches case-insensitively", patterns: []string{"Claude-Sonnet-4-6"}, model: "claude-sonnet-4-6", want: true},
		{name: "model name is trimmed", patterns: []string{"claude-sonnet-4-6"}, model: "  claude-sonnet-4-6 ", want: true},
		{name: "pattern is trimmed", patterns: []string{"  claude-sonnet-4-6  "}, model: "claude-sonnet-4-6", want: true},
		{name: "star matches a middle segment", patterns: []string{"gpt-*-flare"}, model: "gpt-image-2.5-flare", want: true},
		{name: "leading star matches a suffix", patterns: []string{"*-6"}, model: "claude-sonnet-4-6", want: true},
		{name: "star alone matches any name", patterns: []string{"*"}, model: "anything-at-all", want: true},
		{name: "uppercase wildcard matches lowercase name", patterns: []string{"CLAUDE-*"}, model: "claude-opus-4-6", want: true},
		{name: "exact pattern is anchored at the end", patterns: []string{"gpt-image-2"}, model: "gpt-image-2.5", want: false},
		{name: "exact pattern is anchored at the start", patterns: []string{"claude"}, model: "claude-sonnet-4-6", want: false},
		{name: "models prefix is not normalized", patterns: []string{"gemini-2.5-pro"}, model: "models/gemini-2.5-pro", want: false},
		{name: "thinking suffix is not normalized", patterns: []string{"claude-sonnet-4-6"}, model: "claude-sonnet-4-6-thinking", want: false},
		{name: "blank model never matches", patterns: []string{"*"}, model: "   ", want: false},
		{name: "blank pattern is skipped", patterns: []string{"  "}, model: "claude-sonnet-4-6", want: false},
		{name: "nil patterns never match", patterns: nil, model: "claude-sonnet-4-6", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, plazaDisplayPatternAllows(tt.patterns, tt.model))
		})
	}
}

// 展示词法匹配刻意不复用网关 Allows 的请求等价规则：同一条目网关放行 models/ 前缀，展示则不放行。
func TestPlazaCatalogDisplayDoesNotUseGatewayEquivalence(t *testing.T) {
	entries := []string{"gemini-2.5-pro"}
	allowlist := GroupModelAllowlist{Enabled: true, Models: entries}
	require.True(t, allowlist.Allows("models/gemini-2.5-pro"), "网关请求等价规则放行 models/ 前缀")
	require.False(t, plazaDisplayPatternAllows(entries, "models/gemini-2.5-pro"), "展示只做词法匹配")
}

func TestPlazaCatalogAccountSources(t *testing.T) {
	passthrough := map[string]any{"openai_passthrough": true}
	tests := []struct {
		name     string
		account  *Account
		platform string
		want     []string
	}{
		{name: "nil account contributes nothing", account: nil, platform: PlatformAnthropic},
		{
			name:     "native anthropic without mapping uses default ids",
			account:  &Account{Platform: PlatformAnthropic},
			platform: PlatformAnthropic,
			want:     claude.DefaultModelIDs(),
		},
		{
			name: "explicit mapping keeps only valid string keys",
			account: &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{
				"claude-sonnet-4-6": "claude-sonnet-4-6", "claude-opus-4-6": "x", "bad-value": 1, " ": "x",
			}}},
			platform: PlatformAnthropic,
			want:     []string{"claude-sonnet-4-6", "claude-opus-4-6"},
		},
		{
			name: "explicit mapping with only invalid entries contributes nothing",
			account: &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{
				"bad-value": 1,
			}}},
			platform: PlatformAnthropic,
		},
		{
			name:     "invalid json contributes nothing",
			account:  &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": json.RawMessage("{bad")}},
			platform: PlatformAnthropic,
		},
		{
			name:     "json null falls back to defaults",
			account:  &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": json.RawMessage("null")}},
			platform: PlatformAnthropic,
			want:     claude.DefaultModelIDs(),
		},
		{
			name:     "empty json object falls back to defaults",
			account:  &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": json.RawMessage("{}")}},
			platform: PlatformAnthropic,
			want:     claude.DefaultModelIDs(),
		},
		{
			name:     "empty map falls back to defaults",
			account:  &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{}}},
			platform: PlatformAnthropic,
			want:     claude.DefaultModelIDs(),
		},
		{
			name:     "native anthropic keeps wildcard keys",
			account:  &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"claude-*": "claude-sonnet-4-6"}}},
			platform: PlatformAnthropic,
			want:     []string{"claude-*"},
		},
		{
			name:     "openai passthrough serves star and ignores residual mapping",
			account:  &Account{Platform: PlatformOpenAI, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-4o": "gpt-4o"}}, Extra: passthrough},
			platform: PlatformOpenAI,
			want:     []string{"*"},
		},
		{
			name:     "openai passthrough does not serve other platforms",
			account:  &Account{Platform: PlatformOpenAI, Extra: passthrough},
			platform: PlatformAnthropic,
		},
		{
			name:     "native openai without mapping serves star",
			account:  &Account{Platform: PlatformOpenAI},
			platform: PlatformOpenAI,
			want:     []string{"*"},
		},
		{
			name:     "native gemini without mapping serves star",
			account:  &Account{Platform: PlatformGemini},
			platform: PlatformGemini,
			want:     []string{"*"},
		},
		{
			name:     "native account of another platform contributes nothing",
			account:  &Account{Platform: PlatformAnthropic},
			platform: PlatformOpenAI,
		},
		{
			name:     "antigravity explicit mapping keeps exact gemini keys only",
			account:  &Account{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-*": "x", "gemini-2.5-pro": "gemini-2.5-pro"}}},
			platform: PlatformGemini,
			want:     []string{"gemini-2.5-pro"},
		},
		{
			name:     "antigravity explicit mapping feeds anthropic exact keys only",
			account:  &Account{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"claude-*": "x", "claude-opus-4-6-thinking": "claude-opus-4-6-thinking"}}},
			platform: PlatformAnthropic,
			want:     []string{"claude-opus-4-6-thinking"},
		},
		{
			name:     "antigravity does not serve openai",
			account:  &Account{Platform: PlatformAntigravity, Credentials: map[string]any{"model_mapping": map[string]any{"gpt-4o": "gpt-4o"}}},
			platform: PlatformOpenAI,
		},
		{
			name:     "native anthropic does not serve antigravity candidates",
			account:  &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-6": "x"}}},
			platform: PlatformAntigravity,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plazaAccountModelPatterns(tt.account, tt.platform)
			if len(tt.want) == 0 {
				require.Empty(t, got)
				return
			}
			require.ElementsMatch(t, tt.want, got)
		})
	}
}

// Antigravity 默认映射为 anthropic 候选提供精确 key（含 thinking 变体），且不带通配来源。
func TestPlazaCatalogAntigravityDefaultsServeAnthropicExactKeys(t *testing.T) {
	account := &Account{Platform: PlatformAntigravity}
	got := plazaAccountModelPatterns(account, PlatformAnthropic)
	require.Contains(t, got, "claude-opus-4-6-thinking")
	for _, pattern := range got {
		require.NotContains(t, pattern, "*", "跨平台 Antigravity 来源不得携带通配")
	}
}

// 同一组内多个成员的来源模式按小写去重、保留首次出现的写法；目录按请求平台隔离声明。
func TestPlazaCatalogGroupPatternsDedupeAndDeclarePerPlatform(t *testing.T) {
	accounts := []Account{
		{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-4-6": "x"}}},
		{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"CLAUDE-SONNET-4-6": "y", "claude-opus-4-6": "z"}}},
	}
	require.ElementsMatch(t, []string{"claude-sonnet-4-6", "claude-opus-4-6"}, plazaGroupModelPatterns(accounts, PlatformAnthropic))

	directory := newPlazaGroupDirectory(accounts)
	require.True(t, directory.declares(PlatformAnthropic, "CLAUDE-OPUS-4-6"))
	require.False(t, directory.declares(PlatformOpenAI, "gpt-4o"), "anthropic 成员不为 openai 候选提供来源")
}

// 原始 mapping 的四种承载形式（map[string]any、map[string]string、json.RawMessage、[]byte）语义一致：
// 合法 key 原样保留；显式配置但全部无效时贡献空集，不回退平台默认目录。
func TestPlazaCatalogAccountRawMappingForms(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want []string
	}{
		{name: "string map keeps valid keys", raw: map[string]string{"claude-sonnet-4-6": "claude-sonnet-4-6", " ": "x"}, want: []string{"claude-sonnet-4-6"}},
		{name: "byte slice json keeps valid keys", raw: []byte(`{"claude-opus-4-6":"x"}`), want: []string{"claude-opus-4-6"}},
		{name: "raw message drops non-string values", raw: json.RawMessage(`{"claude-sonnet-4-6":"x","bad-value":1}`), want: []string{"claude-sonnet-4-6"}},
		{name: "string map with only blank keys contributes nothing", raw: map[string]string{" ": "x"}},
		{name: "byte slice with invalid json contributes nothing", raw: []byte("{bad")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": tt.raw}}
			got := plazaAccountModelPatterns(account, PlatformAnthropic)
			if len(tt.want) == 0 {
				require.Empty(t, got)
				return
			}
			require.ElementsMatch(t, tt.want, got)
		})
	}
}
