//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type easyPayTerminalFixture struct {
	svc        *PaymentService
	order      *dbent.PaymentOrder
	queries    atomic.Int32
	userRepo   *mockUserRepo
	redeemRepo *paymentOrderLifecycleRedeemRepo
}

func newEasyPayTerminalFixture(t *testing.T, mode string, handler http.HandlerFunc) *easyPayTerminalFixture {
	t.Helper()
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	f := &easyPayTerminalFixture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.queries.Add(1)
		if r.URL.Path != "/api.php" || r.URL.Query().Get("act") != "order" || r.URL.Query().Get("out_trade_no") != "sub2_terminal_sync" {
			t.Error("unexpected provider operation or query reference")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	cfg, err := json.Marshal(map[string]string{
		"pid": "test-merchant", "pkey": "test-key", "apiBase": server.URL,
		"notifyUrl": "https://example.com/notify", "returnUrl": "https://example.com/return",
	})
	require.NoError(t, err)
	inst, err := client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeEasyPay).SetName("Terminal sync EasyPay").
		SetConfig(string(cfg)).SetSupportedTypes(payment.TypeAlipay).
		SetPaymentMode(mode).SetEnabled(true).Save(ctx)
	require.NoError(t, err)
	user, err := client.User.Create().SetEmail("terminal-sync@example.com").
		SetPasswordHash("hash").SetUsername("terminal-sync").Save(ctx)
	require.NoError(t, err)
	instanceID := strconv.FormatInt(inst.ID, 10)
	f.order, err = client.PaymentOrder.Create().
		SetUserID(user.ID).SetUserEmail(user.Email).SetUserName(user.Username).
		SetAmount(10).SetPayAmount(10).SetFeeRate(0).
		SetRechargeCode("TERMINAL-SYNC").SetOutTradeNo("sub2_terminal_sync").
		SetPaymentType(payment.TypeAlipay).SetPaymentTradeNo("original-trade").
		SetOrderType(payment.OrderTypeBalance).SetStatus(OrderStatusPending).
		SetProviderInstanceID(instanceID).SetProviderKey(payment.TypeEasyPay).
		SetProviderSnapshot(map[string]any{"schema_version": 2, "provider_instance_id": instanceID,
			"provider_key": payment.TypeEasyPay, "payment_mode": mode, "merchant_id": "test-merchant"}).
		SetQrCode("weixin://test-payment").SetPayURL("https://example.com/pay").
		SetExpiresAt(time.Now().Add(time.Hour)).SetClientIP("127.0.0.1").SetSrcHost("api.example.com").Save(ctx)
	require.NoError(t, err)
	f.userRepo = &mockUserRepo{getByIDUser: &User{ID: user.ID, Email: user.Email}}
	f.userRepo.updateBalanceFn = func(_ context.Context, _ int64, amount float64) error {
		f.userRepo.getByIDUser.Balance += amount
		return nil
	}
	f.redeemRepo = &paymentOrderLifecycleRedeemRepo{codesByCode: map[string]*RedeemCode{
		f.order.RechargeCode: {ID: 1, Code: f.order.RechargeCode, Type: RedeemTypeBalance, Value: 10, Status: StatusUnused},
	}}
	f.svc = &PaymentService{
		entClient: client, registry: payment.NewRegistry(), providersLoaded: true,
		loadBalancer: payment.NewDefaultLoadBalancer(client, nil), userRepo: f.userRepo,
		redeemService: NewRedeemService(f.redeemRepo, f.userRepo, nil, nil, nil, client, nil, nil),
	}
	return f
}

