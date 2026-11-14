//go:build unit

package handler

import (
	"bytes"
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

// Real repositories, transactions and SQLite state are used here. Only the
// external payment gateway is an HTTP test server; no money repository is mocked.
type paymentInfoHTTPFixture struct {
	client         *dbent.Client
	db             *sql.DB
	config         *service.PaymentConfigService
	svc            *service.PaymentService
	handler        *PaymentHandler
	user           *dbent.User
	providerConfig map[string]string
	creates        atomic.Int32
	status         atomic.Value
	refunds        chan string
}

func newPaymentInfoHTTPFixture(t *testing.T, info json.RawMessage, safeCashier bool) *paymentInfoHTTPFixture {
	t.Helper()
	t.Setenv("PAYMENT_RESUME_SIGNING_KEY", "payment-info-test-signing-key")
	f := &paymentInfoHTTPFixture{refunds: make(chan string, 2)}
	f.status.Store("pending")
	var err error
	f.db, err = sql.Open("sqlite", "file:"+url.QueryEscape(t.Name())+"?mode=memory&cache=shared")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.db.Close() })
	_, err = f.db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)
	f.client = enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, f.db))))
	t.Cleanup(func() { _ = f.client.Close() })
	_, err = f.client.ExecContext(context.Background(), "CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_audit_logs_order_action_uniq ON payment_audit_logs(order_id, action)")
	require.NoError(t, err)
	var gateway *httptest.Server
	gateway = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mapi.php":
			f.creates.Add(1)
			if r.FormValue("money") != "1.00" {
				t.Errorf("create money = %q", r.FormValue("money"))
			}
			cashier := "javascript:unsafe"
			if safeCashier {
				cashier = gateway.URL + "/cashier/" + r.FormValue("out_trade_no")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "trade_no": "gateway-trade", "payurl": cashier, "qrcode": "legacy-secret", "payment_info": info})
		case r.URL.Path == "/api.php" && r.URL.Query().Get("act") == "order":
			status := f.status.Load().(string)
			paid := 0
			if status == "paid" {
				paid = 1
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1, "status": paid, "order_status": status, "money": "1.00", "trade_no": "gateway-trade"})
		case r.URL.Path == "/api.php" && r.URL.Query().Get("act") == "refund":
			f.refunds <- r.FormValue("money")
			_, _ = w.Write([]byte(`{"code":1,"msg":"success"}`))
		default:
			t.Errorf("unexpected gateway operation: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gateway.Close)
	f.providerConfig = map[string]string{"pid": "test-merchant", "pkey": "test-key", "apiBase": gateway.URL, "notifyUrl": "https://site.example/notify", "returnUrl": "https://site.example/payment/result"}
	encoded, err := json.Marshal(f.providerConfig)
	require.NoError(t, err)
	_, err = f.client.PaymentProviderInstance.Create().SetName("payment-info").SetProviderKey(payment.TypeEasyPay).SetConfig(string(encoded)).SetSupportedTypes(payment.TypeWxpay).SetPaymentMode("qrcode").SetEnabled(true).SetRefundEnabled(true).Save(context.Background())
	require.NoError(t, err)
	f.user, err = f.client.User.Create().SetEmail("payment-info-http@example.com").SetPasswordHash("hash").SetUsername("payment-info").Save(context.Background())
	require.NoError(t, err)
	settings := repository.NewSettingRepository(f.client)
	require.NoError(t, settings.SetMultiple(context.Background(), map[string]string{
		service.SettingPaymentEnabled: "true", service.SettingEnabledPaymentTypes: `["wxpay"]`, service.SettingBalanceRechargeMult: "2",
		service.SettingPaymentVisibleMethodWxpayEnabled: "true", service.SettingPaymentVisibleMethodWxpaySource: service.VisibleMethodSourceEasyPayWechat,
	}))
	f.config = service.NewPaymentConfigService(f.client, settings, nil)
	f.restart()
	return f
}

func (f *paymentInfoHTTPFixture) restart() {
	users := repository.NewUserRepository(f.client, f.db)
	redeems := service.NewRedeemService(repository.NewRedeemCodeRepository(f.client), users, nil, nil, nil, f.client, nil, nil)
	f.svc = service.NewPaymentService(f.client, payment.NewRegistry(), payment.NewDefaultLoadBalancer(f.client, nil), redeems, nil, f.config, users, nil, nil)
	f.handler = NewPaymentHandler(f.svc, f.config)
}

