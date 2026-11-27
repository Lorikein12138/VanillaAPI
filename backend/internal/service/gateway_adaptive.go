package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/bits"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
)

const (
	adaptiveWindow            = AdaptiveHistoryRetention
	adaptiveHalfLife          = 6 * time.Hour
	adaptiveRecentWindow      = 15 * time.Minute
	adaptiveMaxSuccessLatency = 15 * time.Minute
	adaptiveCapacity          = 4096
	adaptiveSamples           = 16
	adaptiveMinimum           = 4
)

type adaptiveRoute struct {
	operation       string
	transport       string
	inputSize       int
	effort          string
	duration        string
	tier            string
	messagesFast    bool
	thinkingLevel   string
	thinkingEnabled bool
	protected       bool
	replayable      bool
	nonText         bool
}

type adaptiveRouteKey struct{}
type adaptiveIngressKey struct{}
type adaptiveNoExploreKey struct{}
type adaptiveProbeGroupKey struct{}
type adaptiveProbeModelKey struct{}

type adaptiveIngress struct {
	operation string
	transport string
	explored  atomic.Bool
	counted   atomic.Bool
	recovery  atomic.Pointer[adaptiveRecoveryLease]
	settings  *SettingService
	telemetry *adaptiveTelemetry
	fastOnce  sync.Once
	fast      *OpenAIFastPolicySettings
}

// WithGatewayAdaptiveIngress snapshots the cached setting for the request.
func WithGatewayAdaptiveIngress(ctx context.Context, settings *SettingService, operation, transport string) context.Context {
	if isChannelDetectionProbe(ctx) || settings == nil || !settings.IsGatewayAdaptiveSchedulingEnabled(ctx) || operation == "" {
		return ctx
	}
	ingress := &adaptiveIngress{operation: operation, transport: transport, settings: settings, telemetry: settings.adaptiveTelemetry}
	return withGatewayAdaptiveRoute(context.WithValue(ctx, adaptiveIngressKey{}, ingress), operation, transport, nil)
}

// SetGatewayAdaptiveInput refines the descriptor using the decoded request.
func SetGatewayAdaptiveInput(ctx context.Context, body []byte) context.Context {
	ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
	if ingress == nil {
		return ctx
	}
	return withGatewayAdaptiveRoute(ctx, ingress.operation, ingress.transport, body)
}

// WithGatewayAdaptiveUpstreamSession protects provider-native continuity.
func WithGatewayAdaptiveUpstreamSession(ctx context.Context, headers http.Header) context.Context {
	if adaptiveNativeSession(headers) {
		if shape, ok := ctx.Value(adaptiveWorkloadKey{}).(adaptiveChatShape); ok && shape.work != adaptiveWorkUnknown {
			shape.work |= adaptiveWorkState
			ctx = context.WithValue(ctx, adaptiveWorkloadKey{}, shape)
		}
	}
	if strings.TrimSpace(headers.Get(grokConversationIDHeader)) != "" {
		return protectAdaptiveSelection(ctx)
	}
	return ctx
}

// WithGatewayAdaptiveProbeGroup supplies the authenticated scheduling group.
func WithGatewayAdaptiveProbeGroup(ctx context.Context, groupID *int64) context.Context {
	if _, enabled := gatewayAdaptiveRoute(ctx); enabled && groupID != nil {
		return context.WithValue(ctx, adaptiveProbeGroupKey{}, *groupID)
	}
	return ctx
}

// WithGatewayAdaptiveRequestModel supplies the parsed model for the probe budget.
func WithGatewayAdaptiveRequestModel(ctx context.Context, model string) context.Context {
	model = strings.TrimSpace(model)
	if _, enabled := gatewayAdaptiveRoute(ctx); enabled && model != "" && len(model) <= 128 {
		return context.WithValue(ctx, adaptiveProbeModelKey{}, model)
	}
	return ctx
}

// withGatewayAdaptiveRoute records only routing characteristics. The request
// body is inspected in place and is never retained.
func withGatewayAdaptiveRoute(ctx context.Context, operation, transport string, body []byte) context.Context {
	payload := gjson.ParseBytes(body)
	if transport == "" {
		transport = "http"
		if payload.Get("stream").Bool() || strings.HasSuffix(operation, ":streamGenerateContent") {
			transport = "sse"
		}
	}
	if transport == "ws" {
		payload = openAIRequestPayloadView(body)
	}
	route := adaptiveRoute{
		operation:  operation,
		transport:  transport,
		inputSize:  bits.Len(uint(len(body)) >> 9),
		effort:     adaptiveEffort(payload),
		protected:  adaptiveContinuity(payload) || requestHasGrokEncryptedReasoning(body),
		tier:       adaptiveTier(payload.Get("service_tier").String()),
		replayable: adaptiveReplayable(operation, transport, payload),
		nonText:    payload.Get("tools.#(type==image_generation)").Exists() || payload.Get("modalities.#(==audio)").Exists(),
	}
	var durationOK bool
	route.duration, durationOK = adaptiveDurationBucket(payload.Get("duration"))
	if !durationOK {
		route.effort = "unknown"
	}
	switch {
	case strings.Contains(operation, "/*modelAction:"):
		route.thinkingLevel = geminiThinkingLevelFromBody(body)
	case strings.HasSuffix(operation, "/messages"):
		var thinking *antigravity.ThinkingConfig
		if value := payload.Get("thinking"); value.IsObject() {
			_ = json.Unmarshal([]byte(value.Raw), &thinking)
		}
		route.thinkingLevel = geminiThinkingLevelFromClaudeBody(body, thinking)
		route.thinkingEnabled = thinking != nil && (thinking.Type == "enabled" || thinking.Type == "adaptive")
	}
	if adaptiveStreamOperation(operation) && len(body) != 0 {
		shape := adaptiveChatWorkload(payload, false)
		if requestHasGrokEncryptedReasoning(body) && shape.work != adaptiveWorkUnknown {
			shape.work |= adaptiveWorkState
		}
		ctx = context.WithValue(ctx, adaptiveWorkloadKey{}, shape)
		ctx = withAdaptiveStreamEffort(ctx, payload, route.effort)
	}
	return WithGatewayAdaptiveRequestModel(context.WithValue(ctx, adaptiveRouteKey{}, route), payload.Get("model").String())
}

