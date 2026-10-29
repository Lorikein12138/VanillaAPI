package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type availableModelsAdminService struct {
	*stubAdminService
	account service.Account
}

func (s *availableModelsAdminService) GetAccount(_ context.Context, id int64) (*service.Account, error) {
	if s.account.ID == id {
		acc := s.account
		return &acc, nil
	}
	return s.stubAdminService.GetAccount(context.Background(), id)
}

func setupAvailableModelsRouter(adminSvc service.AdminService) *gin.Engine {
	return setupAvailableModelsRouterWithAccountTestService(adminSvc, nil)
}

func setupAvailableModelsRouterWithAccountTestService(
	adminSvc service.AdminService,
	accountTestService *service.AccountTestService,
) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, accountTestService, nil, nil, nil, nil, nil)
	router.GET("/api/v1/admin/accounts/:id/models", handler.GetAvailableModels)
	return router
}

type availableModelsAccountRepo struct {
	service.AccountRepository
	account service.Account
}

func (r *availableModelsAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	if r.account.ID != id {
		return nil, service.ErrNoAvailableAccounts
	}
	account := r.account
	return &account, nil
}

func setupAvailableModelsRouterWithOpenAIModelDiscovery(
	adminSvc service.AdminService,
	account service.Account,
	upstream service.HTTPUpstream,
) *gin.Engine {
	repo := &availableModelsAccountRepo{account: account}
	cfg := &config.Config{
		RunMode:  config.RunModeSimple,
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}},
	}
	gatewayService := service.NewOpenAIGatewayService(
		repo,
		nil, nil, nil, nil, nil, nil,
		cfg,
		nil, nil, nil, nil, nil,
		upstream,
		nil, nil, nil, nil, nil, nil, nil, nil,
	)
	accountTestService := service.NewAccountTestService(
		repo,
		nil,
		nil,
		nil,
		nil,
		upstream,
		cfg,
		nil,
	)
	accountTestService.SetOpenAIGatewayService(gatewayService)
	return setupAvailableModelsRouterWithAccountTestService(adminSvc, accountTestService)
}

type syncUpstreamHTTPUpstream struct {
	resp      *http.Response
	responses []*http.Response
	err       error
	calls     int
}

func (u *syncUpstreamHTTPUpstream) Do(req *http.Request, proxyURL string, accountID int64, accountConcurrency int) (*http.Response, error) {
	u.calls++
	if u.err != nil {
		return nil, u.err
	}
	if len(u.responses) > 0 {
		resp := u.responses[0]
		u.responses = u.responses[1:]
		return resp, nil
	}
	return u.resp, nil
}

func (u *syncUpstreamHTTPUpstream) DoWithTLS(req *http.Request, proxyURL string, accountID int64, accountConcurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxyURL, accountID, accountConcurrency)
}

func (u *syncUpstreamHTTPUpstream) callCount() int {
	return u.calls
}

func setupSyncUpstreamModelsRouter(adminSvc service.AdminService, upstream service.HTTPUpstream) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	accountTestSvc := service.NewAccountTestService(
		nil,
		nil,
		nil,
		nil,
		nil,
		upstream,
		&config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		nil,
	)
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, accountTestSvc, nil, nil, nil, nil, nil)
	router.POST("/api/v1/admin/accounts/:id/models/sync-upstream", handler.SyncUpstreamModels)
	router.POST("/api/v1/admin/accounts/models/sync-upstream-preview", handler.SyncUpstreamModelsPreview)
	return router
}

type availableModelOptionResponse struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
}

