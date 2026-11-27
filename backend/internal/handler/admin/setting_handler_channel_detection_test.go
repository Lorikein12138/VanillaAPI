package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettingHandler_ChannelDetectionSwitchKeepsEnableGeneration(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, nil)
	key := service.SettingKeyChannelDetectionFeature
	update := func(payload map[string]any, want bool) {
		t.Helper()
		var envelope struct{ Data map[string]any }
		rec := doUpdateSettings(t, h, payload, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		require.Equal(t, want, envelope.Data[key])
	}
	update(map[string]any{"site_name": "First"}, false)
	require.NotContains(t, repo.values, key)
	update(map[string]any{key: true}, true)
	generation := repo.values[key]
	require.NotEmpty(t, generation)
	update(map[string]any{key: true}, true)
	require.Equal(t, generation, repo.values[key])
	require.NotContains(t, repo.lastUpdates, key)
	update(map[string]any{"site_name": "Second"}, true)
	require.Equal(t, generation, repo.values[key])
	update(map[string]any{key: false}, false)
	require.Empty(t, repo.values[key])
	update(map[string]any{key: true}, true)
	require.NotEqual(t, generation, repo.values[key])

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	h.GetSettings(c)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"channel_detection_enabled":true`)
	require.NotContains(t, rec.Body.String(), repo.values[key])
}