func adaptiveEffort(payload gjson.Result) string {
	// Only allow values with a known scheduling meaning.
	effort := payload.Get("reasoning.effort").String()
	if effort == "" {
		effort = payload.Get("reasoning_effort").String()
	}
	effort = adaptiveEffortToken(effort)
	for _, field := range []string{"thinking.budget_tokens", "generationConfig.thinkingConfig.thinkingBudget", "max_tokens", "max_output_tokens", "max_completion_tokens", "generationConfig.maxOutputTokens", "n"} {
		value := payload.Get(field)
		if value.Type == gjson.Number {
			effort += "/" + strconv.FormatInt(value.Int(), 10)
		} else {
			effort += "/"
		}
		if len(effort) > 96 {
			return "unknown"
		}
	}
	for _, field := range []string{"thinking.type", "output_config.effort", "generationConfig.thinkingConfig.thinkingLevel", "speed", "size", "quality"} {
		value := payload.Get(field).String()
		switch value {
		case "", "auto", "enabled", "disabled", "adaptive", "none", "minimal", "low", "medium", "high", "xhigh", "max", "fast", "standard", "hd", "1024x1024", "1024x1536", "1536x1024":
			effort += "/" + value
		default:
			return "unknown"
		}
	}
	if payload.Get("tools").Exists() {
		effort += "/tools"
	}
	return effort
}

func adaptiveEffortToken(value string) string {
	switch value {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return value
	default:
		return "custom"
	}
}

func adaptiveDurationBucket(value gjson.Result) (string, bool) {
	if !value.Exists() {
		return "", true
	}
	raw := strings.TrimSpace(value.String())
	if raw == "auto" || raw == "-1" || (value.Type == gjson.Number && value.Float() == -1) {
		return "auto", true
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || !(seconds >= 1 && seconds <= float64(^uint32(0))) {
		return "", false
	}
	return adaptiveVideoDurationBucket(uint32(seconds)), true
}

func adaptiveVideoDurationBucket(seconds uint32) string {
	return strconv.Itoa(bits.Len32(seconds))
}

// WithAdaptiveGrokVideoDuration supplies the parsed multipart video duration.
func WithAdaptiveGrokVideoDuration(ctx context.Context, seconds int) context.Context {
	route, ok := gatewayAdaptiveRoute(ctx)
	if !ok || seconds <= 0 || uint64(seconds) > uint64(^uint32(0)) {
		return ctx
	}
	route.duration = adaptiveVideoDurationBucket(uint32(seconds))
	return context.WithValue(ctx, adaptiveRouteKey{}, route)
}

func adaptiveTier(value string) string {
	if tier := normalizeOpenAIServiceTier(value); tier != nil {
		return *tier
	}
	switch strings.TrimSpace(value) {
	case "", "standard":
		return strings.TrimSpace(value)
	default:
		return "unknown"
	}
}

func adaptiveTieredOperation(operation string) bool {
	return strings.HasSuffix(operation, "/chat/completions") ||
		strings.HasSuffix(operation, "/messages") ||
		strings.HasSuffix(operation, "/responses") ||
		strings.HasSuffix(operation, "/responses/compact")
}

// WithAdaptiveMessagesFastMode records the beta header used by Messages-to-Responses.
func WithAdaptiveMessagesFastMode(ctx context.Context, betaHeader string) context.Context {
	route, ok := gatewayAdaptiveRoute(ctx)
	if !ok || !containsBetaToken(betaHeader, claude.BetaFastMode) {
		return ctx
	}
	route.messagesFast = true
	return context.WithValue(ctx, adaptiveRouteKey{}, route)
}

func adaptiveTierBucket(tier string) string {
	if tier == "default" {
		return ""
	}
	return tier
}

func adaptiveFastPolicySettings(ctx context.Context) (*OpenAIFastPolicySettings, bool) {
	if settings := openAIFastPolicySettingsFromContext(ctx); settings != nil {
		return settings, true
	}
	ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
	if ingress == nil || ingress.settings == nil {
		return nil, true
	}
	ingress.fastOnce.Do(func() {
		ingress.fast, _ = ingress.settings.GetOpenAIFastPolicySettings(ctx)
	})
	return ingress.fast, ingress.fast != nil
}

func adaptiveCandidateTier(ctx context.Context, route adaptiveRoute, account *Account, model string) string {
	if account.Platform != PlatformOpenAI || !adaptiveTieredOperation(route.operation) {
		return adaptiveTierBucket(route.tier)
	}
	if model == "" {
		return "unknown"
	}
	tier := route.tier
	if route.transport != "ws" {
		// Model-dispatched accounts can require an asynchronous catalog lookup.
		// Only an explicit protocol is safe to predict without that lookup.
		if account.routesByModel() && !isNativeUpstreamProtocol(account.GetAPIProtocol()) {
			return "unknown"
		}
		if strings.HasSuffix(route.operation, "/messages") {
			if resolveUpstreamProtocol(account, APIProtocolAnthropic, model, nil) != APIProtocolResponses {
				if tier != "" {
					return "unknown"
				}
				return ""
			}
			// AnthropicToResponses drops the raw field and uses the beta header.
			tier = ""
			if route.messagesFast {
				tier = OpenAIFastTierPriority
			}
		}
	}
	if forced := openAIGroupForcedServiceTier(ctx, account, model); forced != "" {
		tier = forced
	}
	if tier == "unknown" || tier == "standard" {
		return "unknown"
	}
	settings, ok := adaptiveFastPolicySettings(ctx)
	if !ok {
		return "unknown"
	}
	matchTier := tier
	if matchTier == "" {
		matchTier = OpenAIFastTierMissing
	}
	action, _ := evaluateOpenAIFastPolicyWithSettings(settings, openAIFastPolicyUserID(ctx), account, model, matchTier)
	switch {
	case action == OpenAIFastPolicyActionForcePriority:
		return OpenAIFastTierPriority
	case tier != "" && action == BetaPolicyActionBlock:
		return "unknown"
	case tier != "" && action == BetaPolicyActionFilter:
		return ""
	default:
		return adaptiveTierBucket(tier)
	}
}

func adaptiveObservationRoute(ctx context.Context, route adaptiveRoute, account *Account, model string, sent *string, observed string) (adaptiveRoute, bool) {
	if account == nil {
		return route, false
	}
	route.tier = adaptiveCandidateTier(ctx, route, account, model)
	if route.tier == "unknown" {
		return route, false
	}
	if account.Platform != PlatformOpenAI || !adaptiveTieredOperation(route.operation) {
		return route, true
	}
	outbound := adaptiveTierBucket(adaptiveTier(optionalStringValue(sent)))
	resolution := ResolveOpenAIServiceTierBilling(account, optionalStringValue(sent), observed)
	if outbound != route.tier || resolution.Downgraded {
		return route, false
	}
	return route, observed == "" || adaptiveTierBucket(adaptiveTier(observed)) == outbound ||
		(account.IsOpenAIOAuthLike() && codexOAuthResponseTierIsNonAuthoritative(observed))
}

func adaptiveContinuity(payload gjson.Result) bool {
	for _, field := range []string{"previous_response_id", "conversation", "cachedContent", "cached_content"} {
		if payload.Get(field).Exists() {
			return true
		}
	}
	for _, field := range []string{"input", "messages", "contents"} {
		items := payload.Get(field)
		var entries []gjson.Result
		if items.IsObject() {
			entries = []gjson.Result{items}
		} else {
			entries = items.Array()
		}
		for _, item := range entries {
			if item.Get("role").String() == "tool" || item.Get("tool_calls").Exists() ||
				item.Get("parts.#(functionResponse)").Exists() || item.Get("content.#(type==tool_result)").Exists() ||
				strings.HasSuffix(item.Get("type").String(), "_call_output") {
				return true
			}
		}
	}
	return false
}

func gatewayAdaptiveRoute(ctx context.Context) (adaptiveRoute, bool) {
	route, ok := ctx.Value(adaptiveRouteKey{}).(adaptiveRoute)
	if !ok || route.operation == "" || route.effort == "unknown" {
		return route, false
	}
	return route, true
}

type adaptiveSampleKey struct {
	route       adaptiveRoute
	accountID   int64
	platform    string
	accountType string
	model       string
	groupID     int64
	config      string
}

func adaptiveKey(route adaptiveRoute, account *Account, model string) adaptiveSampleKey {
	route.protected = false
	route.messagesFast = false
	route.replayable = false
	return adaptiveSampleKey{route: route, accountID: account.ID, platform: account.Platform, accountType: account.Type, model: model, config: adaptiveAccountConfig(account)}
}

func adaptiveContextKey(ctx context.Context, route adaptiveRoute, account *Account, model string) adaptiveSampleKey {
	key := adaptiveKey(route, account, model)
	key.groupID, _ = ctx.Value(adaptiveProbeGroupKey{}).(int64)
	return key
}

type adaptiveSample struct {
	at      time.Time
	latency time.Duration
	success bool
	probe   bool
	id      string
}

type adaptiveSeries struct {
	samples     [adaptiveSamples]adaptiveSample
	next        int
	health      adaptiveHealth
	successes   [10]adaptiveSample
	nextSuccess int
}

type adaptiveTelemetry struct {
	mu           sync.Mutex
	series       map[adaptiveSampleKey]*adaptiveSeries
	history      *adaptiveHistory
	exploration  adaptiveExploration
	recovery     adaptiveRecovery
	streamHealth adaptiveStreamHealth
}

var gatewayAdaptiveTelemetry adaptiveTelemetry

func adaptiveTelemetryForContext(ctx context.Context) *adaptiveTelemetry {
	if ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress); ingress != nil && ingress.telemetry != nil {
		return ingress.telemetry
	}
	return &gatewayAdaptiveTelemetry
}

