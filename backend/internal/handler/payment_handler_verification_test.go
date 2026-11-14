//go:build unit

package handler

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestVerifyOrderEasyPayVerificationContract(t *testing.T) {
	for _, tc := range []struct {
		name, body, mode, wantStatus string
		upstreamHTTP, wantHTTP       int
		failSave                     bool
	}{
		{name: "upstream HTTP 503", body: `{"code":1,"status":0}`, upstreamHTTP: http.StatusServiceUnavailable, wantHTTP: http.StatusServiceUnavailable},
		{name: "upstream failure code", body: `{"code":0,"msg":"key=private-secret"}`, wantHTTP: http.StatusServiceUnavailable},
		{name: "unknown", body: `{"code":1,"status":0,"order_status":"unknown"}`, wantHTTP: http.StatusServiceUnavailable},
		{name: "conflict", body: `{"code":1,"status":0,"order_status":"pending","data":{"order_status":"expired"}}`, wantHTTP: http.StatusServiceUnavailable},
		{name: "malformed", body: `not-json`, wantHTTP: http.StatusServiceUnavailable},
		{name: "empty evidence", body: `{"code":1}`, wantHTTP: http.StatusServiceUnavailable},
		{name: "expired save failure", body: `{"code":1,"order_status":"expired"}`, failSave: true, wantHTTP: http.StatusServiceUnavailable},
		{name: "cancelled save failure", body: `{"code":1,"order_status":"cancelled"}`, failSave: true, wantHTTP: http.StatusServiceUnavailable},
		{name: "explicit pending", body: `{"code":1,"order_status":"pending"}`, wantHTTP: http.StatusOK, wantStatus: service.OrderStatusPending},
		{name: "legacy pending", body: `{"code":1,"status":0}`, wantHTTP: http.StatusOK, wantStatus: service.OrderStatusPending},
		{name: "expired", body: `{"code":1,"status":0,"order_status":"expired"}`, wantHTTP: http.StatusOK, wantStatus: service.OrderStatusExpired},
		{name: "cancelled", body: `{"code":1,"status":0,"order_status":"expired","expire_reason":"cancelled"}`, wantHTTP: http.StatusOK, wantStatus: service.OrderStatusCancelled},
		{name: "popup HTTP 503", mode: "popup", body: `{"code":1,"status":0}`, upstreamHTTP: http.StatusServiceUnavailable, wantHTTP: http.StatusOK, wantStatus: service.OrderStatusPending},
		{name: "popup unknown", mode: "popup", body: `{"code":1,"status":0,"order_status":"unknown"}`, wantHTTP: http.StatusOK, wantStatus: service.OrderStatusPending},
		{name: "popup terminal", mode: "popup", body: `{"code":1,"order_status":"expired"}`, wantHTTP: http.StatusOK, wantStatus: service.OrderStatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := sql.Open("sqlite", "file:payment_handler_verify_r1_"+tc.name+"?mode=memory&cache=shared&_fk=1")
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			_, err = db.Exec("PRAGMA foreign_keys = ON")
			require.NoError(t, err)
			client := enttest.NewClient(t, enttest.WithOptions(dbent.Driver(entsql.OpenDB(dialect.SQLite, db))))
			t.Cleanup(func() { _ = client.Close() })
			var queries atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				queries.Add(1)
				require.Equal(t, "/api.php", r.URL.Path)
				require.Equal(t, "order", r.URL.Query().Get("act"))
				require.Equal(t, "sub2_verify_r1", r.URL.Query().Get("out_trade_no"))
				if tc.upstreamHTTP != 0 {
					w.WriteHeader(tc.upstreamHTTP)
				}
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			mode := tc.mode
			if mode == "" {
				mode = "qrcode"
			}
			cfg, err := json.Marshal(map[string]string{
				"pid": "test-merchant", "pkey": "private-secret", "apiBase": server.URL,
				"notifyUrl": "https://example.com/notify", "returnUrl": "https://example.com/return",
			})
			require.NoError(t, err)
			inst, err := client.PaymentProviderInstance.Create().SetProviderKey(payment.TypeEasyPay).
				SetName("verify R1").SetConfig(string(cfg)).SetSupportedTypes(payment.TypeAlipay).
				SetPaymentMode(mode).SetEnabled(true).Save(ctx)
			require.NoError(t, err)
			user, err := client.User.Create().SetEmail("verify-r1@example.com").SetPasswordHash("hash").SetUsername("verify-r1").Save(ctx)
			require.NoError(t, err)
			instanceID := strconv.FormatInt(inst.ID, 10)
			order, err := client.PaymentOrder.Create().SetUserID(user.ID).SetUserEmail(user.Email).
				SetUserName(user.Username).SetAmount(10).SetPayAmount(10).SetFeeRate(0).
				SetRechargeCode("VERIFY-R1").SetOutTradeNo("sub2_verify_r1").SetPaymentType(payment.TypeAlipay).SetPaymentTradeNo("original-trade").
				SetOrderType(payment.OrderTypeBalance).SetStatus(service.OrderStatusPending).
				SetProviderInstanceID(instanceID).SetProviderKey(payment.TypeEasyPay).
				SetProviderSnapshot(map[string]any{"schema_version": 2, "provider_instance_id": instanceID,
					"provider_key": payment.TypeEasyPay, "payment_mode": mode, "merchant_id": "test-merchant"}).
				SetQrCode("weixin://verify-r1").SetExpiresAt(time.Now().Add(time.Hour)).
				SetClientIP("127.0.0.1").SetSrcHost("api.example.com").Save(ctx)
			require.NoError(t, err)
			var saves int
			if tc.failSave {
				client.PaymentOrder.Use(func(next dbent.Mutator) dbent.Mutator {
					return dbent.MutateFunc(func(context.Context, dbent.Mutation) (dbent.Value, error) {
						saves++
						return nil, errors.New("injected terminal save failure")
					})
				})
			}
			svc := service.NewPaymentService(client, payment.NewRegistry(), payment.NewDefaultLoadBalancer(client, nil), nil, nil, nil, nil, nil, nil)
			h := NewPaymentHandler(svc, nil)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/payment/orders/verify", bytes.NewBufferString(`{"out_trade_no":"sub2_verify_r1"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: user.ID})
			h.VerifyOrder(c)
			require.Equal(t, tc.wantHTTP, recorder.Code)
			require.Equal(t, int32(1), queries.Load())
			if tc.failSave {
				require.Equal(t, 1, saves)
			}
			var resp response.Response
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &resp))
			if tc.wantHTTP == http.StatusServiceUnavailable {
				require.Equal(t, http.StatusServiceUnavailable, resp.Code)
				require.Equal(t, "PAYMENT_VERIFICATION_UNAVAILABLE", resp.Reason)
				require.Equal(t, "Payment verification is temporarily unavailable; please retry.", resp.Message)
				require.Nil(t, resp.Data)
			} else {
				require.Zero(t, resp.Code)
				require.Empty(t, resp.Reason)
				data, ok := resp.Data.(map[string]any)
				require.True(t, ok)
				require.Equal(t, tc.wantStatus, data["status"])
			}
			require.NotContains(t, recorder.Body.String(), "private-secret")
			require.NotContains(t, recorder.Body.String(), "PendingVerified")
			persisted, err := client.PaymentOrder.Get(ctx, order.ID)
			require.NoError(t, err)
			wantPersisted := tc.wantStatus
			if wantPersisted == "" {
				wantPersisted = service.OrderStatusPending
			}
			require.Equal(t, wantPersisted, persisted.Status)
		})
	}
}
