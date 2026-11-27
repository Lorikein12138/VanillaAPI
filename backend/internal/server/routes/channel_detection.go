package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	registerChannelDetectionActions(settings, detection)
}

func registerChannelDetectionActions(settings *gin.RouterGroup, detection *service.ChannelDetection) {
	actions := settings.Group("", func(c *gin.Context) {
		if c.GetString("auth_method") != service.AuditAuthMethodJWT {
			middleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Administrator session required")
			return
		}
		c.Next()
	})
	actions.GET("/results", func(c *gin.Context) {
		groups, err := detection.AdminGroups(c.Request.Context())
		if !response.ErrorFrom(c, err) {
			response.Success(c, groups)
		}
	})
	actions.POST("/groups/:id/trigger", func(c *gin.Context) {
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group ID")
			return
		}
		sampleID, err := detection.Trigger(c.Request.Context(), id)
		if !response.ErrorFrom(c, err) {
			response.Accepted(c, gin.H{"sample_id": sampleID})
		}
	})
	actions.POST("/groups/:id/samples/:sampleID/confirm", func(c *gin.Context) {
		actor, ok := middleware.GetAuthSubjectFromContext(c)
		if !ok || actor.UserID <= 0 {
			middleware.AbortWithError(c, http.StatusForbidden, "FORBIDDEN", "Administrator session required")
			return
		}
		id, idErr := strconv.ParseInt(c.Param("id"), 10, 64)
		sampleID, sampleErr := uuid.Parse(c.Param("sampleID"))
		if idErr != nil || id <= 0 || sampleErr != nil || sampleID == uuid.Nil {
			response.BadRequest(c, "Invalid group or sample ID")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
		decoder := json.NewDecoder(c.Request.Body)
		decoder.DisallowUnknownFields()
		var input struct {
			ScopeVersion string `json:"scope_version"`
		}
		if err := decoder.Decode(&input); err != nil || len(input.ScopeVersion) != 64 || decoder.Decode(new(any)) != io.EOF {
			response.BadRequest(c, "Invalid confirmation scope")
			return
		}
		receipt, err := detection.Confirm(c.Request.Context(), id, sampleID.String(), input.ScopeVersion, actor.UserID)
		if !response.ErrorFrom(c, err) {
			response.Success(c, receipt)
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
