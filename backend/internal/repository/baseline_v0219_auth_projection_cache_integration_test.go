//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type baselineV0219CountingAPIKeyRepository struct {
	service.APIKeyRepository
	authReads atomic.Int64
}

func (r *baselineV0219CountingAPIKeyRepository) GetByKeyForAuth(ctx context.Context, key string) (*service.APIKey, error) {
	r.authReads.Add(1)
	return r.APIKeyRepository.GetByKeyForAuth(ctx, key)
}

type baselineV0219CountingAPIKeyCache struct {
	service.APIKeyCache
	authReads atomic.Int64
}

func (c *baselineV0219CountingAPIKeyCache) GetAuthCache(ctx context.Context, key string) (*service.APIKeyAuthCacheEntry, error) {
	c.authReads.Add(1)
	return c.APIKeyCache.GetAuthCache(ctx, key)
}

func baselineV0219AuthCacheKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func baselineV0219RequireAuthProjection(t *testing.T, got *service.APIKey, allowImage, imageCount bool, allowlist ...string) {
	t.Helper()
	require.NotNil(t, got)
	require.NotNil(t, got.Group)
	require.Equal(t, allowImage, got.Group.AllowImageGeneration)
	require.Equal(t, imageCount, got.Group.ImageCountBilling)
	require.True(t, got.Group.ModelAllowlist.Enabled)
	for _, model := range allowlist {
		require.Contains(t, got.Group.ModelAllowlist.Models, model)
	}
}

func baselineV0219EventuallyAuthProjection(t *testing.T, svc *service.APIKeyService, key string, allowImage, imageCount bool, allowlist ...string) *service.APIKey {
	t.Helper()
	var got *service.APIKey
	require.Eventually(t, func() bool {
		var err error
		got, err = svc.GetByKey(context.Background(), key)
		if err != nil || got == nil || got.Group == nil {
			return false
		}
		if got.Group.AllowImageGeneration != allowImage || got.Group.ImageCountBilling != imageCount || !got.Group.ModelAllowlist.Enabled {
			return false
		}
		for _, model := range allowlist {
			if !containsString(got.Group.ModelAllowlist.Models, model) {
				return false
			}
		}
		return true
	}, 8*time.Second, 10*time.Millisecond, "API key auth projection did not converge")
	baselineV0219RequireAuthProjection(t, got, allowImage, imageCount, allowlist...)
	return got
}

