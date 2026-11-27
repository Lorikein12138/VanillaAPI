package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"
)

type detectionAccountScopeKey struct{}

type detectionAccountScope struct {
	repo        AccountRepository
	selected    *Account
	fingerprint [sha256.Size]byte
	valid       bool
}

type detectionAccountQuotaWriter interface {
	IncrementDetectionQuotaUsed(context.Context, int64, float64) error
}

func withDetectionAccountScope(ctx context.Context, repo AccountRepository, account *Account) context.Context {
	if !IsChannelDetectionProbe(ctx) || account == nil {
		return ctx
	}
	fingerprint, err := detectionAccountRouteFingerprint(account)
	return context.WithValue(ctx, detectionAccountScopeKey{}, detectionAccountScope{
		repo: repo, selected: account, fingerprint: fingerprint, valid: err == nil,
	})
}

// Freeze the routing inputs before the upstream request is built. Quota
// and activity counters can change independently of these inputs.
func detectionAccountRouteFingerprint(account *Account) ([sha256.Size]byte, error) {
	route := struct {
		Platform             string
		Type                 string
		ParentAccountID      *int64
		QuotaDimension       string
		Credentials          map[string]any
		CustomBaseURL        string
		ProxyID              *int64
		ProxyURL             string
		OpenAIPassthrough    bool
		OpenAIModelFiltering bool
	}{
		Platform:             account.Platform,
		Type:                 account.Type,
		ParentAccountID:      account.ParentAccountID,
		QuotaDimension:       account.QuotaDimensionOrDefault(),
		Credentials:          account.Credentials,
		ProxyID:              account.ProxyID,
		OpenAIPassthrough:    account.IsOpenAIPassthroughEnabled(),
		OpenAIModelFiltering: account.IsOpenAIPassthroughModelFilterEnabled(),
	}
	if len(route.Credentials) == 0 {
		route.Credentials = nil
	}
	if account.IsCustomBaseURLEnabled() {
		route.CustomBaseURL = account.GetCustomBaseURL()
	}
	if account.ProxyID != nil && account.Proxy != nil {
		route.ProxyURL = account.Proxy.URL()
	}
	data, err := json.Marshal(route)
	return sha256.Sum256(data), err
}

// This read must come from the account repository. Selection may have used a
// cached account that predates a quota crossing, RPM change or group removal.
func (s detectionAccountScope) check(ctx context.Context, groupID, accountID int64) (*Account, error) {
	if !s.valid || accountID <= 0 || s.repo == nil || s.selected == nil || s.selected.ID != accountID {
		return nil, errDetectionAccount
	}
	fresh, err := s.repo.GetByID(ctx, accountID)
	if err != nil || fresh == nil || fresh.ID != accountID || fresh.Platform != s.selected.Platform ||
		fresh.Type != s.selected.Type || !fresh.IsSchedulable() || !slices.Contains(fresh.GroupIDs, groupID) {
		return nil, errDetectionAccount
	}
	fingerprint, err := detectionAccountRouteFingerprint(fresh)
	if err != nil || fingerprint != s.fingerprint {
		return nil, errDetectionAccount
	}
	// The normal RPM counter has no atomic probe reservation. Keep it untouched
	// and decline these accounts until such a reservation is available.
	if fresh.GetBaseRPM() > 0 {
		return nil, errDetectionAccount
	}
	if fresh.SupportsAccountQuota() && fresh.HasAnyQuotaLimit() {
		if _, ok := s.repo.(detectionAccountQuotaWriter); !ok {
			return nil, errDetectionAccount
		}
	}
	return fresh, nil
}

func detectionAccountForUsage(ctx context.Context, selected *Account) *Account {
	binding := detectionBindingFromContext(ctx)
	if binding == nil || selected == nil || binding.budget == nil {
		return nil
	}
	binding.budget.mu.Lock()
	defer binding.budget.mu.Unlock()
	if binding.account == nil || binding.account.ID != selected.ID {
		return nil
	}
	return binding.account
}

