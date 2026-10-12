//go:build unit

package service

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type modelPlazaMembersRepoStub struct {
	AccountRepository
	accountsByGroup map[int64][]Account
	err             error
	calls           map[int64]int
}

func (s *modelPlazaMembersRepoStub) ListAllByGroup(_ context.Context, groupID int64) ([]Account, error) {
	if s.calls != nil {
		s.calls[groupID]++
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.accountsByGroup[groupID], nil
}

// plazaMembersFromGroups 为每个非复合活跃组构造成员快照：启用白名单的组由白名单模型声明，
// 其余组由挂载在该组上的渠道为本组平台提供的模型声明。复合组需要显式成员，不由此推导。
func plazaMembersFromGroups(groups []Group, channels []Channel) *modelPlazaMembersRepoStub {
	accountsByGroup := make(map[int64][]Account)
	for _, group := range groups {
		if group.Platform == PlatformComposite {
			continue
		}
		models := plazaChannelModels(channels, group.ID, group.Platform)
		if group.ModelAllowlist.Enabled {
			models = group.ModelAllowlist.Models
		}
		if len(models) == 0 {
			continue
		}
		accountsByGroup[group.ID] = []Account{plazaMemberAccount(group.Platform, models...)}
	}
	return &modelPlazaMembersRepoStub{accountsByGroup: accountsByGroup, calls: make(map[int64]int)}
}

// plazaMemberAccount 构造声明 models 的成员账号：model_mapping 的 key 即来源声明，value 与声明无关。
func plazaMemberAccount(platform string, models ...string) Account {
	mapping := make(map[string]any, len(models))
	for _, model := range models {
		mapping[model] = model
	}
	return Account{Platform: platform, Credentials: map[string]any{"model_mapping": mapping}}
}

// plazaChannelModels 汇总挂在该组上的活跃渠道为 platform 提供的模型名（定价模型与 mapping 左侧 key）。
func plazaChannelModels(channels []Channel, groupID int64, platform string) []string {
	var models []string
	for i := range channels {
		ch := &channels[i]
		if ch.Status != StatusActive || !slices.Contains(ch.GroupIDs, groupID) {
			continue
		}
		for _, pricing := range ch.ModelPricing {
			if pricing.Platform == platform {
				models = append(models, pricing.Models...)
			}
		}
		for name := range ch.ModelMapping[platform] {
			models = append(models, name)
		}
	}
	return models
}

func plazaModelNames(models []PlazaModel) []string {
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	return names
}

// newPlazaService 构造 ListGroups 测试用的 ModelPlazaService（不接计费服务：展示定价原样透传）。
func newPlazaService(channels []Channel, groups []Group, pricing *PricingService) *ModelPlazaService {
	return newPlazaServiceWithMembers(channels, groups, plazaMembersFromGroups(groups, channels), pricing)
}

// newPlazaServiceWithMembers 构造使用给定成员快照的广场服务，供需要显式声明来源的用例使用。
func newPlazaServiceWithMembers(channels []Channel, groups []Group, members AccountRepository, pricing *PricingService) *ModelPlazaService {
	repo := &mockChannelRepository{
		listAllFn: func(ctx context.Context) ([]Channel, error) { return channels, nil },
	}
	return NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, members, pricing, nil, nil)
}

func plazaPricedChannel(id int64, name string, groupIDs []int64, platform string, models ...string) Channel {
	return Channel{
		ID:       id,
		Name:     name,
		Status:   StatusActive,
		GroupIDs: groupIDs,
		ModelPricing: []ChannelModelPricing{{
			Platform:    platform,
			Models:      models,
			BillingMode: BillingModeToken,
			InputPrice:  testPtrFloat64(3e-6),
			OutputPrice: testPtrFloat64(1.5e-5),
		}},
	}
}

func TestListPlazaGroups_GroupCentricAggregation(t *testing.T) {
	// 两个渠道挂同一分组:模型并入同一 PlazaGroup;无模型的分组不返回。
	channels := []Channel{
		plazaPricedChannel(1, "chA", []int64{10}, "anthropic", "claude-sonnet"),
		plazaPricedChannel(2, "chB", []int64{10}, "anthropic", "claude-opus"),
	}
	groups := []Group{
		{ID: 10, Name: "g-main", Description: "desc", Platform: "anthropic", RateMultiplier: 1},
		{ID: 20, Name: "g-empty", Platform: "anthropic", RateMultiplier: 0.5},
	}
	svc := newPlazaService(channels, groups, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1, "无模型的分组不应返回")
	require.Equal(t, int64(10), out[0].ID)
	require.Equal(t, "desc", out[0].Description)
	require.Len(t, out[0].Models, 2)
	// 组内模型按名称排序
	require.Equal(t, "claude-opus", out[0].Models[0].Name)
	require.Equal(t, "claude-sonnet", out[0].Models[1].Name)
}

func TestListPlazaGroups_ModelAllowlistFiltersChannelPricingModels(t *testing.T) {
	channels := []Channel{
		{
			ID: 1, Name: "associated-active", Status: StatusActive, GroupIDs: []int64{10},
			ModelPricing: []ChannelModelPricing{{
				Platform:   PlatformAnthropic,
				Models:     []string{"model-a", "model-b", "model-c"},
				InputPrice: testPtrFloat64(3e-6),
			}},
		},
		{
			ID: 2, Name: "associated-inactive", Status: "inactive", GroupIDs: []int64{10},
			ModelPricing: []ChannelModelPricing{{
				Platform:   PlatformAnthropic,
				Models:     []string{"model-inactive"},
				InputPrice: testPtrFloat64(3e-6),
			}},
		},
		{
			ID: 3, Name: "unassociated-active", Status: StatusActive, GroupIDs: []int64{99},
			ModelPricing: []ChannelModelPricing{{
				Platform:   PlatformAnthropic,
				Models:     []string{"model-unassociated"},
				InputPrice: testPtrFloat64(3e-6),
			}},
		},
		{
			ID: 4, Name: "associated-wrong-platform", Status: StatusActive, GroupIDs: []int64{10},
			ModelPricing: []ChannelModelPricing{{
				Platform:   PlatformOpenAI,
				Models:     []string{"model-wrong-platform"},
				InputPrice: testPtrFloat64(3e-6),
			}},
		},
	}
	groups := []Group{{
		ID:             10,
		Name:           "allowlisted-group",
		Platform:       PlatformAnthropic,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"model-a"},
		},
	}}

	out, err := newPlazaService(channels, groups, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 1)
	require.Equal(t, "model-a", out[0].Models[0].Name)
}

func TestListPlazaGroups_AutomaticAccountUnionFiltersExistingChannelCandidates(t *testing.T) {
	channels := []Channel{
		plazaPricedChannel(1, "associated-active", []int64{10}, PlatformAnthropic,
			"model-a", "model-b", "model-c", "channel-only"),
	}
	groups := []Group{{
		ID:             10,
		Name:           "automatic-group",
		Platform:       PlatformAnthropic,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"legacy-manual-model"}},
	}}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{
		10: {
			{Platform: PlatformAnthropic, Status: StatusDisabled, ErrorMessage: "expired", Schedulable: false, Credentials: map[string]any{
				"model_mapping": map[string]any{"model-a": "model-a", "model-c": "model-c", "renamed": "upstream-model"},
			}},
			{Platform: PlatformAnthropic, Status: StatusError, Schedulable: false, Credentials: map[string]any{
				"model_mapping": map[string]any{"model-c": "model-c", "model-x": "model-x"},
			}},
		},
	}}
	channelRepo := &mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) { return channels, nil }}
	svc := NewModelPlazaService(channelRepo, &stubGroupRepoForAvailable{activeGroups: groups}, members, nil, nil, nil)

	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, []string{"model-a", "model-c"}, []string{out[0].Models[0].Name, out[0].Models[1].Name})
	for _, model := range out[0].Models {
		require.NotEqual(t, "model-x", model.Name, "allowlist entries must not synthesize channel models")
	}
}

