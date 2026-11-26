package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type adaptiveScoresAdminStub struct {
	stubAdminService
	groupReads int
	listErr    error
	deepTotal  int64
}

func (s *adaptiveScoresAdminStub) GetGroupLite(_ context.Context, id int64) (*service.Group, error) {
	s.groupReads++
	if id > 2 {
		return nil, service.ErrGroupNotFound
	}
	return &service.Group{ID: id}, nil
}

func (s *adaptiveScoresAdminStub) ListAccounts(ctx context.Context, page, size int, platform, kind, status, search string, groupID int64, privacy, sortBy, order string) ([]service.Account, int64, error) {
	if s.listErr != nil {
		return nil, 0, s.listErr
	}
	if groupID == 2 {
		return []service.Account{}, 0, nil
	}
	accounts, total, err := s.stubAdminService.ListAccounts(ctx, page, size, platform, kind, status, search, groupID, privacy, sortBy, order)
	return accounts, max(total, s.deepTotal), err
}

type adaptiveScoresUserRepo struct {
	service.UserRepository
	users map[int64]service.User
}

func (s *adaptiveScoresUserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	user, ok := s.users[id]
	if !ok {
		return nil, service.ErrUserNotFound
	}
	return &user, nil
}

func (*adaptiveScoresUserRepo) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	return nil, nil
}

