//go:build unit

package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestPaymentInfoPersistence(t *testing.T) {
	for _, tc := range []struct {
		name, raw, cashier string
		wantCashier        bool
	}{
		{"default deadline", `{"version":1,"actual_amount":"1.01","qr_content":" /raw-code "}`, "https://gateway.example/pay/original", false},
		{"image", `{"version":1,"actual_amount":"0.99","qr_image_url":"https://gateway.example/qr.png"}`, "https://gateway.example/pay/original", false},
		{"captcha", `{"version":1,"actual_amount":"1.01","qr_content":"captcha-code","requires_cashier":true}`, "https://gateway.example/pay/original", true},
		{"invalid", `{"version":1,"actual_amount":"01.01","qr_content":"bad-code"}`, "https://gateway.example/pay/original", true},
		{"unknown", `{"version":2,"actual_amount":"1.01","qr_content":"bad-code"}`, "https://gateway.example/pay/original", true},
		{"expired", `{"version":1,"actual_amount":"1.01","qr_content":"expired-code","expires_at":"2000-01-01T00:00:00Z"}`, "https://gateway.example/pay/original", true},
		{"invalid expiry", `{"version":1,"actual_amount":"1.01","qr_content":"bad-code","expires_at":"bad-time"}`, "https://gateway.example/pay/original", true},
		{"missing cashier", `{"version":1,"actual_amount":"1.01","qr_content":"bad-code"}`, "", true},
		{"unsafe cashier", `{"version":1,"actual_amount":"1.01","qr_content":"bad-code"}`, "https://u:p@gateway.example/pay", true},
		{"null", `null`, "https://gateway.example/pay/original", true},
		{"numeric actual", `{"version":1,"actual_amount":1.01,"qr_content":"bad-code"}`, "https://gateway.example/pay/original", true},
		{"null actual", `{"version":1,"actual_amount":null,"qr_content":"bad-code"}`, "https://gateway.example/pay/original", true},
		{"oversized actual", `{"version":1,"actual_amount":"1000000000000","qr_content":"bad-code"}`, "https://gateway.example/pay/original", true},
		{"null flag", `{"version":1,"actual_amount":"1.01","qr_content":"bad-code","requires_cashier":null}`, "https://gateway.example/pay/original", true},
		{"null expiry", `{"version":1,"actual_amount":"1.01","qr_content":"bad-code","expires_at":null}`, "https://gateway.example/pay/original", true},
		{"null image", `{"version":1,"actual_amount":"1.01","qr_image_url":null}`, "https://gateway.example/pay/original", true},
		{"content ignores null image", `{"version":1,"actual_amount":"1.01","qr_content":"raw-code","qr_image_url":null}`, "https://gateway.example/pay/original", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			svc, order, sel, calls := newPaymentInfoCreateFixture(t, tc.raw, tc.cashier)
			response, err := svc.invokeProvider(ctx, order, CreateOrderRequest{UserID: order.UserID, Amount: 1, PaymentType: payment.TypeWxpay, IsMobile: true}, &PaymentConfig{}, 1, "1.00", 1, nil, sel)
			require.NoError(t, err)
			stored, err := svc.entClient.PaymentOrder.Get(ctx, order.ID)
			require.NoError(t, err)
			require.Equal(t, int32(1), calls.Load())
			require.Equal(t, OrderStatusPending, stored.Status, "missing display data must not fail the order")
			require.Equal(t, 1.0, stored.PayAmount)
			require.Equal(t, 2.0, stored.Amount)
			require.Equal(t, order.ExpiresAt, stored.ExpiresAt)
			require.Equal(t, "test-merchant", stored.ProviderSnapshot["merchant_id"])
			require.Equal(t, "preserved", stored.ProviderSnapshot["other_snapshot_value"])
			require.Equal(t, sel.InstanceID, stored.ProviderSnapshot["provider_instance_id"])
			require.Contains(t, stored.ProviderSnapshot, "payment_info")
			require.Nil(t, stored.QrCode)
			require.Nil(t, stored.QrCodeImg)
			reloaded, err := svc.GetOrder(ctx, order.ID, order.UserID)
			require.NoError(t, err)
			details := PaymentOrderPaymentPresentation(reloaded)
			require.Equal(t, response.PaymentInfo, details.PaymentInfo)
			require.Equal(t, response.PayURL, details.PayURL)
			require.Equal(t, tc.wantCashier, details.PaymentInfo.RequiresCashier)
			require.Empty(t, response.QRCode)
			if tc.wantCashier {
				require.Empty(t, details.PaymentInfo.QRContent)
				require.Empty(t, details.PaymentInfo.QRImageURL)
			} else {
				require.Equal(t, order.ExpiresAt.Format(time.RFC3339Nano), details.PaymentInfo.ExpiresAt)
			}
			if payment.SafePaymentHTTPURL(tc.cashier) == "" {
				require.Empty(t, details.PayURL)
			}
		})
	}
}