func TestListPlazaGroups_MappedKeyDisplayAndPricing(t *testing.T) {
	channels := []Channel{{ID: 1, Status: StatusActive, GroupIDs: []int64{10}, ModelPricing: []ChannelModelPricing{{Platform: PlatformAnthropic, Models: []string{"client-name"}, InputPrice: testPtrFloat64(3e-6)}}, ModelMapping: map[string]map[string]string{PlatformAnthropic: {"client-name": "upstream-name"}}}}
	groups := []Group{{ID: 10, Platform: PlatformAnthropic, RateMultiplier: 1, ModelAllowlist: GroupModelAllowlist{Enabled: true}}}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{10: {{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": map[string]any{"client-name": "upstream-name"}}}}}}
	svc := NewModelPlazaService(&mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) { return channels, nil }}, &stubGroupRepoForAvailable{activeGroups: groups}, members, nil, nil, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 1)
	require.Equal(t, "client-name", out[0].Models[0].Name)
	require.NotNil(t, out[0].Models[0].Pricing)
	require.Equal(t, 3e-6, *out[0].Models[0].Pricing.InputPrice)
	require.Equal(t, "upstream-name", members.accountsByGroup[10][0].GetModelMapping()["client-name"])
}

func TestListPlazaGroups_AutomaticAccountReadFailureFailsClosed(t *testing.T) {
	sentinel := errors.New("member read failed")
	groups := []Group{{ID: 10, Platform: PlatformAnthropic, ModelAllowlist: GroupModelAllowlist{Enabled: true}}}
	svc := NewModelPlazaService(
		&mockChannelRepository{listAllFn: func(context.Context) ([]Channel, error) { return nil, nil }},
		&stubGroupRepoForAvailable{activeGroups: groups},
		&modelPlazaMembersRepoStub{err: sentinel}, nil, nil, nil,
	)
	out, err := svc.ListGroups(context.Background())
	require.Nil(t, out)
	require.ErrorIs(t, err, sentinel)
}

func TestListPlazaGroups_DisabledModelAllowlistKeepsChannelPricingModels(t *testing.T) {
	channels := []Channel{
		plazaPricedChannel(1, "associated-active", []int64{10}, PlatformAnthropic,
			"model-a", "model-b", "model-c"),
	}
	groups := []Group{{
		ID:             10,
		Name:           "allowlist-disabled",
		Platform:       PlatformAnthropic,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{
			Enabled: false,
			Models:  []string{"model-a"},
		},
	}}

	out, err := newPlazaService(channels, groups, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, []string{"model-a", "model-b", "model-c"}, []string{
		out[0].Models[0].Name,
		out[0].Models[1].Name,
		out[0].Models[2].Name,
	})
}

// 白名单词法匹配：`*` 锚定任意位置、大小写不敏感，不做 -thinking / models/ 归一化，
// 基础 ID 不代表 thinking 变体（与网关 Allows 的请求等价规则不同）。
func TestListPlazaGroups_ModelAllowlistWildcardUsesLexicalMatching(t *testing.T) {
	channels := []Channel{
		{
			ID: 1, Name: "multi", Status: StatusActive, GroupIDs: []int64{10},
			ModelPricing: []ChannelModelPricing{
				{Platform: PlatformOpenAI, Models: []string{
					"gpt-image-2", "gpt-image-2.5-flare", "gpt-5.6-sol",
				}},
				{Platform: PlatformAnthropic, Models: []string{"claude-sonnet-4", "claude-sonnet-4-thinking"}},
			},
		},
	}
	allowlist := GroupModelAllowlist{
		Enabled: true,
		Models:  []string{"GPT-IMAGE-*", "claude-sonnet-4"},
	}
	groups := []Group{{
		ID:             10,
		Name:           "gateway-matching",
		Platform:       PlatformComposite,
		RateMultiplier: 1,
		ModelAllowlist: allowlist,
	}}
	// 成员声明来源：OpenAI 通配 key 与 anthropic 基础 ID；生效白名单为成员 key 的并集。
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{10: {
		plazaMemberAccount(PlatformOpenAI, "GPT-IMAGE-*"),
		plazaMemberAccount(PlatformAnthropic, "claude-sonnet-4"),
	}}}

	out, err := newPlazaServiceWithMembers(channels, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.ElementsMatch(t, []string{"claude-sonnet-4", "gpt-image-2", "gpt-image-2.5-flare"}, plazaModelNames(out[0].Models))
}

func TestListPlazaGroups_ModelAllowlistKeepsAllowedUnpricedModel(t *testing.T) {
	channels := []Channel{{
		ID: 1, Name: "mapping-only", Status: StatusActive, GroupIDs: []int64{10},
		ModelMapping: map[string]map[string]string{
			PlatformAnthropic: {"model-unpriced": "model-unpriced"},
		},
	}}
	groups := []Group{{
		ID:             10,
		Name:           "allowlisted-unpriced",
		Platform:       PlatformAnthropic,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"model-unpriced", "model-not-declared"}},
	}}

	out, err := newPlazaService(channels, groups, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 1)
	require.Equal(t, "model-unpriced", out[0].Models[0].Name)
	require.Nil(t, out[0].Models[0].Pricing)
}

func TestListPlazaGroups_ModelAllowlistOmitsGroupWhenNoModelsMatch(t *testing.T) {
	channels := []Channel{
		plazaPricedChannel(1, "associated-active", []int64{10}, PlatformAnthropic, "model-a", "model-b"),
	}
	groups := []Group{{
		ID:             10,
		Name:           "no-allowlist-match",
		Platform:       PlatformAnthropic,
		RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"model-x"}},
	}}

	out, err := newPlazaService(channels, groups, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Empty(t, out)
}

func TestListPlazaGroups_ExposesIndependentVideoRate(t *testing.T) {
	channels := []Channel{
		plazaPricedChannel(1, "ch", []int64{10, 20}, PlatformOpenAI, "grok-imagine-video"),
	}
	groups := []Group{
		{
			ID:                   10,
			Name:                 "video-independent",
			Platform:             PlatformOpenAI,
			RateMultiplier:       0.15,
			ImageRateIndependent: true,
			ImageRateMultiplier:  1.25,
			VideoRateIndependent: true,
			VideoRateMultiplier:  0.25,
		},
		{
			ID:                   20,
			Name:                 "video-group-rate",
			Platform:             PlatformOpenAI,
			RateMultiplier:       0.2,
			ImageRateIndependent: false,
			ImageRateMultiplier:  0.75,
			VideoRateIndependent: false,
			VideoRateMultiplier:  0,
		},
	}

	out, err := newPlazaService(channels, groups, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 2)
	byID := make(map[int64]PlazaGroup, len(out))
	for _, group := range out {
		byID[group.ID] = group
	}

	independent := byID[10]
	require.True(t, independent.VideoRateIndependent)
	require.Equal(t, 0.25, independent.VideoRateMultiplier)
	require.True(t, independent.ImageRateIndependent)
	require.Equal(t, 1.25, independent.ImageRateMultiplier)

	groupRate := byID[20]
	require.False(t, groupRate.VideoRateIndependent)
	require.Equal(t, 0.0, groupRate.VideoRateMultiplier)
	require.False(t, groupRate.ImageRateIndependent)
	require.Equal(t, 0.75, groupRate.ImageRateMultiplier)
}

