package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSanitizeAdminPaymentOrderForResponseAddsCurrency(t *testing.T) {
	now := time.Now()
	order := &dbent.PaymentOrder{
		ID:          1,
		UserID:      2,
		Amount:      100,
		PayAmount:   108,
		FeeRate:     8,
		OutTradeNo:  "sub2_202606250001",
		PaymentType: "stripe",
		OrderType:   "subscription",
		Status:      "COMPLETED",
		ExpiresAt:   now,
		CreatedAt:   now,
		UpdatedAt:   now,
		ProviderSnapshot: map[string]any{
			"schema_version": 2,
			"currency":       "USD",
		},
	}

	got := sanitizeAdminPaymentOrderForResponse(order)
	if got == nil {
		t.Fatal("expected sanitized order")
	}
	if got.Currency != "USD" {
		t.Fatalf("expected currency USD, got %q", got.Currency)
	}

	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal sanitized order: %v", err)
	}
	if strings.Contains(string(body), "provider_snapshot") {
		t.Fatalf("expected provider_snapshot to be omitted, got %s", string(body))
	}
}

func TestAdminSubscriptionPlansForResponseIncludesCompositeGroupInfo(t *testing.T) {
	weekly := 25.0
	now := time.Now()
	plans := []*dbent.SubscriptionPlan{
		{
			ID:           11,
			GroupID:      7,
			Name:         "All models",
			Description:  "Composite access",
			Price:        19.99,
			Currency:     "CNY",
			ValidityDays: 30,
			ValidityUnit: "days",
			Features:     "OpenAI\nClaude\nGemini\nGrok",
			ProductName:  "Sub2API",
			ForSale:      true,
			SortOrder:    1,
			CreatedAt:    now,
			UpdatedAt:    now,
		},
	}
	groupInfo := map[int64]service.PlanGroupInfo{
		7: {
			Platform:       service.PlatformComposite,
			Name:           "Bucket 2 composite",
			RateMultiplier: 1.5,
			WeeklyLimitUSD: &weekly,
			ModelScopes:    []string{"openai", "claude", "gemini", "grok"},
		},
	}

	got := adminSubscriptionPlansForResponse(plans, groupInfo)

	if len(got) != 1 {
		t.Fatalf("expected one plan, got %d", len(got))
	}
	if got[0].GroupPlatform != service.PlatformComposite {
		t.Fatalf("expected composite group platform, got %q", got[0].GroupPlatform)
	}
	if got[0].GroupName != "Bucket 2 composite" {
		t.Fatalf("expected group name to be included, got %q", got[0].GroupName)
	}
	if got[0].WeeklyLimitUSD == nil || *got[0].WeeklyLimitUSD != weekly {
		t.Fatalf("expected weekly limit to be included, got %#v", got[0].WeeklyLimitUSD)
	}
	if strings.Join(got[0].ModelScopes, ",") != "openai,claude,gemini,grok" {
		t.Fatalf("expected model scopes to be preserved, got %#v", got[0].ModelScopes)
	}
	// 投影必须保留 ent 原始响应的全部套餐字段：currency 丢失曾导致编辑保存时
	// 静默清空套餐货币（PlanEditDialog 回传空串 → SetCurrency("")）。
	if got[0].Currency != "CNY" {
		t.Fatalf("expected currency to be preserved, got %q", got[0].Currency)
	}
	if !got[0].CreatedAt.Equal(now) || !got[0].UpdatedAt.Equal(now) {
		t.Fatalf("expected created_at/updated_at to be preserved, got %v / %v", got[0].CreatedAt, got[0].UpdatedAt)
	}
}

func TestPaymentQuickAmountsAdminConfigSharesSettingsKey(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{values: map[string]string{service.SettingPaymentQuickAmounts: "[10,20]"}}
	configService := service.NewPaymentConfigService(nil, repo, nil)
	h := NewPaymentHandler(nil, configService)

	get := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(get)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/payment/config", nil)
	h.GetConfig(getCtx)
	require.Equal(t, http.StatusOK, get.Code)
	require.Equal(t, []any{float64(10), float64(20)}, paymentQuickAmountResponseData(t, get)["quick_amounts"])

	putBody, err := json.Marshal(map[string]any{"quick_amounts": []float64{50, 25}})
	require.NoError(t, err)
	put := httptest.NewRecorder()
	putCtx, _ := gin.CreateTestContext(put)
	putCtx.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/payment/config", bytes.NewReader(putBody))
	putCtx.Request.Header.Set("Content-Type", "application/json")
	h.UpdateConfig(putCtx)
	require.Equal(t, http.StatusOK, put.Code)
	require.Equal(t, "[25,50]", repo.values[service.SettingPaymentQuickAmounts])

	settingsHandler := NewSettingHandler(
		service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}}),
		nil, nil, nil, configService, nil, nil,
	)
	settingsGet := httptest.NewRecorder()
	settingsCtx, _ := gin.CreateTestContext(settingsGet)
	settingsCtx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	settingsHandler.GetSettings(settingsCtx)
	require.Equal(t, http.StatusOK, settingsGet.Code)
	require.Equal(t, []any{float64(25), float64(50)}, paymentQuickAmountResponseData(t, settingsGet)["payment_quick_amounts"])
}

func TestPaymentQuickAmountsAdminConfigInvalidDoesNotWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{}
	h := NewPaymentHandler(nil, service.NewPaymentConfigService(nil, repo, nil))
	body, err := json.Marshal(map[string]any{"enabled": true, "quick_amounts": []float64{0}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/payment/config", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.UpdateConfig(ctx)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "INVALID_PAYMENT_QUICK_AMOUNTS")
	require.Zero(t, repo.setCalls)
}

func paymentQuickAmountResponseData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	return envelope.Data
}
