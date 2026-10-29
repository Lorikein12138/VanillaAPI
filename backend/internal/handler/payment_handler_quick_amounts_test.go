//go:build unit

package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

type paymentQuickAmountsSettingRepo struct {
	values map[string]string
	err    error
}

func (r *paymentQuickAmountsSettingRepo) Get(context.Context, string) (*service.Setting, error) {
	return nil, nil
}
func (r *paymentQuickAmountsSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	return r.values[key], nil
}
func (r *paymentQuickAmountsSettingRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}
func (r *paymentQuickAmountsSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	if r.err != nil {
		return nil, r.err
	}
	out := make(map[string]string, len(keys))
	for _, key := range keys {
		out[key] = r.values[key]
	}
	return out, nil
}
func (r *paymentQuickAmountsSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}
func (r *paymentQuickAmountsSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return r.values, nil
}
func (r *paymentQuickAmountsSettingRepo) Delete(context.Context, string) error { return nil }

func newQuickAmountsPaymentHandlerFixture(t *testing.T, values map[string]string) (*PaymentHandler, *dbent.Client, *paymentQuickAmountsSettingRepo) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dbName := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	db, err := sql.Open("sqlite", dbName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	drv := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(drv)))
	t.Cleanup(func() { _ = client.Close() })
	repo := &paymentQuickAmountsSettingRepo{values: values}
	configService := service.NewPaymentConfigService(client, repo, nil)
	return NewPaymentHandler(nil, configService), client, repo
}

func callPaymentHandler(t *testing.T, method, path string, handler func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, nil)
	handler(ctx)
	return recorder
}

func paymentResponseData(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	return envelope.Data
}

func TestPaymentQuickAmountsGetPaymentConfigReturnsCompleteConfiguredList(t *testing.T) {
	h, _, _ := newQuickAmountsPaymentHandlerFixture(t, map[string]string{
		service.SettingPaymentQuickAmounts: "[100,25.5,5]",
	})
	recorder := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/config", h.GetPaymentConfig)
	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []any{float64(5), 25.5, float64(100)}, paymentResponseData(t, recorder)["quick_amounts"])
}

func TestPaymentQuickAmountsCheckoutUsesDefaultsAndPreservesOtherFields(t *testing.T) {
	h, _, _ := newQuickAmountsPaymentHandlerFixture(t, map[string]string{
		service.SettingRechargeFeeRate: "3.5",
	})
	recorder := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/checkout-info", h.GetCheckoutInfo)
	require.Equal(t, http.StatusOK, recorder.Code)
	data := paymentResponseData(t, recorder)
	require.Equal(t, []any{float64(10), float64(20), float64(50), float64(100), float64(200), float64(500), float64(1000), float64(2000), float64(5000)}, data["quick_amounts"])
	require.Equal(t, float64(3.5), data["recharge_fee_rate"])
	require.NotNil(t, data["methods"])
	require.NotNil(t, data["plans"])
}

func TestPaymentQuickAmountsCheckoutFiltersGlobalRangeWithoutChangingConfig(t *testing.T) {
	h, client, repo := newQuickAmountsPaymentHandlerFixture(t, map[string]string{
		service.SettingPaymentQuickAmounts: "[5,25.5,100]",
		service.SettingMinRechargeAmount:   "10",
		service.SettingMaxRechargeAmount:   "50",
		service.SettingRechargeFeeRate:     "4.25",
	})
	_, err := client.PaymentProviderInstance.Create().
		SetProviderKey(string(payment.TypeAlipay)).
		SetName("Test Alipay").
		SetConfig("{}").
		SetSupportedTypes(string(payment.TypeAlipay)).
		SetEnabled(true).
		SetLimits(`{"alipay":{"singleMin":1,"singleMax":100}}`).
		Save(context.Background())
	require.NoError(t, err)

	checkout := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/checkout-info", h.GetCheckoutInfo)
	require.Equal(t, http.StatusOK, checkout.Code)
	data := paymentResponseData(t, checkout)
	require.Equal(t, float64(1), data["global_min"])
	require.Equal(t, float64(100), data["global_max"])
	require.Equal(t, []any{25.5}, data["quick_amounts"])
	require.Equal(t, float64(4.25), data["recharge_fee_rate"])
	require.Equal(t, "[5,25.5,100]", repo.values[service.SettingPaymentQuickAmounts])

	config := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/config", h.GetPaymentConfig)
	require.Equal(t, http.StatusOK, config.Code)
	require.Equal(t, []any{float64(5), 25.5, float64(100)}, paymentResponseData(t, config)["quick_amounts"])
}