func TestListPlazaGroups_Fable51HasNoImplicitReasoningMultiplier(t *testing.T) {
	ch := plazaPricedChannel(1, "ch", []int64{10}, "anthropic", "claude-fable-5-1")
	svc := newPlazaService([]Channel{ch}, []Group{{ID: 10, Platform: "anthropic"}}, nil)
	groups, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Len(t, groups[0].Models, 1)
	require.Empty(t, groups[0].Models[0].Pricing.ReasoningEffortMultipliers)
}

func TestListPlazaGroups_DedupFirstWinsWithPricingUpgrade(t *testing.T) {
	// 同名模型:先见者胜;仅当已存条目无定价而新条目有定价时升级替换。
	unpriced := Channel{
		ID: 1, Name: "alpha", Status: StatusActive, GroupIDs: []int64{10},
		// mapping-only → SupportedModels 产出无定价条目
		ModelMapping: map[string]map[string]string{
			"anthropic": {"claude-sonnet": "claude-sonnet"},
		},
	}
	priced := plazaPricedChannel(2, "beta", []int64{10}, "anthropic", "claude-sonnet")
	groups := []Group{{ID: 10, Name: "g", Platform: "anthropic", RateMultiplier: 1}}

	// alpha(无价)按名称序先于 beta(有价):先见者无价,应被有价条目升级。
	svc := newPlazaService([]Channel{priced, unpriced}, groups, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 1)
	require.NotNil(t, out[0].Models[0].Pricing, "无价条目应被有价条目升级")
	require.NotNil(t, out[0].Models[0].Pricing.InputPrice)
}

func TestListPlazaGroups_PlatformIsolation(t *testing.T) {
	// 渠道同时有 anthropic/openai 定价,anthropic 分组只应看到 anthropic 模型。
	ch := Channel{
		ID: 1, Name: "multi", Status: StatusActive, GroupIDs: []int64{10, 20},
		ModelPricing: []ChannelModelPricing{
			{Platform: "anthropic", Models: []string{"claude-sonnet"}, InputPrice: testPtrFloat64(3e-6)},
			{Platform: "openai", Models: []string{"gpt-5"}, InputPrice: testPtrFloat64(2e-6)},
		},
	}
	groups := []Group{
		{ID: 10, Name: "g-claude", Platform: "anthropic", RateMultiplier: 1},
		{ID: 20, Name: "g-gpt", Platform: "openai", RateMultiplier: 1},
	}
	svc := newPlazaService([]Channel{ch}, groups, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 2)
	byName := map[string][]PlazaModel{}
	for _, g := range out {
		byName[g.Name] = g.Models
	}
	require.Len(t, byName["g-claude"], 1)
	require.Equal(t, "claude-sonnet", byName["g-claude"][0].Name)
	require.Len(t, byName["g-gpt"], 1)
	require.Equal(t, "gpt-5", byName["g-gpt"][0].Name)
}

func TestListPlazaGroups_CompositeIncludesConfiguredConcretePlatforms(t *testing.T) {
	anthropicPrice := 3e-6
	openAIPrice := 2e-6
	ch := Channel{
		ID: 1, Name: "multi", Status: StatusActive, GroupIDs: []int64{10},
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformAnthropic, Models: []string{"shared-model"}, InputPrice: &anthropicPrice},
			{Platform: PlatformOpenAI, Models: []string{"shared-model"}, InputPrice: &openAIPrice},
			{Platform: "", Models: []string{"empty-platform"}},
			{Platform: PlatformComposite, Models: []string{"nested-composite"}},
			{Platform: "unknown-platform", Models: []string{"unknown-platform"}},
		},
	}
	groups := []Group{{ID: 10, Name: "composite", Platform: PlatformComposite, RateMultiplier: 1}}
	// 复合组不由渠道推导来源，显式给出两个具体平台各自声明的 shared-model。
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{10: {
		plazaMemberAccount(PlatformAnthropic, "shared-model"),
		plazaMemberAccount(PlatformOpenAI, "shared-model"),
	}}}

	out, err := newPlazaServiceWithMembers([]Channel{ch}, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 2, "only concrete platforms are included and same-named models remain distinct")
	require.Equal(t, PlatformAnthropic, out[0].Models[0].Platform)
	require.Equal(t, PlatformOpenAI, out[0].Models[1].Platform)
	require.InDelta(t, anthropicPrice, *out[0].Models[0].Pricing.InputPrice, 1e-12)
	require.InDelta(t, openAIPrice, *out[0].Models[1].Pricing.InputPrice, 1e-12)
}

