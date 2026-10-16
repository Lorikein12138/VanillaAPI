package handler

import (
	"encoding/json"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/gemini"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGeminiNativeModelsUsesAccountMappings(t *testing.T) {
	for _, tt := range []struct {
		name                     string
		forced, mixed, allowlist bool
		status                   int
	}{
		{"mixed", false, true, false, 200},
		{"mixed allowlist", false, true, true, 200},
		{"disabled mixed", false, false, false, 503},
		{"forced without mixed opt-in", true, false, false, 200},
		{"forced allowlist", true, false, true, 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(45)
			repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{
				groupID: {{ID: 1, Platform: service.PlatformAntigravity,
					Extra:       map[string]any{"mixed_scheduling": tt.mixed},
					Credentials: map[string]any{"model_mapping": map[string]any{"gemini-synced-custom": "gemini-3.8-flash-high", "claude-custom": "claude-sonnet-4-6"}}}},
			}}}
			h := &GatewayHandler{geminiCompatService: service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, nil, nil, nil)}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformGemini,
				ModelAllowlist: service.GroupModelAllowlist{Enabled: tt.allowlist, Models: []string{"gemini-synced-custom"}},
			}})
			if tt.forced {
				c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
			}
			h.GeminiV1BetaListModels(c)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			if tt.status != 200 {
				return
			}
			var got gemini.ModelsListResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			names := []string{}
			for _, model := range got.Models {
				names = append(names, model.Name)
				require.Contains(t, model.SupportedGenerationMethods, "generateContent")
			}
			require.Contains(t, names, "models/gemini-synced-custom")
			require.NotContains(t, names, "models/claude-custom")
			require.NotContains(t, names, "models/gemini-2.0-flash")
			if tt.allowlist {
				require.Equal(t, []string{"models/gemini-synced-custom"}, names)
			}
		})
	}
}

func TestGeminiNativeModelsMergesGeminiAccountMappingsOnEmptyUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(47)
	repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{
					ID:          1,
					Platform:    service.PlatformGemini,
					Type:        service.AccountTypeAPIKey,
					Status:      service.StatusActive,
					Schedulable: true,
					Credentials: map[string]any{
						"api_key":       "test-one",
						"model_mapping": map[string]any{"gemini-local-a": "upstream-a", "gemini-local-b": "upstream-b", "gemini-local-c": "upstream-c"},
					},
				},
				{
					ID:          2,
					Platform:    service.PlatformGemini,
					Type:        service.AccountTypeAPIKey,
					Status:      service.StatusActive,
					Schedulable: true,
					Credentials: map[string]any{
						"api_key":       "test-two",
						"model_mapping": map[string]any{"gemini-local-b": "upstream-b", "gemini-local-d": "upstream-d", "gemini-local-e": "upstream-e"},
					},
				},
			},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)
	h.geminiCompatService = service.NewGeminiMessagesCompatService(
		repo, nil, nil, nil, nil, nil,
		&geminiMixedModelsUpstream{status: http.StatusOK, body: `{"models":[]}`},
		nil, &config.Config{},
	)

	group := &service.Group{ID: groupID, Platform: service.PlatformGemini}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: group})
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var native gemini.ModelsListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &native))
	require.Len(t, native.Models, 5)
	wantNames := []string{
		"models/gemini-local-a",
		"models/gemini-local-b",
		"models/gemini-local-c",
		"models/gemini-local-d",
		"models/gemini-local-e",
	}
	gotNames := make([]string, 0, len(native.Models))
	for _, model := range native.Models {
		gotNames = append(gotNames, model.Name)
		require.ElementsMatch(t, []string{"generateContent", "streamGenerateContent"}, model.SupportedGenerationMethods)
	}
	require.Equal(t, wantNames, gotNames)

	listRecorder := httptest.NewRecorder()
	listContext, _ := gin.CreateTestContext(listRecorder)
	listContext.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	listContext.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: group})
	h.Models(listContext)
	require.Equal(t, http.StatusOK, listRecorder.Code, listRecorder.Body.String())
	var gatewayModels gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(listRecorder.Body.Bytes(), &gatewayModels))
	require.ElementsMatch(t, wantNamesWithoutPrefix(wantNames), modelIDsForTest(gatewayModels.Data))
}

func wantNamesWithoutPrefix(names []string) []string {
	ids := make([]string, 0, len(names))
	for _, name := range names {
		ids = append(ids, strings.TrimPrefix(name, "models/"))
	}
	return ids
}

