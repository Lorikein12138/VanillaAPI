package service

import (
	"encoding/json"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
)

// PaymentOrderPaymentDetails is shared by authenticated and signed-resume DTOs.
// It must not be exposed by anonymous out_trade_no lookups.
type PaymentOrderPaymentDetails struct {
	PayURL      string               `json:"pay_url,omitempty"`
	QRCode      string               `json:"qr_code,omitempty"`
	QRCodeImg   string               `json:"qr_code_img,omitempty"`
	PaymentInfo *payment.PaymentInfo `json:"payment_info,omitempty"`
}

// PaymentOrderPaymentPresentation projects the stored snapshot without mutating
// the entity, recalculating money, extending deadlines or declaring a terminal.
func PaymentOrderPaymentPresentation(order *dbent.PaymentOrder) PaymentOrderPaymentDetails {
	return paymentOrderPaymentPresentationAt(order, time.Now())
}

func paymentOrderPaymentPresentationAt(order *dbent.PaymentOrder, now time.Time) PaymentOrderPaymentDetails {
	var details PaymentOrderPaymentDetails
	if order == nil {
		return details
	}
	value, present := order.ProviderSnapshot["payment_info"]
	if !present {
		if order.Status == OrderStatusPending {
			details.PayURL = psStringValue(order.PayURL)
			details.QRCode = psStringValue(order.QrCode)
			details.QRCodeImg = psStringValue(order.QrCodeImg)
		}
		return details
	}
	raw, err := json.Marshal(value)
	if err != nil {
		raw = json.RawMessage("null")
	}
	details.PaymentInfo, details.PayURL = payment.NormalizePaymentInfo(
		payment.ParsePaymentInfo(raw), psStringValue(order.PayURL), order.ExpiresAt, now,
	)
	if order.Status != OrderStatusPending {
		details.PayURL = ""
		details.PaymentInfo.RequiresCashier = true
		details.PaymentInfo.QRContent = ""
		details.PaymentInfo.QRImageURL = ""
	}
	return details
}
