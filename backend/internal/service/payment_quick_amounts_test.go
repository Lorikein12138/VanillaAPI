package service

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

func TestNormalizePaymentQuickAmounts(t *testing.T) {
	tests := []struct {
		name    string
		input   []float64
		want    []float64
		wantErr bool
	}{
		{name: "empty stays non nil", input: []float64{}, want: []float64{}},
		{name: "sorts a copy and preserves decimal cents", input: []float64{100, 1.15, 0.29}, want: []float64{0.29, 1.15, 100}},
		{name: "minimum and maximum inclusive", input: []float64{99999999.99, 0.01}, want: []float64{0.01, 99999999.99}},
		{name: "twenty allowed", input: makeQuickAmountRange(1, 20), want: makeQuickAmountRange(1, 20)},
		{name: "zero rejected", input: []float64{0}, wantErr: true},
		{name: "negative rejected", input: []float64{-0.01}, wantErr: true},
		{name: "below minimum rejected", input: []float64{0.009}, wantErr: true},
		{name: "above maximum rejected", input: []float64{100000000}, wantErr: true},
		{name: "third decimal rejected", input: []float64{1.001}, wantErr: true},
		{name: "numeric duplicate rejected", input: []float64{10, 10.00}, wantErr: true},
		{name: "nan rejected", input: []float64{math.NaN()}, wantErr: true},
		{name: "positive infinity rejected", input: []float64{math.Inf(1)}, wantErr: true},
		{name: "negative infinity rejected", input: []float64{math.Inf(-1)}, wantErr: true},
		{name: "twenty one rejected", input: makeQuickAmountRange(1, 21), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inputBefore := make([]float64, len(tt.input))
			copy(inputBefore, tt.input)
			got, err := NormalizePaymentQuickAmounts(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NormalizePaymentQuickAmounts() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NormalizePaymentQuickAmounts() = %v, want %v", got, tt.want)
			}
			if len(got) == 0 && got == nil {
				t.Fatalf("empty result nil state = %v, want non-nil", got == nil)
			}
			if !reflect.DeepEqual(tt.input, inputBefore) {
				t.Fatalf("input mutated: got %v, want %v", tt.input, inputBefore)
			}
		})
	}
}

func TestDefaultPaymentQuickAmountsReturnsCopy(t *testing.T) {
	first := DefaultPaymentQuickAmounts()
	second := DefaultPaymentQuickAmounts()
	want := []float64{10, 20, 50, 100, 200, 500, 1000, 2000, 5000}
	if !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
		t.Fatalf("default values = %v and %v, want %v", first, second, want)
	}
	first[0] = 999
	if second[0] != 10 {
		t.Fatalf("mutating one default copy affected another: %v", second)
	}
}

func TestParsePaymentQuickAmounts(t *testing.T) {
	wantDefault := DefaultPaymentQuickAmounts()
	for _, raw := range []string{"", " \t\n", "null", "broken", "{}", "[1,\"2\"]", "[10,10.00]", "[0]", "[1.001]"} {
		t.Run("fallback_"+raw, func(t *testing.T) {
			got := parsePaymentQuickAmounts(raw)
			if !reflect.DeepEqual(got, wantDefault) {
				t.Fatalf("parsePaymentQuickAmounts(%q) = %v, want default %v", raw, got, wantDefault)
			}
		})
	}
	got := parsePaymentQuickAmounts("[100,0.29,1.15]")
	if !reflect.DeepEqual(got, []float64{0.29, 1.15, 100}) {
		t.Fatalf("valid stored list = %v", got)
	}
	empty := parsePaymentQuickAmounts("[]")
	if empty == nil || len(empty) != 0 {
		t.Fatalf("stored [] = %#v, want non-nil empty", empty)
	}
}

func TestFilterPaymentQuickAmountsForRecharge(t *testing.T) {
	stored := []float64{100, 5, 25.5}
	filtered := FilterPaymentQuickAmountsForRecharge(stored, 10, 50)
	if !reflect.DeepEqual(filtered, []float64{25.5}) {
		t.Fatalf("filtered values = %v, want [25.5]", filtered)
	}
	if !reflect.DeepEqual(stored, []float64{100, 5, 25.5}) {
		t.Fatalf("filter mutated stored values: %v", stored)
	}
	if got := FilterPaymentQuickAmountsForRecharge([]float64{10, 50}, 10, 50); !reflect.DeepEqual(got, []float64{10, 50}) {
		t.Fatalf("inclusive boundaries = %v, want [10 50]", got)
	}
	if got := FilterPaymentQuickAmountsForRecharge([]float64{10, 50}, 0, -1); !reflect.DeepEqual(got, []float64{10, 50}) {
		t.Fatalf("unbounded range = %v, want [10 50]", got)
	}
	if got := FilterPaymentQuickAmountsForRecharge([]float64{0.01}, 10, 0); got == nil || len(got) != 0 {
		t.Fatalf("fully filtered list = %#v, want non-nil empty", got)
	}
	if got := FilterPaymentQuickAmountsForRecharge([]float64{}, 0, 0); got == nil || len(got) != 0 {
		t.Fatalf("empty configured list = %#v, want non-nil empty", got)
	}
}

