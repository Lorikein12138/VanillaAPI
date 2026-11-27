//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type clearTempSuspensionRepo struct {
	service.AccountRepository
	account      *service.Account
	clearErr     error
	modelClears  int
	manualClears int
}

func (r *clearTempSuspensionRepo) GetByID(context.Context, int64) (*service.Account, error) {
	return r.account, nil
}

func (r *clearTempSuspensionRepo) ClearRateLimit(context.Context, int64) error {
	r.account.RateLimitResetAt = nil
	return nil
}

func (r *clearTempSuspensionRepo) ClearAntigravityQuotaScopes(context.Context, int64) error {
	return nil
}

func (r *clearTempSuspensionRepo) ClearTempUnschedulableManually(context.Context, int64) error {
	if r.clearErr != nil {
		return r.clearErr
	}
	r.manualClears++
	r.account.TempUnschedulableUntil = nil
	r.account.TempUnschedulableProtectedUntil = nil
	return nil
}

func (r *clearTempSuspensionRepo) ClearModelRateLimits(context.Context, int64) error {
	r.modelClears++
	return nil
}

type recoveryAdminService struct {
	service.AdminService
	repo *clearTempSuspensionRepo
}

func (s recoveryAdminService) GetAccount(ctx context.Context, id int64) (*service.Account, error) {
	return s.repo.GetByID(ctx, id)
}

func TestAdminRecoverStateClearsProtectedTempSuspension(t *testing.T) {
	until := time.Now().Add(time.Hour)
	repo := &clearTempSuspensionRepo{account: &service.Account{
		ID: 42, Status: service.StatusActive, RateLimitResetAt: &until,
		TempUnschedulableUntil: &until, TempUnschedulableProtectedUntil: &until,
	}}
	h := &AccountHandler{
		adminService:     recoveryAdminService{repo: repo},
		rateLimitService: service.NewRateLimitService(repo, nil, &config.Config{}, nil, nil),
	}
	router := gin.New()
	router.POST("/api/v1/admin/accounts/:id/recover-state", h.RecoverState)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/42/recover-state", nil))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Contains(t, recorder.Body.String(), `"temp_unschedulable_until":null`)
	require.Equal(t, 1, repo.manualClears)
	require.Equal(t, 1, repo.modelClears)
	require.Nil(t, repo.account.RateLimitResetAt)
}

func TestAdminClearTempUnschedulableKeepsAccountRateLimit(t *testing.T) {
	until := time.Now().Add(time.Hour)
	repo := &clearTempSuspensionRepo{
		account:  &service.Account{RateLimitResetAt: &until, TempUnschedulableUntil: &until, TempUnschedulableProtectedUntil: &until},
		clearErr: service.ErrAccountNotFound,
	}
	h := &AccountHandler{rateLimitService: service.NewRateLimitService(repo, nil, &config.Config{}, nil, nil)}
	router := gin.New()
	router.DELETE("/accounts/:id/temp-unschedulable", h.ClearTempUnschedulable)
	request := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodDelete, "/accounts/42/temp-unschedulable", nil))
		return recorder
	}

	require.Equal(t, http.StatusNotFound, request().Code)
	require.Zero(t, repo.modelClears)
	repo.clearErr = nil
	require.Equal(t, http.StatusOK, request().Code)
	require.Equal(t, 1, repo.manualClears)
	require.Equal(t, 1, repo.modelClears)
	require.Equal(t, &until, repo.account.RateLimitResetAt)
	require.Nil(t, repo.account.TempUnschedulableUntil)
}