func TestListPlazaGroups_CommandCodeAndClineUseGroupModelKeysAndPlatformPricing(t *testing.T) {
	commandPrice := 0.000011
	clinePrice := 0.000022
	channel := Channel{
		ID:       1,
		Name:     "new-platform-pricing",
		Status:   StatusActive,
		GroupIDs: []int64{10, 15, 20},
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformCommandCode, Models: []string{"command-only", "command-target", "command-shared-target", "cross-platform-only", "shared-model", "command-outsider"}, InputPrice: &commandPrice},
			{Platform: PlatformCline, Models: []string{"cline-only", "cline-target", "cline-shared-target", "cross-platform-only", "cross-platform-target", "shared-model", "cline-outsider"}, InputPrice: &clinePrice},
		},
	}
	groups := []Group{
		{ID: 10, Name: "command-code", Platform: PlatformCommandCode, RateMultiplier: 1},
		{ID: 15, Name: "cline", Platform: PlatformCline, RateMultiplier: 1},
		{ID: 20, Name: "composite", Platform: PlatformComposite, RateMultiplier: 1},
	}
	memberWithRenamedMappings := func(platform string, mappings map[string]string) Account {
		raw := make(map[string]any, len(mappings))
		for key, value := range mappings {
			raw[key] = value
		}
		return Account{Platform: platform, Credentials: map[string]any{"model_mapping": raw}}
	}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{
		10: {memberWithRenamedMappings(PlatformCommandCode, map[string]string{
			"shared-model": "command-shared-target",
			"command-only": "command-target",
		})},
		15: {memberWithRenamedMappings(PlatformCline, map[string]string{
			"shared-model": "cline-shared-target",
			"cline-only":   "cline-target",
		})},
		20: {
			memberWithRenamedMappings(PlatformCommandCode, map[string]string{
				"shared-model": "command-shared-target",
				"command-only": "command-target",
			}),
			memberWithRenamedMappings(PlatformCline, map[string]string{
				"shared-model":        "cline-shared-target",
				"cline-only":          "cline-target",
				"cross-platform-only": "cross-platform-target",
			}),
		},
	}}

	out, err := newPlazaServiceWithMembers([]Channel{channel}, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 3)
	byName := map[string]PlazaGroup{}
	for _, group := range out {
		byName[group.Name] = group
	}

	commandModels := byName["command-code"].Models
	require.Equal(t, []string{"command-only", "shared-model"}, plazaModelNames(commandModels))
	for _, model := range commandModels {
		require.Equal(t, PlatformCommandCode, model.Platform)
		require.NotContains(t, []string{"command-target", "command-shared-target", "command-outsider", "cross-platform-only"}, model.Name)
	}

	clineModels := byName["cline"].Models
	require.Equal(t, []string{"cline-only", "shared-model"}, plazaModelNames(clineModels))
	for _, model := range clineModels {
		require.Equal(t, PlatformCline, model.Platform)
		require.NotContains(t, []string{"cline-target", "cline-shared-target", "cline-outsider"}, model.Name)
	}

	compositeModels := byName["composite"].Models
	var sharedByPlatform = make(map[string]PlazaModel)
	var clineCrossPlatformOnly bool
	var compositeNames []string
	for _, model := range compositeModels {
		compositeNames = append(compositeNames, model.Platform+"/"+model.Name)
		if model.Name == "shared-model" {
			sharedByPlatform[model.Platform] = model
		}
		if model.Platform == PlatformCline && model.Name == "cross-platform-only" {
			clineCrossPlatformOnly = true
		}
		require.NotContains(t, []string{"command-target", "command-shared-target", "cline-target", "cline-shared-target", "cross-platform-target", "command-outsider", "cline-outsider"}, model.Name)
	}
	require.ElementsMatch(t, []string{
		PlatformCommandCode + "/command-only",
		PlatformCommandCode + "/shared-model",
		PlatformCline + "/cline-only",
		PlatformCline + "/cross-platform-only",
		PlatformCline + "/shared-model",
	}, compositeNames)
	require.True(t, clineCrossPlatformOnly, "the Cline source key must survive an earlier unsupported Command Code candidate")
	require.Contains(t, sharedByPlatform, PlatformCommandCode)
	require.Contains(t, sharedByPlatform, PlatformCline)
	require.InDelta(t, commandPrice, *sharedByPlatform[PlatformCommandCode].Pricing.InputPrice, 1e-12)
	require.InDelta(t, clinePrice, *sharedByPlatform[PlatformCline].Pricing.InputPrice, 1e-12)
}

func TestListPlazaGroups_CompositeAndOrdinaryGroupsDoNotLeakPlatforms(t *testing.T) {
	ch := Channel{
		ID: 1, Name: "multi", Status: StatusActive, GroupIDs: []int64{10, 20},
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformAnthropic, Models: []string{"claude-sonnet"}, InputPrice: testPtrFloat64(3e-6)},
			{Platform: PlatformOpenAI, Models: []string{"gpt-5"}, InputPrice: testPtrFloat64(2e-6)},
		},
	}
	groups := []Group{
		{ID: 10, Name: "anthropic-only", Platform: PlatformAnthropic, RateMultiplier: 1},
		{ID: 20, Name: "composite", Platform: PlatformComposite, RateMultiplier: 1},
	}
	// 普通组由渠道推导来源；复合组显式给出两个具体平台各自的成员。
	members := plazaMembersFromGroups(groups, []Channel{ch})
	members.accountsByGroup[20] = []Account{
		plazaMemberAccount(PlatformAnthropic, "claude-sonnet"),
		plazaMemberAccount(PlatformOpenAI, "gpt-5"),
	}

	out, err := newPlazaServiceWithMembers([]Channel{ch}, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 2)
	byName := map[string]PlazaGroup{}
	for _, group := range out {
		byName[group.Name] = group
	}
	require.Len(t, byName["anthropic-only"].Models, 1)
	require.Equal(t, []PlazaModel{{
		Name: "claude-sonnet", Platform: PlatformAnthropic, Pricing: byName["anthropic-only"].Models[0].Pricing,
	}}, byName["anthropic-only"].Models)
	require.Len(t, byName["composite"].Models, 2)
	require.Equal(t, []string{"claude-sonnet", "gpt-5"}, []string{
		byName["composite"].Models[0].Name,
		byName["composite"].Models[1].Name,
	})
	require.Equal(t, []string{PlatformAnthropic, PlatformOpenAI}, []string{
		byName["composite"].Models[0].Platform,
		byName["composite"].Models[1].Platform,
	})
}