func (t *adaptiveTelemetry) observe(key adaptiveSampleKey, now time.Time, latency time.Duration, success bool) {
	t.observeAttempt(key, now, latency, success, false)
}

func (t *adaptiveTelemetry) observeAttempt(key adaptiveSampleKey, now time.Time, latency time.Duration, success, probe bool) {
	if len(key.model) > 128 || (success && (latency <= 0 || latency > adaptiveMaxSuccessLatency)) {
		return
	}
	sample := adaptiveSample{at: now, latency: latency, success: success, probe: probe}
	if t.history != nil {
		sample.id = t.history.nextID()
	}
	t.insertSample(key, sample, true, probe)
	if t.history != nil {
		t.history.enqueue(key, sample)
	}
}

func (t *adaptiveTelemetry) addSample(key adaptiveSampleKey, sample adaptiveSample) {
	t.insertSample(key, sample, false, false)
}

func (t *adaptiveTelemetry) seriesForObservation(key adaptiveSampleKey, at time.Time) *adaptiveSeries {
	series := t.series[key]
	if series == nil {
		if t.series == nil {
			t.series = make(map[adaptiveSampleKey]*adaptiveSeries)
		}
		if len(t.series) >= adaptiveCapacity {
			var oldestKey adaptiveSampleKey
			var oldest time.Time
			for key, item := range t.series {
				last := item.health.Last
				if last.IsZero() {
					last = item.samples[(item.next+adaptiveSamples-1)%adaptiveSamples].at
				}
				if oldest.IsZero() || last.Before(oldest) {
					oldestKey, oldest = key, last
				}
			}
			if at.Before(oldest) {
				return nil
			}
			delete(t.series, oldestKey)
		}
		series = &adaptiveSeries{}
		t.series[key] = series
	}
	return series
}

func (t *adaptiveTelemetry) insertSample(key adaptiveSampleKey, sample adaptiveSample, updateHealth, probe bool) adaptiveHealth {
	t.mu.Lock()
	defer t.mu.Unlock()
	series := t.seriesForObservation(key, sample.at)
	if series == nil {
		return adaptiveHealth{}
	}
	if sample.id != "" {
		for _, existing := range series.samples {
			if existing.id == sample.id {
				return adaptiveHealth{}
			}
		}
	}
	if !insertAdaptiveSample(series.samples[:], &series.next, sample) {
		return adaptiveHealth{}
	}
	if sample.success {
		insertAdaptiveSample(series.successes[:], &series.nextSuccess, sample)
	}
	if updateHealth {
		series.updateHealth(key, sample, probe)
		return series.health
	}
	return adaptiveHealth{}
}

// Startup restore and live observations can overlap. Keep both rings ordered
// by observation time, including when older samples arrive after a full ring.
func insertAdaptiveSample(samples []adaptiveSample, next *int, sample adaptiveSample) bool {
	if old := samples[*next].at; !old.IsZero() && sample.at.Before(old) {
		return false
	}
	position := *next
	for range len(samples) - 1 {
		previous := (position + len(samples) - 1) % len(samples)
		if !sample.at.Before(samples[previous].at) {
			break
		}
		samples[position] = samples[previous]
		position = previous
	}
	samples[position] = sample
	*next = (*next + 1) % len(samples)
	return true
}

