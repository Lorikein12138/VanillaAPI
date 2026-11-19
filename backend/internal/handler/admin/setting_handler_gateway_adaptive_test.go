package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettingHandler_GatewayAdaptiveSchedulingRoundTrip(t *testing.T) {
	key := service.SettingKeyGatewayAdaptiveSchedulingEnabled
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		"openai_advanced_scheduler_enabled": "true",
	})
	ctx := context.Background()
	assertResponse := func(rec *httptest.ResponseRecorder, want bool) {
		t.Helper()
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var envelope struct{ Data map[string]any }
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
		require.Equal(t, want, envelope.Data[key])
		require.Equal(t, true, envelope.Data["openai_advanced_scheduler_enabled"])
	}
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
		h.GetSettings(c)
		return rec
	}

	assertResponse(get(), false)
	require.False(t, h.settingService.IsGatewayAdaptiveSchedulingEnabled(ctx))
	assertResponse(doUpdateSettings(t, h, map[string]any{"site_name": "Renamed"}, nil), false)
	require.NotContains(t, repo.values, key)

	assertResponse(doUpdateSettings(t, h, map[string]any{key: true}, nil), true)
	require.Equal(t, "true", repo.values[key])
	require.True(t, h.settingService.IsGatewayAdaptiveSchedulingEnabled(ctx))
	assertResponse(get(), true)

	assertResponse(doUpdateSettings(t, h, map[string]any{"site_name": "Again"}, nil), true)
	require.NotContains(t, repo.lastUpdates, key)
	require.True(t, h.settingService.IsGatewayAdaptiveSchedulingEnabled(ctx))
	assertResponse(doUpdateSettings(t, h, map[string]any{key: nil}, nil), true)
	require.NotContains(t, repo.lastUpdates, key)

	assertResponse(doUpdateSettings(t, h, map[string]any{key: false}, nil), false)
	require.Equal(t, "false", repo.values[key])
	require.False(t, h.settingService.IsGatewayAdaptiveSchedulingEnabled(ctx))
	assertResponse(get(), false)

	disabled := &service.SystemSettings{}
	enabled := &service.SystemSettings{GatewayAdaptiveSchedulingEnabled: true}
	require.Equal(t, []string{key}, diffSettings(disabled, enabled, nil, nil, UpdateSettingsRequest{}))
	require.Empty(t, diffSettings(enabled, enabled, nil, nil, UpdateSettingsRequest{}))
}