func terminalQueryBody(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

func requirePaymentVerificationUnavailable(t *testing.T, order *dbent.PaymentOrder, err error) {
	t.Helper()
	require.Nil(t, order)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, infraerrors.Code(err))
	require.Equal(t, "PAYMENT_VERIFICATION_UNAVAILABLE", infraerrors.Reason(err))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	require.True(t, response.ErrorFrom(c, err))
	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	var body response.Response
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "PAYMENT_VERIFICATION_UNAVAILABLE", body.Reason)
	require.Equal(t, "Payment verification is temporarily unavailable; please retry.", body.Message)
	require.NotContains(t, recorder.Body.String(), "test-key")
	require.NotContains(t, recorder.Body.String(), "private-secret")
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalPersistence(t *testing.T) {
	tests := []struct{ name, body, want, action string }{
		{"expired", `{"code":1,"status":0,"order_status":"expired","expire_reason":"timeout"}`, OrderStatusExpired, "ORDER_EXPIRED"},
		{"cancelled", `{"code":1,"status":0,"order_status":"expired","expire_reason":"cancelled"}`, OrderStatusCancelled, "ORDER_CANCELLED"},
		{"direct cancelled", `{"code":1,"order_status":"cancelled"}`, OrderStatusCancelled, "ORDER_CANCELLED"},
		{"nested expired", `{"code":1,"data":{"status":"0","order_status":"expired"}}`, OrderStatusExpired, "ORDER_EXPIRED"},
		{"legacy pending", `{"code":1,"status":0}`, OrderStatusPending, ""},
		{"explicit pending", `{"code":1,"status":0,"order_status":"pending"}`, OrderStatusPending, ""},
		{"unknown", `{"code":1,"status":0,"order_status":"unknown"}`, OrderStatusPending, ""},
		{"conflicting", `{"code":1,"order_status":"expired","data":{"order_status":"pending"}}`, OrderStatusPending, ""},
		{"query failure", `{"code":0,"order_status":"expired"}`, OrderStatusPending, ""},
		{"malformed query", `not-json`, OrderStatusPending, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(tt.body))
			for range 2 {
				got, err := f.svc.VerifyOrderByOutTradeNo(ctx, f.order.OutTradeNo, f.order.UserID)
				if tt.name == "unknown" || tt.name == "conflicting" || tt.name == "query failure" || tt.name == "malformed query" {
					requirePaymentVerificationUnavailable(t, got, err)
				} else {
					require.NoError(t, err)
					require.Equal(t, tt.want, got.Status)
				}
				persisted, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
				require.NoError(t, err)
				require.Equal(t, tt.want, persisted.Status)
				require.Equal(t, f.order.QrCode, persisted.QrCode)
				require.Equal(t, f.order.PayURL, persisted.PayURL)
				require.Equal(t, f.order.PaymentTradeNo, persisted.PaymentTradeNo)
				require.Nil(t, persisted.PaidAt)
				require.Nil(t, persisted.CompletedAt)
			}
			logs, err := f.svc.entClient.PaymentAuditLog.Query().All(ctx)
			require.NoError(t, err)
			if tt.action != "" {
				require.Len(t, logs, 1)
				require.Equal(t, tt.action, logs[0].Action)
			} else {
				require.Empty(t, logs)
			}
			wantQueries := int32(2)
			if tt.want == OrderStatusCancelled {
				wantQueries = 1
			}
			require.Equal(t, wantQueries, f.queries.Load())
			require.Zero(t, f.userRepo.getByIDUser.Balance)
			require.Empty(t, f.redeemRepo.useCalls)
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalExplicitPendingAndLegacyZero(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{name: "legacy zero", body: `{"code":1,"status":0}`},
		{name: "explicit pending", body: `{"code":1,"status":0,"order_status":"pending"}`},
		{name: "explicit waiting", body: `{"code":1,"trade_status":"WAITING"}`},
		{name: "pending and null reason", body: `{"code":1,"order_status":"pending","expire_reason":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(tc.body))
			got, err := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
			require.NoError(t, err)
			require.Equal(t, OrderStatusPending, got.Status)
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalStorageFailureUnavailable(t *testing.T) {
	for _, status := range []string{"expired", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(`{"code":1,"status":0,"order_status":"`+status+`"}`))
			var saves int
			f.svc.entClient.PaymentOrder.Use(func(next dbent.Mutator) dbent.Mutator {
				return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
					mutation, ok := m.(*dbent.PaymentOrderMutation)
					require.True(t, ok)
					if _, ok := mutation.Status(); ok {
						saves++
						return nil, errors.New("injected terminal CAS failure")
					}
					return next.Mutate(ctx, m)
				})
			})
			got, err := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
			requirePaymentVerificationUnavailable(t, got, err)
			require.Equal(t, 1, saves)
			require.Equal(t, int32(1), f.queries.Load())
			persisted, err := f.svc.entClient.PaymentOrder.Get(context.Background(), f.order.ID)
			require.NoError(t, err)
			require.Equal(t, OrderStatusPending, persisted.Status)
			logs, err := f.svc.entClient.PaymentAuditLog.Query().All(context.Background())
			require.NoError(t, err)
			require.Empty(t, logs)
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalMissingProviderOrQueryReferenceUnavailable(t *testing.T) {
	t.Run("provider instance missing", func(t *testing.T) {
		f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(`{"code":1,"status":0}`))
		_, err := f.svc.entClient.PaymentProviderInstance.Delete().Exec(context.Background())
		require.NoError(t, err)
		got, err := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
		requirePaymentVerificationUnavailable(t, got, err)
		require.Zero(t, f.queries.Load())
	})
	t.Run("missing query reference", func(t *testing.T) {
		f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(`{"code":1,"status":0}`))
		// Lookup validation rejects an empty public identifier before loading an
		// order. Simulate a corrupt loaded row instead to exercise the missing
		// provider reference guard without bypassing authenticated lookup.
		var reads int
		f.svc.entClient.PaymentOrder.Intercept(dbent.InterceptFunc(func(next dbent.Querier) dbent.Querier {
			return dbent.QuerierFunc(func(ctx context.Context, q dbent.Query) (dbent.Value, error) {
				value, err := next.Query(ctx, q)
				reads++
				if err == nil && reads == 1 {
					orders, ok := value.([]*dbent.PaymentOrder)
					require.True(t, ok)
					require.Len(t, orders, 1)
					orders[0].OutTradeNo = " "
				}
				return value, err
			})
		}))
		got, verifyErr := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
		requirePaymentVerificationUnavailable(t, got, verifyErr)
		require.Equal(t, 2, reads)
		require.Zero(t, f.queries.Load())
	})
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalModeBoundary(t *testing.T) {
	tests := []struct {
		name, instanceMode, snapshotMode, orderType, want string
		legacy                                            bool
	}{
		{name: "popup", instanceMode: "popup", snapshotMode: "popup", orderType: payment.OrderTypeBalance, want: OrderStatusPending},
		{name: "redirect", instanceMode: "redirect", snapshotMode: "redirect", orderType: payment.OrderTypeBalance, want: OrderStatusPending},
		{name: "subscription", instanceMode: "qrcode", snapshotMode: "qrcode", orderType: payment.OrderTypeSubscription, want: OrderStatusPending},
		{name: "unknown mode", instanceMode: "qrcode", snapshotMode: "unknown", orderType: payment.OrderTypeBalance, want: OrderStatusPending},
		{name: "snapshot popup wins", instanceMode: "qrcode", snapshotMode: "popup", orderType: payment.OrderTypeBalance, want: OrderStatusPending},
		{name: "snapshot qrcode wins", instanceMode: "popup", snapshotMode: "qrcode", orderType: payment.OrderTypeBalance, want: OrderStatusExpired},
		{name: "legacy qrcode", instanceMode: "qrcode", orderType: payment.OrderTypeBalance, legacy: true, want: OrderStatusExpired},
		{name: "legacy popup", instanceMode: "popup", orderType: payment.OrderTypeBalance, legacy: true, want: OrderStatusPending},
		{name: "legacy unknown mode", instanceMode: "", orderType: payment.OrderTypeBalance, legacy: true, want: OrderStatusPending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newEasyPayTerminalFixture(t, tt.instanceMode, terminalQueryBody(`{"code":1,"status":0,"order_status":"expired"}`))
			u := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetOrderType(tt.orderType)
			if tt.legacy {
				u.ClearProviderSnapshot()
			} else {
				f.order.ProviderSnapshot["payment_mode"] = tt.snapshotMode
				u.SetProviderSnapshot(f.order.ProviderSnapshot)
			}
			require.NoError(t, u.Exec(ctx))
			got, err := f.svc.VerifyOrderByOutTradeNo(ctx, f.order.OutTradeNo, f.order.UserID)
			if tt.name == "unknown mode" || tt.name == "legacy unknown mode" {
				requirePaymentVerificationUnavailable(t, got, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.want, got.Status)
			}
			persisted, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
			require.NoError(t, err)
			require.Equal(t, tt.want, persisted.Status)
			require.Zero(t, f.userRepo.getByIDUser.Balance)
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalCASProtectsConcurrentState(t *testing.T) {
	for _, upstream := range []struct {
		name, body string
		httpStatus int
	}{
		{"terminal", `{"code":1,"status":0,"order_status":"expired","expire_reason":"cancelled"}`, http.StatusOK},
		{"verified pending", `{"code":1,"status":0}`, http.StatusOK},
		{"unknown", `{"code":1,"status":0,"order_status":"unknown"}`, http.StatusOK},
		{"conflict", `{"code":1,"status":0,"data":{"order_status":"expired"},"order_status":"pending"}`, http.StatusOK},
		{"query failure", `{"code":0}`, http.StatusOK},
		{"HTTP failure", `{"code":1,"status":0}`, http.StatusServiceUnavailable},
	} {
		t.Run(upstream.name, func(t *testing.T) {
			for _, status := range []string{OrderStatusPaid, OrderStatusRecharging, OrderStatusCompleted, OrderStatusCancelled, OrderStatusExpired} {
				t.Run(status, func(t *testing.T) {
					ctx := context.Background()
					started, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					f := newEasyPayTerminalFixture(t, "qrcode", func(w http.ResponseWriter, r *http.Request) {
						close(started)
						select {
						case <-release:
							w.WriteHeader(upstream.httpStatus)
							_, _ = w.Write([]byte(upstream.body))
						case <-r.Context().Done():
						}
					})
					t.Cleanup(func() { once.Do(func() { close(release) }) })
					type result struct {
						order *dbent.PaymentOrder
						err   error
					}
					completed := make(chan result, 1)
					go func() {
						order, err := f.svc.VerifyOrderByOutTradeNo(ctx, f.order.OutTradeNo, f.order.UserID)
						completed <- result{order, err}
					}()
					select {
					case <-started:
					case <-time.After(5 * time.Second):
						t.Fatal("upstream query did not start")
					}
					require.NoError(t, f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).SetStatus(status).SetPaymentTradeNo("concurrent-trade").Exec(ctx))
					once.Do(func() { close(release) })
					select {
					case got := <-completed:
						require.NoError(t, got.err)
						require.Equal(t, status, got.order.Status)
						require.Equal(t, "concurrent-trade", got.order.PaymentTradeNo)
					case <-time.After(5 * time.Second):
						t.Fatal("verification did not complete")
					}
					persisted, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
					require.NoError(t, err)
					require.Equal(t, status, persisted.Status)
					count, err := f.svc.entClient.PaymentAuditLog.Query().Count(ctx)
					require.NoError(t, err)
					require.Zero(t, count)
					require.Zero(t, f.userRepo.getByIDUser.Balance)
					require.Empty(t, f.redeemRepo.useCalls)
				})
			}
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalCASFailureReloadsConcurrentState(t *testing.T) {
	for _, status := range []string{OrderStatusPaid, OrderStatusRecharging, OrderStatusCompleted, OrderStatusCancelled, OrderStatusExpired} {
		t.Run(status, func(t *testing.T) {
			f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(`{"code":1,"order_status":"expired"}`))
			var saves int
			f.svc.entClient.PaymentOrder.Use(func(next dbent.Mutator) dbent.Mutator {
				return dbent.MutateFunc(func(ctx context.Context, m dbent.Mutation) (dbent.Value, error) {
					if m.Op().Is(dbent.OpUpdate) {
						saves++
						if err := f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).
							SetStatus(status).SetPaymentTradeNo("concurrent-trade").Exec(ctx); err != nil {
							return nil, err
						}
						return nil, errors.New("injected terminal CAS failure after concurrent update")
					}
					return next.Mutate(ctx, m)
				})
			})
			got, err := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
			require.NoError(t, err)
			require.Equal(t, status, got.Status)
			require.Equal(t, "concurrent-trade", got.PaymentTradeNo)
			require.Equal(t, 1, saves)
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalCannotReleaseUnverifiedPending(t *testing.T) {
	t.Run("zero row CAS still pending", func(t *testing.T) {
		f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(`{"code":1,"order_status":"expired"}`))
		var saves int
		f.svc.entClient.PaymentOrder.Use(func(next dbent.Mutator) dbent.Mutator {
			return dbent.MutateFunc(func(context.Context, dbent.Mutation) (dbent.Value, error) {
				saves++
				return 0, nil
			})
		})
		got, err := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
		requirePaymentVerificationUnavailable(t, got, err)
		require.Equal(t, 1, saves)
	})
	for _, body := range []string{`{"code":1,"status":0}`, `{"code":1,"order_status":"expired"}`} {
		t.Run("reload failure "+body, func(t *testing.T) {
			f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(body))
			var reads int
			f.svc.entClient.PaymentOrder.Intercept(dbent.InterceptFunc(func(next dbent.Querier) dbent.Querier {
				return dbent.QuerierFunc(func(ctx context.Context, q dbent.Query) (dbent.Value, error) {
					reads++
					if reads == 2 {
						return nil, errors.New("injected reload failure")
					}
					return next.Query(ctx, q)
				})
			}))
			got, err := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
			requirePaymentVerificationUnavailable(t, got, err)
			require.Equal(t, 2, reads)
			require.Equal(t, int32(1), f.queries.Load())
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalPopupFailureBoundary(t *testing.T) {
	for _, mode := range []string{"popup", "redirect"} {
		for _, body := range []string{`{"code":0}`, `{"code":1,"order_status":"unknown"}`, `not-json`} {
			t.Run(mode+" "+body, func(t *testing.T) {
				f := newEasyPayTerminalFixture(t, mode, terminalQueryBody(body))
				got, err := f.svc.VerifyOrderByOutTradeNo(context.Background(), f.order.OutTradeNo, f.order.UserID)
				require.NoError(t, err)
				require.Equal(t, OrderStatusPending, got.Status)
				require.Equal(t, int32(1), f.queries.Load())
			})
		}
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalOnlyAuthenticated(t *testing.T) {
	ctx := context.Background()
	f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(`{"code":1,"status":0,"order_status":"expired"}`))
	got, err := f.svc.VerifyOrderPublic(ctx, f.order.OutTradeNo)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, got.Status)
	_, err = f.svc.VerifyOrderByOutTradeNo(ctx, f.order.OutTradeNo, f.order.UserID+1)
	require.Error(t, err)
	require.Zero(t, f.queries.Load())
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalDoesNotChangeOtherRecovery(t *testing.T) {
	ctx := context.Background()
	f := newEasyPayTerminalFixture(t, "qrcode", terminalQueryBody(`{"code":1,"status":0,"order_status":"expired"}`))
	require.Equal(t, "", f.svc.reconcilePaid(ctx, f.order))
	persisted, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, persisted.Status)
	// Registry-only EasyPay has no provable QR mode; other providers must
	// never opt into terminal synchronization either.
	_, err = f.svc.entClient.PaymentProviderInstance.Delete().Exec(ctx)
	require.NoError(t, err)
	for _, key := range []string{payment.TypeEasyPay, payment.TypeAlipay, payment.TypeWxpay, payment.TypeStripe, payment.TypeAirwallex} {
		t.Run(key, func(t *testing.T) {
			require.NoError(t, f.svc.entClient.PaymentOrder.UpdateOneID(f.order.ID).
				ClearProviderSnapshot().ClearProviderInstanceID().ClearProviderKey().SetPaymentType(key).Exec(ctx))
			prov := &paymentOrderLifecycleQueryProvider{key: key, resp: &payment.QueryOrderResponse{Status: payment.ProviderStatusExpired}}
			f.svc.registry = payment.NewRegistry()
			f.svc.registry.Register(prov)
			got, err := f.svc.VerifyOrderByOutTradeNo(ctx, f.order.OutTradeNo, f.order.UserID)
			if key == payment.TypeEasyPay {
				requirePaymentVerificationUnavailable(t, got, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, OrderStatusPending, got.Status)
			}
			persisted, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
			require.NoError(t, err)
			require.Equal(t, OrderStatusPending, persisted.Status)
			require.Equal(t, 1, prov.queryCalls)
			require.Zero(t, prov.cancelCalls)
		})
	}
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalHTTPFailureUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newEasyPayTerminalFixture(t, "qrcode", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":1,"status":0,"order_status":"expired","expire_reason":"cancelled"}`))
	})
	got, err := f.svc.VerifyOrderByOutTradeNo(ctx, f.order.OutTradeNo, f.order.UserID)
	requirePaymentVerificationUnavailable(t, got, err)
	persisted, err := f.svc.entClient.PaymentOrder.Get(ctx, f.order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, persisted.Status)
	require.Empty(t, f.redeemRepo.useCalls)
}

func TestVerifyOrderByOutTradeNoEasyPayTerminalPaidStillFulfillsOnce(t *testing.T) {
	for _, mode := range []string{"qrcode", "popup"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newEasyPayTerminalFixture(t, mode, terminalQueryBody(`{"code":1,"status":1,"order_status":"expired","expire_reason":"cancelled","trade_no":"paid-trade","money":"10.00"}`))
			for range 2 {
				got, err := f.svc.VerifyOrderByOutTradeNo(ctx, f.order.OutTradeNo, f.order.UserID)
				require.NoError(t, err)
				require.Equal(t, OrderStatusCompleted, got.Status)
				require.Equal(t, "paid-trade", got.PaymentTradeNo)
			}
			require.Equal(t, int32(1), f.queries.Load())
			require.Equal(t, 10.0, f.userRepo.getByIDUser.Balance)
			require.Len(t, f.redeemRepo.useCalls, 1)
		})
	}
}