type adaptiveScore struct {
	reliability int
	latency     int
	known       bool
	recent      bool
	health      adaptiveHealth
	median      time.Duration
	comparable  int
	probe       bool
	regressed   bool
}

type adaptiveWeightedSamples struct {
	count                                            int
	weight, success, weightedLatency, weightedPoints float64
}

func (s *adaptiveWeightedSamples) add(sample adaptiveSample, weight float64) {
	s.count++
	s.weight += weight
	if sample.success {
		s.success += weight
		s.weightedLatency += weight * float64(sample.latency)
		s.weightedPoints += weight * float64(adaptiveTTFTPoints(sample.latency))
	}
}

func (s adaptiveWeightedSamples) score() adaptiveScore {
	if s.count < adaptiveMinimum || s.weight < 2 {
		return adaptiveScore{}
	}
	// Quantize at approximately 20% increments to avoid moving accounts
	// for minor jitter. Compare reliability before latency.
	latency := math.MaxInt
	if s.success > 0 {
		latency = 0
		mean := time.Duration(s.weightedLatency / s.success)
		for bucket := 100 * time.Millisecond; bucket < mean; bucket += bucket / 5 {
			latency++
		}
	}
	return adaptiveScore{reliability: int((s.success / s.weight) * 5), latency: latency, known: true}
}

func (t *adaptiveTelemetry) score(key adaptiveSampleKey, now time.Time) adaptiveScore {
	return scoreAdaptiveSeries(t.series[key], key.route, now)
}

func scoreAdaptiveSeries(series *adaptiveSeries, route adaptiveRoute, now time.Time) adaptiveScore {
	if series == nil {
		return adaptiveScore{}
	}
	score := scoreAdaptiveSamples(series.samples[:], route, now)
	if adaptiveTextStream(route) {
		_, score.probe, score.regressed, score.median, score.comparable = series.latencyHealth(now)
	}
	score.health = adaptiveHealthAt(series.health, now)
	return score
}

func scoreAdaptiveSamples(samples []adaptiveSample, route adaptiveRoute, now time.Time) adaptiveScore {
	var history, recent adaptiveWeightedSamples
	for _, sample := range samples {
		age := now.Sub(sample.at)
		if age < 0 || age >= adaptiveWindow {
			continue
		}
		weight := math.Exp2(-float64(age) / float64(adaptiveHalfLife))
		history.add(sample, weight)
		if age < adaptiveRecentWindow {
			recent.add(sample, weight)
		}
	}
	score := history.score()
	if adaptiveTextStream(route) {
		score.known = history.count >= adaptiveMinimum
		score.latency = math.MaxInt
		if history.success > 0 {
			score.latency = 100 - int(math.Round(history.weightedPoints/history.success))
			score.reliability = int(math.Round(history.success / history.weight * 10000))
		}
	} else if current := recent.score(); current.known {
		score = current
		score.recent = true
	}
	return score
}

func adaptiveHealthAt(health adaptiveHealth, now time.Time) adaptiveHealth {
	age := now.Sub(health.Last)
	if age >= adaptiveHealthRetention {
		return adaptiveHealth{}
	}
	if age >= adaptiveWindow {
		health.Fault = adaptiveHealthy
		health.Verify = true
	}
	return health
}

type adaptivePeerMatch func(*Account, *Account) bool

type adaptiveProbeOptions[T any] struct {
	scope        adaptiveProbeScope
	opportunity  func() bool
	canProbe     func(*Account) bool
	healthPeers  adaptivePeerMatch
	waitOrder    *[]T
	reserve      func(adaptiveSampleKey) bool
	immediate    bool
	streamChoice *adaptiveStreamChoice
	streamModel  string
	streamRank   func([]T, map[int64]adaptiveSampleKey) bool
}

// rankAdaptivePool expects a baseline order and an equivalence relation for
// fixed tiers. Unknown positions stay fixed, except during cold-start sampling.
func rankAdaptivePool[T any](ctx context.Context, pool []T, account func(T) *Account, model func(*Account) string, peers adaptivePeerMatch, explore bool) bool {
	changed, _ := rankAdaptivePoolWithProbe(ctx, pool, account, model, peers, explore, adaptiveProbeOptions[T]{})
	return changed
}

// probeBaseline is the index of the pre-probe winner in the returned pool.
// A zero index means that no cold-start probe was scheduled.
func rankAdaptiveImmediate[T any](ctx context.Context, pool []T, account func(T) *Account, model func(*Account) string, peers adaptivePeerMatch, explore bool) bool {
	changed, _ := rankAdaptivePoolWithProbe(ctx, pool, account, model, peers, explore, adaptiveProbeOptions[T]{immediate: true})
	return changed
}