func TestAppendUpstreamGeminiModelsPreservesMetadata(t *testing.T) {
	body := []byte(`{"models":[{"name":"models/gemini-native","inputTokenLimit":123,"custom":{"a":true}}],"nextPageToken":"next","other":42}`)
	extra := []gemini.Model{gemini.FallbackModel("gemini-native"), gemini.FallbackModel("gemini-synced-custom"), gemini.FallbackModel("gemini-synced-custom")}
	merged, ok := appendUpstreamGeminiModels(body, extra)
	require.True(t, ok)
	require.JSONEq(t, `{"models":[{"name":"models/gemini-native","inputTokenLimit":123,"custom":{"a":true}},{"name":"models/gemini-synced-custom","supportedGenerationMethods":["generateContent","streamGenerateContent"]}],"nextPageToken":"next","other":42}`, string(merged))
	filtered, dropped, ok := filterUpstreamGeminiModelsBody(merged, service.GroupModelAllowlist{Enabled: true, Models: []string{"gemini-synced-*"}})
	require.True(t, ok)
	require.True(t, dropped)
	require.NotContains(t, string(filtered), "gemini-native")
	require.Contains(t, string(filtered), "gemini-synced-custom")
	for _, invalid := range []string{`null`, `not-json`, `{"error":"bad"}`, `{"models":{}}`} {
		got, ok := appendUpstreamGeminiModels([]byte(invalid), extra)
		require.False(t, ok)
		require.Equal(t, invalid, string(got))
	}
}

func TestGeminiNativeModelsCombinesSourcesBeforeFinalAllowlist(t *testing.T) {
	id := int64(51)
	repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{id: {
		{ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "model_mapping": map[string]any{"gemini-local-only": "upstream-local"}}},
		{ID: 2, Platform: service.PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": true}, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-synced-custom": "gemini-4-argon-high"}}},
	}}}}
	upstream := &geminiMixedModelsUpstream{status: http.StatusOK, body: `{"models":[{"name":"models/gemini-native","inputTokenLimit":123,"custom":{"source":"upstream"}},{"name":"models/gemini-rejected"}],"nextPageToken":"next"}`}
	h := newGatewayModelsHandlerForTest(repo)
	h.geminiCompatService = service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, upstream, nil, &config.Config{})
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &id, Group: &service.Group{ID: id, Platform: service.PlatformGemini, ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"gemini-native", "gemini-local-only", "gemini-synced-custom"}}}})
	h.GeminiV1BetaListModels(c)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var got gemini.ModelsListResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
	require.ElementsMatch(t, []string{"models/gemini-native", "models/gemini-local-only", "models/gemini-synced-custom"}, modelNamesForTest(got.Models))
	require.Contains(t, recorder.Body.String(), `"inputTokenLimit":123`)
	require.Contains(t, recorder.Body.String(), `"custom":{"source":"upstream"}`)
	require.Contains(t, recorder.Body.String(), `"nextPageToken":"next"`)
	require.Equal(t, "native-id", recorder.Header().Get("X-Request-Id"))
}

// Exercise the real handler's native-upstream branch, including scope fallback.
type geminiMixedModelsUpstream struct {
	service.HTTPUpstream
	status int
	body   string
}

