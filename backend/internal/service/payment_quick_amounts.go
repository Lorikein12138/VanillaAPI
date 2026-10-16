package service

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

const (
	maxPaymentQuickAmounts          = 20
	minPaymentQuickAmount           = 0.01
	maxPaymentQuickAmount           = 99999999.99
	paymentQuickAmountDecimalPlaces = 2
)

var defaultPaymentQuickAmounts = [...]float64{10, 20, 50, 100, 200, 500, 1000, 2000, 5000}

// DefaultPaymentQuickAmounts returns a fresh copy of the original recharge shortcuts.
func DefaultPaymentQuickAmounts() []float64 {
	return append([]float64(nil), defaultPaymentQuickAmounts[:]...)
}

// NormalizePaymentQuickAmounts validates, copies, and sorts a configured shortcut list.
func NormalizePaymentQuickAmounts(raw []float64) ([]float64, error) {
	if len(raw) > maxPaymentQuickAmounts {
		return nil, fmt.Errorf("quick amount list must contain at most %d entries", maxPaymentQuickAmounts)
	}

	values := make([]float64, len(raw))
	copy(values, raw)
	centsSeen := make(map[int64]struct{}, len(values))
	for i, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("quick amount at index %d must be finite", i)
		}
		amount := decimal.NewFromFloat(value)
		if amount.LessThan(decimal.NewFromFloat(minPaymentQuickAmount)) || amount.GreaterThan(decimal.NewFromFloat(maxPaymentQuickAmount)) {
			return nil, fmt.Errorf("quick amount at index %d must be between %.2f and %.2f", i, minPaymentQuickAmount, maxPaymentQuickAmount)
		}
		if !amount.Truncate(paymentQuickAmountDecimalPlaces).Equal(amount) {
			return nil, fmt.Errorf("quick amount at index %d allows at most %d decimal places", i, paymentQuickAmountDecimalPlaces)
		}
		cents := amount.Shift(paymentQuickAmountDecimalPlaces).IntPart()
		if _, exists := centsSeen[cents]; exists {
			return nil, fmt.Errorf("quick amount at index %d duplicates another value", i)
		}
		centsSeen[cents] = struct{}{}
	}
	sort.Float64s(values)
	return values, nil
}

// parsePaymentQuickAmounts resolves stored JSON and falls back as a whole on corrupt data.
func parsePaymentQuickAmounts(raw string) []float64 {
	if strings.TrimSpace(raw) == "" {
		return DefaultPaymentQuickAmounts()
	}
	var values []float64
	if err := json.Unmarshal([]byte(raw), &values); err != nil || values == nil {
		return DefaultPaymentQuickAmounts()
	}
	normalized, err := NormalizePaymentQuickAmounts(values)
	if err != nil {
		return DefaultPaymentQuickAmounts()
	}
	return normalized
}

// FilterPaymentQuickAmountsForRecharge filters a copy against the global recharge range.
func FilterPaymentQuickAmountsForRecharge(raw []float64, min, max float64) []float64 {
	normalized, err := NormalizePaymentQuickAmounts(raw)
	if err != nil {
		return []float64{}
	}
	filtered := make([]float64, 0, len(normalized))
	for _, amount := range normalized {
		if min > 0 && amount < min {
			continue
		}
		if max > 0 && amount > max {
			continue
		}
		filtered = append(filtered, amount)
	}
	return filtered
}
