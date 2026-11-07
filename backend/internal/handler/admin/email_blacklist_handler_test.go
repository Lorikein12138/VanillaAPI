package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type emailBlacklistSettingRepoStub struct {
	*settingHandlerRepoStub
	setCalls int
}

func (s *emailBlacklistSettingRepoStub) Set(_ context.Context, key, value string) error {
	s.setCalls++
	s.values[key] = value
	return nil
}

type emailBlacklistAdminServiceStub struct {
	*stubAdminService
	findEmails   []string
	matched      []service.User
	disableIDs   []int64
	disableCalls int
}

func (s *emailBlacklistAdminServiceStub) FindActiveUsersByBlacklistedEmails(_ context.Context, emails []string) ([]service.User, error) {
	s.findEmails = append([]string(nil), emails...)
	return s.matched, nil
}

func (s *emailBlacklistAdminServiceStub) BatchDisableUsers(_ context.Context, userIDs []int64) (int, error) {
	s.disableCalls++
	s.disableIDs = append([]int64(nil), userIDs...)
	return len(userIDs), nil
}

func serveEmailBlacklistJSON(t *testing.T, router *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if raw, ok := body.(string); ok {
		reader = bytes.NewReader([]byte(raw))
	} else {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder
}

func setupEmailBlacklistSettingRouter(repo *emailBlacklistSettingRepoStub) *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := service.NewSettingService(repo, &config.Config{})
	handler := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.GET("/api/v1/admin/settings/registration-email-blacklist", handler.GetRegistrationEmailBlacklist)
	router.PUT("/api/v1/admin/settings/registration-email-blacklist", handler.UpdateRegistrationEmailBlacklist)
	return router
}

func TestUpdateRegistrationEmailBlacklist_RequiresEmailsField(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyRegistrationEmailBlacklist: `["keep@gmail.com"]`,
	}}}
	router := setupEmailBlacklistSettingRouter(repo)

	rec := serveEmailBlacklistJSON(t, router, http.MethodPut, "/api/v1/admin/settings/registration-email-blacklist", `{}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, repo.setCalls)
}

func TestUpdateRegistrationEmailBlacklist_ReturnsSavedAndAdded(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{
		service.SettingKeyRegistrationEmailBlacklist: `["keep@gmail.com"]`,
	}}}
	router := setupEmailBlacklistSettingRouter(repo)

	rec := serveEmailBlacklistJSON(t, router, http.MethodPut, "/api/v1/admin/settings/registration-email-blacklist",
		map[string]any{"emails": []string{"keep@gmail.com", "NEW@gmail.com"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Data UpdateRegistrationEmailBlacklistResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, []string{"keep@gmail.com", "new@gmail.com"}, resp.Data.Emails)
	require.Equal(t, []string{"new@gmail.com"}, resp.Data.Added)
	require.Equal(t, service.MaxRegistrationEmailBlacklistSize, resp.Data.Max)

	rec = serveEmailBlacklistJSON(t, router, http.MethodGet, "/api/v1/admin/settings/registration-email-blacklist", `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "new@gmail.com")
}

type concurrentEmailBlacklistRepo struct {
	*settingHandlerRepoStub
	mu          sync.Mutex
	firstStored chan struct{}
	firstReturn chan struct{}
	writes      int
}

func (r *concurrentEmailBlacklistRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settingHandlerRepoStub.GetValue(ctx, key)
}