func TestListPlazaGroups_InactiveChannelSkipped(t *testing.T) {
	inactive := plazaPricedChannel(1, "off", []int64{10}, "anthropic", "claude-sonnet")
	inactive.Status = "inactive"
	groups := []Group{{ID: 10, Name: "g", Platform: "anthropic", RateMultiplier: 1}}
	svc := newPlazaService([]Channel{inactive}, groups, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Empty(t, out)
}

func TestListPlazaGroups_SortedByRateMultiplierAsc(t *testing.T) {
	channels := []Channel{
		plazaPricedChannel(1, "ch", []int64{10, 20, 30}, "anthropic", "claude-sonnet"),
	}
	groups := []Group{
		{ID: 10, Name: "b-standard", Platform: "anthropic", RateMultiplier: 1},
		{ID: 20, Name: "a-standard", Platform: "anthropic", RateMultiplier: 1},
		{ID: 30, Name: "cheap", Platform: "anthropic", RateMultiplier: 0.5},
	}
	svc := newPlazaService(channels, groups, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 3)
	require.Equal(t, "cheap", out[0].Name, "倍率低者在前")
	require.Equal(t, "a-standard", out[1].Name, "同倍率按名称")
	require.Equal(t, "b-standard", out[2].Name)
}

func TestListPlazaGroups_OfficialPricingFill(t *testing.T) {
	pricingSvc := newStubPricingServiceFromMap(map[string]*LiteLLMModelPricing{
		"claude-sonnet": {
			Mode:                                "chat",
			InputCostPerToken:                   3e-6,
			OutputCostPerToken:                  1.5e-5,
			CacheCreationInputTokenCost:         3.75e-6,
			CacheCreationInputTokenCostAbove1hr: 6e-6,
			CacheReadInputTokenCost:             3e-7,
		},
		"token-absent": {Mode: "image_generation", TokenPricingAbsent: true, OutputCostPerImage: 0.04},
	})
	channels := []Channel{
		plazaPricedChannel(1, "ch", []int64{10}, "anthropic", "claude-sonnet", "unknown-model", "token-absent"),
	}
	groups := []Group{{ID: 10, Name: "g", Platform: "anthropic", RateMultiplier: 1}}
	svc := newPlazaService(channels, groups, pricingSvc)
	// 官方价与计费同源：需要计费服务与解析器（官方参考不查渠道，解析器无需渠道服务）。
	svc.billingService = NewBillingService(&config.Config{}, pricingSvc)
	svc.resolver = NewModelPricingResolver(nil, svc.billingService)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Len(t, out[0].Models, 3)

	byName := map[string]PlazaModel{}
	for _, m := range out[0].Models {
		byName[m.Name] = m
	}
	// 命中:填充完整官方价(含 1h 缓存写入)
	official := byName["claude-sonnet"].OfficialPricing
	require.NotNil(t, official)
	require.InDelta(t, 3e-6, *official.InputPrice, 1e-12)
	require.InDelta(t, 6e-6, *official.CacheWrite1hPrice, 1e-12)
	require.InDelta(t, 3e-7, *official.CacheReadPrice, 1e-12)
	// 未命中:nil(GetModelPricing 的 claude 系列模糊匹配对非 claude 名不生效)
	require.Nil(t, byName["unknown-model"].OfficialPricing)
	// TokenPricingAbsent 条目不作为官方 token 价展示
	require.Nil(t, byName["token-absent"].OfficialPricing)
}

func TestListPlazaGroups_GroupImagePriceOverridesChannelPricing(t *testing.T) {
	// 图片计费模型:档位价按实收口径合成(分组图片价 > 渠道档位价 > 渠道默认按次价),
	// 分组独立倍率字段透传;未配图片价的分组保持渠道定价原样。
	perReq := 0.2
	tier4K := 0.3
	imgPrice := 0.02
	channels := []Channel{{
		ID: 1, Name: "img-ch", Status: StatusActive, GroupIDs: []int64{10, 20},
		ModelPricing: []ChannelModelPricing{{
			Platform:        "openai",
			Models:          []string{"gpt-image-2"},
			BillingMode:     BillingModeImage,
			PerRequestPrice: &perReq,
			Intervals:       []PricingInterval{{TierLabel: "4K", PerRequestPrice: &tier4K}},
		}},
	}}
	groups := []Group{
		{ID: 10, Name: "g-media", Platform: "openai", RateMultiplier: 1,
			ImagePrice1K: &imgPrice, ImageRateIndependent: true, ImageRateMultiplier: 1,
			VideoRateIndependent: true, VideoRateMultiplier: 0.7},
		{ID: 20, Name: "g-plain", Platform: "openai", RateMultiplier: 0.1},
	}
	svc := newPlazaService(channels, groups, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 2)
	byName := map[string]PlazaGroup{}
	for _, g := range out {
		byName[g.Name] = g
	}

	media := byName["g-media"]
	require.True(t, media.ImageRateIndependent)
	require.True(t, media.VideoRateIndependent)
	require.InDelta(t, 0.7, media.VideoRateMultiplier, 1e-9)
	require.InDelta(t, 1.0, media.ImageRateMultiplier, 1e-9)
	require.Len(t, media.Models, 1)
	p := media.Models[0].Pricing
	require.NotNil(t, p)
	require.Len(t, p.Intervals, 3)
	tierPrices := map[string]float64{}
	for _, iv := range p.Intervals {
		require.NotNil(t, iv.PerRequestPrice)
		tierPrices[iv.TierLabel] = *iv.PerRequestPrice
	}
	require.InDelta(t, 0.02, tierPrices["1K"], 1e-9, "1K 用分组图片价")
	require.InDelta(t, 0.2, tierPrices["2K"], 1e-9, "2K 分组未配,回落渠道默认按次价")
	require.InDelta(t, 0.3, tierPrices["4K"], 1e-9, "4K 分组未配,回落渠道档位价")

	plain := byName["g-plain"]
	require.False(t, plain.ImageRateIndependent)
	require.False(t, plain.VideoRateIndependent)
	require.Len(t, plain.Models, 1)
	pp := plain.Models[0].Pricing
	require.NotNil(t, pp)
	require.Len(t, pp.Intervals, 1, "未配分组图片价:渠道定价原样")
	require.InDelta(t, 0.2, *pp.PerRequestPrice, 1e-9)

	// 合成为克隆,渠道原始定价不被修改
	require.Len(t, channels[0].ModelPricing[0].Intervals, 1)
}

func TestListPlazaGroups_GroupImagePriceIgnoredForNonImageModes(t *testing.T) {
	// token 模式定价不受分组图片价影响。
	imgPrice := 0.02
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, "openai", "gpt-5")}
	groups := []Group{{ID: 10, Name: "g", Platform: "openai", RateMultiplier: 1, ImagePrice1K: &imgPrice}}
	svc := newPlazaService(channels, groups, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	p := out[0].Models[0].Pricing
	require.NotNil(t, p)
	require.Empty(t, p.Intervals)
	require.NotNil(t, p.InputPrice)
	require.Nil(t, p.PerRequestPrice)
}

func TestListPlazaGroups_RepoErrorsPropagate(t *testing.T) {
	sentinel := errors.New("boom")
	repo := &mockChannelRepository{
		listAllFn: func(ctx context.Context) ([]Channel, error) { return nil, sentinel },
	}
	svc := NewModelPlazaService(repo, &stubGroupRepoForAvailable{}, nil, nil, nil, nil)
	out, err := svc.ListGroups(context.Background())
	require.Nil(t, out)
	require.ErrorIs(t, err, sentinel)

	svc2 := NewModelPlazaService(
		&mockChannelRepository{listAllFn: func(ctx context.Context) ([]Channel, error) { return nil, nil }},
		&stubGroupRepoForAvailable{listActiveErr: sentinel},
		nil, nil, nil, nil,
	)
	out2, err2 := svc2.ListGroups(context.Background())
	require.Nil(t, out2)
	require.ErrorIs(t, err2, sentinel)
}

// newPlazaServiceWithBilling 构造接入计费服务与解析器的广场服务：解析器的渠道服务与广场共用同一份渠道数据。
func newPlazaServiceWithBilling(channels []Channel, groups []Group, groupPlatforms map[int64]string, catalog *PricingService) *ModelPlazaService {
	repo := &mockChannelRepository{
		listAllFn: func(ctx context.Context) ([]Channel, error) { return channels, nil },
		getGroupPlatformsFn: func(ctx context.Context, _ []int64) (map[int64]string, error) {
			return groupPlatforms, nil
		},
	}
	cs := NewChannelService(repo, nil, nil, nil, nil)
	bs := NewBillingService(&config.Config{}, catalog)
	return NewModelPlazaService(repo, &stubGroupRepoForAvailable{activeGroups: groups}, plazaMembersFromGroups(groups, channels), catalog, bs, NewModelPricingResolver(cs, bs))
}

func plazaModelsByName(models []PlazaModel) map[string]PlazaModel {
	out := make(map[string]PlazaModel, len(models))
	for _, m := range models {
		out[m.Name] = m
	}
	return out
}