func TestPaymentQuickAmountsDoNotRestrictOrderInput(t *testing.T) {
	service := &PaymentService{}
	cfg := &PaymentConfig{MinAmount: 1, MaxAmount: 100, QuickAmounts: []float64{10, 20}}
	_, err := service.validateOrderInput(context.Background(), CreateOrderRequest{
		Amount: 17.25, OrderType: payment.OrderTypeBalance,
	}, cfg)
	if err != nil {
		t.Fatalf("unlisted amount should be accepted within configured limits: %v", err)
	}
	for _, amount := range []float64{0.5, 101} {
		_, err := service.validateOrderInput(context.Background(), CreateOrderRequest{
			Amount: amount, OrderType: payment.OrderTypeBalance,
		}, cfg)
		if err == nil {
			t.Fatalf("amount %v outside existing limits should be rejected", amount)
		}
	}
}

func TestPaymentQuickAmountsConfigUsesAndPersists(t *testing.T) {
	repo := &paymentConfigSettingRepoStub{values: map[string]string{SettingPaymentQuickAmounts: "[100,25.5]"}}
	svc := &PaymentConfigService{settingRepo: repo}
	cfg, err := svc.GetPaymentConfig(context.Background())
	if err != nil {
		t.Fatalf("GetPaymentConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg.QuickAmounts, []float64{25.5, 100}) {
		t.Fatalf("configured quick amounts = %v", cfg.QuickAmounts)
	}
	if !containsString(repo.requestedKeys, SettingPaymentQuickAmounts) {
		t.Fatalf("GetMultiple keys do not contain %q: %v", SettingPaymentQuickAmounts, repo.requestedKeys)
	}
	if repo.setCalls != 0 {
		t.Fatalf("GetPaymentConfig wrote settings %d times", repo.setCalls)
	}

	values := []float64{100, 0.29, 25.5}
	err = svc.UpdatePaymentConfig(context.Background(), UpdatePaymentConfigRequest{QuickAmounts: &values})
	if err != nil {
		t.Fatalf("UpdatePaymentConfig: %v", err)
	}
	if repo.values[SettingPaymentQuickAmounts] != "[0.29,25.5,100]" {
		t.Fatalf("stored quick amounts = %q", repo.values[SettingPaymentQuickAmounts])
	}
	if !reflect.DeepEqual(values, []float64{100, 0.29, 25.5}) {
		t.Fatalf("service mutated request: %v", values)
	}

	empty := []float64{}
	if err := svc.UpdatePaymentConfig(context.Background(), UpdatePaymentConfigRequest{QuickAmounts: &empty}); err != nil {
		t.Fatalf("persist empty list: %v", err)
	}
	if repo.values[SettingPaymentQuickAmounts] != "[]" {
		t.Fatalf("empty list stored as %q", repo.values[SettingPaymentQuickAmounts])
	}
}

func TestUpdatePaymentConfigRejectsInvalidQuickAmountsBeforeWrite(t *testing.T) {
	invalidLists := [][]float64{{0}, {1.001}, {10, 10.00}, makeQuickAmountRange(1, 21)}
	for _, values := range invalidLists {
		repo := &paymentConfigSettingRepoStub{}
		svc := &PaymentConfigService{settingRepo: repo}
		enabled := true
		err := svc.UpdatePaymentConfig(context.Background(), UpdatePaymentConfigRequest{
			Enabled: &enabled, QuickAmounts: &values,
		})
		if infraerrors.Reason(err) != "INVALID_PAYMENT_QUICK_AMOUNTS" {
			t.Fatalf("invalid quick amounts error reason = %q, err=%v", infraerrors.Reason(err), err)
		}
		if repo.setCalls != 0 {
			t.Fatalf("invalid quick amounts called SetMultiple %d times", repo.setCalls)
		}
	}
}

func TestUpdatePaymentConfigOmittedQuickAmountsArePreserved(t *testing.T) {
	repo := &paymentConfigSettingRepoStub{values: map[string]string{SettingPaymentQuickAmounts: "[25,50]"}}
	svc := &PaymentConfigService{settingRepo: repo}
	enabled := true
	if err := svc.UpdatePaymentConfig(context.Background(), UpdatePaymentConfigRequest{Enabled: &enabled}); err != nil {
		t.Fatalf("update other field: %v", err)
	}
	if _, exists := repo.updates[SettingPaymentQuickAmounts]; exists {
		t.Fatalf("omitted quick amounts were written: %v", repo.updates)
	}
	if repo.values[SettingPaymentQuickAmounts] != "[25,50]" {
		t.Fatalf("omitted quick amounts changed to %q", repo.values[SettingPaymentQuickAmounts])
	}
}

func TestGetPaymentConfigReturnsRepositoryError(t *testing.T) {
	wantErr := context.Canceled
	repo := &paymentConfigSettingRepoStub{getErr: wantErr}
	_, err := (&PaymentConfigService{settingRepo: repo}).GetPaymentConfig(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("GetPaymentConfig error = %v, want wrapped %v", err, wantErr)
	}
}

func TestUpdatePaymentConfigReturnsRepositoryWriteError(t *testing.T) {
	wantErr := context.DeadlineExceeded
	repo := &paymentConfigSettingRepoStub{setErr: wantErr}
	values := []float64{25, 50}
	err := (&PaymentConfigService{settingRepo: repo}).UpdatePaymentConfig(context.Background(), UpdatePaymentConfigRequest{
		QuickAmounts: &values,
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("UpdatePaymentConfig error = %v, want wrapped %v", err, wantErr)
	}
	if repo.setCalls != 1 {
		t.Fatalf("SetMultiple calls = %d, want 1", repo.setCalls)
	}
}

func makeQuickAmountRange(first, last int) []float64 {
	values := make([]float64, last-first+1)
	for i := range values {
		values[i] = float64(first + i)
	}
	return values
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
