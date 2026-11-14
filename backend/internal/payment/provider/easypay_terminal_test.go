package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestEasyPayQueryOrderTerminalStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, body, want string
		httpStatus       int
		wantErr          bool
	}{
		{name: "expired timeout", body: `{"code":1,"status":0,"order_status":"expired","expire_reason":"timeout"}`, want: payment.ProviderStatusExpired},
		{name: "expired without reason", body: `{"code":1,"order_status":"expired"}`, want: payment.ProviderStatusExpired},
		{name: "expired null reason", body: `{"code":1,"status":"0","order_status":"expired","expire_reason":null}`, want: payment.ProviderStatusExpired},
		{name: "expired empty reason", body: `{"code":1,"order_status":"expired","expire_reason":""}`, want: payment.ProviderStatusExpired},
		{name: "cancelled expired order", body: `{"code":1,"status":0,"order_status":"expired","expire_reason":"cancelled"}`, want: payment.ProviderStatusCancelled},
		{name: "direct cancelled", body: `{"code":1,"status":"0","order_status":"cancelled"}`, want: payment.ProviderStatusCancelled},
		{name: "nested cancellation", body: `{"code":"1","data":{"status":"0","order_status":"expired","expire_reason":"cancelled"}}`, want: payment.ProviderStatusCancelled},
		{name: "null fields fall back", body: `{"code":1,"order_status":null,"expire_reason":null,"data":{"status":0,"order_status":"expired","expire_reason":"timeout"}}`, want: payment.ProviderStatusExpired},
		{name: "matching duplicate fields", body: `{"code":1,"status":0,"order_status":"expired","expire_reason":"cancelled","data":{"status":"0","order_status":"expired","expire_reason":"cancelled"}}`, want: payment.ProviderStatusCancelled},
		{name: "normalized extension", body: `{"code":1,"status":0,"order_status":" EXPIRED ","expire_reason":" CANCELLED "}`, want: payment.ProviderStatusCancelled},
		{name: "legacy zero", body: `{"code":1,"status":0}`, want: payment.ProviderStatusPending},
		{name: "legacy string zero", body: `{"code":1,"status":"0"}`, want: payment.ProviderStatusPending},
		{name: "reason alone", body: `{"code":1,"status":0,"expire_reason":"cancelled"}`, want: payment.ProviderStatusPending},
		{name: "pending extension", body: `{"code":1,"status":0,"order_status":"pending"}`, want: payment.ProviderStatusPending},
		{name: "pending with cancellation reason", body: `{"code":1,"status":0,"order_status":"pending","expire_reason":"cancelled"}`, want: payment.ProviderStatusPending},
		{name: "unknown extension", body: `{"code":1,"status":0,"order_status":"closed"}`, want: payment.ProviderStatusPending},
		{name: "unknown legacy status", body: `{"code":1,"status":2,"order_status":"expired"}`, want: payment.ProviderStatusPending},
		{name: "unknown reason", body: `{"code":1,"status":0,"order_status":"expired","expire_reason":"unknown"}`, want: payment.ProviderStatusPending},
		{name: "malformed reason", body: `{"code":1,"status":0,"order_status":"expired","expire_reason":{}}`, want: payment.ProviderStatusPending},
		{name: "malformed extension", body: `{"code":1,"status":0,"order_status":false}`, want: payment.ProviderStatusPending},
		{name: "waiting trade status conflicts", body: `{"code":1,"trade_status":"WAITING","status":0,"order_status":"expired"}`, want: payment.ProviderStatusPending},
		{name: "empty trade status conflicts", body: `{"code":1,"trade_status":"","status":0,"order_status":"expired"}`, want: payment.ProviderStatusPending},
		{name: "nested pending conflicts", body: `{"code":1,"order_status":"expired","data":{"order_status":"pending"}}`, want: payment.ProviderStatusPending},
		{name: "nested unknown conflicts", body: `{"code":1,"order_status":"expired","data":{"order_status":"unknown"}}`, want: payment.ProviderStatusPending},
		{name: "different terminals conflict", body: `{"code":1,"order_status":"expired","data":{"order_status":"cancelled"}}`, want: payment.ProviderStatusPending},
		{name: "reasons conflict", body: `{"code":1,"order_status":"expired","expire_reason":"timeout","data":{"expire_reason":"cancelled"}}`, want: payment.ProviderStatusPending},
		{name: "cancelled timeout conflicts", body: `{"code":1,"order_status":"cancelled","expire_reason":"timeout"}`, want: payment.ProviderStatusPending},
		{name: "suppressed nested paid blocks terminal", body: `{"code":1,"status":0,"order_status":"expired","data":{"status":1}}`, want: payment.ProviderStatusPending},
		{name: "numeric paid wins", body: `{"code":1,"status":1,"order_status":"expired","expire_reason":"cancelled"}`, want: payment.ProviderStatusPaid},
		{name: "trade success wins", body: `{"code":1,"trade_status":"TRADE_SUCCESS","status":0,"order_status":"expired","expire_reason":"cancelled"}`, want: payment.ProviderStatusPaid},
		{name: "nested paid wins", body: `{"code":1,"order_status":"expired","data":{"status":"1"}}`, want: payment.ProviderStatusPaid},
		{name: "nested success wins top pending status", body: `{"code":1,"trade_status":"WAITING","status":0,"order_status":"expired","data":{"trade_status":"TRADE_SUCCESS"}}`, want: payment.ProviderStatusPaid},
		{name: "extension paid alone cannot close", body: `{"code":1,"order_status":"paid","expire_reason":"cancelled"}`, want: payment.ProviderStatusPending},
		{name: "failure code", body: `{"code":0,"order_status":"expired"}`, wantErr: true},
		{name: "foreign order", body: `{"code":1,"out_trade_no":"another-order","order_status":"expired"}`, wantErr: true},
		{name: "conflicting nested order", body: `{"code":1,"out_trade_no":"order-123","order_status":"expired","data":{"out_trade_no":"another-order"}}`, wantErr: true},
		{name: "http failure with terminal body", body: `{"code":1,"status":0,"order_status":"expired"}`, httpStatus: http.StatusServiceUnavailable, wantErr: true},
		{name: "invalid json", body: `{"code":1,"order_status":"expired"`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.httpStatus != 0 {
					w.WriteHeader(tt.httpStatus)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(server.Close)
			resp, err := newTestEasyPay(t, server.URL).QueryOrder(context.Background(), "order-123")
			if tt.wantErr {
				if err == nil || resp != nil {
					t.Fatalf("QueryOrder = %+v, %v; want nil response and error", resp, err)
				}
				return
			}
			if err != nil || resp == nil || resp.Status != tt.want {
				t.Fatalf("QueryOrder = %+v, %v; want status %s", resp, err, tt.want)
			}
			if resp.Metadata["pid"] != "pid-1" {
				t.Fatal("merchant metadata was lost")
			}
		})
	}
}