func TestListGroups_TokenLadderFollowsGroupToggle(t *testing.T) {
	// 同一渠道挂开启/关闭阶梯的两个分组：实付档位随分组开关，官方阶梯不受影响。
	channels := []Channel{{
		ID: 1, Name: "ch", Status: StatusActive, GroupIDs: []int64{10, 20},
		ModelPricing: []ChannelModelPricing{{Platform: PlatformOpenAI, Models: []string{"gpt-5.4"}, BillingMode: BillingModeToken}},
	}}
	groups := []Group{
		{ID: 10, Name: "on", Platform: PlatformOpenAI, RateMultiplier: 1, LongContextPricingEnabled: true},
		{ID: 20, Name: "off", Platform: PlatformOpenAI, RateMultiplier: 2, LongContextPricingEnabled: false},
	}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{10: PlatformOpenAI, 20: PlatformOpenAI},
		newStubPricingServiceFromJSON(t, openAILadderCatalogJSON))
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 2)

	on, off := out[0], out[1]
	require.True(t, on.LongContextPricingEnabled)
	require.False(t, off.LongContextPricingEnabled)

	onModel := on.Models[0]
	require.Equal(t, ContextPricingBasisWholeRequest, onModel.LongContextBasis)
	require.Len(t, onModel.Pricing.Intervals, 2)
	require.Equal(t, "≤272K", onModel.Pricing.Intervals[0].TierLabel)
	require.Equal(t, ">272K", onModel.Pricing.Intervals[1].TierLabel)
	require.InDelta(t, 2.5e-6, *onModel.Pricing.InputPrice, 1e-15)
	require.InDelta(t, 5e-6, *onModel.Pricing.Intervals[1].InputPrice, 1e-15)
	require.InDelta(t, 22.5e-6, *onModel.Pricing.Intervals[1].OutputPrice, 1e-15)
	require.InDelta(t, 5e-6, *onModel.Pricing.Intervals[1].CacheWritePrice, 1e-15)
	require.InDelta(t, 0.5e-6, *onModel.Pricing.Intervals[1].CacheReadPrice, 1e-15)

	offModel := off.Models[0]
	require.Empty(t, offModel.LongContextBasis)
	require.Empty(t, offModel.Pricing.Intervals)
	require.InDelta(t, 2.5e-6, *offModel.Pricing.InputPrice, 1e-15)

	for _, m := range []PlazaModel{onModel, offModel} {
		require.NotNil(t, m.OfficialPricing)
		require.Len(t, m.OfficialPricing.Intervals, 2, "官方阶梯不受分组开关影响")
		require.InDelta(t, 5e-6, *m.OfficialPricing.Intervals[1].InputPrice, 1e-15)
		require.InDelta(t, 2.5e-6, *m.OfficialPricing.InputPrice, 1e-15)
	}
}

func TestListGroups_GeminiCatalogLadderShownWholeRequest(t *testing.T) {
	channels := []Channel{{
		ID: 1, Name: "ch", Status: StatusActive, GroupIDs: []int64{10},
		ModelMapping: map[string]map[string]string{PlatformGemini: {"gemini-2.5-pro": "gemini-2.5-pro"}},
	}}
	groups := []Group{{ID: 10, Name: "g", Platform: PlatformGemini, RateMultiplier: 1, LongContextPricingEnabled: true}}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{10: PlatformGemini},
		newStubPricingServiceFromJSON(t, geminiLadderCatalogJSON))
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	m := out[0].Models[0]
	require.Equal(t, ContextPricingBasisWholeRequest, m.LongContextBasis)
	require.Len(t, m.Pricing.Intervals, 2)
	require.Equal(t, "≤200K", m.Pricing.Intervals[0].TierLabel)
	require.Equal(t, ">200K", m.Pricing.Intervals[1].TierLabel)
	require.InDelta(t, 2.5e-6, *m.Pricing.Intervals[1].InputPrice, 1e-15)
	require.InDelta(t, 15e-6, *m.Pricing.Intervals[1].OutputPrice, 1e-15)
	// 官方参考价与实付同源：都来自目录数据的阶梯字段
	require.NotNil(t, m.OfficialPricing)
	require.Len(t, m.OfficialPricing.Intervals, 2)
}

func TestListGroups_GroupTokenCardOverridesChannelPricing(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, PlatformAnthropic, "claude-sonnet-4")}
	groups := []Group{{
		ID: 10, Name: "g", Platform: PlatformAnthropic, RateMultiplier: 1, LongContextPricingEnabled: true,
		ModelPricing: []ChannelModelPricing{{Models: []string{"claude-sonnet-*"}, BillingMode: BillingModeToken, InputPrice: testPtrFloat64(1e-6)}},
	}}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{10: PlatformAnthropic}, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	m := out[0].Models[0]
	require.InDelta(t, 1e-6, *m.Pricing.InputPrice, 1e-15, "分组价卡优先于渠道平价")
	require.InDelta(t, 15e-6, *m.Pricing.OutputPrice, 1e-15, "卡未配置的项回落目录价")
	require.Empty(t, m.Pricing.Intervals)
}

func TestListGroups_ImageModelKeepsTierSynthesisWithBilling(t *testing.T) {
	channels := []Channel{{
		ID: 1, Name: "ch", Status: StatusActive, GroupIDs: []int64{10},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformOpenAI, Models: []string{"gpt-image-2"}, BillingMode: BillingModeImage,
			PerRequestPrice: testPtrFloat64(0.04),
		}},
	}}
	groups := []Group{{
		ID: 10, Name: "g", Platform: PlatformOpenAI, RateMultiplier: 1, LongContextPricingEnabled: true,
		ImagePrice1K: testPtrFloat64(0.02),
	}}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{10: PlatformOpenAI}, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	m := out[0].Models[0]
	require.Equal(t, BillingModeImage, m.Pricing.BillingMode)
	require.Empty(t, m.LongContextBasis)
	require.Len(t, m.Pricing.Intervals, 3)
	require.InDelta(t, 0.02, *m.Pricing.Intervals[0].PerRequestPrice, 1e-12)
	require.InDelta(t, 0.04, *m.Pricing.Intervals[1].PerRequestPrice, 1e-12)
}

func TestListGroups_CatalogMissingStillShowsChannelFlatPricing(t *testing.T) {
	// 目录查不到的模型：计费按渠道平价（未配置项 $0），广场单档展示渠道平价，官方价为空。
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, PlatformAnthropic, "unknown-model-xyz")}
	groups := []Group{{ID: 10, Name: "g", Platform: PlatformAnthropic, RateMultiplier: 1, LongContextPricingEnabled: true}}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{10: PlatformAnthropic}, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	m := out[0].Models[0]
	require.NotNil(t, m.Pricing)
	require.InDelta(t, 3e-6, *m.Pricing.InputPrice, 1e-15)
	require.Empty(t, m.Pricing.Intervals)
	require.Nil(t, m.Pricing.CacheWritePrice, "目录无价且渠道未配置 → 无价")
	require.Nil(t, m.OfficialPricing)
}