func (u *geminiMixedModelsUpstream) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return &http.Response{StatusCode: u.status, Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"native-id"}}, Body: io.NopCloser(strings.NewReader(u.body))}, nil
}
func TestGeminiNativeModelsMergesNativeUpstream(t *testing.T) {
	for _, tt := range []struct {
		name           string
		status         int
		body           string
		expectedNative string
	}{
		{"native", 200, `{"models":[{"name":"models/gemini-native","inputTokenLimit":123,"custom":{"source":"upstream"}}],"nextPageToken":"next","other":42}`, "models/gemini-native"},
		{"scope fallback", 403, `{"error":"insufficient authentication scopes"}`, "models/gemini-2.5-pro"},
		{"upstream error", 429, `{"error":"rate limited"}`, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id := int64(46)
			repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{byGroup: map[int64][]service.Account{id: {
				{ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test", "model_mapping": map[string]any{"gemini-native": "upstream-native", "gemini-local-only": "upstream-local"}}},
				{ID: 2, Platform: service.PlatformAntigravity, Extra: map[string]any{"mixed_scheduling": true}, Credentials: map[string]any{"model_mapping": map[string]any{"gemini-synced-custom": "gemini-3.8-flash-high"}}},
			}}}}
			upstream := &geminiMixedModelsUpstream{status: tt.status, body: tt.body}
			h := newGatewayModelsHandlerForTest(repo)
			h.geminiCompatService = service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, upstream, nil, &config.Config{})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &id, Group: &service.Group{ID: id, Platform: service.PlatformGemini}})
			h.GeminiV1BetaListModels(c)
			if tt.status == 429 {
				require.Equal(t, 429, rec.Code)
				require.JSONEq(t, tt.body, rec.Body.String())
				return
			}
			require.Equal(t, 200, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tt.expectedNative)
			require.Contains(t, rec.Body.String(), "models/gemini-synced-custom")
			require.Contains(t, rec.Body.String(), "models/gemini-local-only")
			if tt.status == 200 {
				require.Contains(t, rec.Body.String(), `"inputTokenLimit":123`)
				require.Contains(t, rec.Body.String(), `"custom":{"source":"upstream"}`)
				require.Contains(t, rec.Body.String(), `"nextPageToken":"next"`)
				require.Contains(t, rec.Body.String(), `"other":42`)
				require.Equal(t, 1, strings.Count(rec.Body.String(), "models/gemini-native"))
				require.Equal(t, "native-id", rec.Header().Get("X-Request-Id"))
			}
		})
	}
}

func TestGeminiNativeModelsFiltersMergedGeminiMappingsByAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(48)
	repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {{
				ID:       1,
				Platform: service.PlatformGemini,
				Type:     service.AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key": "test",
					"model_mapping": map[string]any{
						"gemini-exact":      "upstream-exact",
						"gemini-star-alpha": "upstream-star-alpha",
						"gemini-star-beta":  "upstream-star-beta",
						"gemini-prefix":     "upstream-prefix",
						"gemini-rejected":   "upstream-rejected",
					},
				},
			}},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)
	h.geminiCompatService = service.NewGeminiMessagesCompatService(
		repo, nil, nil, nil, nil, nil,
		&geminiMixedModelsUpstream{status: http.StatusOK, body: `{"models":[]}`},
		nil, &config.Config{},
	)

	group := &service.Group{
		ID:       groupID,
		Platform: service.PlatformGemini,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"gemini-exact", "gemini-star-*", "models/gemini-prefix"},
		},
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: group})
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got gemini.ModelsListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{
		"models/gemini-exact",
		"models/gemini-prefix",
		"models/gemini-star-alpha",
		"models/gemini-star-beta",
	}, modelNamesForTest(got.Models))
	require.NotContains(t, modelNamesForTest(got.Models), "models/gemini-rejected")
}

func TestGeminiNativeModelsForcedAntigravityExcludesGeminiMappings(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(49)
	repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{
					"api_key":       "test",
					"model_mapping": map[string]any{"gemini-normal-only": "upstream-normal"},
				}},
				{ID: 2, Platform: service.PlatformAntigravity, Credentials: map[string]any{
					"model_mapping": map[string]any{"gemini-antigravity-only": "upstream-antigravity"},
				}},
			},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)
	h.geminiCompatService = service.NewGeminiMessagesCompatService(repo, nil, nil, nil, nil, nil, nil, nil, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/antigravity/v1beta/models", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformGemini}})
	c.Set(string(middleware.ContextKeyForcePlatform), service.PlatformAntigravity)
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got gemini.ModelsListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Contains(t, modelNamesForTest(got.Models), "models/gemini-antigravity-only")
	require.NotContains(t, modelNamesForTest(got.Models), "models/gemini-normal-only")
}

func TestGeminiNativeModelsKeepsEmptyUpstreamWithoutLocalMappings(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(50)
	repo := &geminiAllowlistAccountRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {{ID: 1, Platform: service.PlatformGemini, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "test"}}},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)
	h.geminiCompatService = service.NewGeminiMessagesCompatService(
		repo, nil, nil, nil, nil, nil,
		&geminiMixedModelsUpstream{status: http.StatusOK, body: `{"models":[]}`},
		nil, &config.Config{},
	)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformGemini}})
	h.GeminiV1BetaListModels(c)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, `{"models":[]}`, rec.Body.String())
}

func modelNamesForTest(models []gemini.Model) []string {
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.Name)
	}
	return names
}
