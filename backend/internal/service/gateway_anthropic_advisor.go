package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	anthropicAdvisorBeta = "advisor-tool-2026-03-01"
	anthropicAdvisorType = "advisor_20260301"
	anthropicAdvisorKey  = "anthropic_advisor_response"
)

func hasAnthropicAdvisorBeta(headers http.Header) bool {
	for name, values := range headers {
		if strings.EqualFold(name, "anthropic-beta") {
			for _, value := range values {
				if containsBetaToken(value, anthropicAdvisorBeta) {
					return true
				}
			}
		}
	}
	return false
}

func retainAnthropicAdvisorBeta(header string) string {
	if header == "" {
		return anthropicAdvisorBeta
	}
	if containsBetaToken(header, anthropicAdvisorBeta) {
		return header
	}
	return header + "," + anthropicAdvisorBeta
}

func restoreAnthropicAdvisorBeta(incoming, outgoing http.Header) {
	if !hasAnthropicAdvisorBeta(incoming) || hasAnthropicAdvisorBeta(outgoing) {
		return
	}
	for name, values := range outgoing {
		if strings.EqualFold(name, "anthropic-beta") && len(values) > 0 {
			values[0] = retainAnthropicAdvisorBeta(values[0])
			return
		}
	}
	setHeaderRaw(outgoing, "anthropic-beta", anthropicAdvisorBeta)
}

type anthropicAdvisorUsage struct {
	model string
	usage ClaudeUsage
}

type anthropicAdvisorResponse struct {
	requestedModel string
	iterations     []anthropicAdvisorUsage
	sawIterations  bool
	billingIDOnce  sync.Once
	billingID      string
}

func beginAnthropicAdvisorResponse(c *gin.Context, account *Account, body []byte) *anthropicAdvisorResponse {
	if c == nil {
		return nil
	}
	var advisor *anthropicAdvisorResponse
	if account != nil && !account.IsBedrock() &&
		account.Type != AccountTypeServiceAccount && c.Request != nil && hasAnthropicAdvisorBeta(c.Request.Header) {
		tool := gjson.GetBytes(body, `tools.#(type=="advisor_20260301")`)
		if model := tool.Get("model").String(); model != "" {
			advisor = &anthropicAdvisorResponse{requestedModel: model}
		}
	}
	c.Set(anthropicAdvisorKey, advisor)
	return advisor
}

func anthropicAdvisorResponseFromContext(c *gin.Context) *anthropicAdvisorResponse {
	if c == nil {
		return nil
	}
	value, _ := c.Get(anthropicAdvisorKey)
	advisor, _ := value.(*anthropicAdvisorResponse)
	return advisor
}

func (a *anthropicAdvisorResponse) observe(iterations gjson.Result) {
	if a == nil || !iterations.IsArray() {
		return
	}
	a.sawIterations = true
	a.iterations = nil
	iterations.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "advisor_message" {
			usage := ClaudeUsage{
				InputTokens:              int(item.Get("input_tokens").Int()),
				OutputTokens:             int(item.Get("output_tokens").Int()),
				CacheCreationInputTokens: int(item.Get("cache_creation_input_tokens").Int()),
				CacheReadInputTokens:     int(item.Get("cache_read_input_tokens").Int()),
				CacheCreation5mTokens:    int(item.Get("cache_creation.ephemeral_5m_input_tokens").Int()),
				CacheCreation1hTokens:    int(item.Get("cache_creation.ephemeral_1h_input_tokens").Int()),
			}
			if usage.CacheCreationInputTokens == 0 {
				usage.CacheCreationInputTokens = usage.CacheCreation5mTokens + usage.CacheCreation1hTokens
			}
			model := strings.Clone(item.Get("model").String())
			a.iterations = append(a.iterations, anthropicAdvisorUsage{model: model, usage: usage})
		}
		return true
	})
}

func (a *anthropicAdvisorResponse) disconnectedBeforeIterations() bool {
	return a != nil && !a.sawIterations
}

func (a *anthropicAdvisorResponse) resolveBillingID(ctx context.Context, upstreamID string) string {
	a.billingIDOnce.Do(func() {
		a.billingID = resolveUsageBillingRequestID(ctx, upstreamID)
	})
	return a.billingID
}

func isAnthropicAdvisorBlock(block gjson.Result) bool {
	switch block.Get("type").String() {
	case "server_tool_use":
		return block.Get("name").String() == "advisor"
	case "advisor_tool_result":
		return true
	default:
		return false
	}
}

// Restore the existing client tool names outside advisor-owned JSON spans.
// The same code handles a buffered JSON body and a framed SSE data line.
func (a *anthropicAdvisorResponse) restore(c *gin.Context, data []byte, payload string) []byte {
	if a == nil {
		return reverseToolNamesIfPresent(c, data)
	}
	offset := bytes.Index(data, []byte(payload))
	if payload == "" || offset < 0 {
		return reverseToolNamesIfPresent(c, data)
	}
	parsed := gjson.Parse(payload)
	var protected []gjson.Result
	if block := parsed.Get("content_block"); isAnthropicAdvisorBlock(block) {
		protected = append(protected, block)
	}
	parsed.Get("content").ForEach(func(_, block gjson.Result) bool {
		if isAnthropicAdvisorBlock(block) {
			protected = append(protected, block)
		}
		return true
	})
	iterations := parsed.Get("usage.iterations")
	if iterations.IsArray() {
		protected = append(protected, iterations)
	}
	if len(protected) == 0 {
		return reverseToolNamesIfPresent(c, data)
	}
	slices.SortFunc(protected, func(left, right gjson.Result) int { return left.Index - right.Index })
	var out bytes.Buffer
	pos := 0
	for _, span := range protected {
		start := offset + span.Index
		_, _ = out.Write(reverseToolNamesIfPresent(c, data[pos:start]))
		if span.IsArray() {
			_, _ = out.WriteString(a.restoreIterationModels(span))
		} else {
			_, _ = out.WriteString(span.Raw)
		}
		pos = start + len(span.Raw)
	}
	_, _ = out.Write(reverseToolNamesIfPresent(c, data[pos:]))
	return out.Bytes()
}