func TestOfficialPricingStableSnapshot(t *testing.T) {
	const catalogAJSON = `{"gpt-5.4":{"litellm_provider":"openai","mode":"chat",
		"input_cost_per_token":2.5e-6,"output_cost_per_token":15e-6,
		"cache_read_input_token_cost":0.25e-6,"cache_creation_input_token_cost":3.75e-6,
		"cache_creation_input_token_cost_above_1hr":8e-6,
		"input_cost_per_token_above_200k_tokens":5e-6,"output_cost_per_token_above_200k_tokens":30e-6,
		"cache_read_input_token_cost_above_200k_tokens":0.5e-6,"cache_creation_input_token_cost_above_200k_tokens":6e-6,
		"cache_creation_input_token_cost_above_1hr_above_200k_tokens":10e-6}}`
	const catalogBJSON = `{"gpt-5.4":{"litellm_provider":"openai","mode":"chat",
		"input_cost_per_token":7.5e-6,"output_cost_per_token":45e-6,
		"cache_read_input_token_cost":0.75e-6,"cache_creation_input_token_cost":11.25e-6,
		"cache_creation_input_token_cost_above_1hr":24e-6,
		"input_cost_per_token_above_100k_tokens":15e-6,"output_cost_per_token_above_100k_tokens":90e-6,
		"cache_read_input_token_cost_above_100k_tokens":1.5e-6,"cache_creation_input_token_cost_above_100k_tokens":18e-6,
		"cache_creation_input_token_cost_above_1hr_above_100k_tokens":36e-6}}`
	catalogA := newStubPricingServiceFromJSON(t, catalogAJSON)
	catalogB := newStubPricingServiceFromJSON(t, catalogBJSON)
	catalog := newStubPricingServiceFromJSON(t, catalogAJSON)
	bs := NewBillingService(&config.Config{}, catalog)
	resolver := NewModelPricingResolver(nil, bs)
	svc := &ModelPlazaService{billingService: bs, resolver: resolver}

	validateSnapshot := func(got *PlazaOfficialPricing) error {
		if got == nil || len(got.Intervals) != 2 {
			return errors.New("official pricing summary or context tiers missing")
		}
		if got.InputPrice == nil || got.OutputPrice == nil || got.CacheWritePrice == nil || got.CacheWrite1hPrice == nil || got.CacheReadPrice == nil {
			return errors.New("one of the five official summary prices is missing")
		}
		for _, value := range []float64{*got.InputPrice, *got.OutputPrice, *got.CacheWritePrice, *got.CacheWrite1hPrice, *got.CacheReadPrice} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return errors.New("official summary contains an invalid price")
			}
		}
		topInput := got.Intervals[1].InputPrice
		if topInput == nil {
			return errors.New("top context tier input price missing")
		}
		switch {
		case math.Abs(*got.InputPrice-2.5e-6) < 1e-15:
			if math.Abs(*got.OutputPrice-15e-6) > 1e-15 || math.Abs(*got.CacheWritePrice-3.75e-6) > 1e-15 ||
				math.Abs(*got.CacheWrite1hPrice-8e-6) > 1e-15 || math.Abs(*got.CacheReadPrice-0.25e-6) > 1e-15 || math.Abs(*topInput-5e-6) > 1e-15 {
				return errors.New("catalog A summary and tiers came from different generations")
			}
		case math.Abs(*got.InputPrice-7.5e-6) < 1e-15:
			if math.Abs(*got.OutputPrice-45e-6) > 1e-15 || math.Abs(*got.CacheWritePrice-11.25e-6) > 1e-15 ||
				math.Abs(*got.CacheWrite1hPrice-24e-6) > 1e-15 || math.Abs(*got.CacheReadPrice-0.75e-6) > 1e-15 || math.Abs(*topInput-15e-6) > 1e-15 {
				return errors.New("catalog B summary and tiers came from different generations")
			}
		default:
			return errors.New("summary does not match either controlled catalog")
		}
		return nil
	}

	// Check each generation explicitly before racing reloads.
	for _, source := range []*PricingService{catalogA, catalogB} {
		catalog.mu.Lock()
		catalog.pricingData = source.pricingData
		catalog.mu.Unlock()
		if err := validateSnapshot(svc.lookupOfficialPricing(context.Background(), "gpt-5.4", make(map[string]*PlazaOfficialPricing))); err != nil {
			t.Fatal(err)
		}
	}

	stop := make(chan struct{})
	var swapper sync.WaitGroup
	swapper.Add(1)
	go func() {
		defer swapper.Done()
		useB := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			source := catalogA
			if useB {
				source = catalogB
			}
			catalog.mu.Lock()
			catalog.pricingData = source.pricingData
			catalog.mu.Unlock()
			useB = !useB
		}
	}()

	const requests = 100
	results := make([]*PlazaOfficialPricing, requests)
	requestErrors := make([]error, requests)
	var workers sync.WaitGroup
	for i := range results {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			results[index] = svc.lookupOfficialPricing(context.Background(), "gpt-5.4", make(map[string]*PlazaOfficialPricing))
			requestErrors[index] = validateSnapshot(results[index])
		}(i)
	}
	workers.Wait()
	close(stop)
	swapper.Wait()
	for _, err := range requestErrors {
		require.NoError(t, err)
	}

	// Nil dependencies and missing official prices keep their existing degradation.
	require.Nil(t, (&ModelPlazaService{}).lookupOfficialPricing(context.Background(), "gpt-5.4", nil))
	missingBilling := NewBillingService(&config.Config{}, &PricingService{pricingData: map[string]*LiteLLMModelPricing{}})
	missingResolver := NewModelPricingResolver(nil, missingBilling)
	missing := (&ModelPlazaService{billingService: missingBilling, resolver: missingResolver}).lookupOfficialPricing(context.Background(), "unknown-official-model", make(map[string]*PlazaOfficialPricing))
	require.Nil(t, missing)

	invalidCatalog := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gpt-5.4": {
			Mode: "chat", InputCostPerToken: math.NaN(), OutputCostPerToken: 1e-5,
			LongContextInputTokenThreshold: 10, LongContextInputCostMultiplier: 2,
		},
	}}
	invalidBilling := NewBillingService(&config.Config{}, invalidCatalog)
	invalidResolver := NewModelPricingResolver(nil, invalidBilling)
	degraded := (&ModelPlazaService{billingService: invalidBilling, resolver: invalidResolver}).lookupOfficialPricing(
		context.Background(), "gpt-5.4", make(map[string]*PlazaOfficialPricing),
	)
	require.NotNil(t, degraded, "a schedule probe error retains the official summary")
	require.Empty(t, degraded.Intervals)

	channels := []Channel{plazaPricedChannel(1, "shared", []int64{10, 20}, PlatformAnthropic,
		"claude-opus-4-6", "claude-opus-4-6-thinking")}
	groups := []Group{
		{ID: 10, Name: "native", Platform: PlatformAnthropic, RateMultiplier: 1},
		{ID: 20, Name: "antigravity", Platform: PlatformAnthropic, RateMultiplier: 1},
	}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{
		10: {plazaMemberAccount(PlatformAnthropic, "claude-opus-4-6")},
		20: {plazaMemberAccount(PlatformAntigravity, "claude-opus-4-6-thinking")},
	}}
	isolated, err := newPlazaServiceWithMembers(channels, groups, members, nil).ListGroups(context.Background())
	require.NoError(t, err)
	require.Len(t, isolated, 2)
	byName := make(map[string][]string, len(isolated))
	for _, group := range isolated {
		byName[group.Name] = plazaModelNames(group.Models)
	}
	require.Equal(t, []string{"claude-opus-4-6"}, byName["native"])
	require.Equal(t, []string{"claude-opus-4-6-thinking"}, byName["antigravity"])
}

func TestListGroups_TimePricingPassthrough(t *testing.T) {
	channels := []Channel{{
		ID: 1, Name: "ch", Status: StatusActive, GroupIDs: []int64{10},
		ModelPricing: []ChannelModelPricing{{
			Platform: PlatformDeepseek, Models: []string{"deepseek-chat"}, BillingMode: BillingModeToken,
			InputPrice: testPtrFloat64(0.28e-6), OutputPrice: testPtrFloat64(0.42e-6),
			TimePricing: &ChannelTimePricing{Timezone: "Asia/Shanghai", Periods: []ChannelTimePricingPeriod{
				{StartTime: "00:30", EndTime: "08:30", Multiplier: 0.5},
			}},
		}},
	}}
	groups := []Group{{ID: 10, Name: "cn", Platform: PlatformDeepseek, RateMultiplier: 1, LongContextPricingEnabled: true}}
	svc := newPlazaServiceWithBilling(channels, groups, map[int64]string{10: PlatformDeepseek}, nil)
	out, err := svc.ListGroups(context.Background())
	require.NoError(t, err)
	m := out[0].Models[0]
	require.NotNil(t, m.TimePricing)
	require.Equal(t, "Asia/Shanghai", m.TimePricing.Timezone)
	require.Len(t, m.TimePricing.Periods, 1)
	require.InDelta(t, 0.5, m.TimePricing.Periods[0].Multiplier, 1e-12)
	// 展示单价为标准时段价
	require.InDelta(t, 0.28e-6, *m.Pricing.InputPrice, 1e-15)
}

