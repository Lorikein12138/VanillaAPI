package service

import "testing"

func TestAPIKeyService_RejectsV10AuthSnapshotWithoutModelAllowlist(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-models-list", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  10,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeStandard,
				RateMultiplier:   1,
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatalf("expected v10 auth snapshot to be rejected after model_allowlist was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_RejectsV15AuthSnapshotWithoutReasoningEffortPolicy(t *testing.T) {
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-reasoning-mappings", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{Version: 15},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatal("expected v15 auth snapshot to be rejected after reasoning effort policy was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_RejectsV24AuthSnapshotWithLegacyManualModelAllowlist(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-v24-manual-allowlist", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  24,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:     groupID,
				Status: StatusActive,
				ModelAllowlist: GroupModelAllowlist{
					Enabled: true,
					Models:  []string{"legacy-manual-model"},
				},
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale v24 snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatal("expected v24 snapshot to be rejected after automatic account unions were introduced")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_RejectsV25AndRoundTripsV26ImageBillingAndAutomaticAllowlist(t *testing.T) {
	groupID := int64(29)
	accounts := []Account{
		{Platform: PlatformGemini, Status: StatusError, Credentials: map[string]any{"model_mapping": map[string]any{"client-gemini": "upstream-gemini"}}},
		{Platform: PlatformGemini, Status: StatusDisabled, Credentials: map[string]any{"model_mapping": map[string]any{"client-disabled": "upstream-disabled"}}},
	}
	allowlist := GroupModelAllowlist{Enabled: true, Models: BuildAccountModelAllowlistUnion(accounts)}
	key := &APIKey{
		ID: 4, UserID: 8, GroupID: &groupID, Status: StatusActive,
		User:  &User{ID: 8, Status: StatusActive, Role: RoleUser},
		Group: &Group{ID: groupID, Status: StatusActive, ImageCountBilling: true, ModelAllowlist: allowlist},
	}

	svc := &APIKeyService{}
	snapshot := svc.snapshotFromAPIKey(t.Context(), key)
	if snapshot == nil || snapshot.Version != 26 {
		t.Fatalf("expected v26 snapshot, got %#v", snapshot)
	}
	if got := svc.snapshotToAPIKey("round-trip", snapshot); got.Group == nil || !got.Group.ImageCountBilling {
		t.Fatalf("image_count_billing did not survive the auth snapshot: %#v", got)
	}
	got := svc.snapshotToAPIKey("round-trip", snapshot)
	if !got.Group.ModelAllowlist.Enabled || len(got.Group.ModelAllowlist.Models) != 2 {
		t.Fatalf("automatic allowlist did not survive the auth snapshot: %#v", got.Group.ModelAllowlist)
	}

	apiKey, ok, err := svc.applyAuthCacheEntry("stale-v25", &APIKeyAuthCacheEntry{Snapshot: &APIKeyAuthSnapshot{Version: 25}})
	if err != nil || ok || apiKey != nil {
		t.Fatalf("expected v25 snapshot rejection, got key=%#v ok=%v err=%v", apiKey, ok, err)
	}
	apiKey, ok, err = svc.applyAuthCacheEntry("current-v26", &APIKeyAuthCacheEntry{Snapshot: snapshot})
	if err != nil || !ok || apiKey == nil || apiKey.Group == nil || !apiKey.Group.ImageCountBilling {
		t.Fatalf("expected v26 snapshot acceptance, got key=%#v ok=%v err=%v", apiKey, ok, err)
	}
}
