package service

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// GroupModelAllowlist 是 service 层的分组模型白名单（与 domain.GroupModelAllowlist
// 字段一致，ent 持久化用 domain 类型，边界处显式转换）。
type GroupModelAllowlist struct {
	Enabled bool     `json:"enabled"`
	Models  []string `json:"models,omitempty"`
}

// DomainGroupModelAllowlist 把 service 白名单转换为 ent 持久化使用的 domain 类型。
func DomainGroupModelAllowlist(cfg GroupModelAllowlist) domain.GroupModelAllowlist {
	return domain.GroupModelAllowlist{Enabled: cfg.Enabled, Models: cfg.Models}
}

// GroupModelAllowlistFromDomain 把 ent 读出的 domain 白名单转换为 service 类型。
func GroupModelAllowlistFromDomain(cfg domain.GroupModelAllowlist) GroupModelAllowlist {
	return GroupModelAllowlist{Enabled: cfg.Enabled, Models: cfg.Models}
}

// BuildAccountModelAllowlistUnion builds a deterministic union from raw
// credentials.model_mapping identity entries. Account health and scheduling
// state do not affect whether an account contributes its declared models.
func BuildAccountModelAllowlistUnion(accounts []Account) []string {
	models, _ := buildAccountModelAllowlistUnion(accounts)
	return models
}

// CountAccountModelAllowlistSources returns the number of accounts that
// contribute at least one identity entry to the allowlist union.
func CountAccountModelAllowlistSources(accounts []Account) int {
	_, count := buildAccountModelAllowlistUnion(accounts)
	return count
}

func buildAccountModelAllowlistUnion(accounts []Account) ([]string, int) {
	allModels := make([]string, 0)
	sourceCount := 0
	for i := range accounts {
		keys := accountModelAllowlistIdentityKeys(accounts[i].Credentials["model_mapping"])
		if len(keys) == 0 {
			continue
		}
		sourceCount++
		allModels = append(allModels, keys...)
	}

	sort.Slice(allModels, func(i, j int) bool {
		lowerI := strings.ToLower(allModels[i])
		lowerJ := strings.ToLower(allModels[j])
		if lowerI != lowerJ {
			return lowerI < lowerJ
		}
		return allModels[i] < allModels[j]
	})
	models := make([]string, 0, len(allModels))
	lastLower := ""
	for _, model := range allModels {
		lower := strings.ToLower(model)
		if len(models) > 0 && lower == lastLower {
			continue
		}
		models = append(models, model)
		lastLower = lower
	}
	return models, sourceCount
}

func accountModelAllowlistIdentityKeys(raw any) []string {
	var mappings map[string]any
	switch value := raw.(type) {
	case map[string]any:
		mappings = value
	case map[string]string:
		mappings = make(map[string]any, len(value))
		for key, model := range value {
			mappings[key] = model
		}
	case json.RawMessage:
		if err := json.Unmarshal(value, &mappings); err != nil {
			return nil
		}
	case []byte:
		if err := json.Unmarshal(value, &mappings); err != nil {
			return nil
		}
	default:
		return nil
	}
	if mappings == nil {
		return nil
	}

	keys := make([]string, 0, len(mappings))
	for key, rawModel := range mappings {
		model, ok := rawModel.(string)
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		model = strings.TrimSpace(model)
		if key == "" || model == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		lowerI := strings.ToLower(keys[i])
		lowerJ := strings.ToLower(keys[j])
		if lowerI != lowerJ {
			return lowerI < lowerJ
		}
		return keys[i] < keys[j]
	})
	return keys
}

// ResolveEffectiveGroupModelAllowlist keeps manual entries when disabled and
// derives enabled entries from the bound accounts without modifying inputs.
func ResolveEffectiveGroupModelAllowlist(group GroupModelAllowlist, accounts []Account) GroupModelAllowlist {
	if !group.Enabled {
		return GroupModelAllowlist{Enabled: false, Models: slices.Clone(group.Models)}
	}
	return GroupModelAllowlist{Enabled: true, Models: BuildAccountModelAllowlistUnion(accounts)}
}

// supplementUnmappedOpenAIModels ensures a partial mapping catalog does not
// hide models from unmapped or passthrough OpenAI accounts (passthrough routing
// ignores model_mapping, so it serves the same default set as an unmapped
// account). An empty catalog is left unchanged so callers retain their existing
// discovery fallback.
func supplementUnmappedOpenAIModels(accounts []Account, models []string) []string {
	if len(models) == 0 {
		return models
	}
	for i := range accounts {
		account := &accounts[i]
		if account.Platform == PlatformOpenAI && (account.IsOpenAIPassthroughEnabled() || len(account.GetModelMapping()) == 0) {
			return dedupeAndSortModelIDs(slices.Concat(models, openai.DefaultModelIDs()))
		}
	}
	return models
}

// normalizeGroupModelAllowlist 归一化管理端提交的分组模型白名单：
// 条目 TrimSpace、按小写去重保序；`*` 可出现在任意位置；
// enabled=true 时丢弃客户端条目，运行时由所属账号身份映射计算（允许空合集）。
func normalizeGroupModelAllowlist(cfg GroupModelAllowlist) (GroupModelAllowlist, error) {
	if cfg.Enabled {
		// Enabled allowlists are derived from account credentials. Client-supplied
		// models, including legacy entries, are intentionally discarded.
		return GroupModelAllowlist{Enabled: true, Models: []string{}}, nil
	}
	out := GroupModelAllowlist{Enabled: cfg.Enabled}
	if len(cfg.Models) == 0 {
		return out, nil
	}

	seen := make(map[string]struct{}, len(cfg.Models))
	out.Models = make([]string, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out.Models = append(out.Models, model)
	}
	if len(out.Models) == 0 {
		out.Models = nil
	}
	return out, nil
}