func decodeAvailableModelOptions(t *testing.T, body []byte) []availableModelOptionResponse {
	t.Helper()
	var resp struct {
		Data []availableModelOptionResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	return resp.Data
}

func TestAccountHandlerGetAvailableModels_ExplicitMappingShortCircuitsUpstream(t *testing.T) {
	account := service.Account{
		ID:       52,
		Name:     "openai-explicit-mapping",
		Platform: service.PlatformOpenAI,
		Type:     service.AccountTypeAPIKey,
		Status:   service.StatusActive,
		Credentials: map[string]any{
			"api_key":  "test-key",
			"base_url": "https://provider.example/v1",
			"model_mapping": map[string]any{
				" gpt-image-2 ": " upstream-image-model ",
				"gpt-5.6-sol":   "gpt-5.6-sol",
			},
		},
	}
	svc := &availableModelsAdminService{stubAdminService: newStubAdminService(), account: account}
	upstream := &syncUpstreamHTTPUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"upstream-only"}]}`)),
	}}
	router := setupAvailableModelsRouterWithOpenAIModelDiscovery(svc, account, upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/52/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	models := decodeAvailableModelOptions(t, rec.Body.Bytes())
	require.Equal(t, []availableModelOptionResponse{
		{ID: "gpt-5.6-sol", DisplayName: "gpt-5.6-sol", Type: "model"},
		{ID: "gpt-image-2", DisplayName: "gpt-image-2", Type: "model"},
	}, models)
	require.NotContains(t, rec.Body.String(), "upstream-image-model")
	require.NotContains(t, rec.Body.String(), "upstream-only")
	require.Equal(t, 0, upstream.callCount())
}

type availableModelsScopedAdminService struct {
	*availableModelsAdminService
	service.ScopedAdminAccounts
}

func TestAccountHandlerGetAvailableModels_ScopedAndJWTUseSameExplicitMappingPicker(t *testing.T) {
	account := service.Account{
		ID: 53, Name: "mapped", Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, GroupIDs: []int64{7},
		Credentials: map[string]any{"model_mapping": map[string]any{"client-key": "upstream-value"}},
	}
	serviceStub := &availableModelsAdminService{stubAdminService: newStubAdminService(), account: account}
	upstream := &syncUpstreamHTTPUpstream{}
	jwtRouter := setupAvailableModelsRouterWithOpenAIModelDiscovery(serviceStub, account, upstream)
	scopedService := &availableModelsScopedAdminService{availableModelsAdminService: serviceStub}
	gin.SetMode(gin.TestMode)
	scopedRouter := gin.New()
	handler := NewAccountHandler(scopedService, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	scopedRouter.GET("/api/v1/admin/accounts/:id/models", func(c *gin.Context) {
		principal := &adminaccess.Principal{Policy: adminaccess.Policy{Rules: []adminaccess.Rule{{
			Permissions: []string{"accounts.read"}, AllGroups: true, AccountTypes: []string{service.AccountTypeAPIKey},
		}}}}
		c.Request = c.Request.WithContext(adminaccess.WithPrincipal(c.Request.Context(), principal))
		handler.ScopedAdminKeyAccounts(c)
	})

	var responseBodies []string
	for _, router := range []*gin.Engine{jwtRouter, scopedRouter} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/53/models", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		responseBodies = append(responseBodies, rec.Body.String())
	}
	require.JSONEq(t, responseBodies[0], responseBodies[1])
	require.Contains(t, responseBodies[0], "client-key")
	require.NotContains(t, responseBodies[0], "upstream-value")
	require.Equal(t, 0, upstream.callCount(), "explicit mappings must avoid upstream calls in both JWT and scoped handlers")
}

func TestAntigravityAccountTestModelsFallbackContract(t *testing.T) {
	defaults := antigravityAccountTestModels(nil)
	require.NotEmpty(t, defaults)
	require.Equal(t, defaults, antigravityAccountTestModels(map[string]any{"wild*": "target", "invalid": false}))
	got := antigravityAccountTestModels(map[string]any{"wild*": "target", "custom": "target", "invalid": false, "empty": ""})
	require.Len(t, got, 1)
	require.Equal(t, "custom", got[0].ID)
	require.Equal(t, "custom", got[0].DisplayName)
	// The endpoint's explicit-key contract is deliberately broader than fallback.
	account := service.Account{ID: 52, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth, Status: service.StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{"wild*": "target"}}}
	svc := &availableModelsAdminService{stubAdminService: newStubAdminService(), account: account}
	upstream := &syncUpstreamHTTPUpstream{}
	router := setupAvailableModelsRouterWithOpenAIModelDiscovery(svc, account, upstream)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/52/models", nil))
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []availableModelOptionResponse{{ID: "wild*", DisplayName: "wild*", Type: "model"}}, decodeAvailableModelOptions(t, recorder.Body.Bytes()))
	require.Zero(t, upstream.callCount())
}

func TestAccountHandlerGetAvailableModels_ExplicitMappingReturnsSortedKeys(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       53,
			Name:     "gemini-explicit-mapping",
			Platform: service.PlatformGemini,
			Type:     service.AccountTypeAPIKey,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"model_mapping": map[string]any{
					" zeta-alias ": " target-zeta ",
					"zeta-alias":   "other-target",
					"alpha-alias":  "target-alpha",
					"claude-*":     "target-claude",
				},
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/53/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	models := decodeAvailableModelOptions(t, rec.Body.Bytes())
	require.Equal(t, []availableModelOptionResponse{
		{ID: "alpha-alias", DisplayName: "alpha-alias", Type: "model"},
		{ID: "claude-*", DisplayName: "claude-*", Type: "model"},
		{ID: "zeta-alias", DisplayName: "zeta-alias", Type: "model"},
	}, models)
	require.NotContains(t, rec.Body.String(), "target-alpha")
	require.NotContains(t, rec.Body.String(), "target-zeta")
	require.Equal(t, "target-alpha", svc.account.GetModelMapping()["alpha-alias"], "model listing preserves the account mapping cache")
}

func TestAccountHandlerGetAvailableModels_CommandCodeAndClineMappings(t *testing.T) {
	for index, platform := range []string{service.PlatformCommandCode, service.PlatformCline} {
		t.Run(platform, func(t *testing.T) {
			accountID := int64(80 + index)
			account := service.Account{
				ID:       accountID,
				Name:     platform + "-explicit-mapping",
				Platform: platform,
				Type:     service.AccountTypeAPIKey,
				Status:   service.StatusActive,
				Credentials: map[string]any{
					"api_key":  "test-key",
					"base_url": "https://provider.example/v1",
					"model_mapping": map[string]any{
						" zeta-client-id ": " upstream-zeta ",
						"alpha-client-id":  "upstream-alpha",
						"invalid-value":    false,
					},
				},
			}
			svc := &availableModelsAdminService{stubAdminService: newStubAdminService(), account: account}
			upstream := &syncUpstreamHTTPUpstream{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"upstream-only"}]}`)),
			}}
			router := setupAvailableModelsRouterWithOpenAIModelDiscovery(svc, account, upstream)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/admin/accounts/%d/models", accountID), nil)
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, []availableModelOptionResponse{
				{ID: "alpha-client-id", DisplayName: "alpha-client-id", Type: "model"},
				{ID: "zeta-client-id", DisplayName: "zeta-client-id", Type: "model"},
			}, decodeAvailableModelOptions(t, rec.Body.Bytes()))
			require.NotContains(t, rec.Body.String(), "upstream-alpha")
			require.NotContains(t, rec.Body.String(), "upstream-zeta")
			require.NotContains(t, rec.Body.String(), "upstream-only")
			require.Zero(t, upstream.callCount())
		})
	}
}