func TestPaymentQuickAmountsCheckoutKeepsProviderRangeSeparateFromGlobalRange(t *testing.T) {
	h, client, _ := newQuickAmountsPaymentHandlerFixture(t, map[string]string{
		service.SettingPaymentQuickAmounts: "[5,25.5,100]",
		service.SettingMinRechargeAmount:   "1",
		service.SettingMaxRechargeAmount:   "100",
	})
	_, err := client.PaymentProviderInstance.Create().
		SetProviderKey(string(payment.TypeAlipay)).
		SetName("Test Alipay").
		SetConfig("{}").
		SetSupportedTypes(string(payment.TypeAlipay)).
		SetEnabled(true).
		SetLimits(`{"alipay":{"singleMin":10,"singleMax":50}}`).
		Save(context.Background())
	require.NoError(t, err)

	checkout := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/checkout-info", h.GetCheckoutInfo)
	require.Equal(t, http.StatusOK, checkout.Code)
	data := paymentResponseData(t, checkout)
	require.Equal(t, float64(10), data["global_min"])
	require.Equal(t, float64(50), data["global_max"])
	require.Equal(t, []any{float64(5), 25.5, float64(100)}, data["quick_amounts"])
}

func TestPaymentQuickAmountsCheckoutIncludesGlobalBoundariesAndSupportsOneSidedLimits(t *testing.T) {
	tests := []struct {
		name   string
		min    string
		max    string
		stored string
		want   []any
	}{
		{name: "inclusive boundaries", min: "10", max: "50", stored: "[5,10,25.5,50,100]", want: []any{float64(10), 25.5, float64(50)}},
		{name: "minimum only", min: "10", max: "0", stored: "[5,10,100]", want: []any{float64(10), float64(100)}},
		{name: "maximum only", min: "0", max: "50", stored: "[5,25.5,50,100]", want: []any{float64(5), 25.5, float64(50)}},
		{name: "all filtered remains array", min: "10", max: "50", stored: "[5,9]", want: []any{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, client, repo := newQuickAmountsPaymentHandlerFixture(t, map[string]string{
				service.SettingPaymentQuickAmounts: tt.stored,
				service.SettingMinRechargeAmount:   tt.min,
				service.SettingMaxRechargeAmount:   tt.max,
			})
			_, err := client.PaymentProviderInstance.Create().
				SetProviderKey(string(payment.TypeAlipay)).
				SetName("Test Alipay").
				SetConfig("{}").
				SetSupportedTypes(string(payment.TypeAlipay)).
				SetEnabled(true).
				SetLimits(`{"alipay":{"singleMin":1,"singleMax":100}}`).
				Save(context.Background())
			require.NoError(t, err)

			recorder := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/checkout-info", h.GetCheckoutInfo)
			require.Equal(t, http.StatusOK, recorder.Code)
			data := paymentResponseData(t, recorder)
			amounts, ok := data["quick_amounts"].([]any)
			require.True(t, ok, "quick_amounts must remain a non-null array")
			require.Equal(t, tt.want, amounts)
			require.Equal(t, tt.stored, repo.values[service.SettingPaymentQuickAmounts])
		})
	}
}

func TestPaymentQuickAmountsCheckoutKeepsExplicitEmptyList(t *testing.T) {
	h, _, _ := newQuickAmountsPaymentHandlerFixture(t, map[string]string{
		service.SettingPaymentQuickAmounts: "[]",
	})
	recorder := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/checkout-info", h.GetCheckoutInfo)
	require.Equal(t, http.StatusOK, recorder.Code)
	amounts, ok := paymentResponseData(t, recorder)["quick_amounts"].([]any)
	require.True(t, ok)
	require.Empty(t, amounts)
}

func TestPaymentQuickAmountsCheckoutPropagatesStorageReadError(t *testing.T) {
	h, _, repo := newQuickAmountsPaymentHandlerFixture(t, nil)
	repo.err = fmt.Errorf("settings database unavailable")
	recorder := callPaymentHandler(t, http.MethodGet, "/api/v1/payment/checkout-info", h.GetCheckoutInfo)
	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "quick_amounts")
}