func rankAdaptivePoolWithProbe[T any](ctx context.Context, pool []T, account func(T) *Account, model func(*Account) string, peers adaptivePeerMatch, explore bool, probe adaptiveProbeOptions[T]) (changed bool, probeBaseline int) {
	route, ok := gatewayAdaptiveRoute(ctx)
	if !ok || route.protected || len(pool) == 0 || ctx.Err() != nil {
		return false, 0
	}
	if eligible, _ := ctx.Value(adaptiveEligibilityKey{}).(func(*Account) bool); eligible != nil {
		previousPeers := peers
		peers = func(a, b *Account) bool { return eligible(a) && eligible(b) && previousPeers(a, b) }
		if healthPeers := probe.healthPeers; healthPeers != nil {
			probe.healthPeers = func(a, b *Account) bool { return eligible(a) && eligible(b) && healthPeers(a, b) }
		}
	}
	probe.scope.groupID, _ = ctx.Value(adaptiveProbeGroupKey{}).(int64)
	probe.scope.model, _ = ctx.Value(adaptiveProbeModelKey{}).(string)
	telemetry := adaptiveTelemetryForContext(ctx)
	now := time.Now()
	if probe.streamChoice != nil && ctx.Value(adaptiveStreamPermissionKey{}) != nil {
		now = telemetry.streamHealth.now()
	}
	var reserveStream func(adaptiveSampleKey, adaptiveStreamKey) bool
	if explore && ctx.Value(adaptiveNoExploreKey{}) == nil {
		if ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress); ingress != nil {
			probe.opportunity = func() bool { return ingress.explored.CompareAndSwap(false, true) }
			if probe.waitOrder != nil || probe.immediate {
				scope := probe.scope
				scope.route = route
				scope.route.replayable = false
				reserveStream = telemetry.recovery.opportunityWithStream(ctx, ingress, scope, now)
				if reserveStream != nil {
					probe.reserve = func(key adaptiveSampleKey) bool { return reserveStream(key, adaptiveStreamKey{}) }
				}
			}
		}
	}
	if probe.streamChoice != nil && ctx.Value(adaptiveStreamPermissionKey{}) != nil {
		probe.streamRank = func(ordered []T, oldKeys map[int64]adaptiveSampleKey) bool {
			var reserve func(adaptiveStreamKey) bool
			if reserveStream != nil {
				reserve = func(key adaptiveStreamKey) bool {
					oldKey, ok := oldKeys[key.sample.accountID]
					return ok && reserveStream(oldKey, key)
				}
			}
			*probe.streamChoice = chooseAdaptiveStreamAccount(ctx, ordered, account, peers, probe.canProbe, probe.streamModel, now, reserve)
			return probe.streamChoice.accountID != 0
		}
	}
	return rankAdaptiveWithTelemetry(telemetry, route, pool, account, model, peers, probe, now, func(candidate *Account, upstreamModel string) string {
		return adaptiveCandidateTier(ctx, route, candidate, upstreamModel)
	})
}

func withoutAdaptiveExploration(ctx context.Context) context.Context {
	if _, enabled := gatewayAdaptiveRoute(ctx); enabled && ctx.Value(adaptiveNoExploreKey{}) == nil {
		return context.WithValue(ctx, adaptiveNoExploreKey{}, true)
	}
	return ctx
}

func rankAdaptiveWithTelemetry[T any](telemetry *adaptiveTelemetry, route adaptiveRoute, pool []T, account func(T) *Account, model func(*Account) string, peers adaptivePeerMatch, probe adaptiveProbeOptions[T], now time.Time, candidateTier func(*Account, string) string) (bool, int) {
	keys := make(map[int64]adaptiveSampleKey, len(pool))
	for _, item := range pool {
		candidate := account(item)
		upstreamModel := model(candidate)
		candidateRoute := route
		if candidateTier != nil {
			candidateRoute.tier = candidateTier(candidate, upstreamModel)
		}
		if candidateRoute.tier != "unknown" {
			key := adaptiveKey(candidateRoute, candidate, upstreamModel)
			key.groupID = probe.scope.groupID
			keys[candidate.ID] = key
		}
	}
	sameTier := func(a, b *Account) bool {
		aKey, aOK := keys[a.ID]
		bKey, bOK := keys[b.ID]
		return aOK && bOK && aKey.route.tier == bKey.route.tier
	}
	sameTierPeers := func(a, b *Account) bool { return sameTier(a, b) && peers(a, b) }
	// A health-only peer can cross a ranking preference using the same actual model.
	healthOnlyPeers := func(a, b *Account) bool {
		return probe.healthPeers != nil && sameTier(a, b) && keys[a.ID].model != "" && keys[a.ID].model == keys[b.ID].model && probe.healthPeers(a, b)
	}
	scores := make(map[int64]adaptiveScore, len(pool))
	telemetry.mu.Lock()
	for id, key := range keys {
		scores[id] = telemetry.score(key, now)
	}
	telemetry.mu.Unlock()

	firstID := account(pool[0]).ID
	text := adaptiveTextStream(route)
	if text {
		rankAdaptivePriority(pool, account, sameTierPeers)
	}
	seen := make([]bool, len(pool))
	for i, item := range pool {
		if seen[i] || !scores[account(item).ID].known {
			continue
		}
		positions := []int{i}
		group := []T{item}
		for j := i + 1; j < len(pool); j++ {
			if !seen[j] && scores[account(pool[j]).ID].known && sameTierPeers(account(item), account(pool[j])) &&
				(!text || (account(item).Priority == account(pool[j]).Priority && account(item).BillingRateMultiplier() == account(pool[j]).BillingRateMultiplier())) {
				seen[j] = true
				positions = append(positions, j)
				group = append(group, pool[j])
			}
		}
		sort.SliceStable(group, func(i, j int) bool {
			a, b := scores[account(group[i]).ID], scores[account(group[j]).ID]
			if a.reliability != b.reliability {
				return a.reliability > b.reliability
			}
			return a.latency < b.latency
		})
		for j, position := range positions {
			pool[position] = group[j]
		}
	}
	for i := 0; i < len(pool); i++ {
		for j := i + 1; j < len(pool); j++ {
			current, candidate := account(pool[i]), account(pool[j])
			regularPeers := sameTierPeers(current, candidate)
			if !regularPeers && !healthOnlyPeers(current, candidate) {
				continue
			}
			a, b := scores[current.ID], scores[candidate.ID]
			sameModel := keys[current.ID].model == keys[candidate.ID].model
			if (adaptiveHealthPrefers(current, candidate, a, b, sameModel) &&
				(regularPeers || a.health.Fault == adaptiveUpstreamDegraded || adaptiveProvenFaster(a, b, sameModel))) ||
				(regularPeers && text && adaptiveSamePriorityPrefers(current, candidate, a, b, sameModel)) {
				pool[i], pool[j] = pool[j], pool[i]
			}
		}
	}
	if probe.streamRank != nil && probe.streamRank(pool, keys) {
		return true, 0
	}
	baseline := account(pool[0])
	if probe.reserve != nil {
		for i := 1; i < len(pool); i++ {
			candidate := account(pool[i])
			regularPeers := sameTierPeers(baseline, candidate)
			if (!regularPeers && !healthOnlyPeers(baseline, candidate)) || (probe.canProbe != nil && !probe.canProbe(candidate)) {
				continue
			}
			candidateScore := scores[candidate.ID]
			comparison := adaptiveComparisonProbe(scores[baseline.ID], candidateScore, keys[baseline.ID].model == keys[candidate.ID].model)
			retest := candidateScore.canRetest(now) && (regularPeers || candidateScore.health.Fault != adaptiveHealthy || candidateScore.health.Verify)
			if (!retest && !comparison) || !probe.reserve(keys[candidate.ID]) {
				continue
			}
			if probe.waitOrder != nil {
				*probe.waitOrder = slices.Clone(pool)
			}
			// Moving a probe preserves the order of every regular candidate.
			item := pool[i]
			copy(pool[1:i+1], pool[:i])
			pool[0] = item
			return true, 1
		}
		if scores[baseline.ID].canRetest(now) && (probe.canProbe == nil || probe.canProbe(baseline)) {
			probe.reserve(keys[baseline.ID])
		}
	}
	if probe.opportunity != nil && scores[baseline.ID].known && (probe.canProbe == nil || probe.canProbe(baseline)) {
		var members []adaptiveProbeMember
		for i := 1; i < len(pool); i++ {
			candidate := account(pool[i])
			if !scores[candidate.ID].known && scores[candidate.ID].health.Last.IsZero() && candidate.Priority == baseline.Priority && sameTierPeers(baseline, candidate) &&
				(probe.canProbe == nil || probe.canProbe(candidate)) {
				members = append(members, adaptiveProbeMember{id: candidate.ID, position: i})
			}
		}
		probe.scope.route = route
		probe.scope.route.replayable = false
		if pick := telemetry.exploration.pick(probe.scope, members, now, probe.opportunity); pick > 0 {
			if probe.waitOrder != nil {
				*probe.waitOrder = slices.Clone(pool)
			}
			item := pool[pick]
			// Rotate only through peer positions. Every unprobed peer keeps
			// its relative order and every other tier keeps its slot.
			for i := pick - 1; i > 0; i-- {
				candidate := account(pool[i])
				if candidate.Priority == baseline.Priority && sameTierPeers(baseline, candidate) {
					pool[pick], pick = pool[i], i
				}
			}
			pool[pick], pool[0] = pool[0], item
			slog.Debug("gateway.adaptive_selection", "reason", "exploration", "account_id", account(item).ID)
			return true, pick
		}
	}
	if firstID != account(pool[0]).ID {
		slog.Debug("gateway.adaptive_selection", "reason", "recent_observations", "account_id", account(pool[0]).ID)
		return true, 0
	}
	return false, 0
}