func TestAccountHandlerGetAvailableModels_ExplicitMappingFiltersInvalidMixedEntries(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       54,
			Name:     "claude-mixed-mapping",
			Platform: service.PlatformAnthropic,
			Type:     service.AccountTypeAPIKey,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"model_mapping": map[string]any{
					" valid-alias ": " upstream-model ",
					"blank-key":     " ",
					"blank-value":   "   ",
					"non-string":    42,
					"nil-value":     nil,
					"":              "upstream-empty-key",
				},
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/54/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	models := decodeAvailableModelOptions(t, rec.Body.Bytes())
	require.Equal(t, []availableModelOptionResponse{
		{ID: "valid-alias", DisplayName: "valid-alias", Type: "model"},
	}, models)
	require.NotContains(t, rec.Body.String(), "claude-3-7-sonnet")
}

func TestAccountHandlerGetAvailableModels_ExplicitMappingInvalidFallsBack(t *testing.T) {
	tests := []struct {
		name           string
		credentials    map[string]any
		fallbackIDs    []string
		defaultCatalog bool
	}{
		{name: "missing", credentials: nil, defaultCatalog: true},
		{name: "empty", credentials: map[string]any{"model_mapping": map[string]any{}}, defaultCatalog: true},
		{name: "wrong type", credentials: map[string]any{"model_mapping": "not-a-map"}, defaultCatalog: true},
		{name: "all invalid", credentials: map[string]any{
			"model_mapping": map[string]any{
				"blank-key":   " ",
				"blank-value": "",
				"non-string":  false,
			},
		}, fallbackIDs: []string{"blank-key", "blank-value"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &availableModelsAdminService{
				stubAdminService: newStubAdminService(),
				account: service.Account{
					ID:          55,
					Name:        "gemini-no-explicit-mapping",
					Platform:    service.PlatformGemini,
					Type:        service.AccountTypeAPIKey,
					Status:      service.StatusActive,
					Credentials: tt.credentials,
				},
			}
			router := setupAvailableModelsRouter(svc)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/55/models", nil)
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			models := decodeAvailableModelOptions(t, rec.Body.Bytes())
			require.NotEmpty(t, models)
			if tt.defaultCatalog {
				require.Contains(t, rec.Body.String(), "gemini-3.5-flash")
				require.NotContains(t, rec.Body.String(), "gemini-2.5-pro")
				return
			}
			ids := make([]string, 0, len(models))
			for _, model := range models {
				ids = append(ids, model.ID)
			}
			require.ElementsMatch(t, tt.fallbackIDs, ids)
		})
	}
}