func TestBaselineV0219AuthProjectionCacheColdHotAndInvalidation(t *testing.T) {
	ctx := context.Background()
	suffix := time.Now().UnixNano()
	fixture := newChannelMonitorV2V0215TestDatabase(t)
	require.NoError(t, ApplyMigrations(ctx, fixture.db))
	client := dbent.NewClient(dbent.Driver(entsql.OpenDB(dialect.Postgres, fixture.db)))
	t.Cleanup(func() { require.NoError(t, client.Close()) })

	rdb := testRedis(t)
	rawAPIKeyCache := NewAPIKeyCache(rdb)
	countingCache := &baselineV0219CountingAPIKeyCache{APIKeyCache: rawAPIKeyCache}
	groupRepo := NewAdminGroupRepository(client, fixture.db)
	accountRepo := NewAdminAccountRepository(client, fixture.db, nil)
	baseAPIKeyRepo := NewAPIKeyRepository(client, fixture.db)
	countingAPIKeyRepo := &baselineV0219CountingAPIKeyRepository{APIKeyRepository: baseAPIKeyRepo}

	group := &service.Group{
		Name: fmt.Sprintf("baseline-v0219-auth-cache-%d", suffix), Platform: service.PlatformOpenAI,
		Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard, RateMultiplier: 1,
		AllowImageGeneration: true, ImageCountBilling: true,
		ModelAllowlist: service.GroupModelAllowlist{Enabled: true, Models: []string{"manual-model"}},
	}
	require.NoError(t, groupRepo.Create(ctx, group))
	account := mustCreateAccount(t, client, &service.Account{
		Name:     fmt.Sprintf("baseline-v0219-auth-cache-account-%d", suffix),
		Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive,
		Credentials: map[string]any{"model_mapping": map[string]any{"old-model": "old-model"}},
	})
	require.NoError(t, accountRepo.BindGroups(ctx, account.ID, []int64{group.ID}))
	user := mustCreateUser(t, client, &service.User{
		Email: fmt.Sprintf("baseline-v0219-auth-cache-%d@example.test", suffix), Concurrency: 5,
		AllowedGroups: []int64{group.ID},
	})
	keyValue := fmt.Sprintf("sk-baseline-v0219-auth-cache-%d", suffix)
	groupID := group.ID
	apiKey := &service.APIKey{UserID: user.ID, GroupID: &groupID, Key: keyValue, Name: "baseline-v0219-auth-cache", Status: service.StatusAPIKeyActive}
	require.NoError(t, baseAPIKeyRepo.Create(ctx, apiKey))
	t.Cleanup(func() {
		for _, value := range []string{keyValue, fmt.Sprintf("sk-baseline-v0219-auth-cache-copy-%d", suffix)} {
			_ = rawAPIKeyCache.DeleteAuthCache(context.Background(), baselineV0219AuthCacheKey(value))
		}
	})

	cfg := &config.Config{}
	cfg.APIKeyAuth.L1Size = 256
	cfg.APIKeyAuth.L1TTLSeconds = 60
	cfg.APIKeyAuth.L2TTLSeconds = 300
	svc1 := service.NewAPIKeyService(countingAPIKeyRepo, nil, groupRepo, nil, nil, countingCache, cfg)
	svc2 := service.NewAPIKeyService(countingAPIKeyRepo, nil, groupRepo, nil, nil, countingCache, cfg)
	for _, svc := range []*service.APIKeyService{svc1, svc2} {
		subCtx, cancel := context.WithCancel(ctx)
		svc.StartAuthCacheInvalidationSubscriber(subCtx)
		t.Cleanup(func() {
			cancel()
			svc.StopAuthCacheInvalidationSubscriber()
		})
		require.Eventually(t, func() bool { return svc.AuthCacheInvalidationSubscriberHealth().Connected }, 5*time.Second, 10*time.Millisecond)
	}

	got, err := svc1.GetByKey(ctx, keyValue)
	require.NoError(t, err)
	baselineV0219RequireAuthProjection(t, got, true, true, "old-model")
	require.EqualValues(t, 1, countingAPIKeyRepo.authReads.Load(), "cold lookup must query PostgreSQL once")
	require.EqualValues(t, 1, countingCache.authReads.Load(), "cold lookup must read Redis once")
	baselineV0219EventuallyAuthProjection(t, svc1, keyValue, true, true, "old-model")
	repoReadsBeforeHot := countingAPIKeyRepo.authReads.Load()
	redisReadsBeforeHot := countingCache.authReads.Load()
	got, err = svc1.GetByKey(ctx, keyValue)
	require.NoError(t, err)
	baselineV0219RequireAuthProjection(t, got, true, true, "old-model")
	require.Equal(t, repoReadsBeforeHot, countingAPIKeyRepo.authReads.Load(), "hot L1 lookup must not query PostgreSQL")
	require.Equal(t, redisReadsBeforeHot, countingCache.authReads.Load(), "hot L1 lookup must not read Redis")

	repoReadsBeforeL2 := countingAPIKeyRepo.authReads.Load()
	redisReadsBeforeL2 := countingCache.authReads.Load()
	got, err = svc2.GetByKey(ctx, keyValue)
	require.NoError(t, err)
	baselineV0219RequireAuthProjection(t, got, true, true, "old-model")
	require.Equal(t, repoReadsBeforeL2, countingAPIKeyRepo.authReads.Load(), "hot L2 lookup must not query PostgreSQL")
	require.Equal(t, redisReadsBeforeL2+1, countingCache.authReads.Load(), "new service instance must read the shared Redis entry")
	baselineV0219EventuallyAuthProjection(t, svc2, keyValue, true, true, "old-model")

	admin := service.NewAdminService(cfg, nil, groupRepo, accountRepo, nil, countingAPIKeyRepo, nil, nil, nil, nil, nil, nil, svc1, client, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	imageCountOff := false
	_, err = admin.UpdateGroup(ctx, group.ID, &service.UpdateGroupInput{ImageCountBilling: &imageCountOff})
	require.NoError(t, err)
	repoReadsBeforeGroupUpdate := countingAPIKeyRepo.authReads.Load()
	redisReadsBeforeGroupUpdate := countingCache.authReads.Load()
	baselineV0219EventuallyAuthProjection(t, svc1, keyValue, true, false, "old-model")
	baselineV0219EventuallyAuthProjection(t, svc2, keyValue, true, false, "old-model")
	require.GreaterOrEqual(t, countingAPIKeyRepo.authReads.Load(), repoReadsBeforeGroupUpdate+1, "group invalidation must force a PostgreSQL reload; the other instance may reuse the refreshed L2 entry")
	require.GreaterOrEqual(t, countingCache.authReads.Load(), redisReadsBeforeGroupUpdate+2, "both instances must observe the invalidated L2 entry")

	_, err = admin.UpdateAccount(ctx, account.ID, &service.UpdateAccountInput{Credentials: map[string]any{
		"model_mapping": map[string]any{"new-model": "new-model"},
	}})
	require.NoError(t, err)
	repoReadsBeforeAccountUpdate := countingAPIKeyRepo.authReads.Load()
	redisReadsBeforeAccountUpdate := countingCache.authReads.Load()
	baselineV0219EventuallyAuthProjection(t, svc1, keyValue, true, false, "new-model")
	baselineV0219EventuallyAuthProjection(t, svc2, keyValue, true, false, "new-model")
	require.GreaterOrEqual(t, countingAPIKeyRepo.authReads.Load(), repoReadsBeforeAccountUpdate+1, "account model mapping update must force a PostgreSQL reload; the other instance may reuse the refreshed L2 entry")
	require.GreaterOrEqual(t, countingCache.authReads.Load(), redisReadsBeforeAccountUpdate+2, "both instances must read the refreshed L2 entry")

	duplicate, err := admin.DuplicateGroup(ctx, group.ID, "baseline-v0219", fmt.Sprintf("copy-%d", suffix))
	require.NoError(t, err)
	require.False(t, duplicate.ImageCountBilling)
	require.True(t, duplicate.AllowImageGeneration)
	copyKeyValue := fmt.Sprintf("sk-baseline-v0219-auth-cache-copy-%d", suffix)
	copyGroupID := duplicate.ID
	copyKey := &service.APIKey{UserID: user.ID, GroupID: &copyGroupID, Key: copyKeyValue, Name: "baseline-v0219-auth-cache-copy", Status: service.StatusAPIKeyActive}
	require.NoError(t, baseAPIKeyRepo.Create(ctx, copyKey))
	got, err = svc1.GetByKey(ctx, copyKeyValue)
	require.NoError(t, err)
	baselineV0219RequireAuthProjection(t, got, true, false, "new-model")

	cacheKey := baselineV0219AuthCacheKey(keyValue)
	oldEntry, err := rawAPIKeyCache.GetAuthCache(ctx, cacheKey)
	require.NoError(t, err)
	require.NotNil(t, oldEntry.Snapshot)
	oldSnapshot := *oldEntry.Snapshot
	oldSnapshot.Version = 25
	oldEntry.Snapshot = &oldSnapshot
	require.NoError(t, rawAPIKeyCache.SetAuthCache(ctx, cacheKey, oldEntry, 5*time.Minute))
	svcV25 := service.NewAPIKeyService(countingAPIKeyRepo, nil, groupRepo, nil, nil, countingCache, cfg)
	repoReadsBeforeV25 := countingAPIKeyRepo.authReads.Load()
	redisReadsBeforeV25 := countingCache.authReads.Load()
	got, err = svcV25.GetByKey(ctx, keyValue)
	require.NoError(t, err)
	baselineV0219RequireAuthProjection(t, got, true, false, "new-model")
	require.Equal(t, repoReadsBeforeV25+1, countingAPIKeyRepo.authReads.Load(), "v25 Redis payload must be rejected and reloaded from PostgreSQL")
	require.Equal(t, redisReadsBeforeV25+1, countingCache.authReads.Load(), "v25 Redis payload must be inspected once")
	refreshedEntry, err := rawAPIKeyCache.GetAuthCache(ctx, cacheKey)
	require.NoError(t, err)
	require.Equal(t, 26, refreshedEntry.Snapshot.Version)
	require.True(t, refreshedEntry.Snapshot.Group.AllowImageGeneration)
	require.False(t, refreshedEntry.Snapshot.Group.ImageCountBilling)
	require.Contains(t, refreshedEntry.Snapshot.Group.ModelAllowlist.Models, "new-model")
}