func rankAdaptivePriority[T any](pool []T, account func(T) *Account, peers adaptivePeerMatch) {
	seen := make([]bool, len(pool))
	for i, item := range pool {
		if seen[i] {
			continue
		}
		positions := []int{i}
		group := []T{item}
		for j := i + 1; j < len(pool); j++ {
			if !seen[j] && peers(account(item), account(pool[j])) {
				seen[j] = true
				positions = append(positions, j)
				group = append(group, pool[j])
			}
		}
		sort.SliceStable(group, func(i, j int) bool {
			return account(group[i]).Priority < account(group[j]).Priority
		})
		for j, position := range positions {
			pool[position] = group[j]
		}
	}
}

func adaptiveAccount(account *Account) *Account { return account }

func adaptiveMappedModel(ctx context.Context, model string) func(*Account) string {
	route, _ := gatewayAdaptiveRoute(ctx)
	if mapped, ok := ctx.Value(adaptiveChannelModelKey{}).(string); ok {
		model = mapped
	}
	return func(account *Account) string {
		switch {
		case account.Platform == PlatformAntigravity:
			mapped := resolveAntigravityMappedModelForThinkingLevel(account, model, route.thinkingLevel)
			return applyThinkingModelSuffix(mapped, route.thinkingEnabled)
		case account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey:
			if account.IsBedrock() {
				if mapped, ok := ResolveBedrockModelID(account, model); ok {
					return mapped
				}
			}
			mapped, _ := resolveAnthropicForwardModel(account, model)
			return mapped
		default:
			return resolveAccountUpstreamModel(account, model)
		}
	}
}

type adaptiveChannelModelKey struct{}

// WithAdaptiveChannelModel supplies the channel rewrite used by the forwarder.
// The original model still drives selection gates and routing rules.
func WithAdaptiveChannelModel(ctx context.Context, model string) context.Context {
	if _, enabled := gatewayAdaptiveRoute(ctx); !enabled || model == "" {
		return ctx
	}
	return context.WithValue(ctx, adaptiveChannelModelKey{}, model)
}

func adaptiveAnyPeer(*Account, *Account) bool { return true }

func adaptiveLoadedAccount(item accountWithLoad) *Account { return item.account }

type adaptiveLegacyProbeKey struct{}

func adaptiveGatewayWinner(ctx context.Context, baseline *Account, pool []*Account, model string, explore bool) *Account {
	mapped := adaptiveMappedModel(ctx, model)
	if owner, _ := ctx.Value(adaptiveSoftStickyKey{}).(*Account); owner != nil {
		trial := adaptiveStickyTrialContext(ctx, owner, mapped, adaptiveAnyPeer)
		if trial != nil {
			if adaptiveStickyTrialAllows(trial, baseline) {
				return baseline
			}
			for _, candidate := range pool {
				if adaptiveStickyTrialAllows(trial, candidate) {
					return candidate
				}
			}
		}
		return owner
	}
	if ctx.Value(adaptiveStickyTrialKey{}) != nil {
		if adaptiveStickyTrialAllows(ctx, baseline) {
			return baseline
		}
		for _, candidate := range pool {
			if adaptiveStickyTrialAllows(ctx, candidate) {
				return candidate
			}
		}
		return nil
	}
	if baseline == nil || len(pool) == 0 {
		return baseline
	}
	for i, candidate := range pool {
		if candidate.ID == baseline.ID {
			copy(pool[1:i+1], pool[:i])
			pool[0] = baseline
			break
		}
	}
	if probe, _ := ctx.Value(adaptiveLegacyProbeKey{}).(**Account); probe != nil {
		changed, normal := rankAdaptivePoolWithProbe(ctx, pool, adaptiveAccount, adaptiveMappedModel(ctx, model), adaptiveAnyPeer, true, adaptiveProbeOptions[*Account]{immediate: true})
		if normal > 0 {
			*probe = pool[0]
			return pool[normal]
		}
		if changed {
			return pool[0]
		}
	} else if rankAdaptivePool(ctx, pool, adaptiveAccount, adaptiveMappedModel(ctx, model), adaptiveAnyPeer, explore) {
		return pool[0]
	}
	return baseline
}

type adaptiveSelectionModelKey struct{}
type adaptiveEligibilityKey struct{}

