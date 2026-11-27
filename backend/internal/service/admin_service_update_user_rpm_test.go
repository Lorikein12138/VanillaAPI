//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// rpmUserRepoStub 复用 admin_service_update_balance_test.go 的基础 stub 结构，
// 只在 Update 时把入参克隆一份，便于断言修改后的 RPMLimit。
type rpmUserRepoStub struct {
	*userRepoStub
	lastUpdated *User
}

func (s *rpmUserRepoStub) Update(_ context.Context, user *User, _ UserUpdateFields) error {
	if user == nil {
		return nil
	}
	clone := *user
	s.lastUpdated = &clone
	if s.userRepoStub != nil {
		s.userRepoStub.user = &clone
	}
	return nil
}

func TestAdminService_UpdateUser_InvalidatesAuthCacheOnRPMLimitChange(t *testing.T) {
	base := &userRepoStub{user: &User{ID: 42, Email: "u@example.com", RPMLimit: 10}}
	repo := &rpmUserRepoStub{userRepoStub: base}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		userRepo:             repo,
		redeemCodeRepo:       &redeemRepoStub{},
		authCacheInvalidator: invalidator,
	}

	newRPM := 60
	updated, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		RPMLimit: &newRPM,
	})
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 60, updated.RPMLimit)
	require.Equal(t, []int64{42}, invalidator.userIDs, "仅修改 RPMLimit 也应失效 API Key 认证缓存")
}

func TestAdminService_UpdateUser_NoInvalidateWhenRPMLimitUnchanged(t *testing.T) {
	base := &userRepoStub{user: &User{ID: 42, Email: "u@example.com", RPMLimit: 10, Username: "old"}}
	repo := &rpmUserRepoStub{userRepoStub: base}
	invalidator := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		userRepo:             repo,
		redeemCodeRepo:       &redeemRepoStub{},
		authCacheInvalidator: invalidator,
	}

	newName := "new"
	sameRPM := 10
	_, err := svc.UpdateUser(context.Background(), 42, &UpdateUserInput{
		Username: &newName,
		RPMLimit: &sameRPM,
	})
	require.NoError(t, err)
	require.Empty(t, invalidator.userIDs, "只改 username 不应触发认证缓存失效")
}

type failingUserRates struct {
	UserGroupRateRepository
	err error
}

func (r failingUserRates) SyncUserGroupRates(context.Context, int64, map[int64]*float64) error {
	return r.err
}

type concurrencyAdjustmentRecorder struct {
	RedeemCodeRepository
	record *RedeemCode
}

func (r *concurrencyAdjustmentRecorder) Create(_ context.Context, code *RedeemCode) error {
	r.record = code
	return nil
}

func TestAdminService_UpdateUser_GroupRatesPartialFailureKeepsProfileSideEffects(t *testing.T) {
	repo := &rpmUserRepoStub{userRepoStub: &userRepoStub{user: &User{ID: 42, Role: RoleUser, Concurrency: 1}}}
	invalidator := &authCacheInvalidatorStub{}
	audit := &concurrencyAdjustmentRecorder{}
	rateErr := errors.New("fixture write failed")
	svc := &adminServiceImpl{userRepo: repo, userGroupRateRepo: failingUserRates{err: rateErr},
		authCacheInvalidator: invalidator, redeemCodeRepo: audit}
	updated, err := svc.UpdateUser(t.Context(), 42, &UpdateUserInput{
		Role: RoleAdmin, Concurrency: new(3), GroupRates: map[int64]*float64{7: new(1.5)},
	})
	require.Nil(t, updated)
	require.ErrorIs(t, err, rateErr)
	status, details := infraerrors.ToHTTP(err)
	require.Equal(t, http.StatusInternalServerError, status)
	require.Equal(t, "USER_GROUP_RATES_SAVE_FAILED", details.Reason)
	require.Equal(t, map[string]string{"user_details_saved": "true", "group_rates_saved": "false"}, details.Metadata)
	require.Equal(t, RoleAdmin, repo.lastUpdated.Role)
	require.Equal(t, 3, repo.lastUpdated.Concurrency)
	require.Equal(t, []int64{42}, invalidator.userIDs)
	require.NotNil(t, audit.record)
	require.Equal(t, AdjustmentTypeAdminConcurrency, audit.record.Type)
	require.Equal(t, 2.0, audit.record.Value)
	require.Equal(t, int64(42), *audit.record.UsedBy)
}
