package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type detectionRouteSettings struct{ service.SettingRepository }

func (d detectionRouteSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (d detectionRouteSettings) GetValue(context.Context, string) (string, error) {
	return `{"enabled":true,"public_ip_rpm":2}`, nil
}

func TestChannelDetectionAnonymousRoutesNoStoreAndRateLimit(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := detectionRouteSettings{}
	settings := service.NewSettingService(repo, &config.Config{})
	detection := service.NewChannelDetection(repo, nil, nil, nil, nil, &config.Config{})
	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	v1 := router.Group("/api/v1")
	admin := v1.Group("/admin", func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	RegisterChannelDetectionRoutes(v1, admin, detection, middleware.NewPanelRateLimiter(rdb, settings))

	request := func(path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
		r.RemoteAddr = "203.0.113.42:10000"
		router.ServeHTTP(w, r)
		require.Equal(t, status, w.Code, w.Body.String())
		return w
	}
	status := request("/channel-detection/status", http.StatusOK)
	require.Contains(t, status.Body.String(), `"enabled":false`)
	require.Equal(t, "no-store", status.Header().Get("Cache-Control"))
	off := request("/channel-detection", http.StatusNotFound)
	require.Equal(t, "no-store", off.Header().Get("Cache-Control"))
	limited := request("/channel-detection", http.StatusTooManyRequests)
	require.Equal(t, "no-store", limited.Header().Get("Cache-Control"))
	request("/admin/channel-detection", http.StatusUnauthorized)
}