func (a *anthropicAdvisorResponse) restoreIterationModels(iterations gjson.Result) string {
	raw := iterations.Raw
	index := 0
	iterations.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "advisor_message" && item.Get("model").String() != a.requestedModel {
			if next, err := sjson.Set(raw, fmt.Sprintf("%d.model", index), a.requestedModel); err == nil {
				raw = next
			}
		}
		index++
		return true
	})
	return raw
}

// The raw relay has no tool-name rewrites. Only an advisor model alias can
// change its response bytes.
func (a *anthropicAdvisorResponse) restoreRelay(data []byte, payload string, iterations gjson.Result) []byte {
	if a == nil || !iterations.IsArray() {
		return data
	}
	raw := a.restoreIterationModels(iterations)
	if raw == iterations.Raw {
		return data
	}
	offset := bytes.Index(data, []byte(payload))
	if offset < 0 {
		return data
	}
	start := offset + iterations.Index
	return slices.Concat(data[:start], []byte(raw), data[start+len(iterations.Raw):])
}

func (s *GatewayService) recordAdvisorUsages(ctx context.Context, input *recordUsageCoreInput, parent *UsageLog, parentPricingModel string, pricingAt time.Time, params postUsageBillingParams, logOnly bool) error {
	if !logOnly && s.usageBillingRepo == nil {
		return errors.New("advisor billing requires the usage billing repository for deduplication")
	}
	advisor := input.Result.advisor
	var errs []error
	segmentInput := *input
	segmentInput.ModelMappingChain = ""
	for i, iteration := range advisor.iterations {
		requestID := fmt.Sprintf("%s:advisor:%d", parent.RequestID, i+1)
		model := iteration.model
		if model == "" {
			model = advisor.requestedModel
		}
		pricingModel := model
		if input.BillingModelSource == BillingModelSourceRequested || input.BillingModelSource == BillingModelSourceChannelMapped {
			pricingModel = advisor.requestedModel
		}
		tokens := claudeBillingTokens(iteration.usage)
		cost, err := s.calculateAdvisorTokenCost(ctx, input.APIKey, pricingModel, tokens, parent.RateMultiplier, pricingAt, true)
		if errors.Is(err, ErrModelPricingUnavailable) {
			slog.Warn("billing.advisor_price_fallback", "advisor_model", pricingModel, "main_model", parentPricingModel, "request_id", requestID)
			cost, err = s.calculateAdvisorTokenCost(ctx, input.APIKey, parentPricingModel, tokens, parent.RateMultiplier, pricingAt, false)
			if err == nil && cost.TotalCost <= 0 && iteration.usage.hasObservedTokens() {
				err = fmt.Errorf("%w for parent model: %s", ErrModelPricingUnavailable, parentPricingModel)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("bill advisor iteration %d: %w", i+1, err))
			continue
		}
		segment := ForwardResult{
			Usage:                 iteration.usage,
			Model:                 model,
			UpstreamModel:         model,
			UpstreamResponseModel: iteration.model,
			UpstreamHeaders:       input.Result.UpstreamHeaders,
			Stream:                input.Result.Stream,
			Duration:              input.Result.Duration,
		}
		log := s.buildRecordUsageLog(&segmentInput, &segment, requestID, input.APIKey, input.User, input.Account,
			input.Subscription, advisor.requestedModel, parent.RateMultiplier, parent.RateMultiplier,
			params.AccountRateMultiplier, parent.BillingType, false, cost)
		if input.APIKey.GroupID != nil {
			applyAccountStatsCost(ctx, log, s.channelService, s.billingService, input.Account.ID, *input.APIKey.GroupID,
				model, advisor.requestedModel, tokens, cost.TotalCost, pricingAt, accountStatsLongContextPricingEnabled(nil))
		}
		params.Cost = cost
		if err := s.settleRecordUsage(ctx, log, &params, logOnly, false); err != nil {
			errs = append(errs, fmt.Errorf("settle advisor iteration %d: %w", i+1, err))
		}
	}
	return errors.Join(errs...)
}

func (s *GatewayService) calculateAdvisorTokenCost(ctx context.Context, apiKey *APIKey, model string, tokens UsageTokens, multiplier float64, pricingAt time.Time, requireIdentified bool) (*CostBreakdown, error) {
	var resolved *ResolvedPricing
	if s.resolver != nil && apiKey.Group != nil {
		gid := apiKey.Group.ID
		resolved = s.resolver.Resolve(ctx, PricingInput{Model: model, GroupID: &gid, Group: apiKey.Group})
		if resolved.Mode != BillingModeToken {
			catalog := s.resolver.Resolve(ctx, PricingInput{Model: model})
			catalog.longContextPricingEnabled = resolved.longContextPricingEnabled
			resolved = catalog
		}
	}
	configured := resolved != nil && (resolved.Source == PricingSourceGroup || resolved.Source == PricingSourceChannel)
	if requireIdentified && !configured && !s.billingService.HasIdentifiedTokenPricing(model) {
		return nil, ErrModelPricingUnavailable
	}
	return s.billingService.CalculateTokenCostForRequest(TokenCostRequest{
		Ctx: ctx, Model: model, Group: apiKey.Group, Tokens: tokens, RateMultiplier: multiplier,
		PricingAt: pricingAt, Resolver: s.resolver, Resolved: resolved,
	})
}
