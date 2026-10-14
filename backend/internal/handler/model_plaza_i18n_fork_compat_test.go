//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	plazaAlphaGroupID   int64 = 17
	plazaBetaGroupID    int64 = 18
	plazaPrivateGroupID int64 = 19
	plazaSharedModel          = "claude-sonnet-4"
)

func TestLocalizedPlazaKeepsGroupCatalogAndPrices(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, _, assertSourcesUnchanged := newLocalizedPlazaScenario(t)

	zh := requestModelPlaza(t, router, "https://a.example", "zh", false)
	require.NotEmpty(t, zh.Groups)
	require.Len(t, zh.Groups, 2, "anonymous users see both public groups and no exclusive group")
	zhGroups := plazaGroupsByID(zh.Groups)
	alpha := zhGroups[plazaAlphaGroupID]
	beta := zhGroups[plazaBetaGroupID]
	require.Equal(t, "甲组", alpha.Name)
	require.Equal(t, "甲组说明", alpha.Description)
	require.Equal(t, "乙组", beta.Name)
	require.Equal(t, "乙组说明", beta.Description)
	require.Equal(t, service.PlatformAnthropic, alpha.Platform)
	require.Equal(t, service.PlatformAnthropic, beta.Platform)
	require.Equal(t, 1.25, alpha.RateMultiplier)
	require.Equal(t, 0.75, beta.RateMultiplier)
	assertLocalizedPlazaGroupModels(t, alpha, []string{plazaSharedModel, "group-a-alias"}, 0.000006, 0.000012)
	assertLocalizedPlazaGroupModels(t, beta, []string{plazaSharedModel, "group-b-alias"}, 0.000009, 0.000018)
	assertPlazaCustomPrice(t, alpha, "group-a-alias", 0.000004, 0.000008)
	assertPlazaCustomPrice(t, beta, "group-b-alias", 0.000005, 0.00001)
	assertPlazaOfficialPrice(t, alpha, 0.000003, 0.000015)
	assertPlazaOfficialPrice(t, beta, 0.000003, 0.000015)
	assertPlazaNoI18nLeak(t, zh)

	en := requestModelPlaza(t, router, "https://b.example", "en", false)
	require.Len(t, en.Groups, 2)
	enGroups := plazaGroupsByID(en.Groups)
	require.Equal(t, "Alpha", enGroups[plazaAlphaGroupID].Name)
	require.Equal(t, "Alpha public pool", enGroups[plazaAlphaGroupID].Description)
	require.Equal(t, "Beta", enGroups[plazaBetaGroupID].Name)
	require.Equal(t, "Beta public pool", enGroups[plazaBetaGroupID].Description)
	require.Equal(t, plazaLanguageInvariants(t, zh.Groups), plazaLanguageInvariants(t, en.Groups),
		"only group name/description may change between languages; model set, sources, custom/official prices and multipliers stay identical")
	assertPlazaNoI18nLeak(t, en)

	authorized := requestModelPlaza(t, router, "https://a.example", "en", true)
	require.Len(t, authorized.Groups, 3, "the simulated user is authorized for the exclusive group")
	authorizedGroups := plazaGroupsByID(authorized.Groups)
	private := authorizedGroups[plazaPrivateGroupID]
	require.Equal(t, "Private", private.Name)
	require.Equal(t, "Private pool", private.Description)
	require.True(t, private.IsExclusive)
	require.Equal(t, 2.0, private.RateMultiplier)
	assertLocalizedPlazaGroupModels(t, private, []string{"private-model"}, 0.000002, 0.000004)
	assertPlazaCustomPrice(t, private, "private-model", 0.000002, 0.000004)
	require.NotContains(t, zhGroups, plazaPrivateGroupID)
	require.NotContains(t, enGroups, plazaPrivateGroupID)
	authorizedPublic := make([]modelPlazaGroup, 0, 2)
	for _, group := range authorized.Groups {
		if group.ID != plazaPrivateGroupID {
			authorizedPublic = append(authorizedPublic, group)
		}
	}
	require.Equal(t, plazaLanguageInvariants(t, en.Groups), plazaLanguageInvariants(t, authorizedPublic),
		"authorization only adds the exclusive group and leaves public groups unchanged")
	assertPlazaNoI18nLeak(t, authorized)
	assertSourcesUnchanged(t)
}

