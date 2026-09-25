//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type modelAllowlistCacheAccountRepoStub struct {
	AccountRepository
	accounts         map[int64]*Account
	createdAccount   *Account
	deletedAccountID []int64
	boundGroupIDs    map[int64][]int64
	shadowAccounts   []*Account
}

func (r *modelAllowlistCacheAccountRepoStub) Create(_ context.Context, account *Account) error {
	if account.ID == 0 {
		account.ID = 100
	}
	r.createdAccount = account
	if r.accounts == nil {
		r.accounts = make(map[int64]*Account)
	}
	r.accounts[account.ID] = account
	return nil
}

func (r *modelAllowlistCacheAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	return r.accounts[id], nil
}

func (r *modelAllowlistCacheAccountRepoStub) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	accounts := make([]*Account, 0, len(ids))
	for _, id := range ids {
		if account := r.accounts[id]; account != nil {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func (r *modelAllowlistCacheAccountRepoStub) Update(_ context.Context, account *Account) error {
	if r.accounts == nil {
		r.accounts = make(map[int64]*Account)
	}
	r.accounts[account.ID] = account
	return nil
}

func (r *modelAllowlistCacheAccountRepoStub) BindGroups(_ context.Context, accountID int64, groupIDs []int64) error {
	if r.boundGroupIDs == nil {
		r.boundGroupIDs = make(map[int64][]int64)
	}
	r.boundGroupIDs[accountID] = append([]int64(nil), groupIDs...)
	if account := r.accounts[accountID]; account != nil {
		account.GroupIDs = append([]int64(nil), groupIDs...)
	}
	return nil
}

func (r *modelAllowlistCacheAccountRepoStub) BulkUpdate(_ context.Context, _ []int64, _ AccountBulkUpdate) (int64, error) {
	return int64(len(r.accounts)), nil
}

func (r *modelAllowlistCacheAccountRepoStub) ListShadowsByParent(_ context.Context, _ int64) ([]*Account, error) {
	return r.shadowAccounts, nil
}

func (r *modelAllowlistCacheAccountRepoStub) Delete(_ context.Context, id int64) error {
	r.deletedAccountID = append(r.deletedAccountID, id)
	delete(r.accounts, id)
	return nil
}

func (r *modelAllowlistCacheAccountRepoStub) SetSchedulable(_ context.Context, id int64, schedulable bool) error {
	if account := r.accounts[id]; account != nil {
		account.Schedulable = schedulable
	}
	return nil
}

func TestAdminService_CreateAccountInvalidatesBoundGroupAllowlistCache(t *testing.T) {
	repo := &modelAllowlistCacheAccountRepoStub{}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		accountRepo:          repo,
		groupRepo:            &groupRepoStubForAdmin{getByID: &Group{ID: 7}},
		authCacheInvalidator: cache,
	}

	account, err := svc.CreateAccount(context.Background(), &CreateAccountInput{
		Name:                  "new account",
		Platform:              PlatformAnthropic,
		Type:                  AccountTypeAPIKey,
		Credentials:           map[string]any{"model_mapping": map[string]any{"model-a": "model-a"}},
		GroupIDs:              []int64{7, 7},
		SkipDefaultGroupBind:  true,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(100), repo.createdAccount.ID)
	require.Equal(t, []int64{7, 7}, repo.boundGroupIDs[account.ID])
	require.Equal(t, []int64{7}, cache.groupIDs)
}

func TestAdminService_UpdateAccountInvalidatesPreviousAndNewGroupsOnce(t *testing.T) {
	account := &Account{
		ID:          11,
		Name:        "account",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		GroupIDs:    []int64{7, 8},
		Credentials: map[string]any{"model_mapping": map[string]any{"model-a": "model-a"}},
	}
	repo := &modelAllowlistCacheAccountRepoStub{accounts: map[int64]*Account{11: account}}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		accountRepo:          repo,
		groupRepo:            &groupRepoStubForAdmin{getByID: &Group{ID: 9}},
		authCacheInvalidator: cache,
	}
	newGroupIDs := []int64{8, 9, 9}

	updated, err := svc.UpdateAccount(context.Background(), 11, &UpdateAccountInput{
		Credentials:           map[string]any{"model_mapping": map[string]any{"model-b": "model-b"}},
		Status:                StatusDisabled,
		GroupIDs:              &newGroupIDs,
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, []int64{8, 9, 9}, repo.boundGroupIDs[11])
	require.Equal(t, []int64{7, 8, 9}, cache.groupIDs)
}

func TestAdminService_BulkUpdateAccountsInvalidatesOldAndNewGroupsOnce(t *testing.T) {
	repo := &modelAllowlistCacheAccountRepoStub{accounts: map[int64]*Account{
		21: {ID: 21, GroupIDs: []int64{7, 8}},
		22: {ID: 22, GroupIDs: []int64{8, 11}},
	}}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{
		accountRepo:          repo,
		groupRepo:            &groupRepoStubForAdmin{getByID: &Group{ID: 9}},
		authCacheInvalidator: cache,
	}
	newGroupIDs := []int64{8, 9}

	result, err := svc.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:            []int64{21, 22},
		GroupIDs:              &newGroupIDs,
		Status:                StatusDisabled,
		Schedulable:           boolPtr(false),
		SkipMixedChannelCheck: true,
	})

	require.NoError(t, err)
	require.Equal(t, 2, result.Success)
	require.Equal(t, []int64{7, 8, 11, 9}, cache.groupIDs)

	statusSchedulableCache := &authCacheInvalidatorStub{}
	statusSchedulableRepo := &modelAllowlistCacheAccountRepoStub{accounts: map[int64]*Account{
		21: {ID: 21, GroupIDs: []int64{7, 8}},
		22: {ID: 22, GroupIDs: []int64{8, 11}},
	}}
	statusSchedulableService := &adminServiceImpl{
		accountRepo:          statusSchedulableRepo,
		authCacheInvalidator: statusSchedulableCache,
	}
	schedulable := false
	_, err = statusSchedulableService.BulkUpdateAccounts(context.Background(), &BulkUpdateAccountsInput{
		AccountIDs:  []int64{21, 22},
		Status:      StatusDisabled,
		Schedulable: &schedulable,
	})
	require.NoError(t, err)
	require.Equal(t, []int64{7, 8, 11}, statusSchedulableCache.groupIDs)
}

