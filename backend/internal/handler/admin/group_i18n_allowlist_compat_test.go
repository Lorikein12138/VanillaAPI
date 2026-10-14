//go:build unit

package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGroupI18nPreservesAutomaticAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	router := newSimpleModeGroupRouter(svc)
	i18n := `{"en":{"name":"Standard","description":"Pro pool"}}`

	create := `{"name":"标准","platform":"anthropic","i18n":` + i18n + `,"model_allowlist":{"enabled":true,"models":[]}}`
	createResponse := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, "/groups", bytes.NewBufferString(create))
	createRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(createResponse, createRequest)

	require.Equal(t, http.StatusOK, createResponse.Code, createResponse.Body.String())
	require.Len(t, svc.createdGroups, 1)
	require.Equal(t, service.GroupI18n{"en": {Name: "Standard", Description: "Pro pool"}}, svc.createdGroups[0].I18n)
	require.True(t, svc.createdGroups[0].ModelAllowlist.Enabled)
	require.Empty(t, svc.createdGroups[0].ModelAllowlist.Models)

	update := `{"i18n":{"en":{"name":"Standard v2","description":"Updated pool"}},"model_allowlist":{"enabled":true,"models":[]}}`
	updateResponse := httptest.NewRecorder()
	updateRequest := httptest.NewRequest(http.MethodPut, "/groups/7", bytes.NewBufferString(update))
	updateRequest.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(updateResponse, updateRequest)

	require.Equal(t, http.StatusOK, updateResponse.Code, updateResponse.Body.String())
	require.Len(t, svc.updatedGroups, 1)
	require.NotNil(t, svc.updatedGroups[0].I18n)
	require.Equal(t, service.GroupI18n{"en": {Name: "Standard v2", Description: "Updated pool"}}, *svc.updatedGroups[0].I18n)
	require.NotNil(t, svc.updatedGroups[0].ModelAllowlist)
	require.True(t, svc.updatedGroups[0].ModelAllowlist.Enabled)
	require.Empty(t, svc.updatedGroups[0].ModelAllowlist.Models)
}

func TestSimpleGroupDTOIncludesI18nAndModelAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := newStubAdminService()
	svc.groups = []service.Group{{
		ID: 7, Name: "标准", Description: "默认描述", Platform: service.PlatformAnthropic,
		ModelAllowlist:                   service.GroupModelAllowlist{Enabled: true, Models: []string{}},
		ModelAllowlistSourceAccountCount: 2,
		I18n:                             service.GroupI18n{"en": {Name: "Standard", Description: "Pro pool"}},
	}}
	router := newSimpleModeGroupRouter(svc)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/groups", nil))

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Len(t, payload.Data.Items, 1)
	item := payload.Data.Items[0]
	require.Equal(t, map[string]any{"en": map[string]any{"name": "Standard", "description": "Pro pool"}}, item["i18n"])
	allowlist, ok := item["model_allowlist"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, allowlist["enabled"])
	require.Empty(t, allowlist["models"])
	require.Equal(t, float64(2), allowlist["source_account_count"])
}