// 原生 Anthropic 的基础 ID 与 Antigravity 显式声明的 thinking 变体互不借用：
// 组内谁声明谁展示，基础 ID 不代表 thinking 变体，thinking 变体也不代表基础 ID。
func TestListPlazaGroups_ThinkingCatalogIsolation(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10, 20, 30}, PlatformAnthropic,
		"claude-opus-4-6", "claude-opus-4-6-thinking", "claude-sonnet-4-6")}
	groups := []Group{
		{ID: 10, Name: "native-only", Platform: PlatformAnthropic, RateMultiplier: 1},
		{ID: 20, Name: "antigravity-only", Platform: PlatformAnthropic, RateMultiplier: 1},
		{ID: 30, Name: "both", Platform: PlatformAnthropic, RateMultiplier: 1},
	}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{
		10: {plazaMemberAccount(PlatformAnthropic, "claude-opus-4-6")},
		20: {plazaMemberAccount(PlatformAntigravity, "claude-opus-4-6-thinking")},
		30: {
			plazaMemberAccount(PlatformAnthropic, "claude-opus-4-6"),
			plazaMemberAccount(PlatformAntigravity, "claude-opus-4-6-thinking"),
		},
	}}

	out, err := newPlazaServiceWithMembers(channels, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 3)
	byName := map[string][]string{}
	for _, group := range out {
		byName[group.Name] = plazaModelNames(group.Models)
	}
	require.Equal(t, []string{"claude-opus-4-6"}, byName["native-only"])
	require.Equal(t, []string{"claude-opus-4-6-thinking"}, byName["antigravity-only"])
	require.Equal(t, []string{"claude-opus-4-6", "claude-opus-4-6-thinking"}, byName["both"])
}

// 成员账号仓储不可用时整体失败关闭，不退化为按渠道推导来源。
func TestListPlazaGroups_MissingMemberRepositoryFailsClosed(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, PlatformAnthropic, "claude-sonnet")}
	groups := []Group{{ID: 10, Name: "g", Platform: PlatformAnthropic, RateMultiplier: 1}}

	out, err := newPlazaServiceWithMembers(channels, groups, nil, nil).ListGroups(context.Background())

	require.Nil(t, out)
	require.ErrorContains(t, err, "account group member repository is unavailable")
}

// 未启用白名单的组同样读取成员，成员读取失败不得被忽略。
func TestListPlazaGroups_DisabledMemberReadFailureFailsClosed(t *testing.T) {
	sentinel := errors.New("disabled group member read failed")
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, PlatformAnthropic, "claude-sonnet")}
	groups := []Group{{
		ID: 10, Name: "g", Platform: PlatformAnthropic, RateMultiplier: 1,
		ModelAllowlist: GroupModelAllowlist{Enabled: false},
	}}

	out, err := newPlazaServiceWithMembers(channels, groups, &modelPlazaMembersRepoStub{err: sentinel}, nil).ListGroups(context.Background())

	require.Nil(t, out)
	require.ErrorIs(t, err, sentinel)
}

// 无成员的组不展示任何候选，即使挂载的渠道提供了模型。
func TestListPlazaGroups_GroupWithoutMembersOmitted(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10, 20}, PlatformAnthropic, "claude-sonnet")}
	groups := []Group{
		{ID: 10, Name: "no-members", Platform: PlatformAnthropic, RateMultiplier: 1},
		{ID: 20, Name: "with-member", Platform: PlatformAnthropic, RateMultiplier: 1},
	}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{
		20: {plazaMemberAccount(PlatformAnthropic, "claude-sonnet")},
	}}

	out, err := newPlazaServiceWithMembers(channels, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, "with-member", out[0].Name)
	require.Equal(t, []string{"claude-sonnet"}, plazaModelNames(out[0].Models))
}

// 账号暂停（不可调度）或异常不影响来源声明：目录只表达组已声明的模型。
func TestListPlazaGroups_PausedMembersStillDeclare(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10}, PlatformAnthropic, "claude-sonnet", "claude-opus")}
	groups := []Group{{ID: 10, Name: "g", Platform: PlatformAnthropic, RateMultiplier: 1}}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{10: {
		{Platform: PlatformAnthropic, Status: StatusActive, Schedulable: false, Credentials: map[string]any{
			"model_mapping": map[string]any{"claude-sonnet": "claude-sonnet"},
		}},
		{Platform: PlatformAnthropic, Status: StatusError, ErrorMessage: "expired", Schedulable: false, Credentials: map[string]any{
			"model_mapping": map[string]any{"claude-opus": "claude-opus"},
		}},
	}}}

	out, err := newPlazaServiceWithMembers(channels, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	require.ElementsMatch(t, []string{"claude-opus", "claude-sonnet"}, plazaModelNames(out[0].Models))
}

// 复合组中同名模型只由其所属平台的成员声明：另一平台的同名候选不得借来源显示。
func TestListPlazaGroups_CompositeSameNameDeclaredOnlyByOwningPlatform(t *testing.T) {
	ch := Channel{
		ID: 1, Name: "multi", Status: StatusActive, GroupIDs: []int64{10},
		ModelPricing: []ChannelModelPricing{
			{Platform: PlatformAnthropic, Models: []string{"shared"}, InputPrice: testPtrFloat64(3e-6)},
			{Platform: PlatformOpenAI, Models: []string{"shared", "other"}, InputPrice: testPtrFloat64(2e-6)},
		},
	}
	groups := []Group{{ID: 10, Name: "composite", Platform: PlatformComposite, RateMultiplier: 1}}
	members := &modelPlazaMembersRepoStub{accountsByGroup: map[int64][]Account{10: {
		plazaMemberAccount(PlatformAnthropic, "shared"),
		plazaMemberAccount(PlatformOpenAI, "other"),
	}}}

	out, err := newPlazaServiceWithMembers([]Channel{ch}, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 1)
	got := make([]string, 0, len(out[0].Models))
	for _, m := range out[0].Models {
		got = append(got, m.Platform+"/"+m.Name)
	}
	require.ElementsMatch(t, []string{PlatformAnthropic + "/shared", PlatformOpenAI + "/other"}, got)
}

// 每个活跃分组恰好读取一次成员快照，启用白名单与否都读取。
func TestListPlazaGroups_ReadsMembersOncePerActiveGroup(t *testing.T) {
	channels := []Channel{plazaPricedChannel(1, "ch", []int64{10, 20}, PlatformAnthropic, "claude-sonnet", "claude-opus")}
	groups := []Group{
		{ID: 10, Name: "enabled", Platform: PlatformAnthropic, RateMultiplier: 1,
			ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"claude-sonnet"}}},
		{ID: 20, Name: "disabled", Platform: PlatformAnthropic, RateMultiplier: 1},
	}
	members := plazaMembersFromGroups(groups, channels)

	out, err := newPlazaServiceWithMembers(channels, groups, members, nil).ListGroups(context.Background())

	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, map[int64]int{10: 1, 20: 1}, members.calls)
}
