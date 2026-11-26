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
	adaptiveScoresMaxPageSize = 50
	adaptiveScoresMaxOffset   = 10000
)

func (h *GroupHandler) AdaptiveAccountScores(settings *service.SettingService) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		ctx := c.Request.Context()
		if adminaccess.RestrictedFromContext(ctx) != nil {
			response.Forbidden(c, "Adaptive account scores require full administrator access")
			return
		}
		if settings == nil || !settings.IsGatewayAdaptiveSchedulingEnabled(ctx) {
			response.ErrorFrom(c, infraerrors.NotFound("ADAPTIVE_ACCOUNT_SCORES_DISABLED", "Adaptive scheduling is disabled"))
			return
		}
		groupID, err := strconv.ParseUint(c.Param("id"), 10, 63)
		if err != nil || groupID == 0 {
			response.BadRequest(c, "Invalid group ID")
			return
		}
		page, size := response.ParsePagination(c)
		if !validAdaptiveScoresPagination(c, page, size) {
			response.BadRequest(c, "Invalid pagination")
			return
		}
		group, err := h.adminService.GetGroupLite(ctx, int64(groupID))
		if err != nil {
			response.ErrorFrom(c, err)
			return
		}
		if group == nil {
			response.NotFound(c, "Group not found")
			return
		}
		accounts, total, err := h.adminService.ListAccounts(ctx, page, size, "", "", "", "", group.ID, "", "id", "asc")
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
			AdaptiveAccountScoresSnapshot: settings.AdaptiveAccountScores(group.ID, accounts),
			GroupID:                       group.ID, Total: total, Page: page, PageSize: size,
			MaxQueryableTotal: min(total, int64((adaptiveScoresMaxOffset/size+1)*size)),
		})
	}
}

func validAdaptiveScoresPagination(c *gin.Context, page, size int) bool {
	if size > adaptiveScoresMaxPageSize || page-1 > adaptiveScoresMaxOffset/size {
		return false
	}
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