func (f *paymentInfoHTTPFixture) create(t *testing.T) *service.CreateOrderResponse {
	t.Helper()
	created, err := f.svc.CreateOrder(context.Background(), service.CreateOrderRequest{UserID: f.user.ID, Amount: 1, PaymentType: payment.TypeWxpay, ClientIP: "127.0.0.1", SrcHost: "site.example", SrcURL: "https://site.example/payment", ReturnURL: "https://site.example/payment/result", IsMobile: true})
	require.NoError(t, err)
	require.Equal(t, int32(1), f.creates.Load())
	return created
}

func (f *paymentInfoHTTPFixture) request(t *testing.T, method, path, body string, authUserID int64, handler func(*gin.Context)) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if authUserID != 0 {
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: authUserID})
	}
	parts := strings.Split(path, "/")
	c.Params = gin.Params{{Key: "id", Value: parts[len(parts)-1]}}
	handler(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var result struct {
		Code int            `json:"code"`
		Data map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
	require.Zero(t, result.Code)
	return result.Data
}

func TestPaymentInfoHTTPProjectionAndHistory(t *testing.T) {
	f := newPaymentInfoHTTPFixture(t, json.RawMessage(`{"version":1,"actual_amount":"1.01","qr_content":"secret-raw-code"}`), true)
	created := f.create(t)
	require.NotNil(t, created.PaymentInfo)
	require.False(t, created.PaymentInfo.RequiresCashier)
	require.Equal(t, "1.01", created.PaymentInfo.ActualAmount)
	require.Equal(t, created.ExpiresAt.Format(time.RFC3339Nano), created.PaymentInfo.ExpiresAt)
	require.NotEmpty(t, created.ResumeToken)
	f.restart() // Recreate the services: recovery must use DB data, not a provider response cache.
	verifyBody := `{"out_trade_no":"` + created.OutTradeNo + `"}`
	resolveBody := `{"resume_token":"` + created.ResumeToken + `"}`
	for _, status := range []string{service.OrderStatusPending, service.OrderStatusPaid, service.OrderStatusCompleted, service.OrderStatusCancelled, service.OrderStatusExpired, service.OrderStatusFailed, service.OrderStatusRefunded} {
		t.Run(status, func(t *testing.T) {
			_, err := f.client.PaymentOrder.UpdateOneID(created.OrderID).SetStatus(status).Save(context.Background())
			require.NoError(t, err)
			for _, endpoint := range []struct {
				name, method, path, body string
				auth                     int64
				handle                   func(*gin.Context)
			}{
				{"detail", http.MethodGet, fmt.Sprintf("/api/v1/payment/orders/%d", created.OrderID), "", f.user.ID, f.handler.GetOrder},
				{"verify", http.MethodPost, "/api/v1/payment/orders/verify", verifyBody, f.user.ID, f.handler.VerifyOrder},
				{"resume", http.MethodPost, "/api/v1/payment/public/orders/resolve", resolveBody, 0, f.handler.ResolveOrderPublicByResumeToken},
				{"list", http.MethodGet, "/api/v1/payment/orders", "", f.user.ID, f.handler.GetMyOrders},
			} {
				t.Run(endpoint.name, func(t *testing.T) {
					data := f.request(t, endpoint.method, endpoint.path, endpoint.body, endpoint.auth, endpoint.handle)
					if endpoint.name == "list" {
						data = data["items"].([]any)[0].(map[string]any)
					}
					info := data["payment_info"].(map[string]any)
					require.Equal(t, "1.01", info["actual_amount"])
					require.Equal(t, float64(1), data["pay_amount"])
					require.Equal(t, float64(2), data["amount"])
					if status == service.OrderStatusPending {
						require.Equal(t, created.PayURL, data["pay_url"])
						require.Equal(t, "secret-raw-code", info["qr_content"])
						require.Equal(t, created.PaymentInfo.ExpiresAt, info["expires_at"])
					} else {
						require.NotContains(t, data, "pay_url")
						require.NotContains(t, data, "qr_code")
						require.NotContains(t, data, "qr_code_img")
						require.NotContains(t, info, "qr_content")
						require.NotContains(t, info, "qr_image_url")
					}
				})
			}
		})
	}
	public := f.request(t, http.MethodPost, "/api/v1/payment/public/orders/verify", verifyBody, 0, f.handler.VerifyOrderPublic)
	for _, field := range []string{"payment_info", "pay_url", "qr_code", "qr_code_img", "amount", "pay_amount"} {
		require.NotContains(t, public, field)
	}
	require.Equal(t, int32(1), f.creates.Load(), "refresh/verify/resume never creates another order")
}

func TestPaymentInfoHTTPNoCashierDoesNotFailOrder(t *testing.T) {
	f := newPaymentInfoHTTPFixture(t, json.RawMessage(`{"version":1,"actual_amount":"1.01","qr_content":"secret-code"}`), false)
	created := f.create(t)
	require.Empty(t, created.PayURL)
	require.Empty(t, created.QRCode)
	require.True(t, created.PaymentInfo.RequiresCashier)
	require.Empty(t, created.PaymentInfo.QRContent)
	stored, err := f.client.PaymentOrder.Get(context.Background(), created.OrderID)
	require.NoError(t, err)
	require.Equal(t, service.OrderStatusPending, stored.Status)
	require.Equal(t, "1.01", service.PaymentOrderPaymentPresentation(stored).PaymentInfo.ActualAmount)
}

func TestPaymentInfoSettlementAndRefundIsolation(t *testing.T) {
	for _, actual := range []string{"1.01", "0.99", "1.00"} {
		t.Run(actual, func(t *testing.T) {
			ctx := context.Background()
			f := newPaymentInfoHTTPFixture(t, json.RawMessage(`{"version":1,"actual_amount":"`+actual+`","qr_content":"secret-code"}`), true)
			created := f.create(t)
			prov, err := provider.NewEasyPay("test", f.providerConfig)
			require.NoError(t, err)
			if actual != "1.00" {
				wrong, err := prov.VerifyNotification(ctx, paymentInfoSignedNotify(created.OutTradeNo, actual), nil)
				require.NoError(t, err)
				require.ErrorContains(t, f.svc.HandlePaymentNotification(ctx, wrong, payment.TypeEasyPay), "amount mismatch")
				unpaid, err := f.client.PaymentOrder.Get(ctx, created.OrderID)
				require.NoError(t, err)
				require.Equal(t, service.OrderStatusPending, unpaid.Status)
				u, err := f.client.User.Get(ctx, f.user.ID)
				require.NoError(t, err)
				require.Zero(t, u.Balance)
			}
			_, err = prov.VerifyNotification(ctx, strings.Replace(paymentInfoSignedNotify(created.OutTradeNo, "1.00"), "money=1.00", "money=5.00", 1), nil)
			require.Error(t, err, "the real adapter must reject a tampered signature")
			f.status.Store("paid")
			n, err := prov.VerifyNotification(ctx, paymentInfoSignedNotify(created.OutTradeNo, "1.00"), nil)
			require.NoError(t, err)
			for range 2 {
				require.NoError(t, f.svc.ConfirmNotificationUpstream(ctx, prov, n))
				require.NoError(t, f.svc.HandlePaymentNotification(ctx, n, payment.TypeEasyPay))
			}
			stored, err := f.client.PaymentOrder.Get(ctx, created.OrderID)
			require.NoError(t, err)
			require.Equal(t, service.OrderStatusCompleted, stored.Status)
			require.Equal(t, 1.0, stored.PayAmount)
			require.Equal(t, 2.0, stored.Amount)
			u, err := f.client.User.Get(ctx, f.user.ID)
			require.NoError(t, err)
			require.Equal(t, 2.0, u.Balance, "actual SQLite balance is credited once by the original amount")
			require.Equal(t, 2.0, u.TotalRecharged)
			codes, err := f.client.RedeemCode.Query().All(ctx)
			require.NoError(t, err)
			require.Len(t, codes, 1)
			require.Equal(t, service.StatusUsed, codes[0].Status)
			require.Equal(t, 2.0, codes[0].Value)
			plan, early, err := f.svc.PrepareRefund(ctx, created.OrderID, 0, "amount isolation", false, false)
			require.NoError(t, err)
			require.Nil(t, early)
			require.Equal(t, 1.0, plan.GatewayAmount)
			result, err := f.svc.ExecuteRefund(ctx, plan)
			require.NoError(t, err)
			require.True(t, result.Success)
			require.Equal(t, "1.00", <-f.refunds, "the real HTTP refund request uses original money")
			refunded, err := f.client.PaymentOrder.Get(ctx, created.OrderID)
			require.NoError(t, err)
			require.Equal(t, service.OrderStatusRefunded, refunded.Status)
			require.Equal(t, 2.0, refunded.RefundAmount)
			history := service.PaymentOrderPaymentPresentation(refunded)
			require.Equal(t, actual, history.PaymentInfo.ActualAmount)
			require.Empty(t, history.PayURL)
			require.Empty(t, history.PaymentInfo.QRContent)
		})
	}
}

func paymentInfoSignedNotify(orderNo, money string) string {
	values := url.Values{"pid": {"test-merchant"}, "trade_no": {"gateway-trade"}, "out_trade_no": {orderNo}, "money": {money}, "trade_status": {"TRADE_SUCCESS"}, "type": {"wxpay"}, "name": {"test"}}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+values.Get(key))
	}
	hash := md5.Sum([]byte(strings.Join(pairs, "&") + "test-key"))
	values.Set("sign", hex.EncodeToString(hash[:]))
	values.Set("sign_type", "MD5")
	return values.Encode()
}