func TestPaymentInfoQRDeadlineDoesNotExpireOrder(t *testing.T) {
	ctx := context.Background()
	qrExpiry := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	raw := `{"version":1,"actual_amount":"1.01","qr_content":"qr-secret","expires_at":"` + qrExpiry.Format(time.RFC3339) + `"}`
	svc, order, sel, _ := newPaymentInfoCreateFixture(t, raw, "https://gateway.example/pay/original")
	_, err := svc.invokeProvider(ctx, order, CreateOrderRequest{UserID: order.UserID, PaymentType: payment.TypeWxpay}, &PaymentConfig{}, 1, "1.00", 1, nil, sel)
	require.NoError(t, err)
	stored, err := svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetQrCode("stale-legacy-secret").SetQrCodeImg("https://gateway.example/stale.png").Save(ctx)
	require.NoError(t, err)
	require.True(t, qrExpiry.Before(stored.ExpiresAt))
	before := paymentOrderPaymentPresentationAt(stored, qrExpiry.Add(-time.Second))
	require.False(t, before.PaymentInfo.RequiresCashier)
	require.Equal(t, "qr-secret", before.PaymentInfo.QRContent)
	after := paymentOrderPaymentPresentationAt(stored, qrExpiry)
	require.True(t, after.PaymentInfo.RequiresCashier)
	require.Empty(t, after.QRCode)
	require.Empty(t, after.QRCodeImg)
	require.Empty(t, after.PaymentInfo.QRContent)
	require.Equal(t, "https://gateway.example/pay/original", after.PayURL)
	reloaded, err := svc.entClient.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, reloaded.Status)
	require.Equal(t, order.ExpiresAt, reloaded.ExpiresAt)
	stillExpired := paymentOrderPaymentPresentationAt(reloaded, qrExpiry.Add(time.Second))
	require.True(t, stillExpired.PaymentInfo.RequiresCashier)
	require.Empty(t, stillExpired.QRCode)
	require.Empty(t, stillExpired.PaymentInfo.QRContent)
}

