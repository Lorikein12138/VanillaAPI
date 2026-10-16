//go:build unit

package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newPaymentQuickAmountsSettingHandler(t *testing.T, stored map[string]string, withPaymentService bool) (*SettingHandler, *settingHandlerRepoStub) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	repo := &settingHandlerRepoStub{values: stored}
	settingsService := service.NewSettingService(repo, &config.Config{Default: config.DefaultConfig{UserConcurrency: 5}})
	var paymentConfigService *service.PaymentConfigService
	if withPaymentService {
		paymentConfigService = service.NewPaymentConfigService(nil, repo, nil)
	}
	return NewSettingHandler(settingsService, nil, nil, nil, paymentConfigService, nil, nil), repo
}

func invokeSettingsGet(t *testing.T, h *SettingHandler) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	h.GetSettings(ctx)
	return recorder
}

func decodeResponseData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	return envelope.Data
}

func TestSettingHandlerPaymentQuickAmountsGetMappingAndFallback(t *testing.T) {
	t.Run("configured list", func(t *testing.T) {
		h, _ := newPaymentQuickAmountsSettingHandler(t, map[string]string{
			service.SettingPaymentQuickAmounts: "[100,25.5]",
		}, true)
		recorder := invokeSettingsGet(t, h)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, []any{25.5, float64(100)}, decodeResponseData(t, recorder)["payment_quick_amounts"])
	})

	t.Run("nil service returns default copy", func(t *testing.T) {
		h, _ := newPaymentQuickAmountsSettingHandler(t, nil, false)
		recorder := invokeSettingsGet(t, h)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, []any{float64(10), float64(20), float64(50), float64(100), float64(200), float64(500), float64(1000), float64(2000), float64(5000)}, decodeResponseData(t, recorder)["payment_quick_amounts"])
	})

	t.Run("empty list is preserved", func(t *testing.T) {
		h, _ := newPaymentQuickAmountsSettingHandler(t, map[string]string{
			service.SettingPaymentQuickAmounts: "[]",
		}, true)
		recorder := invokeSettingsGet(t, h)
		require.Equal(t, http.StatusOK, recorder.Code)
		amounts, ok := decodeResponseData(t, recorder)["payment_quick_amounts"].([]any)
		require.True(t, ok)
		require.Empty(t, amounts)
	})
}

func TestSettingHandlerPaymentQuickAmountsReadErrorIsPropagated(t *testing.T) {
	h, repo := newPaymentQuickAmountsSettingHandler(t, nil, true)
	repo.quickAmountsReadErr = errors.New("payment settings unavailable")
	recorder := invokeSettingsGet(t, h)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.NotContains(t, decodeResponseData(t, recorder), "payment_quick_amounts")
}

func TestSettingHandlerUpdateQuickAmountsReadErrorIsPropagated(t *testing.T) {
	h, repo := newPaymentQuickAmountsSettingHandler(t, nil, true)
	repo.quickAmountsReadErr = errors.New("payment settings unavailable after update")
	recorder := doUpdateSettings(t, h, map[string]any{
		"payment_quick_amounts": []float64{20},
	}, nil)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.Equal(t, "[20]", repo.values[service.SettingPaymentQuickAmounts])
}

func TestSettingHandlerQuickAmountsUpdateValidationAndPatchSemantics(t *testing.T) {
	h, repo := newPaymentQuickAmountsSettingHandler(t, map[string]string{
		service.SettingPaymentQuickAmounts: "[5,10]",
	}, true)

	recorder := doUpdateSettings(t, h, map[string]any{
		"site_name":             "Quick Amounts",
		"payment_quick_amounts": []float64{100, 0.29, 25.5},
	}, nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "[0.29,25.5,100]", repo.values[service.SettingPaymentQuickAmounts])
	require.Equal(t, []any{0.29, 25.5, float64(100)}, decodeResponseData(t, recorder)["payment_quick_amounts"])

	for _, body := range []map[string]any{
		{"payment_quick_amounts": nil},
		{},
	} {
		recorder = doUpdateSettings(t, h, body, nil)
		require.Equal(t, http.StatusOK, recorder.Code)
		require.Equal(t, "[0.29,25.5,100]", repo.values[service.SettingPaymentQuickAmounts])
	}

	recorder = doUpdateSettings(t, h, map[string]any{"payment_quick_amounts": []float64{}}, nil)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "[]", repo.values[service.SettingPaymentQuickAmounts])
	require.Equal(t, []any{}, decodeResponseData(t, recorder)["payment_quick_amounts"])
}

func TestSettingHandlerInvalidQuickAmountsCauseNoSettingsWrites(t *testing.T) {
	h, repo := newPaymentQuickAmountsSettingHandler(t, map[string]string{"site_name": "Existing"}, true)
	recorder := doUpdateSettings(t, h, map[string]any{
		"site_name":             "Must Not Persist",
		"payment_quick_amounts": []float64{1.001},
	}, nil)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Body.String(), "INVALID_PAYMENT_QUICK_AMOUNTS")
	require.Zero(t, repo.setCalls)
	require.Equal(t, "Existing", repo.values["site_name"])
}

func TestSettingHandlerQuickAmountsRequirePaymentServiceBeforeSettingsWrites(t *testing.T) {
	h, repo := newPaymentQuickAmountsSettingHandler(t, map[string]string{"site_name": "Existing"}, false)
	recorder := doUpdateSettings(t, h, map[string]any{
		"site_name":             "Must Not Persist",
		"payment_quick_amounts": []float64{},
	}, nil)
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	require.Zero(t, repo.setCalls)
	require.Equal(t, "Existing", repo.values["site_name"])
}

func TestSettingHandlerHasPaymentFieldsIncludesExplicitEmptyQuickAmounts(t *testing.T) {
	empty := []float64{}
	require.True(t, hasPaymentFields(UpdateSettingsRequest{PaymentQuickAmounts: &empty}))
	require.False(t, hasPaymentFields(UpdateSettingsRequest{}))
}
