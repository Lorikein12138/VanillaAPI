package service

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

// plazaGroupDirectory 保存单个活跃组在一次 ListGroups 中的成员快照，并按请求平台缓存汇总后的来源模式。
// 快照只在本次请求内使用：不跨请求缓存，也不修改成员账号或其 mapping。
type plazaGroupDirectory struct {
	accounts []Account
	patterns map[string][]string
}

func newPlazaGroupDirectory(accounts []Account) *plazaGroupDirectory {
	return &plazaGroupDirectory{accounts: accounts, patterns: make(map[string][]string)}
}

// declares 报告组内成员是否为 platform 下的 model 提供来源声明。无成员时任何候选都不满足。
func (d *plazaGroupDirectory) declares(platform, model string) bool {
	patterns, ok := d.patterns[platform]
	if !ok {
		patterns = plazaGroupModelPatterns(d.accounts, platform)
		d.patterns[platform] = patterns
	}
	return plazaDisplayPatternAllows(patterns, model)
}

// plazaGroupModelPatterns 汇总组内全部成员对 platform 候选的来源模式，按小写去重并保持首次出现的顺序。
// 账号状态（暂停、异常、不可调度）不影响结果：目录只表达组已声明的模型。
func plazaGroupModelPatterns(accounts []Account, platform string) []string {
	seen := make(map[string]struct{})
	var patterns []string
	for i := range accounts {
		for _, pattern := range plazaAccountModelPatterns(&accounts[i], platform) {
			key := strings.ToLower(pattern)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			patterns = append(patterns, pattern)
		}
	}
	return patterns
}

// plazaAccountModelPatterns 返回单个成员账号为 requestPlatform 候选提供的来源模式。
// 模式只取 mapping 的左侧 key，不取 value；判定顺序固定：
//  1. 平台隔离：原生账号平台必须等于候选平台。Antigravity 账号只为 anthropic/gemini/antigravity 候选
//     提供精确 key 声明（不含 `*`），其余跨平台账号不贡献来源；
//  2. OpenAI passthrough 只为 openai 候选提供 `*`，忽略残留 mapping；
//  3. 显式 model_mapping 的合法非空 key 优先；非空但全部无效的显式 mapping 贡献空集，不回退默认目录；
//  4. 未配置 mapping 时使用平台默认 mapping 的 key；原生 Anthropic 使用默认原始 ID；其余原生平台保持 `*`。
func plazaAccountModelPatterns(account *Account, requestPlatform string) []string {
	if account == nil {
		return nil
	}
	crossPlatform := account.Platform != requestPlatform
	if crossPlatform && !(account.Platform == PlatformAntigravity && plazaAntigravityServes(requestPlatform)) {
		return nil
	}
	if !crossPlatform && account.Platform == PlatformOpenAI && account.IsOpenAIPassthroughEnabled() {
		return []string{"*"}
	}

	raw := account.Credentials["model_mapping"]
	if keys := accountModelAllowlistIdentityKeys(raw); len(keys) > 0 {
		return plazaSourceKeys(keys, crossPlatform)
	}
	if plazaRawMappingPresent(raw) {
		return nil
	}
	if defaults := plazaDefaultMappingKeys(account.GetModelMapping()); len(defaults) > 0 {
		return plazaSourceKeys(defaults, crossPlatform)
	}
	if crossPlatform {
		return nil
	}
	if account.Platform == PlatformAnthropic {
		return claude.DefaultModelIDs()
	}
	return []string{"*"}
}

// plazaAntigravityServes 报告 Antigravity 账号的精确 key 是否可以为该候选平台提供来源声明。
func plazaAntigravityServes(platform string) bool {
	return platform == PlatformAnthropic || platform == PlatformGemini || platform == PlatformAntigravity
}

// plazaSourceKeys 对跨平台 Antigravity 账号只保留精确 key；原生账号的 key 原样保留（允许 `*`）。
func plazaSourceKeys(keys []string, crossPlatform bool) []string {
	if !crossPlatform {
		return keys
	}
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if !strings.Contains(key, "*") {
			out = append(out, key)
		}
	}
	return out
}

// plazaDefaultMappingKeys 提取平台默认 mapping 的非空 key。只读取 key，不修改共享的默认 map。
func plazaDefaultMappingKeys(defaults map[string]string) []string {
	keys := make([]string, 0, len(defaults))
	for key := range defaults {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// plazaRawMappingPresent 报告 credentials.model_mapping 是否存在非空的显式配置。
// 即使其中没有任何合法条目也返回 true：此时应贡献空集，而不是静默回退到平台默认目录。
func plazaRawMappingPresent(raw any) bool {
	switch value := raw.(type) {
	case nil:
		return false
	case map[string]any:
		return len(value) > 0
	case map[string]string:
		return len(value) > 0
	case json.RawMessage:
		return plazaRawJSONMappingPresent(value)
	case []byte:
		return plazaRawJSONMappingPresent(value)
	default:
		return true
	}
}

func plazaRawJSONMappingPresent(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	var mappings map[string]any
	if err := json.Unmarshal(trimmed, &mappings); err != nil {
		return true
	}
	return len(mappings) > 0
}

// plazaDisplayPatternAllows 用候选的原始名字做展示词法匹配：只 trim，大小写不敏感，`*` 锚定任意位置。
// 不做 models/ 前缀、-thinking 或推理后缀归一化，因此基础 ID 不会代表 thinking 变体。
// 这里刻意不调用 Allows、groupModelAllowlistCandidates 或 IsModelSupported，以免展示继承网关的请求等价规则。
func plazaDisplayPatternAllows(patterns []string, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern != "" && groupAllowlistPatternMatches(pattern, model) {
			return true
		}
	}
	return false
}
