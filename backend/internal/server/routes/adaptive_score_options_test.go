//go:build unit

package routes

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/adminaccess"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type adaptiveScoreRouteGroups struct {
	service.AdminGroupRepository
	groups    []service.Group
	calls     int
	deletedID int64
}

func (r *adaptiveScoreRouteGroups) ListAdaptiveScoreOptions(_ context.Context, afterID int64, limit int) ([]service.AdaptiveScoreOption, error) {
	r.calls++
	options := make([]service.AdaptiveScoreOption, 0, limit)
	for _, g := range r.groups {
		if g.ID <= afterID || g.ID == r.deletedID {
			continue
		}
		options = append(options, service.AdaptiveScoreOption{ID: g.ID, Name: g.Name, Description: g.Description})
		if len(options) == limit {
			break
		}
	}
	return options, nil
}

func (r *adaptiveScoreRouteGroups) GetByIDLite(_ context.Context, id int64) (*service.Group, error) {
	if id < 1 || id > int64(len(r.groups)) {
		return nil, service.ErrGroupNotFound
	}
	return &r.groups[id-1], nil
}

type adaptiveScoreRouteAccounts struct {
	service.AdminAccountRepository
	t *testing.T
}

func (r *adaptiveScoreRouteAccounts) ListWithFilters(_ context.Context, params pagination.PaginationParams, platform, kind, status, search string, groupID int64, privacy string) ([]service.Account, *pagination.PaginationResult, error) {
	require.Equal(r.t, pagination.PaginationParams{Page: 1, PageSize: 20, SortBy: "id", SortOrder: "asc"}, params)
	require.Empty(r.t, platform+kind+status+search+privacy)
	require.EqualValues(r.t, 10001, groupID)
	return []service.Account{{ID: 10, Name: "Existing member", GroupIDs: []int64{groupID}}}, &pagination.PaginationResult{Total: 1}, nil
}

func TestAdaptiveScoreOptionsRegisteredRoutesInSimpleMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeSimple, JWT: config.JWTConfig{Secret: "fixture", ExpireHour: 1}}
	groups := &adaptiveScoreRouteGroups{groups: make([]service.Group, 10002)}
	for i := range groups.groups {
		groups.groups[i] = service.Group{ID: int64(i + 1), Name: fmt.Sprintf("Group %d", i+1)}
	}
	groups.groups[10000] = service.Group{ID: 10001, Name: "Composite", Platform: service.PlatformComposite, Status: service.StatusDisabled,
		Description: "Searchable", ModelAllowlist: service.GroupModelAllowlist{Enabled: true}, RateMultiplier: 7}
	groups.groups[10001] = service.Group{ID: 10002, Name: "Empty", Platform: service.PlatformOpenAI, Status: service.StatusActive}
	svc := service.NewAdminService(cfg, nil, groups, &adaptiveScoreRouteAccounts{t: t}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	settings := service.NewSettingService(&channelMonitorRouteSettingRepoStub{
		values: map[string]string{service.SettingKeyGatewayAdaptiveSchedulingEnabled: "true"},
	}, cfg)
	settings.SetScopedAdminKeyRepository(&scopedRouteKeys{})
	users := &scopedRouteUsers{}
	authService := service.NewAuthService(nil, users, nil, nil, cfg, settings, nil, nil, nil, nil, nil, nil, nil)
	auth := middleware.NewAdminAuthMiddleware(authService, service.NewUserService(users, nil, nil, nil), settings, nil)
	newRouter := func(gate *service.SettingService) *gin.Engine {
		r := gin.New()
		registerGroupRoutes(r.Group("/api/v1/admin", gin.HandlerFunc(auth)), &handler.Handlers{
			Admin: &handler.AdminHandlers{Group: adminhandler.NewGroupHandlerWithConfig(svc, nil, nil, cfg)},
		}, gate)
		return r
	}
	r := newRouter(settings)
	get := func(router *gin.Engine, path, token, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/api/v1/admin/groups/"+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	const options = "adaptive-score-options"
	require.Equal(t, 401, get(r, options, "", "").Code)
	key, _, err := settings.CreateScopedAdminKey(context.Background(), service.ScopedAdminKeyInput{
		Name: "Restricted", Enabled: true, Policy: adminaccess.Policy{GlobalPermissions: []string{"groups.read"}},
	}, 9)
	require.NoError(t, err)
	require.Equal(t, 403, get(r, options, "", key).Code)
	token, err := authService.GenerateToken(context.Background(), &service.User{ID: 9, Role: service.RoleAdmin})
	require.NoError(t, err)
	off := newRouter(service.NewSettingService(&channelMonitorRouteSettingRepoStub{}, cfg))
	disabled := get(off, options, token, "")
	require.Equal(t, 404, disabled.Code)
	require.Contains(t, disabled.Body.String(), "ADAPTIVE_ACCOUNT_SCORES_DISABLED")
	for _, invalid := range []string{"after_id=-1", "after_id=0&after_id=1", "after_id=9223372036854775808", "limit=0", "limit=1001", "limit="} {
		require.Equal(t, 400, get(r, options+"?"+invalid, token, "").Code, invalid)
	}
	require.Zero(t, groups.calls)

	first := get(r, options+"?after_id=0&limit=2", token, "")
	require.Equal(t, 200, first.Code, first.Body.String())
	require.Equal(t, "no-store", first.Header().Get("Cache-Control"))
	require.JSONEq(t, `{"code":0,"message":"success","data":{"items":[
		{"id":1,"name":"Group 1","description":""},
		{"id":2,"name":"Group 2","description":""}
	],"next_after_id":2}}`, first.Body.String())

	// Deleting the cursor row after page one must still leave the next ID first.
	groups.deletedID = 2
	second := get(r, options+"?after_id=2&limit=2", token, "")
	require.Equal(t, 200, second.Code, second.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"items":[
		{"id":3,"name":"Group 3","description":""},
		{"id":4,"name":"Group 4","description":""}
	],"next_after_id":4}}`, second.Body.String())

	last := get(r, options+"?after_id=10000&limit=200", token, "")
	require.Equal(t, 200, last.Code, last.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"items":[
		{"id":10001,"name":"Composite","description":"Searchable"},
		{"id":10002,"name":"Empty","description":""}
	],"next_after_id":null}}`, last.Body.String())
	empty := get(r, options+"?after_id=10002", token, "")
	require.Equal(t, 200, empty.Code, empty.Body.String())
	require.JSONEq(t, `{"code":0,"message":"success","data":{"items":[],"next_after_id":null}}`, empty.Body.String())
	require.Equal(t, 4, groups.calls)

	member := get(r, "10001/adaptive-account-scores", token, "")
	require.Equal(t, 200, member.Code, member.Body.String())
	require.Contains(t, member.Body.String(), `"name":"Existing member"`)
	require.Contains(t, member.Body.String(), `"overall_sample_count":0`)
	missing := get(r, "10003/adaptive-account-scores", token, "")
	require.Equal(t, 404, missing.Code)
	require.NotContains(t, missing.Body.String(), "ADAPTIVE_ACCOUNT_SCORES_DISABLED")
}
