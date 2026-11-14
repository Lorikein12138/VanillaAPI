package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

func TestEasyPayPaymentInfo(t *testing.T) {
	for _, tc := range []struct {
		name, raw, cashier string
		wantCashier        bool
	}{
		{"content", `{"version":1,"actual_amount":"1.01","qr_content":" /opaque payload "}`, "https://gateway.example/pay/order", false},
		{"image", `{"version":1,"actual_amount":"0.99","qr_image_url":"https://gateway.example/qr.png"}`, "https://gateway.example/pay/order", false},
		{"captcha", `{"version":1,"actual_amount":"1.01","qr_content":"code","requires_cashier":true}`, "https://gateway.example/pay/order", true},
		{"malformed", `[]`, "https://gateway.example/pay/order", true},
		{"null", `null`, "https://gateway.example/pay/order", true},
		{"unknown", `{"version":2,"actual_amount":"1.01","qr_content":"code"}`, "https://gateway.example/pay/order", true},
		{"expired", `{"version":1,"actual_amount":"1.01","qr_content":"code","expires_at":"2000-01-01T00:00:00Z"}`, "https://gateway.example/pay/order", true},
		{"no cashier", `{"version":1,"actual_amount":"1.01","qr_content":"code"}`, "", true},
		{"unsafe cashier", `{"version":1,"actual_amount":"1.01","qr_content":"code"}`, "https://u:p@gateway.example/pay", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"code": 1, "trade_no": "same-order", "payurl": tc.cashier, "payurl2": "https://gateway.example/mobile-order", "qrcode": "legacy-must-not-reappear", "payment_info": json.RawMessage(tc.raw)})
			require.NoError(t, err)
			prov, _ := newEasyPayMapiStub(t, "", string(body))
			got, err := prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "original-order", Amount: "1.00", IsMobile: true})
			require.NoError(t, err)
			require.Equal(t, "same-order", got.TradeNo)
			require.NotNil(t, got.PaymentInfo)
			require.Equal(t, tc.wantCashier, got.PaymentInfo.RequiresCashier)
			require.Equal(t, payment.SafePaymentHTTPURL(tc.cashier), got.PayURL)
			require.Empty(t, got.QRCode)
			if tc.wantCashier {
				require.Empty(t, got.PaymentInfo.QRContent)
				require.Empty(t, got.PaymentInfo.QRImageURL)
			} else if tc.name == "content" {
				require.Equal(t, " /opaque payload ", got.PaymentInfo.QRContent)
			}
		})
	}
}

func TestEasyPayPaymentInfoDoesNotChangeSignedRequest(t *testing.T) {
	var calls atomic.Int32
	forms := make(chan url.Values, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/mapi.php", r.URL.Path)
		require.NoError(t, r.ParseForm())
		forms <- r.PostForm
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"code":1,"payurl":"https://gateway.example/pay/order","qrcode":"old-token"}`))
		} else {
			_, _ = w.Write([]byte(`{"code":1,"payurl":"https://gateway.example/pay/order","payment_info":{"version":1,"actual_amount":"1.01","qr_content":"new-token"}}`))
		}
	}))
	defer server.Close()
	prov, err := NewEasyPay("test", map[string]string{"pid": "merchant", "pkey": "test-key", "apiBase": server.URL, "notifyUrl": "https://site.example/notify", "returnUrl": "https://site.example/return", "cid": "7"})
	require.NoError(t, err)
	req := payment.CreatePaymentRequest{OrderID: "same-order", Amount: "1.00", PaymentType: "wxpay", Subject: "test", ClientIP: "127.0.0.1", IsMobile: true}
	legacy, err := prov.CreatePayment(context.Background(), req)
	require.NoError(t, err)
	require.Nil(t, legacy.PaymentInfo)
	require.Equal(t, "old-token", legacy.QRCode)
	_, err = prov.CreatePayment(context.Background(), req)
	require.NoError(t, err)
	first, second := <-forms, <-forms
	require.Equal(t, first, second, "the complete signed form must remain unchanged")
	require.Equal(t, "1.00", second.Get("money"))
	require.Equal(t, int32(2), calls.Load(), "one request per create, no retry or checkout scraping")
}

func TestEasyPayPaymentInfoInvalidLegacyFields(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		wantCashier  bool
	}{
		{"numeric cashier", `"payurl":123`, true},
		{"null cashier", `"payurl":null`, true},
		{"missing cashier", `"unused":true`, true},
		{"unused legacy types", `"payurl":"https://gateway.example/pay/original","qrcode":{},"payurl2":false`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov, _ := newEasyPayMapiStub(t, "", `{"code":1,"trade_no":"original",`+tc.fields+`,"payment_info":{"version":1,"actual_amount":"1.01","qr_content":"raw"}}`)
			got, err := prov.CreatePayment(context.Background(), payment.CreatePaymentRequest{OrderID: "original", Amount: "1.00", IsMobile: true})
			require.NoError(t, err)
			require.Equal(t, tc.wantCashier, got.PaymentInfo.RequiresCashier)
			require.Empty(t, got.QRCode)
			if tc.wantCashier {
				require.Empty(t, got.PayURL)
				require.Empty(t, got.PaymentInfo.QRContent)
			}
		})
	}
}