// plazaLanguageInvariants 去掉随语言变化的名称/描述，按分组 ID 给出其余字段（模型集合按名称排序）的 JSON。
func plazaLanguageInvariants(t *testing.T, groups []modelPlazaGroup) map[int64]string {
	t.Helper()
	out := make(map[int64]string, len(groups))
	for _, group := range groups {
		group.Name, group.Description = "", ""
		group.Models = append([]modelPlazaModel(nil), group.Models...)
		sort.Slice(group.Models, func(i, j int) bool { return group.Models[i].Name < group.Models[j].Name })
		raw, err := json.Marshal(group)
		require.NoError(t, err)
		out[group.ID] = string(raw)
	}
	return out
}

type plazaVarySettingRepo struct{ service.SettingRepository }

func (plazaVarySettingRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{service.SettingKeyModelPlazaEnabled: "true"}, nil
}

// 以下仓储桩与真实仓储一样每次读出新切片：服务对读出副本的就地规范化/排序不算改源对象，
// 但切片元素里共享的 map/指针（价格、I18n、凭据）仍与源对象同一份，任何写穿都会被源快照断言发现。
type plazaVaryChannelRepo struct {
	service.ChannelRepository
	channels []service.Channel
}

func (r plazaVaryChannelRepo) ListAll(context.Context) ([]service.Channel, error) {
	return append([]service.Channel(nil), r.channels...), nil
}

type plazaVaryGroupRepo struct {
	service.GroupRepository
	groups []service.Group
}

func (r plazaVaryGroupRepo) ListActive(context.Context) ([]service.Group, error) {
	return append([]service.Group(nil), r.groups...), nil
}

type plazaScenarioMemberRepo struct {
	service.AccountRepository
	accountsByGroup map[int64][]service.Account
}

func (r plazaScenarioMemberRepo) ListAllByGroup(_ context.Context, groupID int64) ([]service.Account, error) {
	return append([]service.Account(nil), r.accountsByGroup[groupID]...), nil
}

type plazaScenarioUserRepo struct {
	service.UserRepository
	user *service.User
}

func (r plazaScenarioUserRepo) GetByID(context.Context, int64) (*service.User, error) {
	return r.user, nil
}

type plazaScenarioSubscriptionRepo struct {
	service.UserSubscriptionRepository
}

func (plazaScenarioSubscriptionRepo) ListActiveByUserID(context.Context, int64) ([]service.UserSubscription, error) {
	return nil, nil
}