func TestAdminService_DeleteAccountInvalidatesParentAndShadowGroupsOnce(t *testing.T) {
	parent := &Account{ID: 31, GroupIDs: []int64{7, 8}}
	shadow := &Account{ID: 32, GroupIDs: []int64{8, 9}}
	repo := &modelAllowlistCacheAccountRepoStub{
		accounts:       map[int64]*Account{31: parent, 32: shadow},
		shadowAccounts: []*Account{shadow},
	}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{accountRepo: repo, authCacheInvalidator: cache}

	err := svc.DeleteAccount(context.Background(), parent.ID)

	require.NoError(t, err)
	require.Equal(t, []int64{32, 31}, repo.deletedAccountID)
	require.Equal(t, []int64{7, 8, 9}, cache.groupIDs)
}

func TestAdminService_SetAccountSchedulableInvalidatesBoundGroups(t *testing.T) {
	account := &Account{ID: 41, GroupIDs: []int64{7, 8}}
	repo := &modelAllowlistCacheAccountRepoStub{accounts: map[int64]*Account{41: account}}
	cache := &authCacheInvalidatorStub{}
	svc := &adminServiceImpl{accountRepo: repo, authCacheInvalidator: cache}

	updated, err := svc.SetAccountSchedulable(context.Background(), account.ID, false)

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, []int64{7, 8}, cache.groupIDs)
}