func (r *concurrentEmailBlacklistRepo) Set(ctx context.Context, key, value string) error {
	r.mu.Lock()
	r.values[key] = value
	r.writes++
	first := r.writes == 1
	r.mu.Unlock()
	if first {
		close(r.firstStored)
		select {
		case <-r.firstReturn:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestUpdateRegistrationEmailBlacklist_ConcurrentPUTKeepsStoreAndCacheConsistent(t *testing.T) {
	repo := &concurrentEmailBlacklistRepo{
		settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{}},
		firstStored:            make(chan struct{}),
		firstReturn:            make(chan struct{}),
	}
	releaseFirst := sync.OnceFunc(func() { close(repo.firstReturn) })
	t.Cleanup(releaseFirst)
	svc := service.NewSettingService(repo, &config.Config{})
	handler := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	const path = "/api/v1/admin/settings/registration-email-blacklist"
	secondEntered := make(chan struct{})
	router.PUT(path, func(c *gin.Context) {
		if c.GetHeader("X-Test-Request") == "second" {
			close(secondEntered)
		}
		handler.UpdateRegistrationEmailBlacklist(c)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.False(t, svc.IsRegistrationEmailBlacklisted(ctx, "shared@example.test"))
	put := func(id, body string) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(ctx, http.MethodPut, path, bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Test-Request", id)
			router.ServeHTTP(rec, req)
			done <- rec
		}()
		return done
	}
	first := put("first", `{"emails":["shared@example.test","first@example.test"]}`)
	select {
	case <-repo.firstStored:
	case <-ctx.Done():
		t.Fatal("first PUT did not write its list")
	}
	second := put("second", `{"emails":["shared@example.test","second@example.test"]}`)
	select {
	case <-secondEntered:
	case <-ctx.Done():
		t.Fatal("second PUT did not enter the handler")
	}
	select {
	case <-second:
		t.Fatal("second PUT completed while the first was publishing its list")
	case <-time.After(100 * time.Millisecond):
	}
	releaseFirst()

	for _, result := range []struct {
		done  <-chan *httptest.ResponseRecorder
		added []string
	}{
		{first, []string{"shared@example.test", "first@example.test"}},
		{second, []string{"second@example.test"}},
	} {
		select {
		case rec := <-result.done:
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp struct {
				Data UpdateRegistrationEmailBlacklistResponse `json:"data"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.Equal(t, result.added, resp.Data.Added)
		case <-ctx.Done():
			t.Fatal("concurrent PUT did not complete")
		}
	}
	stored, err := svc.GetRegistrationEmailBlacklist(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"shared@example.test", "second@example.test"}, stored)
	require.True(t, svc.IsRegistrationEmailBlacklisted(ctx, "shared@example.test"))
	require.True(t, svc.IsRegistrationEmailBlacklisted(ctx, "second@example.test"))
	require.False(t, svc.IsRegistrationEmailBlacklisted(ctx, "first@example.test"))
}

func TestUpdateRegistrationEmailBlacklist_InvalidEntryRejected(t *testing.T) {
	repo := &emailBlacklistSettingRepoStub{settingHandlerRepoStub: &settingHandlerRepoStub{values: map[string]string{}}}
	router := setupEmailBlacklistSettingRouter(repo)

	rec := serveEmailBlacklistJSON(t, router, http.MethodPut, "/api/v1/admin/settings/registration-email-blacklist",
		map[string]any{"emails": []string{"ok@gmail.com", "broken"}})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "EMAIL_BLACKLIST_INVALID_ENTRY")
	require.Zero(t, repo.setCalls)
}

func setupEmailBlacklistUserRouter(stub service.AdminService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	handler := NewUserHandler(stub, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/api/v1/admin/users/blacklist-matches", handler.FindBlacklistedEmailUsers)
	router.POST("/api/v1/admin/users/batch-disable", handler.BatchDisable)
	return router
}

func TestFindBlacklistedEmailUsers_ReturnsMatchedUsers(t *testing.T) {
	stub := &emailBlacklistAdminServiceStub{
		stubAdminService: newStubAdminService(),
		matched:          []service.User{{ID: 3, Email: "a.b@gmail.com", Username: "ab", PasswordHash: "secret"}},
	}
	router := setupEmailBlacklistUserRouter(stub)

	rec := serveEmailBlacklistJSON(t, router, http.MethodPost, "/api/v1/admin/users/blacklist-matches",
		map[string]any{"emails": []string{"ab@gmail.com"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{"ab@gmail.com"}, stub.findEmails)

	var resp struct {
		Data struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 1, resp.Data.Total)
	require.Equal(t, map[string]any{"id": float64(3), "email": "a.b@gmail.com", "username": "ab"}, resp.Data.Items[0])
}

func TestFindBlacklistedEmailUsers_RejectsOversizedRequest(t *testing.T) {
	stub := &emailBlacklistAdminServiceStub{stubAdminService: newStubAdminService()}
	router := setupEmailBlacklistUserRouter(stub)

	emails := make([]string, service.MaxRegistrationEmailBlacklistSize+1)
	for i := range emails {
		emails[i] = "x@gmail.com"
	}
	rec := serveEmailBlacklistJSON(t, router, http.MethodPost, "/api/v1/admin/users/blacklist-matches",
		map[string]any{"emails": emails})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Nil(t, stub.findEmails)
}

func TestBatchDisable_ValidatesAndForwards(t *testing.T) {
	stub := &emailBlacklistAdminServiceStub{stubAdminService: newStubAdminService()}
	router := setupEmailBlacklistUserRouter(stub)

	rec := serveEmailBlacklistJSON(t, router, http.MethodPost, "/api/v1/admin/users/batch-disable",
		map[string]any{"user_ids": []int64{}})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Zero(t, stub.disableCalls)

	rec = serveEmailBlacklistJSON(t, router, http.MethodPost, "/api/v1/admin/users/batch-disable",
		map[string]any{"user_ids": []int64{4, 5}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []int64{4, 5}, stub.disableIDs)
	require.Contains(t, rec.Body.String(), `"affected":2`)
}
