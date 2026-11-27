package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type detectionRouteSettings struct {
	service.SettingRepository
	values map[string]string
}

func (d detectionRouteSettings) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		values[key] = d.values[key]
	}
	return values, nil
}

func (d detectionRouteSettings) GetValue(_ context.Context, key string) (string, error) {
	if key == service.SettingKeyAdminAPIKey {
		return "fixture-only-admin-key", nil
	}
	return `{"enabled":true,"public_ip_rpm":2}`, nil
}

type detectionRouteUsers struct {
	service.UserRepository
	users map[int64]*service.User
}

func (r detectionRouteUsers) GetByID(_ context.Context, id int64) (*service.User, error) {
	return r.users[id], nil
}

func (r detectionRouteUsers) GetFirstAdmin(context.Context) (*service.User, error) {
	return r.users[1], nil
}

func (r detectionRouteUsers) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	return nil, nil
}

func TestChannelDetectionActionsRequireAdminSession(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{JWT: config.JWTConfig{Secret: "fixture-only-jwt-secret", ExpireHour: 1}}
	auth := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, nil, nil)
	users := detectionRouteUsers{users: map[int64]*service.User{
		1: {ID: 1, Status: service.StatusActive, Role: service.RoleAdmin},
		2: {ID: 2, Status: service.StatusActive, Role: service.RoleUser},
	}}
	adminToken, err := auth.GenerateToken(ctx, users.users[1])
	require.NoError(t, err)
	userToken, err := auth.GenerateToken(ctx, users.users[2])
	require.NoError(t, err)
	repo := detectionRouteSettings{}
	settings := service.NewSettingService(repo, cfg)
	router := gin.New()
	admin := router.Group("/api/v1/admin", gin.HandlerFunc(middleware.NewAdminAuthMiddleware(auth, service.NewUserService(users, nil, nil, nil), settings, nil)))
	registerChannelDetectionActions(admin.Group("/channel-detection", detectionNoStore), service.NewChannelDetection(repo, nil, nil, nil))

	for _, endpoint := range []struct {
		method      string
		path        string
		adminStatus int
	}{
		{http.MethodGet, "/results", http.StatusNotFound},
		{http.MethodPost, "/groups/1/trigger", http.StatusConflict},
		{http.MethodPost, "/groups/1/samples/2cfb25be-f231-412a-a1d6-7aa1df2b2be1/confirm", http.StatusBadRequest},
	} {
		for _, principal := range []struct {
			name   string
			bearer string
			apiKey string
			status int
		}{
			{name: "administrator", bearer: adminToken, status: endpoint.adminStatus},
			{name: "ordinary user", bearer: userToken, status: http.StatusForbidden},
			{name: "visitor", status: http.StatusUnauthorized},
			{name: "administrator key", bearer: adminToken, apiKey: "fixture-only-admin-key", status: http.StatusForbidden},
		} {
			t.Run(endpoint.path+"/"+principal.name, func(t *testing.T) {
				request := httptest.NewRequest(endpoint.method, "/api/v1/admin/channel-detection"+endpoint.path, strings.NewReader(`{"scope_version":"`+strings.Repeat("0", 64)+`","account_id":1}`))
				if principal.bearer != "" {
					request.Header.Set("Authorization", "Bearer "+principal.bearer)
				}
				if principal.apiKey != "" {
					request.Header.Set("x-api-key", principal.apiKey)
				}
				recorder := httptest.NewRecorder()
				router.ServeHTTP(recorder, request)
				require.Equal(t, principal.status, recorder.Code, recorder.Body.String())
				if principal.name == "administrator" || principal.apiKey != "" {
					require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
				}
			})
		}
	}
}

func TestChannelDetectionAnonymousRoutesNoStoreAndRateLimit(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	repo := detectionRouteSettings{}
	settings := service.NewSettingService(repo, &config.Config{})
	detection := service.NewChannelDetection(repo, nil, nil, nil)
	router := gin.New()
	require.NoError(t, router.SetTrustedProxies(nil))
	v1 := router.Group("/api/v1")
	admin := v1.Group("/admin", func(c *gin.Context) { c.AbortWithStatus(http.StatusUnauthorized) })
	RegisterChannelDetectionRoutes(v1, admin, detection, middleware.NewPanelRateLimiter(rdb, settings))

	request := func(path string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/api/v1"+path, nil)
		r.RemoteAddr = "203.0.113.42:10000"
		router.ServeHTTP(w, r)
		require.Equal(t, status, w.Code, w.Body.String())
		return w
	}
	status := request("/channel-detection/status", http.StatusOK)
	require.Contains(t, status.Body.String(), `"enabled":false`)
	require.Equal(t, "no-store", status.Header().Get("Cache-Control"))
	off := request("/channel-detection", http.StatusNotFound)
	require.Equal(t, "no-store", off.Header().Get("Cache-Control"))
	limited := request("/channel-detection", http.StatusTooManyRequests)
	require.Equal(t, "no-store", limited.Header().Get("Cache-Control"))
	request("/admin/channel-detection", http.StatusUnauthorized)
}

func TestChannelDetectionAdminConcurrencyConfig(t *testing.T) {
	repo := detectionRouteSettings{values: map[string]string{
		service.SettingKeyChannelDetectionFeature: "on",
		service.SettingKeyChannelDetectionConfig:  `{"enabled":false,"interval_minutes":60,"groups":[]}`,
	}}
	router := gin.New()
	v1 := router.Group("/api/v1")
	RegisterChannelDetectionRoutes(v1, v1.Group("/admin"), service.NewChannelDetection(repo, nil, nil, nil), nil)
	request := func(method, body string, status int) string {
		t.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, "/api/v1/admin/channel-detection", strings.NewReader(body)))
		require.Equal(t, status, recorder.Code, recorder.Body.String())
		return recorder.Body.String()
	}
	require.Contains(t, request(http.MethodGet, "", http.StatusOK), `"concurrent_groups":3`)
	request(http.MethodPut, `{"enabled":false,"interval_minutes":60,"groups":[]}`, http.StatusOK)
	request(http.MethodPut, `{"enabled":false,"interval_minutes":60,"groups":[],"concurrent_groups":3}`, http.StatusOK)
	for _, value := range []string{"0", "-1", "11", "1.5", `"3"`, "null"} {
		request(http.MethodPut, `{"interval_minutes":60,"concurrent_groups":`+value+`}`, http.StatusBadRequest)
	}
}

func TestChannelDetectionDoesNotAuthenticatePublicOrUnmarkedRequests(t *testing.T) {
	public := gin.New()
	cfg := &config.Config{}
	auth := middleware.NewAPIKeyAuthMiddleware(nil, nil, cfg)
	private := RegisterGatewayRoutes(public, &handler.Handlers{}, auth, nil, nil, nil, nil, nil, cfg)
	for _, test := range []struct {
		handler http.Handler
		path    string
		status  int
	}{
		{public, "/v1/responses?internal=true&group_id=1", http.StatusUnauthorized},
		{private, "/v1/responses", http.StatusForbidden},
		{private, "/v1/responses/compact", http.StatusNotFound},
	} {
		request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"model":"fixture","stream":false}`))
		request.RemoteAddr = "127.0.0.1:12345"
		request.Header.Set("X-Group-ID", "1")
		request.Header.Set("X-Channel-Detection", "true")
		request.Header.Set("User-Agent", "VanillaAPI-ChannelDetection/1")
		response := httptest.NewRecorder()
		test.handler.ServeHTTP(response, request)
		require.Equal(t, test.status, response.Code, response.Body.String())
	}
}