// Probe accounting updates the selected upstream account only. It does not
// enqueue normal usage, charge a user or trigger customer notifications.
func (s *GatewayService) RecordDetectionAccountUsage(ctx context.Context, account *Account, routing *APIKey, result *ForwardResult, fields ChannelUsageFields, pricingAt time.Time) {
	account = detectionAccountForUsage(ctx, account)
	if result == nil || account == nil {
		return
	}
	if s.deferredService != nil {
		s.deferredService.ScheduleLastUsedUpdate(account.ID)
	}
	if !account.SupportsAccountQuota() || !account.HasAnyQuotaLimit() {
		return
	}
	model := s.recordUsageBillingModel(ctx, routing, result, fields)
	cost, err := s.calculateTokenCostChecked(ctx, result, routing, model, 1, pricingAt)
	if err == nil && s.upstreamPriceFactorApplies(ctx, result, routing, model) {
		applyUpstreamPriceFactor(cost, result)
	}
	recordDetectionAccountQuota(ctx, s.accountRepo, account, cost, err)
}

func (s *OpenAIGatewayService) RecordDetectionAccountUsage(ctx context.Context, account *Account, routing *APIKey, result *OpenAIForwardResult, fields ChannelUsageFields, pricingAt time.Time) {
	account = detectionAccountForUsage(ctx, account)
	if result == nil || account == nil {
		return
	}
	if s.deferredService != nil {
		s.deferredService.ScheduleLastUsedUpdate(account.ID)
	}
	if s.rateLimitService != nil && account.Platform == PlatformOpenAI && !result.ClientDisconnect {
		s.rateLimitService.ResetOpenAI403Counter(ctx, account.ID)
	}
	if !account.SupportsAccountQuota() || !account.HasAnyQuotaLimit() {
		return
	}
	model := forwardResultBillingModel(result.Model, result.UpstreamModel)
	if result.BillingModel != "" {
		model = strings.TrimSpace(result.BillingModel)
	}
	if fields.BillingModelSource == BillingModelSourceChannelMapped && fields.ChannelMappedModel != "" && fields.ChannelMappedModel != fields.OriginalModel {
		model = fields.ChannelMappedModel
	}
	if fields.BillingModelSource == BillingModelSourceRequested && fields.OriginalModel != "" {
		model = fields.OriginalModel
	}
	models := s.filterCNProviderBillingModelCandidates(ctx, account, routing,
		usageBillingModelCandidates(model, result.BillingModel, fields.ChannelMappedModel, fields.OriginalModel, result.UpstreamModel, result.Model))
	cost, err := s.calculateOpenAIRecordUsageCost(ctx, result, routing, models, 1, 1, 1, 1,
		result.Usage.billingUsageTokens(), optionalStringValue(result.ServiceTier), openAILongContextBillingGate(account),
		openAISubscriptionLongContextExempt(account, result), pricingAt)
	recordDetectionAccountQuota(ctx, s.accountRepo, account, cost, err)
}

func recordDetectionAccountQuota(ctx context.Context, repo AccountRepository, account *Account, cost *CostBreakdown, pricingErr error) {
	if pricingErr != nil || cost == nil || cost.TotalCost < 0 || math.IsNaN(cost.TotalCost) || math.IsInf(cost.TotalCost, 0) {
		failDetectionAccounting(ctx, account.ID, "account_pricing")
		return
	}
	amount := cost.TotalCost * account.BillingRateMultiplier()
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		failDetectionAccounting(ctx, account.ID, "account_quota")
		return
	}
	if amount == 0 {
		return
	}
	writer, ok := repo.(detectionAccountQuotaWriter)
	if !ok {
		failDetectionAccounting(ctx, account.ID, "account_quota")
		return
	}
	accountingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := writer.IncrementDetectionQuotaUsed(accountingCtx, account.ID, amount); err != nil {
		failDetectionAccounting(ctx, account.ID, "account_quota")
	}
}

func failDetectionAccounting(ctx context.Context, accountID int64, category string) {
	if binding := detectionBindingFromContext(ctx); binding != nil && binding.budget != nil {
		binding.budget.mu.Lock()
		binding.budget.accountingFailed = true
		binding.budget.mu.Unlock()
	}
	slog.Warn("channel_detection.account_usage", "account_id", accountID, "category", category)
}
