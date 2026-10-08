package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2AdminRoutesRetirePresentation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{ChannelMonitorV2: &handler.ChannelMonitorV2Handler{}}

	registerChannelMonitorV2Routes(router.Group("/admin"), handlers, nil)
	routes := make(map[string]struct{}, len(router.Routes()))
	for _, route := range router.Routes() {
		if route.Method == "GET" {
			routes[route.Path] = struct{}{}
		}
	}

	for _, path := range []string{
		"/admin/channel-monitor-v2/dimensions",
		"/admin/channel-monitor-v2/snapshot",
		"/admin/channel-monitor-v2/models",
		"/admin/channel-monitor-v2/matrix",
		"/admin/channel-monitor-v2/errors",
		"/admin/channel-monitor-v2/users",
	} {
		_, ok := routes[path]
		require.True(t, ok, "missing GET route %s", path)
	}
	for _, resource := range []string{"dimensions", "snapshot", "models", "matrix", "errors", "users"} {
		t.Run(resource, func(t *testing.T) {
			path := "/admin/channel-monitor-v2/presentation/" + resource
			require.NotContains(t, routes, path)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusNotFound, recorder.Code)
		})
	}
	require.Contains(t, routes, "/admin/channel-monitor-v2/config")
}
