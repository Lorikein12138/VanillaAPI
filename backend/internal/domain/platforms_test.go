package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 重构前各处手写的平台列表，作为平台清单派生结果的等价基准；重构后新登记的平台
// （Command Code、Cline）按同类平台（OpenCode）的位置补入。
var (
	legacyDisplayOrder = []string{
		PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
	}
	legacyCompositePrecedence = []string{
		PlatformAnthropic, PlatformGemini, PlatformOpenAI, PlatformAntigravity, PlatformGrok,
		PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo, PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
	}
	legacyOpenAIGateway = []string{
		PlatformOpenAI, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo,
		PlatformCommandCode,
		PlatformCline,
	}
	legacyCNProviders    = []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax}
	legacyLiteLLMByPlatf = map[string]string{
		PlatformAnthropic: "anthropic", PlatformOpenAI: "openai", PlatformGemini: "gemini",
		PlatformAntigravity: "anthropic", PlatformGrok: "xai", PlatformKimi: "moonshot",
		PlatformZhipu: "zhipu", PlatformDeepseek: "deepseek", PlatformMiniMax: "minimax",
		PlatformOpenCodeGo: "opencode-go", PlatformTypeSafe: "typesafe", PlatformCommandCode: "", PlatformCline: "",
	}
)

func TestPlatformListMatchesLegacyLists(t *testing.T) {
	require.Equal(t, legacyDisplayOrder, ConcretePlatformIDs())
	require.Equal(t, legacyCompositePrecedence, CompositePrecedencePlatformIDs())
	require.Equal(t, legacyOpenAIGateway, PlatformIDsWhere(func(spec PlatformSpec) bool {
		return spec.Gateway == PlatformGatewayOpenAI
	}))
	require.Equal(t, legacyCNProviders, PlatformIDsWhere(func(spec PlatformSpec) bool { return spec.CNProvider }))
	for platform, provider := range legacyLiteLLMByPlatf {
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.Equal(t, provider, spec.LiteLLMProvider, platform)
	}
}

func TestPlatformListPredicates(t *testing.T) {
	for _, platform := range legacyDisplayOrder {
		require.True(t, IsConcretePlatform(platform), platform)
		require.True(t, IsGroupPlatform(platform), platform)
		spec, ok := LookupPlatform(platform)
		require.True(t, ok, platform)
		require.NotEmpty(t, spec.DisplayName, platform)
		require.NotEmpty(t, spec.Gateway, platform)
	}
	require.False(t, IsConcretePlatform(PlatformComposite))
	require.True(t, IsGroupPlatform(PlatformComposite))
	for _, invalid := range []string{"", "moonshot", "Kimi", "openai ", "glm", "bogus"} {
		require.False(t, IsConcretePlatform(invalid), invalid)
		require.False(t, IsGroupPlatform(invalid), invalid)
		require.False(t, UsesOpenAIGateway(invalid), invalid)
	}
}

func TestPlatformsReturnsCopy(t *testing.T) {
	platforms := Platforms()
	platforms[0].ID = "mutated"
	require.Equal(t, PlatformAnthropic, Platforms()[0].ID)
}

func TestSupportedGroupPlatformsUsesCatalog(t *testing.T) {
	want := []string{
		PlatformAnthropic,
		PlatformOpenAI,
		PlatformGemini,
		PlatformAntigravity,
		PlatformGrok,
		PlatformKimi,
		PlatformZhipu,
		PlatformDeepseek,
		PlatformMiniMax,
		PlatformOpenCodeGo,
		PlatformTypeSafe,
		PlatformCommandCode,
		PlatformCline,
		PlatformComposite,
	}

	platforms := SupportedGroupPlatforms()
	require.Equal(t, want, platforms)
	require.Equal(t, PlatformComposite, platforms[len(platforms)-1])
	require.Contains(t, platforms, PlatformTypeSafe)
	require.Contains(t, platforms, PlatformCommandCode)
	require.Contains(t, platforms, PlatformCline)
	require.NotContains(t, platforms, "kiro")
	require.NotContains(t, platforms, "unknown")

	seen := make(map[string]struct{}, len(platforms))
	compositeCount := 0
	for _, platform := range platforms {
		_, duplicate := seen[platform]
		require.False(t, duplicate, "duplicate platform %q", platform)
		seen[platform] = struct{}{}
		if platform == PlatformComposite {
			compositeCount++
		}
	}
	require.Equal(t, 1, compositeCount)

	platforms[0] = "mutated"
	require.Equal(t, PlatformAnthropic, SupportedGroupPlatforms()[0])
}
