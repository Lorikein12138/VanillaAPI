package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type userGroupRatesPartialFailureAdmin struct {
	service.AdminService
	input *service.UpdateUserInput
}

func (s *userGroupRatesPartialFailureAdmin) UpdateUser(_ context.Context, _ int64, input *service.UpdateUserInput) (*service.User, error) {
	s.input = input
	return nil, infraerrors.InternalServer("USER_GROUP_RATES_SAVE_FAILED", "User details were saved, but group rate changes failed").
		WithMetadata(map[string]string{"user_details_saved": "true", "group_rates_saved": "false"})
}

func TestUserHandlerUpdateGroupRatesPartialFailure(t *testing.T) {
	admin := &userGroupRatesPartialFailureAdmin{}
	handler := &UserHandler{adminService: admin}
	router := gin.New()
	router.PUT("/api/v1/admin/users/:id", handler.Update)
	request := httptest.NewRequest(http.MethodPut, "/api/v1/admin/users/42", strings.NewReader(`{"username":"new","group_rates":{"7":1.5}}`))
	request.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, request)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.JSONEq(t, `{"code":500,"message":"User details were saved, but group rate changes failed","reason":"USER_GROUP_RATES_SAVE_FAILED","metadata":{"user_details_saved":"true","group_rates_saved":"false"}}`, rec.Body.String())
	require.NotNil(t, admin.input)
	require.Equal(t, 1.5, *admin.input.GroupRates[7])
}
