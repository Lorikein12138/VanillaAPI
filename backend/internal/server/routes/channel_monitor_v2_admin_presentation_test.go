package routes

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestChannelMonitorV2AdminPresentationRoutes(t *testing.T) {
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
		"/admin/channel-monitor-v2/presentation/dimensions",
		"/admin/channel-monitor-v2/presentation/snapshot",
		"/admin/channel-monitor-v2/presentation/models",
		"/admin/channel-monitor-v2/presentation/matrix",
		"/admin/channel-monitor-v2/presentation/errors",
		"/admin/channel-monitor-v2/presentation/users",
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
}