func TestPaymentInfoLifecyclePresentation(t *testing.T) {
	ctx := context.Background()
	svc, order, sel, _ := newPaymentInfoCreateFixture(t, `{"version":1,"actual_amount":"1.01","qr_content":"secret-code","qr_image_url":"https://gateway.example/secret.png"}`, "https://gateway.example/pay/original")
	_, err := svc.invokeProvider(ctx, order, CreateOrderRequest{UserID: order.UserID, PaymentType: payment.TypeWxpay}, &PaymentConfig{}, 1, "1.00", 1, nil, sel)
	require.NoError(t, err)
	stored, err := svc.entClient.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	before := paymentOrderPaymentPresentationAt(stored, stored.ExpiresAt.Add(-time.Second))
	require.False(t, before.PaymentInfo.RequiresCashier)
	after := paymentOrderPaymentPresentationAt(stored, stored.ExpiresAt)
	require.True(t, after.PaymentInfo.RequiresCashier)
	require.Empty(t, after.PaymentInfo.QRContent)
	require.Equal(t, "https://gateway.example/pay/original", after.PayURL)
	reloaded, err := svc.entClient.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, reloaded.Status)
	require.Equal(t, stored.ProviderSnapshot, reloaded.ProviderSnapshot, "projection must not rewrite the absolute deadline")
	for _, status := range []string{OrderStatusPaid, OrderStatusRecharging, OrderStatusCompleted, OrderStatusCancelled, OrderStatusExpired, OrderStatusFailed, OrderStatusRefundRequested, OrderStatusRefunding, OrderStatusRefundPending, OrderStatusRefunded, OrderStatusPartiallyRefunded, OrderStatusRefundFailed, "UNKNOWN"} {
		t.Run(status, func(t *testing.T) {
			terminal, err := svc.entClient.PaymentOrder.UpdateOneID(order.ID).SetStatus(status).Save(ctx)
			require.NoError(t, err)
			details := PaymentOrderPaymentPresentation(terminal)
			require.Empty(t, details.PayURL)
			require.Empty(t, details.QRCode)
			require.Empty(t, details.QRCodeImg)
			require.Empty(t, details.PaymentInfo.QRContent)
			require.Empty(t, details.PaymentInfo.QRImageURL)
			require.Equal(t, "1.01", details.PaymentInfo.ActualAmount)
		})
	}
}

func newPaymentInfoCreateFixture(t *testing.T, raw, cashier string) (*PaymentService, *dbent.PaymentOrder, *payment.InstanceSelection, *atomic.Int32) {
	t.Helper()
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/mapi.php", r.URL.Path)
		require.Equal(t, "1.00", r.FormValue("money"))
		require.Equal(t, "original-order", r.FormValue("out_trade_no"))
		body := map[string]any{"code": 1, "trade_no": "gateway-trade", "payurl": cashier, "payurl2": "https://gateway.example/wrong-mobile", "qrcode": "legacy-secret", "payment_info": json.RawMessage(raw)}
		require.NoError(t, json.NewEncoder(w).Encode(body))
	}))
	t.Cleanup(server.Close)
	config := map[string]string{"pid": "test-merchant", "pkey": "test-key", "apiBase": server.URL, "notifyUrl": "https://site.example/notify", "returnUrl": "https://site.example/return"}
	encoded, err := json.Marshal(config)
	require.NoError(t, err)
	inst, err := client.PaymentProviderInstance.Create().SetName("payment-info").SetProviderKey(payment.TypeEasyPay).SetConfig(string(encoded)).SetSupportedTypes(payment.TypeWxpay).SetPaymentMode("qrcode").SetEnabled(true).Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("payment-info@example.com").SetPasswordHash("hash").SetUsername("payment-info").Save(ctx)
	require.NoError(t, err)
	sel := &payment.InstanceSelection{InstanceID: strconv.FormatInt(inst.ID, 10), ProviderKey: payment.TypeEasyPay, Config: config, PaymentMode: "qrcode"}
	snapshot := buildPaymentOrderProviderSnapshot(sel, CreateOrderRequest{})
	snapshot["other_snapshot_value"] = "preserved"
	order, err := client.PaymentOrder.Create().SetUserID(user.ID).SetUserEmail(user.Email).SetUserName(user.Username).
		SetAmount(2).SetPayAmount(1).SetFeeRate(0).SetRechargeCode("PAYMENT-INFO").SetOutTradeNo("original-order").SetPaymentType(payment.TypeWxpay).SetPaymentTradeNo("").
		SetProviderInstanceID(sel.InstanceID).SetProviderKey(sel.ProviderKey).SetProviderSnapshot(snapshot).
		SetStatus(OrderStatusPending).SetExpiresAt(time.Now().Add(time.Hour).UTC().Truncate(time.Second)).SetClientIP("127.0.0.1").SetSrcHost("site.example").Save(ctx)
	require.NoError(t, err)
	return &PaymentService{entClient: client}, order, sel, calls
}