func TestEasyPayQueryOrderTerminalTransportError(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint := server.URL
	server.Close()
	resp, err := newTestEasyPay(t, endpoint).QueryOrder(context.Background(), "order-123")
	if err == nil || resp != nil {
		t.Fatalf("QueryOrder = %+v, %v; want transport error", resp, err)
	}
	if strings.Contains(err.Error(), "pkey-1") || strings.Contains(err.Error(), "key=") {
		t.Fatal("transport error leaked merchant credentials")
	}
}

func TestEasyPayQueryOrderTerminalPendingEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		verified   bool
	}{
		{"legacy zero", `{"code":1,"status":0}`, true},
		{"legacy string zero", `{"code":1,"status":"0"}`, true},
		{"explicit pending", `{"code":1,"order_status":"pending"}`, true},
		{"waiting", `{"code":1,"trade_status":"WAITING"}`, true},
		{"waiting buyer", `{"code":1,"trade_status":"WAIT_BUYER_PAY","status":0}`, true},
		{"matching nested", `{"code":1,"status":0,"data":{"order_status":"pending","status":"0"}}`, true},
		{"null reason", `{"code":1,"status":0,"expire_reason":null}`, true},
		{"empty reason", `{"code":1,"status":0,"expire_reason":""}`, true},
		{"null fields and nested pending", `{"code":1,"status":null,"data":{"order_status":"pending"}}`, true},
		{"empty", `{"code":1}`, false},
		{"null", `null`, false},
		{"null state", `{"code":1,"status":null}`, false},
		{"unknown legacy", `{"code":1,"status":2}`, false},
		{"unknown extension", `{"code":1,"status":0,"order_status":"unknown"}`, false},
		{"unknown nested", `{"code":1,"status":0,"data":{"order_status":"unknown"}}`, false},
		{"unknown trade", `{"code":1,"trade_status":"UNKNOWN","status":0}`, false},
		{"empty trade", `{"code":1,"trade_status":"","status":0}`, false},
		{"nested trade conflict", `{"code":1,"trade_status":"WAITING","data":{"trade_status":"UNKNOWN"}}`, false},
		{"nested numeric conflict", `{"code":1,"status":0,"data":{"status":1}}`, false},
		{"nested terminal conflict", `{"code":1,"order_status":"pending","data":{"order_status":"expired"}}`, false},
		{"reason without terminal", `{"code":1,"status":0,"expire_reason":"cancelled"}`, false},
		{"unknown reason", `{"code":1,"status":0,"expire_reason":"unknown"}`, false},
		{"malformed reason", `{"code":1,"status":0,"expire_reason":{}}`, false},
		{"malformed status", `{"code":1,"status":false,"order_status":"pending"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			resp, err := newTestEasyPay(t, server.URL).QueryOrder(context.Background(), "order-123")
			if err != nil || resp == nil {
				t.Fatalf("QueryOrder = %+v, %v", resp, err)
			}
			if resp.Status != payment.ProviderStatusPending || resp.PendingVerified != tc.verified {
				t.Fatalf("QueryOrder = %+v; want pending with PendingVerified=%t", resp, tc.verified)
			}
		})
	}
}

func TestEasyPayQueryOrderTerminalDiagnosticsDoNotLeakCredentials(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"code":0,"msg":"key=pkey-1","status":0}`,
		`{"code":0,"key":"pkey-1"}`,
		`{"code":1,"out_trade_no":"key=pkey-1","status":0}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(server.Close)
			resp, err := newTestEasyPay(t, server.URL).QueryOrder(context.Background(), "order-123")
			if err == nil || resp != nil {
				t.Fatal("expected query failure")
			}
			if strings.Contains(err.Error(), "pkey-1") {
				t.Fatal("query error leaked credentials")
			}
		})
	}
}
