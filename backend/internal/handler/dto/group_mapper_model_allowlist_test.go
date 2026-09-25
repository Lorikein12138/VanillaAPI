package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestGroupMapperIncludesAutomaticSummaryOnlyForAdmin(t *testing.T) {
	group := &service.Group{
		ID:     14,
		Name:   "automatic-group",
		Status: service.StatusActive,
		ModelAllowlist: service.GroupModelAllowlist{
			Enabled: true,
			Models:  []string{"alpha", "beta"},
		},
		ModelAllowlistSourceAccountCount: 3,
	}

	admin := marshalToMap(t, GroupFromServiceAdmin(group))
	adminAllowlist, ok := admin["model_allowlist"].(map[string]any)
	if !ok {
		t.Fatalf("admin model_allowlist has unexpected shape: %#v", admin["model_allowlist"])
	}
	if adminAllowlist["source_account_count"] != float64(3) {
		t.Fatalf("source_account_count = %#v, want 3", adminAllowlist["source_account_count"])
	}
	if _, ok := adminAllowlist["models"]; !ok {
		t.Fatal("admin model_allowlist must include models")
	}

	user := marshalToMap(t, GroupFromService(group))
	if _, ok := user["model_allowlist"]; ok {
		t.Fatal("ordinary group DTO must not expose model_allowlist")
	}
}
