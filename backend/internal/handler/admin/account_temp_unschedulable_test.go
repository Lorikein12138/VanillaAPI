//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type clearTempSuspensionRepo struct {
	service.AccountRepository
	clearErr    error
	modelClears int
}

func (r *clearTempSuspensionRepo) ClearTempUnschedulable(context.Context, int64) error {
	return r.clearErr
}

func (r *clearTempSuspensionRepo) ClearModelRateLimits(context.Context, int64) error {
	r.modelClears++
	return nil
}

func TestAdminClearTempUnschedulableConflictLeavesModelLimits(t *testing.T) {
	repo := &clearTempSuspensionRepo{clearErr: service.ErrTempUnschedulableProtected}
	h := &AccountHandler{rateLimitService: service.NewRateLimitService(repo, nil, &config.Config{}, nil, nil)}
	router := gin.New()
	router.DELETE("/accounts/:id/temp-unschedulable", h.ClearTempUnschedulable)
	request := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/accounts/42/temp-unschedulable", nil))
		return recorder
	}

	protected := request()
	require.Equal(t, http.StatusConflict, protected.Code)
	require.Contains(t, protected.Body.String(), "ACCOUNT_TEMP_UNSCHEDULABLE_PROTECTED")
	require.Zero(t, repo.modelClears)
	repo.clearErr = nil
	require.Equal(t, http.StatusOK, request().Code)
	require.Equal(t, 1, repo.modelClears)
}