func TestGroupAdaptiveAccountScoresPermissionsGateAndPagination(t *testing.T) {
	settingHandler, settingsRepo := newStepUpSwitchTestHandler(t, map[string]string{})
	settings := settingHandler.settingService
	adminService := &adaptiveScoresAdminStub{}
	adminService.accounts = []service.Account{
		{ID: 1, Name: "first", GroupIDs: []int64{1}},
		{ID: 2, Name: "second", GroupIDs: []int64{1}, Platform: service.PlatformOpenAI,
			Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
			Credentials: map[string]any{"api_key": "fixture-secret"}, Extra: map[string]any{"private": "fixture-extra"},
			ErrorMessage: "fixture-private-error"},
	}
	h := NewGroupHandler(adminService, nil, nil)
	users := &adaptiveScoresUserRepo{users: map[int64]service.User{
		1: {ID: 1, Role: service.RoleAdmin, Status: service.StatusActive},
		2: {ID: 2, Role: service.RoleUser, Status: service.StatusActive},
	}}
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "local-test-only", ExpireHour: 1}}
	auth := service.NewAuthService(nil, users, nil, nil, cfg, settings, nil, nil, nil, nil, nil, nil, nil)
	userService := service.NewUserService(users, settingsRepo, nil, nil)
	router := gin.New()
	route := router.Group("/api/v1/admin/groups", gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, userService, settings, nil)))
	route.Use(h.ScopedAdminKeyGroupModels)
	route.GET("/:id/adaptive-account-scores", h.AdaptiveAccountScores(settings))
	adminToken, err := auth.GenerateToken(context.Background(), &service.User{ID: 1, Role: service.RoleAdmin})
	require.NoError(t, err)
	userToken, err := auth.GenerateToken(context.Background(), &service.User{ID: 2, Role: service.RoleUser})
	require.NoError(t, err)

	request := func(method, suffix, token string, p *adminaccess.Principal) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v1/admin/groups/"+suffix, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if p != nil {
			req = req.WithContext(adminaccess.WithPrincipal(req.Context(), p))
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	const page = "1/adaptive-account-scores"
	require.Equal(t, 401, request("GET", page, "", nil).Code)
	require.Equal(t, 403, request("GET", page, userToken, nil).Code)
	disabled := request("GET", page, adminToken, nil)
	require.Equal(t, 404, disabled.Code)
	require.Contains(t, disabled.Body.String(), "ADAPTIVE_ACCOUNT_SCORES_DISABLED")
	require.Zero(t, adminService.groupReads)
	require.Equal(t, 200, doUpdateSettings(t, settingHandler, map[string]any{service.SettingKeyGatewayAdaptiveSchedulingEnabled: true}, nil).Code)
	restricted := &adminaccess.Principal{Policy: adminaccess.Policy{GlobalPermissions: []string{"groups.read", "accounts.read"}}}
	require.Equal(t, 403, request("GET", page, adminToken, restricted).Code)
	require.Equal(t, 404, request("POST", page, adminToken, nil).Code)
	for _, invalid := range []string{
		"0/adaptive-account-scores", "7x/adaptive-account-scores", page + "?page=0", page + "?page=1002&page_size=20",
		page + "?page_size=51", page + "?page_size=1001", page + "?page=1&page=2", page + "?page_size=",
	} {
		require.Equal(t, 400, request("GET", invalid, adminToken, nil).Code, invalid)
	}
	require.Zero(t, adminService.groupReads)
	require.Equal(t, 404, request("GET", "3/adaptive-account-scores", adminToken, nil).Code)
	full := &adminaccess.Principal{Policy: adminaccess.Policy{FullAccess: true}}
	rec := request("GET", page+"?page=2&page_size=1", adminToken, full)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var envelope struct {
		Data struct {
			service.AdaptiveAccountScoresSnapshot
			GroupID           int64 `json:"group_id"`
			Total             int64
			MaxQueryableTotal int64 `json:"max_queryable_total"`
			Page              int
			PageSize          int `json:"page_size"`
		}
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.EqualValues(t, 1, envelope.Data.GroupID)
	require.EqualValues(t, 2, envelope.Data.Total)
	require.EqualValues(t, 2, envelope.Data.MaxQueryableTotal)
	require.Equal(t, 2, envelope.Data.Page)
	require.Equal(t, 1, envelope.Data.PageSize)
	require.Len(t, envelope.Data.Items, 1)
	require.EqualValues(t, 2, envelope.Data.Items[0].ID)
	require.True(t, envelope.Data.Items[0].SchedulableNow)
	require.Nil(t, envelope.Data.Items[0].OverallScore)
	require.Zero(t, envelope.Data.Items[0].OverallSampleCount)
	require.Contains(t, rec.Body.String(), "\"overall_score\":null")
	require.NotNil(t, envelope.Data.Items[0].V1)
	require.NotNil(t, envelope.Data.Items[0].V2)
	for _, term := range []string{"fixture-secret", "fixture-extra", "fixture-private-error", "\"credentials\"", "\"extra\""} {
		require.NotContains(t, rec.Body.String(), term)
	}
	require.Equal(t, 2, adminService.groupReads)
	require.Equal(t, 1, adminService.lastListAccounts.calls)
	require.EqualValues(t, 1, adminService.lastListAccounts.groupID)
	require.Equal(t, "id", adminService.lastListAccounts.sortBy)
	require.Equal(t, "asc", adminService.lastListAccounts.sortOrder)
	require.Zero(t, adminService.schedulerScoreFilterCalls)
	empty := request("GET", "2/adaptive-account-scores", adminToken, nil)
	require.Equal(t, 200, empty.Code)
	require.Contains(t, empty.Body.String(), "\"items\":[]")
	adminService.deepTotal = 10021
	deep := request("GET", page+"?page=501&page_size=20", adminToken, nil)
	require.Equal(t, 200, deep.Code, deep.Body.String())
	require.NoError(t, json.Unmarshal(deep.Body.Bytes(), &envelope))
	require.EqualValues(t, 10021, envelope.Data.Total)
	require.EqualValues(t, 10020, envelope.Data.MaxQueryableTotal)
	require.Equal(t, 501, envelope.Data.Page)
	require.Equal(t, 400, request("GET", page+"?page=502&page_size=20", adminToken, nil).Code)
	adminService.listErr = errors.New("fixture storage error")
	require.Equal(t, 500, request("GET", page, adminToken, nil).Code)
	require.Equal(t, 200, doUpdateSettings(t, settingHandler, map[string]any{service.SettingKeyGatewayAdaptiveSchedulingEnabled: false}, nil).Code)
	require.Equal(t, 404, request("GET", page, adminToken, nil).Code)
}