func newLocalizedPlazaScenario(t *testing.T) (*gin.Engine, *service.BillingService, func(*testing.T)) {
	t.Helper()
	channelPrice := func(model string, input, output float64) service.ChannelModelPricing {
		return service.ChannelModelPricing{
			Platform: service.PlatformAnthropic, Models: []string{model}, BillingMode: service.BillingModeToken,
			InputPrice: plazaPricePointer(input), OutputPrice: plazaPricePointer(output),
		}
	}
	groupPrice := func(models []string, input, output float64) service.ChannelModelPricing {
		return service.ChannelModelPricing{
			Platform: service.PlatformAnthropic, Models: models, BillingMode: service.BillingModeToken,
			InputPrice: plazaPricePointer(input), OutputPrice: plazaPricePointer(output),
		}
	}
	groups := []service.Group{
		{
			ID: plazaAlphaGroupID, Name: "Alpha base", Description: "Alpha base description", Platform: service.PlatformAnthropic,
			Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1.25,
			I18n:         service.GroupI18n{"zh": {Name: "甲组", Description: "甲组说明"}, "en": {Name: "Alpha", Description: "Alpha public pool"}},
			ModelPricing: []service.ChannelModelPricing{groupPrice([]string{plazaSharedModel}, 0.000006, 0.000012), groupPrice([]string{"group-a-alias"}, 0.000004, 0.000008)},
		},
		{
			ID: plazaBetaGroupID, Name: "Beta base", Description: "Beta base description", Platform: service.PlatformAnthropic,
			Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 0.75,
			I18n:         service.GroupI18n{"zh": {Name: "乙组", Description: "乙组说明"}, "en": {Name: "Beta", Description: "Beta public pool"}},
			ModelPricing: []service.ChannelModelPricing{groupPrice([]string{plazaSharedModel}, 0.000009, 0.000018), groupPrice([]string{"group-b-alias"}, 0.000005, 0.00001)},
		},
		{
			ID: plazaPrivateGroupID, Name: "Private base", Description: "Private base description", Platform: service.PlatformAnthropic,
			Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 2,
			IsExclusive:  true,
			I18n:         service.GroupI18n{"zh": {Name: "专属组", Description: "专属说明"}, "en": {Name: "Private", Description: "Private pool"}},
			ModelPricing: []service.ChannelModelPricing{groupPrice([]string{"private-model"}, 0.000002, 0.000004)},
		},
	}
	channels := []service.Channel{
		{ID: 1, Name: "alpha", Status: service.StatusActive, GroupIDs: []int64{plazaAlphaGroupID}, ModelPricing: []service.ChannelModelPricing{
			channelPrice(plazaSharedModel, 0.000001, 0.000002), channelPrice("group-a-alias", 0.000001, 0.000002), channelPrice("channel-only-decoy", 0.000001, 0.000002),
		}},
		{ID: 2, Name: "beta", Status: service.StatusActive, GroupIDs: []int64{plazaBetaGroupID}, ModelPricing: []service.ChannelModelPricing{
			channelPrice(plazaSharedModel, 0.000001, 0.000002), channelPrice("group-b-alias", 0.000001, 0.000002), channelPrice("channel-only-decoy", 0.000001, 0.000002),
		}},
		{ID: 3, Name: "private", Status: service.StatusActive, GroupIDs: []int64{plazaPrivateGroupID}, ModelPricing: []service.ChannelModelPricing{
			channelPrice("private-model", 0.000001, 0.000002), channelPrice("channel-only-decoy", 0.000001, 0.000002),
		}},
	}
	member := func(models ...string) service.Account {
		mapping := make(map[string]any, len(models))
		for _, model := range models {
			mapping[model] = "upstream-" + model
		}
		return service.Account{Platform: service.PlatformAnthropic, Credentials: map[string]any{"model_mapping": mapping}}
	}
	accountRepo := plazaScenarioMemberRepo{accountsByGroup: map[int64][]service.Account{
		plazaAlphaGroupID:   {member(plazaSharedModel, "group-a-alias")},
		plazaBetaGroupID:    {member(plazaSharedModel, "group-b-alias")},
		plazaPrivateGroupID: {member("private-model")},
	}}
	snapshotSources := func(t *testing.T) string {
		t.Helper()
		raw, err := json.MarshalIndent(struct {
			Groups   []service.Group
			Channels []service.Channel
			Accounts map[int64][]service.Account
		}{groups, channels, accountRepo.accountsByGroup}, "", " ")
		require.NoError(t, err)
		return string(raw)
	}
	before := snapshotSources(t)
	assertSourcesUnchanged := func(t *testing.T) {
		t.Helper()
		require.Equal(t, before, snapshotSources(t), "localized plaza output must not modify repository groups, channels or accounts")
	}
	channelRepo := plazaVaryChannelRepo{channels: channels}
	groupRepo := plazaVaryGroupRepo{groups: groups}
	billing := service.NewBillingService(&config.Config{}, nil)
	plaza := service.NewModelPlazaService(channelRepo, groupRepo, accountRepo, nil, billing, nil)
	userRepo := plazaScenarioUserRepo{user: &service.User{AllowedGroups: []int64{plazaPrivateGroupID}}}
	apiKeys := service.NewAPIKeyService(nil, userRepo, nil, plazaScenarioSubscriptionRepo{}, nil, nil, &config.Config{})
	handler := NewModelPlazaHandler(plaza, apiKeys, service.NewSettingService(plazaVarySettingRepo{}, &config.Config{}))

	router := gin.New()
	router.Use(middleware.CORS(config.CORSConfig{AllowedOrigins: []string{"https://a.example", "https://b.example"}}))
	router.GET("/api/v1/model-plaza", func(c *gin.Context) {
		if c.GetHeader("X-Test-Authorized") == "true" {
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 42})
		}
		handler.Get(c)
	})
	return router, billing, assertSourcesUnchanged
}

