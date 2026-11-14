package payment

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// PaymentInfo is a non-standard, optional gateway presentation extension.
// ActualAmount must never participate in settlement, notification or refunds.
type PaymentInfo struct {
	Version         int    `json:"version"`
	QRContent       string `json:"qr_content,omitempty"`
	QRImageURL      string `json:"qr_image_url,omitempty"`
	ActualAmount    string `json:"actual_amount,omitempty"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	RequiresCashier bool   `json:"requires_cashier"`
}

var paymentInfoAmountPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]{0,11})(?:\.[0-9]{1,2})?$`)

// ParsePaymentInfo preserves absence separately from a malformed/null extension.
func ParsePaymentInfo(raw json.RawMessage) *PaymentInfo {
	if len(raw) == 0 {
		return nil
	}
	invalid := &PaymentInfo{Version: 1, RequiresCashier: true}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return invalid
	}
	var info PaymentInfo
	if !decodePaymentInfoField(fields, "version", &info.Version, true) || info.Version != 1 ||
		!decodePaymentInfoField(fields, "actual_amount", &info.ActualAmount, true) ||
		!decodePaymentInfoField(fields, "requires_cashier", &info.RequiresCashier, false) ||
		!decodePaymentInfoField(fields, "expires_at", &info.ExpiresAt, false) ||
		!decodePaymentInfoField(fields, "qr_content", &info.QRContent, false) {
		return invalid
	}
	// Only the selected QR representation is interpreted, including its type.
	if info.QRContent == "" && !decodePaymentInfoField(fields, "qr_image_url", &info.QRImageURL, false) {
		return &PaymentInfo{Version: 1, RequiresCashier: true}
	}
	if _, present := fields["expires_at"]; present && info.ExpiresAt == "" {
		return invalid
	}
	return &info
}

func decodePaymentInfoField(fields map[string]json.RawMessage, name string, dst any, required bool) bool {
	raw, present := fields[name]
	if !present {
		return !required
	}
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, dst) == nil
}

// SafePaymentHTTPURL validates an absolute browser URL without fetching it.
// In particular, a URL alone cannot prove that an image endpoint serves an image.
func SafePaymentHTTPURL(raw string) string {
	if raw == "" || !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\") || strings.IndexFunc(raw, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	}) >= 0 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return ""
	}
	decoded, err := url.PathUnescape(raw)
	if err != nil || !utf8.ValidString(decoded) || strings.Contains(decoded, "\\") || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
		return ""
	}
	return raw
}

// NormalizePaymentInfo returns a copy and the only permitted cashier URL.
// A zero orderExpiry is used by the provider before the local order is available.
// QR expiration only changes presentation; it never changes an order's status.
func NormalizePaymentInfo(info *PaymentInfo, cashier string, orderExpiry, now time.Time) (*PaymentInfo, string) {
	if info == nil {
		return nil, cashier
	}
	cashier = SafePaymentHTTPURL(cashier)
	out := *info
	valid := out.Version == 1
	out.Version = 1
	if !valid || !validPaymentInfoAmount(out.ActualAmount) {
		out.ActualAmount = ""
		valid = false
	}
	if out.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339Nano, out.ExpiresAt)
		if err != nil {
			out.ExpiresAt = ""
			valid = false
		} else {
			if !orderExpiry.IsZero() && orderExpiry.Before(expiry) {
				expiry = orderExpiry
			}
			out.ExpiresAt = expiry.Format(time.RFC3339Nano)
			valid = valid && now.Before(expiry)
		}
	} else if !orderExpiry.IsZero() {
		out.ExpiresAt = orderExpiry.Format(time.RFC3339Nano)
		valid = valid && now.Before(orderExpiry)
	}
	if out.QRContent != "" {
		valid = valid && validPaymentQRContent(out.QRContent)
		out.QRImageURL = "" // Raw content always takes precedence over an image.
	} else if out.QRImageURL != "" {
		out.QRImageURL = SafePaymentHTTPURL(out.QRImageURL)
		valid = valid && out.QRImageURL != ""
	} else {
		valid = false
	}
	if out.RequiresCashier || !valid || cashier == "" {
		out.RequiresCashier = true
		out.QRContent = ""
		out.QRImageURL = ""
	}
	return &out, cashier
}

func validPaymentInfoAmount(amount string) bool {
	return paymentInfoAmountPattern.MatchString(amount) && strings.ContainsAny(amount, "123456789")
}

func validPaymentQRContent(content string) bool {
	if !utf8.ValidString(content) || strings.IndexFunc(content, unicode.IsControl) >= 0 || strings.TrimSpace(content) == "" {
		return false
	}
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "<") {
		return false
	}
	if i := strings.IndexByte(trimmed, ':'); i >= 0 {
		switch strings.ToLower(trimmed[:i]) {
		case "javascript", "vbscript", "data", "file", "blob":
			return false
		}
	}
	return true
}
