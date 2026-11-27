package routes

import (
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func RegisterChannelDetectionRoutes(v1, admin *gin.RouterGroup, detection *service.ChannelDetection, limiter *middleware.PanelRateLimiter) {
	public := v1.Group("/channel-detection", detectionNoStore, limiter.PublicIP())
	public.GET("/status", func(c *gin.Context) {
		enabled, err := detection.PublicEnabled(c.Request.Context())
		if !response.ErrorFrom(c, err) {
			response.Success(c, gin.H{"enabled": enabled})
		}
	})
	public.GET("", func(c *gin.Context) {
		groups, err := detection.PublicGroups(c.Request.Context())
		if !response.ErrorFrom(c, err) {
			response.Success(c, groups)
		}
	})

	settings := admin.Group("/channel-detection", detectionNoStore)
	settings.GET("", func(c *gin.Context) {
		config, err := detection.Config(c.Request.Context())
		if !response.ErrorFrom(c, err) {
			response.Success(c, config)
		}
	})
	settings.PUT("", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
		var input service.ChannelDetectionConfig
		if err := c.ShouldBindJSON(&input); err != nil {
			response.BadRequest(c, "Invalid detection configuration")
			return
		}
		if !response.ErrorFrom(c, detection.Save(c.Request.Context(), input)) {
			response.Success(c, nil)
		}
	})
	settings.GET("/groups", func(c *gin.Context) {
		groups, err := detection.GroupOptions(c.Request.Context())
		if !response.ErrorFrom(c, err) {
			response.Success(c, groups)
		}
	})
	settings.GET("/groups/:id/models", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group ID")
			return
		}
		models, err := detection.ModelOptions(c.Request.Context(), id)
		if !response.ErrorFrom(c, err) {
			response.Success(c, models)
		}
	})
}

func detectionNoStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Next()
}

func authenticateChannelDetection(c *gin.Context) {
	key, ok := service.ChannelDetectionIngress(c.Request)
	if !ok {
		c.AbortWithStatus(http.StatusForbidden)
		return
	}
	middleware.SetPrivateDetectionRoutingContext(c, key)
	c.Next()
}