func plazaPricePointer(value float64) *float64 { return &value }

func requestModelPlaza(t *testing.T, router http.Handler, origin, language string, authorized bool) modelPlazaResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/model-plaza", nil)
	req.Header.Set("Origin", origin)
	req.Header.Set("Accept-Language", language)
	if authorized {
		req.Header.Set("X-Test-Authorized", "true")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, origin, w.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, []string{"Origin", "Accept-Language"}, w.Header().Values("Vary"))
	var envelope struct {
		Data modelPlazaResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope), w.Body.String())
	require.NotEmpty(t, envelope.Data.Groups, "the real service-to-handler response must contain catalog groups")
	return envelope.Data
}

func plazaGroupsByID(groups []modelPlazaGroup) map[int64]modelPlazaGroup {
	byID := make(map[int64]modelPlazaGroup, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	return byID
}

func assertLocalizedPlazaGroupModels(t *testing.T, group modelPlazaGroup, names []string, sharedInput, sharedOutput float64) {
	t.Helper()
	modelByName := make(map[string]modelPlazaModel, len(group.Models))
	for _, model := range group.Models {
		modelByName[model.Name] = model
		require.Equal(t, service.PlatformAnthropic, model.Platform)
	}
	actual := make([]string, 0, len(modelByName))
	for name := range modelByName {
		actual = append(actual, name)
	}
	require.ElementsMatch(t, names, actual, "member model_mapping keys are the source of visible catalog names; channel-only decoys stay hidden")
	shared := modelByName[plazaSharedModel]
	if shared.Name != "" {
		require.NotNil(t, shared.Pricing)
		require.InDelta(t, sharedInput, *shared.Pricing.InputPrice, 1e-12)
		require.InDelta(t, sharedOutput, *shared.Pricing.OutputPrice, 1e-12)
	}
	for _, name := range names {
		require.NotContains(t, name, "decoy")
	}
}

func assertPlazaCustomPrice(t *testing.T, group modelPlazaGroup, name string, input, output float64) {
	t.Helper()
	for _, model := range group.Models {
		if model.Name != name {
			continue
		}
		require.NotNil(t, model.Pricing)
		require.InDelta(t, input, *model.Pricing.InputPrice, 1e-12)
		require.InDelta(t, output, *model.Pricing.OutputPrice, 1e-12)
		return
	}
	t.Fatalf("group %d has no model %q", group.ID, name)
}

func assertPlazaOfficialPrice(t *testing.T, group modelPlazaGroup, input, output float64) {
	t.Helper()
	for _, model := range group.Models {
		if model.Name != plazaSharedModel {
			continue
		}
		require.NotNil(t, model.OfficialPricing)
		require.NotNil(t, model.OfficialPricing.InputPrice)
		require.NotNil(t, model.OfficialPricing.OutputPrice)
		require.InDelta(t, input, *model.OfficialPricing.InputPrice, 1e-12)
		require.InDelta(t, output, *model.OfficialPricing.OutputPrice, 1e-12)
		return
	}
	t.Fatalf("group %d has no shared model %q", group.ID, plazaSharedModel)
}

func assertPlazaNoI18nLeak(t *testing.T, response modelPlazaResponse) {
	t.Helper()
	raw, err := json.Marshal(response)
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"i18n"`, "the user-facing DTO must not expose admin translations")
}

func TestLocalizedPlazaPreservesVaryOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router, _, assertSourcesUnchanged := newLocalizedPlazaScenario(t)
	for _, origin := range []string{"https://a.example", "https://b.example"} {
		got := requestModelPlaza(t, router, origin, "en", false)
		require.Len(t, got.Groups, 2)
	}
	assertSourcesUnchanged(t)
}