func TestAccountHandlerGetAvailableModels_GrokUsesXAIModels(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       44,
			Name:     "grok-oauth",
			Platform: service.PlatformGrok,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"model_mapping": map[string]any{
					"grok-4.3": "grok-4.3",
				},
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/44/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	require.Equal(t, "grok-4.3", resp.Data[0].ID)
}

func TestAccountHandlerGetAvailableModels_GrokDefaultsToXAIModelsWithoutMapping(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       45,
			Name:     "grok-oauth-defaults",
			Platform: service.PlatformGrok,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/45/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Data)

	var ids []string
	for _, model := range resp.Data {
		id := model.ID
		ids = append(ids, id)
		require.NotContains(t, strings.ToLower(id), "claude")
	}
	require.Contains(t, ids, "grok-4.3")
	require.Contains(t, ids, "grok-build-0.1")
}

func TestAccountHandlerGetAvailableModels_OpenAIOAuthUsesExplicitModelMapping(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       42,
			Name:     "openai-oauth",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"model_mapping": map[string]any{
					"gpt-5": "gpt-5.1",
				},
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/42/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	require.Equal(t, "gpt-5", resp.Data[0].ID)
}

func TestAccountHandlerGetAvailableModels_OpenAIOAuthPassthroughFallsBackToDefaults(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       43,
			Name:     "openai-oauth-passthrough",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"model_mapping": map[string]any{
					"gpt-5": "gpt-5.1",
				},
			},
			Extra: map[string]any{
				"openai_passthrough": true,
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/43/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	models := decodeAvailableModelOptions(t, rec.Body.Bytes())
	require.NotEmpty(t, models)
	for _, model := range models {
		require.NotEqual(t, "gpt-5", model.ID)
	}
}

func TestAccountHandlerGetAvailableModels_OpenAIAPIKeyDefaultsToConcreteGPT56Sol(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       46,
			Name:     "openai-apikey",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"api_key": "test-key",
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/46/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Data)
	require.Equal(t, "gpt-5.6-sol", resp.Data[0].ID)
}

func TestAccountHandlerGetAvailableModels_OpenAISparkShadowReturnsMappingModels(t *testing.T) {
	parentID := int64(100)
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:              44,
			Name:            "openai-spark-shadow",
			Platform:        service.PlatformOpenAI,
			Type:            service.AccountTypeOAuth,
			Status:          service.StatusActive,
			ParentAccountID: &parentID,
			QuotaDimension:  service.QuotaDimensionSpark,
			Credentials: map[string]any{
				"model_mapping": map[string]any{
					"gpt-5.3-codex-spark": "gpt-5.3-codex-spark",
				},
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/44/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
	}
	require.ElementsMatch(t, []string{
		"gpt-5.3-codex-spark",
	}, ids, "影子可用模型由 model_mapping 派生（非写死）")
}

func TestAccountHandlerGetAvailableModels_GeminiGoogleOneUsesConservativeCatalog(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       45,
			Name:     "google-one",
			Platform: service.PlatformGemini,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"oauth_type": "google_one",
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/45/models", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := make([]string, 0, len(resp.Data))
	for _, model := range resp.Data {
		ids = append(ids, model.ID)
	}
	require.ElementsMatch(t, []string{"gemini-2.0-flash", "gemini-2.5-flash", "gemini-2.5-pro"}, ids)
	require.NotContains(t, ids, "gemini-3.5-flash")
	require.NotContains(t, ids, "gemini-2.5-flash-image")
}