func withAdaptiveEligibility(ctx context.Context, eligible func(*Account) bool) context.Context {
	if _, enabled := gatewayAdaptiveRoute(ctx); !enabled {
		return ctx
	}
	return context.WithValue(ctx, adaptiveEligibilityKey{}, eligible)
}

// WithAdaptiveSelectionModel supplies the upstream model for capability-only selection.
func WithAdaptiveSelectionModel(ctx context.Context, model string) context.Context {
	if _, enabled := gatewayAdaptiveRoute(ctx); !enabled {
		return ctx
	}
	return context.WithValue(ctx, adaptiveSelectionModelKey{}, model)
}

func adaptiveSelectionModel(ctx context.Context, fallback string) string {
	if value, ok := ctx.Value(adaptiveSelectionModelKey{}).(string); ok {
		return value
	}
	return fallback
}

func protectAdaptiveSelection(ctx context.Context) context.Context {
	if route, ok := gatewayAdaptiveRoute(ctx); ok && !route.protected {
		route.protected = true
		return context.WithValue(ctx, adaptiveRouteKey{}, route)
	}
	return ctx
}

func adaptiveOpenAIModel(ctx context.Context, model string, compact bool) func(*Account) string {
	if effective, ok := ctx.Value(adaptiveSelectionModelKey{}).(string); ok {
		return func(*Account) string { return effective }
	}
	return func(account *Account) string {
		if model == "" {
			return ""
		}
		return resolveOpenAIAccountUpstreamModelForRequest(account, model, compact)
	}
}

func adaptiveOpenAIPeers(compact bool, rateOrder openAILegacyUpstreamRateOrder) adaptivePeerMatch {
	structuralPeers := adaptiveOpenAIHealthPeers(compact)
	return func(a, b *Account) bool { return structuralPeers(a, b) && rateOrder.compare(a, b) == 0 }
}

func adaptiveOpenAIHealthPeers(compact bool) adaptivePeerMatch {
	return func(a, b *Account) bool {
		return !compact || openAICompactSupportTier(a) == openAICompactSupportTier(b)
	}
}

type adaptiveObservationKey struct{}

type adaptiveObservation struct {
	mu              sync.Mutex
	first           time.Time
	calls           int
	status          int
	failureRecorded bool
	streamOwner     bool
	streamObserved  bool
	streamFacts     adaptiveStreamFacts
}

func beginAdaptiveObservation(ctx context.Context) (context.Context, *adaptiveObservation) {
	if _, enabled := gatewayAdaptiveRoute(ctx); !enabled || ctx.Value(adaptiveObservationKey{}) != nil {
		return ctx, nil
	}
	observation := &adaptiveObservation{}
	return context.WithValue(ctx, adaptiveObservationKey{}, observation), observation
}

func (o *adaptiveObservation) recordHTTP(send func() (*http.Response, error)) (*http.Response, error) {
	if o == nil {
		return send()
	}
	o.mu.Lock()
	if o.calls == 0 {
		if o.streamFacts.clock != nil {
			o.first = o.streamFacts.clock()
		} else {
			o.first = time.Now()
		}
	}
	o.calls++
	o.mu.Unlock()
	resp, err := send()
	o.mu.Lock()
	defer o.mu.Unlock()
	if resp != nil {
		o.status = resp.StatusCode
	}
	return resp, err
}

func adaptiveHTTP(ctx context.Context, send func() (*http.Response, error)) (*http.Response, error) {
	return adaptiveHTTPObservation(ctx).recordHTTP(send)
}

func adaptiveHTTPObservation(ctx context.Context) *adaptiveObservation {
	observation, _ := ctx.Value(adaptiveObservationKey{}).(*adaptiveObservation)
	return observation
}

func observeAdaptiveGatewayResult(ctx context.Context, observation *adaptiveObservation, account *Account, result *ForwardResult, err error) {
	if result != nil && (result.ClientDisconnect || result.UpstreamResponseModelConflict) {
		return
	}
	if err != nil && observation != nil {
		observeAdaptiveForwardFailure(ctx, observation, account, err, adaptiveMappedModel(ctx, adaptiveIngressModel(ctx)))
	}
	if result == nil {
		return
	}
	observeAdaptiveResult(ctx, observation, account, result.Model, result.UpstreamModel, result.Stream, result.Duration, result.FirstTokenMs, result.ServiceTier, result.UpstreamResponseServiceTier, err)
}

// ObserveAdaptiveOpenAIResult is also used by the Responses WebSocket
// dispatcher at its upstream terminal callback.
func ObserveAdaptiveOpenAIResult(ctx context.Context, account *Account, result *OpenAIForwardResult, err error) {
	if err != nil {
		observeAdaptiveForwardFailure(ctx, nil, account, err, adaptiveOpenAIModel(ctx, adaptiveIngressModel(ctx), false))
		return
	}
	observeAdaptiveOpenAIResult(ctx, nil, account, result, nil)
}

func adaptiveIngressModel(ctx context.Context) string {
	model, _ := ctx.Value(adaptiveProbeModelKey{}).(string)
	return model
}

func observeAdaptiveForwardFailure(ctx context.Context, observation *adaptiveObservation, account *Account, err error, model func(*Account) string) {
	route, enabled := gatewayAdaptiveRoute(ctx)
	if !enabled || route.protected || account == nil || ctx.Err() != nil {
		return
	}
	var upstream *UpstreamFailoverError
	if !errors.As(err, &upstream) || !adaptiveFailureStatus(upstream.StatusCode) || upstream.IsCredentialFailure() {
		return
	}
	upstreamModel := model(account)
	if upstreamModel == "" {
		return
	}
	if observation != nil {
		observation.mu.Lock()
		defer observation.mu.Unlock()
		if observation.calls != 1 || observation.failureRecorded || observation.status >= 400 {
			return
		}
		observation.failureRecorded = true
	} else if route.transport != "ws" {
		return
	}
	route.tier = adaptiveCandidateTier(ctx, route, account, upstreamModel)
	if route.tier != "unknown" {
		observeAdaptiveAttempt(ctx, route, account, upstreamModel, time.Now(), 0, false)
	}
}

func observeAdaptiveOpenAIResult(ctx context.Context, observation *adaptiveObservation, account *Account, result *OpenAIForwardResult, err error) {
	observeAdaptiveOpenAIResultWithObserver(ctx, observation, account, result, err, nil)
}

