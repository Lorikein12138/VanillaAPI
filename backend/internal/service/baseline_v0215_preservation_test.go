//go:build unit

package service

import (
	"context"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestBaselineV0215AccountMappingSnapshot(t *testing.T) {
	account := Account{
		Platform: PlatformCommandCode,
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-sonnet-4-6": "provider/claude-sonnet-4-6",
				"gpt-5.2":           "provider/gpt-5.2",
			},
		},
	}
	// Account values are copied by repository/service adapters. Copy before the
	// atomic.Value cache is initialized, then populate both independent values
	// concurrently to ensure the cache does not make Account non-copyable.
	accountCopy := account
	accounts := []*Account{&account, &accountCopy}

	const readersPerAccount = 12
	var wg sync.WaitGroup
	type mappingResult struct {
		claude string
		gpt    string
	}
	results := make(chan mappingResult, len(accounts)*readersPerAccount)
	for _, current := range accounts {
		for range readersPerAccount {
			wg.Add(1)
			go func(current *Account) {
				defer wg.Done()
				mapping := current.GetModelMapping()
				results <- mappingResult{
					claude: mapping["claude-sonnet-4-6"],
					gpt:    mapping["gpt-5.2"],
				}
			}(current)
		}
	}
	wg.Wait()
	close(results)
	for result := range results {
		require.Equal(t, "provider/claude-sonnet-4-6", result.claude)
		require.Equal(t, "provider/gpt-5.2", result.gpt)
	}

	// Each published mapping is a stable snapshot even though the raw source is
	// later revised: the next lookup invalidates by signature and publishes a
	// new mapping without mutating the prior result.
	first := account.GetModelMapping()
	account.Credentials["model_mapping"].(map[string]any)["gpt-5.2"] = "provider/gpt-5.3"
	second := account.GetModelMapping()
	require.Equal(t, "provider/gpt-5.2", first["gpt-5.2"])
	require.Equal(t, "provider/gpt-5.3", second["gpt-5.2"])
	require.Equal(t, "provider/gpt-5.3", accountCopy.GetModelMapping()["gpt-5.2"])
}

func TestBaselineV0215GroupDeclarationInvalidation(t *testing.T) {
	// Command Code is registered in the upstream concrete-platform catalog. A
	// group platform change to it must invalidate the cached groupID→platform map.
	require.True(t, domain.IsConcretePlatform(PlatformCommandCode))
	repo := &groupPlatformRepoStub{group: &Group{ID: 215, Name: "platform-catalog", Platform: PlatformAnthropic}}
	invalidator := &channelCacheInvalidatorSpy{}
	svc := &adminServiceImpl{groupRepo: repo, channelCacheInvalidator: invalidator}

	updated, err := svc.UpdateGroup(context.Background(), 215, &UpdateGroupInput{Platform: PlatformCommandCode})
	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, PlatformCommandCode, updated.Platform)
	require.Equal(t, 1, invalidator.calls)
}
