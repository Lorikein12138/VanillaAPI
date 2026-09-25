//go:build unit

package repository

import (
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupEntityToServiceWithAccountsResolvesAutomaticAllowlist(t *testing.T) {
	group := &dbent.Group{
		ID:             12,
		ModelAllowlist: service.DomainGroupModelAllowlist(service.GroupModelAllowlist{Enabled: true, Models: []string{"legacy"}}),
	}
	accounts := []service.Account{
		{Status: service.StatusDisabled, Schedulable: false, Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.4": "gpt-5.4", "alias": "upstream"},
		}},
		{Status: service.StatusError, Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-5.5-*": "gpt-5.5-*", "gpt-5.4": "gpt-5.4"},
		}},
	}

	got := groupEntityToServiceWithAccounts(group, accounts)
	require.True(t, got.ModelAllowlist.Enabled)
	require.Equal(t, []string{"gpt-5.4", "gpt-5.5-*"}, got.ModelAllowlist.Models)
	require.Equal(t, []string{"legacy"}, group.ModelAllowlist.Models, "projection must not mutate the Ent group")
}

func TestGroupEntityToServiceWithAccountsPreservesDisabledManualAllowlist(t *testing.T) {
	group := &dbent.Group{
		ID:             13,
		ModelAllowlist: service.DomainGroupModelAllowlist(service.GroupModelAllowlist{Enabled: false, Models: []string{"manual"}}),
	}
	got := groupEntityToServiceWithAccounts(group, []service.Account{{Credentials: map[string]any{
		"model_mapping": map[string]any{"account-model": "account-model"},
	}}})
	require.False(t, got.ModelAllowlist.Enabled)
	require.Equal(t, []string{"manual"}, got.ModelAllowlist.Models)
}