func observeAdaptiveOpenAIResultWithObserver(ctx context.Context, observation *adaptiveObservation, account *Account, result *OpenAIForwardResult, err error, observer *upstreamResponseModelObserver) {
	observeAdaptiveStreamResult(ctx, observation, account, result, err)
	if result != nil && (result.ClientDisconnect || result.UpstreamResponseModelConflict) {
		return
	}
	if err != nil && observation != nil {
		observeAdaptiveForwardFailure(ctx, observation, account, err, adaptiveOpenAIModel(ctx, adaptiveIngressModel(ctx), false))
	}
	if result == nil {
		return
	}
	route, enabled := gatewayAdaptiveRoute(ctx)
	if !enabled {
		return
	}
	if route.transport == "ws" {
		if !result.OpenAIWSMode || (result.UpstreamTerminalEvent != "response.completed" && result.UpstreamTerminalEvent != "response.done") {
			return
		}
	} else if result.OpenAIWSMode {
		// An HTTP ingress that is dynamically bridged to WebSocket is not
		// comparable to a direct HTTP forward on the same account.
		return
	}
	observedTier := result.UpstreamResponseServiceTier
	if observer != nil && account != nil && account.Platform == PlatformOpenAI && adaptiveTieredOperation(route.operation) {
		tier, conflict := observer.adaptiveTier.value()
		if conflict {
			return
		}
		if tier != "" {
			observedTier = tier
		}
	}
	observeAdaptiveResult(ctx, observation, account, result.Model, result.UpstreamModel, result.Stream, result.Duration, result.FirstTokenMs, result.ServiceTier, observedTier, err)
}

func observeAdaptiveResult(ctx context.Context, observation *adaptiveObservation, account *Account, requestedModel, upstreamModel string, streaming bool, elapsed time.Duration, first *int, sentTier *string, observedTier string, err error) {
	route, ok := gatewayAdaptiveRoute(ctx)
	if !ok || route.protected || err != nil || account == nil || ctx.Err() != nil {
		return
	}
	now := time.Now()
	if observation != nil {
		observation.mu.Lock()
		defer observation.mu.Unlock()
	}
	if route.transport != "ws" && (observation == nil || observation.calls != 1 || observation.status < 200 || observation.status >= 300) {
		return
	}
	if upstreamModel == "" {
		upstreamModel = requestedModel
	}
	route, ok = adaptiveObservationRoute(ctx, route, account, upstreamModel, sentTier, observedTier)
	if !ok {
		return
	}
	if route.transport == "ws" || streaming {
		if first == nil || *first <= 0 {
			return
		}
		firstLatency := time.Duration(*first) * time.Millisecond
		if observation != nil {
			firstLatency -= observation.first.Sub(now.Add(-elapsed))
		}
		elapsed = firstLatency
	} else {
		elapsed = now.Sub(observation.first)
	}
	observeAdaptiveAttempt(ctx, route, account, upstreamModel, now, elapsed, true)
}

func observeAdaptiveCompletion(ctx context.Context, account *Account, model string, start time.Time, status int, success bool) {
	route, ok := gatewayAdaptiveRoute(ctx)
	if !ok || route.protected || account == nil || ctx.Err() != nil {
		return
	}
	if success || adaptiveFailureStatus(status) {
		observeAdaptiveAttempt(ctx, route, account, model, time.Now(), time.Since(start), success)
	}
}

func observeAdaptiveHTTPFailure(ctx context.Context, observation *adaptiveObservation, account *Account, model string, status int, sentTier ...*string) {
	if observation == nil || !adaptiveFailureStatus(status) || ctx.Err() != nil {
		return
	}
	route, ok := gatewayAdaptiveRoute(ctx)
	if !ok || route.protected {
		return
	}
	var sent *string
	if len(sentTier) > 0 {
		sent = sentTier[0]
	}
	route, ok = adaptiveObservationRoute(ctx, route, account, model, sent, "")
	if !ok {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if observation.calls == 1 && observation.status == status && !observation.failureRecorded && ctx.Err() == nil {
		observation.failureRecorded = true
		observeAdaptiveAttempt(ctx, route, account, model, time.Now(), time.Since(observation.first), false)
	}
}

func adaptiveFailureStatus(status int) bool {
	// A rejected prompt, invalid payload, or shared rate limit is insufficient
	// evidence of an unhealthy account.
	return status >= 500 && status <= 599
}

// WithAdaptiveImageInput uses fields the existing image parser validated,
// including for multipart requests.
func WithAdaptiveImageInput(ctx context.Context, body []byte, input *OpenAIImagesRequest) context.Context {
	ctx = SetGatewayAdaptiveInput(ctx, body)
	route, ok := gatewayAdaptiveRoute(ctx)
	if !ok || input == nil {
		return ctx
	}
	if input.Stream {
		route.transport = "sse"
	}
	quality := input.Quality
	switch quality {
	case "", "auto", "standard", "hd", "low", "medium", "high":
	default:
		return context.WithValue(ctx, adaptiveRouteKey{}, adaptiveRoute{})
	}
	// Size, counts, and transport were normalized by the image parser.
	route.effort = fmt.Sprintf("%d/%s/%s/%d/%t/%t", input.N, input.Size, quality, len(input.Uploads)+len(input.InputImageURLs), input.HasMask, input.Multipart)
	if len(route.effort) > 96 {
		return context.WithValue(ctx, adaptiveRouteKey{}, adaptiveRoute{})
	}
	return WithGatewayAdaptiveRequestModel(context.WithValue(ctx, adaptiveRouteKey{}, route), input.Model)
}

// WithAdaptiveImageTask checks the setting again in the detached executor.
func (s *OpenAIGatewayService) WithAdaptiveImageTask(ctx context.Context, path string) context.Context {
	if s == nil {
		return ctx
	}
	operation := "async_image_generation"
	if strings.HasSuffix(path, "/edits") || strings.HasSuffix(path, "/edits/async") {
		operation = "async_image_edit"
	}
	if s.settingService == nil || !s.settingService.IsGatewayAdaptiveSchedulingEnabled(ctx) {
		if _, exists := gatewayAdaptiveRoute(ctx); exists {
			ctx = context.WithValue(ctx, adaptiveIngressKey{}, (*adaptiveIngress)(nil))
			ctx = context.WithValue(ctx, adaptiveRouteKey{}, adaptiveRoute{})
		}
		return ctx
	}
	return WithGatewayAdaptiveIngress(ctx, s.settingService, operation, "")
}