// ModelAllowlistEnabled 报告该分组是否启用了模型白名单。
// 开启后所有携带模型的网关请求与模型列表接口都受白名单约束。
func (g *Group) ModelAllowlistEnabled() bool {
	return g != nil && g.ModelAllowlist.Enabled
}

// Allows 判断客户端请求的模型是否命中白名单。
// 准入只看客户端书写的模型名，与账号映射、渠道映射、合成路由改写无关；
// 候选形式覆盖代码中已有的模型名等价规则（Gemini models/ 前缀、
// Antigravity/Claude -thinking 宽容规则、OpenAI 推理后缀），不做模糊匹配。
func (a GroupModelAllowlist) Allows(model string) bool {
	if !a.Enabled {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return true
	}
	candidates := groupModelAllowlistCandidates(model)
	for _, entry := range a.Models {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		for _, candidate := range candidates {
			if groupAllowlistPatternMatches(entry, candidate) {
				return true
			}
		}
	}
	return false
}

// groupAllowlistPatternMatches treats only * as a wildcard, including in the middle
// or at either end. Matching is anchored and case-insensitive.
func groupAllowlistPatternMatches(pattern, model string) bool {
	pattern = strings.ToLower(pattern)
	model = strings.ToLower(model)
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == model
	}
	if !strings.HasPrefix(model, parts[0]) {
		return false
	}
	model = model[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		index := strings.Index(model, part)
		if index < 0 {
			return false
		}
		model = model[index+len(part):]
	}
	return strings.HasSuffix(model, parts[len(parts)-1])
}

// groupModelAllowlistCandidates 返回客户端模型名在白名单匹配中的候选形式（均已小写）。
func groupModelAllowlistCandidates(model string) []string {
	candidates := make([]string, 0, 4)
	add := func(value string) {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			return
		}
		for _, existing := range candidates {
			if existing == value {
				return
			}
		}
		candidates = append(candidates, value)
	}

	add(model)
	add(strings.TrimPrefix(model, "models/"))
	add(claude.NormalizeModelID(strings.TrimSuffix(model, "-thinking")))
	add(NormalizeOpenAICompatRequestedModel(model))
	return candidates
}

// FilterForListing 按白名单条目顺序生成模型列表输出。
// 精确条目沿用既有规则：只有出现在 source（账号映射键 ∪ 平台默认列表）的模式
// 集合中才输出；通配条目展开为 source 中所有匹配项并保持 source 顺序；全局去重。
// 当 source 是前缀通配模式且白名单模式的固定前缀位于该前缀之内时，
// 可直接输出白名单模式：它的所有实例都属于 source。其他未处理的无限
// 交集不会被枚举为模型 ID，列表可能省略仍可请求的模型。
func (a GroupModelAllowlist) FilterForListing(source []string) []string {
	if !a.Enabled {
		return source
	}
	if len(a.Models) == 0 {
		// 自动合集可以为空；此时没有来源模型可对外列出，准入也保持关闭。
		return nil
	}

	patterns := make([]string, 0, len(source))
	for _, pattern := range source {
		pattern = strings.TrimSpace(pattern)
		if pattern != "" {
			patterns = append(patterns, pattern)
		}
	}
	if len(patterns) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(a.Models)+len(patterns))
	filtered := make([]string, 0, len(patterns))
	add := func(model string) {
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		filtered = append(filtered, model)
	}

	for _, entry := range a.Models {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "*") {
			for _, pattern := range patterns {
				if groupAllowlistPatternMatches(entry, pattern) {
					add(pattern)
				} else if strings.HasSuffix(pattern, "*") &&
					!strings.Contains(strings.TrimSuffix(pattern, "*"), "*") &&
					strings.HasPrefix(strings.ToLower(entry), strings.ToLower(strings.TrimSuffix(pattern, "*"))) {
					add(entry)
				}
			}
			continue
		}
		if allowlistSourcePatternAllowsModel(patterns, entry) {
			add(entry)
		}
	}
	return filtered
}

// allowlistSourcePatternAllowsModel 沿用 filterModelsByCustomList 时代的匹配规则：
// 精确相等、source 通配模式的前缀匹配，以及 Claude 归一化（-thinking 后缀）
// 后的精确匹配。比较与 Allows 一律大小写不敏感，保证「准入允许 ⇒ 列表可见」。
func allowlistSourcePatternAllowsModel(patterns []string, model string) bool {
	for _, pattern := range patterns {
		if strings.EqualFold(pattern, model) {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(strings.ToLower(model), strings.ToLower(strings.TrimSuffix(pattern, "*"))) {
			return true
		}
	}
	normalizedClaudeModel := claude.NormalizeModelID(strings.TrimSuffix(model, "-thinking"))
	if !strings.EqualFold(normalizedClaudeModel, model) {
		for _, pattern := range patterns {
			if strings.EqualFold(pattern, normalizedClaudeModel) {
				return true
			}
		}
	}
	return false
}