func TestAccountHandlerGetAvailableModels_Antigravity(t *testing.T) {
	defaults := antigravity.DefaultModels()
	known := defaults[0]
	custom := antigravity.ClaudeModel{ID: "claude-custom-alias", Type: "model", DisplayName: "claude-custom-alias"}
	knownMapped := antigravity.ClaudeModel{ID: known.ID, Type: "model", DisplayName: known.ID}
	cases := []struct {
		name        string
		credentials map[string]any
		want        []antigravity.ClaudeModel
	}{
		{name: "nil credentials", want: defaults},
		{name: "missing mapping", credentials: map[string]any{}, want: defaults},
		{name: "empty JSON mapping", credentials: map[string]any{"model_mapping": map[string]any{}}, want: defaults},
		{name: "empty string mapping", credentials: map[string]any{"model_mapping": map[string]string{}}, want: defaults},
		{name: "blank key", credentials: map[string]any{"model_mapping": map[string]any{" ": "upstream-model"}}, want: defaults},
		{name: "blank value", credentials: map[string]any{"model_mapping": map[string]any{known.ID: " "}}, want: defaults},
		{name: "non-string value", credentials: map[string]any{"model_mapping": map[string]any{known.ID: 42}}, want: defaults},
		{name: "blank string value", credentials: map[string]any{"model_mapping": map[string]string{known.ID: " "}}, want: defaults},
		{name: "JSON mapping keys and metadata", credentials: map[string]any{"model_mapping": map[string]any{
			known.ID: known.ID, custom.ID: "upstream-model", "ignored-empty": " ", "ignored-non-string": 42,
		}}, want: []antigravity.ClaudeModel{custom, knownMapped}},
		{name: "string mapping keys and metadata", credentials: map[string]any{"model_mapping": map[string]string{
			known.ID: known.ID, custom.ID: "upstream-model", "ignored-empty": " ",
		}}, want: []antigravity.ClaudeModel{custom, knownMapped}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, accountType := range []string{service.AccountTypeOAuth, service.AccountTypeAPIKey} {
				t.Run(accountType, func(t *testing.T) {
					svc := &availableModelsAdminService{
						stubAdminService: newStubAdminService(),
						account:          service.Account{ID: 51, Platform: service.PlatformAntigravity, Type: accountType, Credentials: tc.credentials},
					}
					rec := httptest.NewRecorder()
					setupAvailableModelsRouter(svc).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/51/models", nil))
					require.Equal(t, http.StatusOK, rec.Code)
					var resp struct {
						Data []antigravity.ClaudeModel `json:"data"`
					}
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					require.Equal(t, tc.want, resp.Data)
				})
			}
		})
	}
}

func TestAccountHandlerSyncUpstreamModels_ConfigErrorReturnsBadRequest(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       44,
			Name:     "openai-apikey-missing-key",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"base_url": "https://openai.example.com/v1",
			},
		},
	}
	router := setupSyncUpstreamModelsRouter(svc, &syncUpstreamHTTPUpstream{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/44/models/sync-upstream", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "No OpenAI API key is available")
}

func TestAccountHandlerSyncUpstreamModelsReturnsCapabilityMetadata(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID: 48, Name: "custom-openai", Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive,
			Credentials: map[string]any{"api_key": "key", "base_url": "https://provider.example/v1"},
		},
	}
	upstream := &syncUpstreamHTTPUpstream{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(strings.NewReader(`{"models":[{
			"id":"custom-thinking-model",
			"reasoning":true,
			"default_reasoning_level":"high",
			"supported_reasoning_levels":["low","high"],
			"input_modalities":["text","image"],
			"context_window":256000
		}]}`)),
	}}
	router := setupSyncUpstreamModelsRouter(svc, upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/48/models/sync-upstream", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Data service.UpstreamModelCatalog `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []string{"custom-thinking-model"}, resp.Data.Models)
	metadata := resp.Data.Metadata["custom-thinking-model"]
	require.NotNil(t, metadata.Reasoning)
	require.True(t, *metadata.Reasoning)
	require.Equal(t, []string{"low", "high"}, metadata.SupportedReasoningLevels)
	require.Equal(t, []string{"text", "image"}, metadata.InputModalities)
}

