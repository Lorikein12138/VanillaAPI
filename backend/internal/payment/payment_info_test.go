package payment

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPaymentInfoAmount(t *testing.T) {
	for _, amount := range []string{"1", "0.01", "0.1", "1.00", "999999999999.99"} {
		t.Run("valid_"+amount, func(t *testing.T) { require.True(t, validPaymentInfoAmount(amount)) })
	}
	for _, amount := range []string{"", "0", "0.0", "0.00", "00.01", "01", "1000000000000", "1.001", ".01", "1.", "-1", "+1", "1e2", "1,00", " 1.00", "1.00\n", "NaN", "Inf"} {
		t.Run("invalid_"+amount, func(t *testing.T) { require.False(t, validPaymentInfoAmount(amount)) })
	}
}

func TestPaymentInfoParse(t *testing.T) {
	require.Nil(t, ParsePaymentInfo(nil))
	for _, raw := range []string{
		`null`, `[]`, `true`, `"code"`, `{}`, `{"version":2}`, `{"version":"1"}`,
		`{"version":1,"actual_amount":1.01}`, `{"version":1,"actual_amount":null}`,
		`{"version":1,"actual_amount":"1.01","requires_cashier":null}`,
		`{"version":1,"actual_amount":"1.01","requires_cashier":"false"}`,
		`{"version":1,"actual_amount":"1.01","expires_at":null}`,
		`{"version":1,"actual_amount":"1.01","expires_at":0}`,
		`{"version":1,"actual_amount":"1.01","expires_at":""}`,
		`{"version":1,"actual_amount":"1.01","qr_content":null}`,
		`{"version":1,"actual_amount":"1.01","qr_content":1}`,
		`{"version":1,"actual_amount":"1.01","qr_image_url":null}`,
		`{"version":1,"actual_amount":"1.01","qr_image_url":{}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			info := ParsePaymentInfo(json.RawMessage(raw))
			require.NotNil(t, info)
			require.True(t, info.RequiresCashier)
			require.Empty(t, info.QRContent)
			require.Empty(t, info.QRImageURL)
		})
	}
	for _, ignoredImage := range []string{`null`, `1`, `{}`, `"javascript:bad"`} {
		t.Run("content_ignores_image_"+ignoredImage, func(t *testing.T) {
			info := ParsePaymentInfo(json.RawMessage(`{"version":1,"actual_amount":"1.01","qr_content":"/opaque raw","qr_image_url":` + ignoredImage + `}`))
			require.False(t, info.RequiresCashier)
			require.Equal(t, "/opaque raw", info.QRContent)
			require.Empty(t, info.QRImageURL)
		})
	}
}

func TestPaymentInfoSafeHTTPURL(t *testing.T) {
	for _, raw := range []string{"https://gateway.example/pay/o", "http://127.0.0.1/pay/o?x=1", "https://gateway.example/image%20one.png", "https://[::1]:8080/pay"} {
		t.Run("valid_"+raw, func(t *testing.T) { require.Equal(t, raw, SafePaymentHTTPURL(raw)) })
	}
	for _, raw := range []string{"", "/pay/order", "//gateway.example/pay", "javascript:alert(1)", "data:image/png;base64,x", "file:///tmp/qr", "https://user:pass@gateway.example/pay", "https://user@gateway.example/pay", "https://gateway.example/\x00pay", "https://gateway.example/\npay", "https://gateway.example/%0apay", "https://gateway.example/?q=%00", "https://gateway.example/%5cpay", "https://gateway.example/\\pay", "https://gateway.example/%ff", "https://gateway.example/%zz", " https://gateway.example/pay", "https:///pay"} {
		t.Run("invalid_"+raw, func(t *testing.T) { require.Empty(t, SafePaymentHTTPURL(raw)) })
	}
}

func TestPaymentInfoNormalize(t *testing.T) {
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	base := PaymentInfo{Version: 1, ActualAmount: "1.01", QRContent: "  /raw opaque token  "}
	info, cashier := NormalizePaymentInfo(&base, "https://gateway.example/pay/order", expiry, now)
	require.False(t, info.RequiresCashier)
	require.Equal(t, base.QRContent, info.QRContent)
	require.Equal(t, expiry.Format(time.RFC3339), info.ExpiresAt)
	require.Equal(t, "https://gateway.example/pay/order", cashier)
	require.Empty(t, base.ExpiresAt, "normalization must not mutate the caller")

	for _, tc := range []struct {
		name   string
		mutate func(*PaymentInfo, *string)
	}{
		{"captcha", func(i *PaymentInfo, _ *string) { i.RequiresCashier = true }},
		{"unknown version", func(i *PaymentInfo, _ *string) { i.Version = 2 }},
		{"amount zero", func(i *PaymentInfo, _ *string) { i.ActualAmount = "0.00" }},
		{"missing QR", func(i *PaymentInfo, _ *string) { i.QRContent = "" }},
		{"expired QR", func(i *PaymentInfo, _ *string) { i.ExpiresAt = now.Format(time.RFC3339) }},
		{"invalid expiry", func(i *PaymentInfo, _ *string) { i.ExpiresAt = "tomorrow" }},
		{"unsafe content", func(i *PaymentInfo, _ *string) { i.QRContent = "javascript:bad" }},
		{"NUL content", func(i *PaymentInfo, _ *string) { i.QRContent = "qr\x00payload" }},
		{"HTML content", func(i *PaymentInfo, _ *string) { i.QRContent = "<html>captcha</html>" }},
		{"unsafe image", func(i *PaymentInfo, _ *string) { i.QRContent = ""; i.QRImageURL = "data:image/svg+xml,bad" }},
		{"missing cashier", func(_ *PaymentInfo, c *string) { *c = "" }},
		{"unsafe cashier", func(_ *PaymentInfo, c *string) { *c = "https://u:p@gateway.example/pay" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, c := base, "https://gateway.example/pay/order"
			tc.mutate(&i, &c)
			got, _ := NormalizePaymentInfo(&i, c, expiry, now)
			require.True(t, got.RequiresCashier)
			require.Empty(t, got.QRContent)
			require.Empty(t, got.QRImageURL)
		})
	}
	t.Run("HTTP image", func(t *testing.T) {
		i := base
		i.QRContent, i.QRImageURL = "", "http://gateway.example/image.png"
		got, _ := NormalizePaymentInfo(&i, cashier, expiry, now)
		require.False(t, got.RequiresCashier)
		require.Equal(t, i.QRImageURL, got.QRImageURL)
	})
	t.Run("deadline cannot exceed order", func(t *testing.T) {
		i := base
		i.ExpiresAt = expiry.Add(time.Hour).Format(time.RFC3339)
		got, _ := NormalizePaymentInfo(&i, cashier, expiry, now)
		require.Equal(t, expiry.Format(time.RFC3339), got.ExpiresAt)
		got, _ = NormalizePaymentInfo(got, cashier, expiry, expiry)
		require.True(t, got.RequiresCashier)
	})
}
