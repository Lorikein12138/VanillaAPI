package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	adaptiveScoresMaxPageSize        = 50
	adaptiveScoresMaxOffset          = 10000
	adaptiveScoreOptionsDefaultLimit = 200
	adaptiveScoreOptionsMaxLimit     = 1000
)

func allowAdaptiveScores(c *gin.Context, settings *service.SettingService) bool {
	c.Header("Cache-Control", "no-store")
	ctx := c.Request.Context()
	if adminaccess.RestrictedFromContext(ctx) != nil {
		response.Forbidden(c, "Adaptive account scores require full administrator access")
		return false
	}
	if settings == nil || !settings.IsGatewayAdaptiveSchedulingEnabled(ctx) {
		response.ErrorFrom(c, infraerrors.NotFound("ADAPTIVE_ACCOUNT_SCORES_DISABLED", "Adaptive scheduling is disabled"))
		return false
	}
	return true
}

func (h *GroupHandler) AdaptiveScoreOptions(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowAdaptiveScores(c, settings) {
			return
		}
		afterID, limit, ok := adaptiveScoreOptionsCursor(c)
		if !ok {
			response.BadRequest(c, "Invalid cursor or limit")
			return
		}
		items, nextAfterID, err := h.adminService.ListAdaptiveScoreOptions(c.Request.Context(), afterID, limit)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, struct {
			Items       []service.AdaptiveScoreOption `json:"items"`
			NextAfterID *int64                        `json:"next_after_id"`
		}{Items: items, NextAfterID: nextAfterID})
	}
}

func adaptiveScoreOptionsCursor(c *gin.Context) (int64, int, bool) {
	var afterID uint64
	if values, present := c.GetQueryArray("after_id"); present {
		if len(values) != 1 {
			return 0, 0, false
		}
		value, err := strconv.ParseUint(values[0], 10, 63)
		if err != nil {
			return 0, 0, false
		}
		afterID = value
	}
	limit := uint64(adaptiveScoreOptionsDefaultLimit)
	if values, present := c.GetQueryArray("limit"); present {
		if len(values) != 1 {
			return 0, 0, false
		}
		value, err := strconv.ParseUint(values[0], 10, 64)
		if err != nil || value == 0 || value > adaptiveScoreOptionsMaxLimit {
			return 0, 0, false
		}
		limit = value
	}
	return int64(afterID), int(limit), true
}

func (h *GroupHandler) AdaptiveAccountScores(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowAdaptiveScores(c, settings) {
			return
		}
		groupID, err := strconv.ParseUint(c.Param("id"), 10, 63)
		if err != nil || groupID == 0 {
			response.BadRequest(c, "Invalid group ID")
			return
		}
		page, size := response.ParsePagination(c)
		if size > adaptiveScoresMaxPageSize || page-1 > adaptiveScoresMaxOffset/size || !adaptiveScoresPaginationMatches(c, page, size) {
			response.BadRequest(c, "Invalid pagination")
			return
		}
		id := int64(groupID)
		accounts, total, err := h.adminService.ListAdaptiveScoreMembers(c.Request.Context(), id, page, size)
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		response.Success(c, struct {
			service.AdaptiveAccountScoresSnapshot
			GroupID           int64 `json:"group_id"`
			Total             int64 `json:"total"`
			MaxQueryableTotal int64 `json:"max_queryable_total"`
			Page              int   `json:"page"`
			PageSize          int   `json:"page_size"`
		}{
			AdaptiveAccountScoresSnapshot: settings.AdaptiveAccountScores(id, accounts),
			GroupID:                       id, Total: total, Page: page, PageSize: size,
			MaxQueryableTotal: min(total, int64((adaptiveScoresMaxOffset/size+1)*size)),
		})
	}
}

func adaptiveScoresPaginationMatches(c *gin.Context, page, size int) bool {
	for _, param := range []struct {
		name  string
		value int
	}{{"page", page}, {"page_size", size}, {"limit", size}} {
		if values, ok := c.GetQueryArray(param.name); ok {
			if len(values) != 1 {
				return false
			}
			value, err := strconv.ParseUint(values[0], 10, strconv.IntSize-1)
			if err != nil || int(value) != param.value {
				return false
			}
		}
	}
	return true
}