// Scenario: 创建账号 preview 将具体 mapping 传给 404/405 配置回退。
func TestAccountHandlerSyncUpstreamModelsPreviewUsesProvidedModelMapping(t *testing.T) {
	upstream := &syncUpstreamHTTPUpstream{responses: []*http.Response{
		{
			StatusCode: http.StatusNotFound,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
				"configured-provider": {
					"api": "https://provider.example/v1",
					"models": {
						"glm-5.3": {
							"id": "glm-5.3",
							"reasoning": true,
							"reasoning_options": [{"type":"effort","values":["low","high"]}],
							"modalities": {"input":["text"],"output":["text"]},
							"limit": {"context":1000000,"output":131072}
						}
					}
				}
			}`)),
		},
	}}
	router := setupSyncUpstreamModelsRouter(newStubAdminService(), upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/admin/accounts/models/sync-upstream-preview",
		strings.NewReader(`{
			"platform":"openai",
			"type":"apikey",
			"base_url":"https://provider.example/v1",
			"api_key":"key",
			"model_mapping":{"public-glm":"glm-5.3"}
		}`),
	)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Data service.UpstreamModelCatalog `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []string{"glm-5.3"}, resp.Data.Models)
	require.Equal(t, []string{"low", "high"}, resp.Data.Metadata["glm-5.3"].SupportedReasoningLevels)
}

func TestAccountHandlerSyncUpstreamModels_UpstreamErrorDoesNotExposeBody(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       45,
			Name:     "openai-apikey-upstream-error",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"api_key":  "openai-key",
				"base_url": "https://openai.example.com/v1",
			},
		},
	}
	upstream := &syncUpstreamHTTPUpstream{resp: &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":"SECRET_TOKEN should not be exposed"}`)),
	}}
	router := setupSyncUpstreamModelsRouter(svc, upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/45/models/sync-upstream", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.Contains(t, rec.Body.String(), "Upstream model list request failed with HTTP 502")
	require.NotContains(t, rec.Body.String(), "SECRET_TOKEN")
}

// Scenario: 能力补全失败显示部分成功。
func TestAccountHandlerSyncUpstreamModels_MetadataEnrichmentFailureReturnsWarning(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       46,
			Name:     "opencode-id-only-model-list",
			Platform: service.PlatformOpenAI,
			Type:     service.AccountTypeAPIKey,
			Status:   service.StatusActive,
			Credentials: map[string]any{
				"api_key":  "opencode-key",
				"base_url": "https://opencode.ai/zen/v1",
			},
		},
	}
	upstream := &syncUpstreamHTTPUpstream{responses: []*http.Response{
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"data":[{"id":"x-preview-f-free"}]}`)),
		},
		{
			StatusCode: http.StatusBadGateway,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"registry unavailable"}`)),
		},
	}}
	router := setupSyncUpstreamModelsRouter(svc, upstream)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/46/models/sync-upstream", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Data struct {
			Models   []string `json:"models"`
			Warnings []struct {
				Code string `json:"code"`
			} `json:"warnings"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []string{"x-preview-f-free"}, resp.Data.Models)
	require.Len(t, resp.Data.Warnings, 1)
	require.Equal(t, "upstream_model_metadata_incomplete", resp.Data.Warnings[0].Code)
}

func TestAccountHandlerGetAvailableModels_TypeSafeOnlyReturnsJev(t *testing.T) {
	for _, credentials := range []map[string]any{
		{"api_key": "ts-secret"},
		{"api_key": "ts-secret", "model_mapping": map[string]any{"jev-latest": "jev-latest"}},
	} {
		svc := &availableModelsAdminService{
			stubAdminService: newStubAdminService(),
			account: service.Account{
				ID:          46,
				Name:        "typesafe",
				Platform:    service.PlatformTypeSafe,
				Type:        service.AccountTypeAPIKey,
				Status:      service.StatusActive,
				Credentials: credentials,
			},
		}
		router := setupAvailableModelsRouter(svc)

		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/46/models", nil))
		require.Equal(t, http.StatusOK, rec.Code)

		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.Len(t, resp.Data, 1)
		require.Equal(t, "jev-latest", resp.Data[0].ID)
	}
}
