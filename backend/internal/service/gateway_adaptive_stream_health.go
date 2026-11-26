package service

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

const adaptiveStreamWindow = 15 * time.Minute
const adaptiveStreamSamples = 10

type adaptiveStreamEffortKey struct{}

type adaptiveStreamEffort struct {
	bucket, nested, flat string
	maxOutput            int64
}

func withAdaptiveStreamEffort(ctx context.Context, payload gjson.Result, bucket string) context.Context {
	nested := payload.Get("reasoning.effort").String()
	flat := payload.Get("reasoning_effort").String()
	if len(nested) > 64 || len(flat) > 64 {
		return context.WithValue(ctx, adaptiveStreamEffortKey{}, adaptiveStreamEffort{})
	}
	return context.WithValue(ctx, adaptiveStreamEffortKey{}, adaptiveStreamEffort{
		bucket: bucket, nested: strings.Clone(nested), flat: strings.Clone(flat),
		maxOutput: maxEffortOutput(payload),
	})
}

func maxEffortOutput(payload gjson.Result) int64 {
	maxTokens, maxCompletion := payload.Get("max_tokens").Int(), payload.Get("max_completion_tokens").Int()
	if maxCompletion > maxTokens {
		return maxCompletion
	}
	return maxTokens
}

// SetGatewayAdaptiveChatPolicyInput refreshes the new lane's effort after the
// group policy runs. Ingress byte, activity and legacy descriptors stay intact.
func SetGatewayAdaptiveChatPolicyInput(ctx context.Context, body []byte) context.Context {
	if ctx.Value(adaptiveStreamEffortKey{}) == nil {
		return ctx
	}
	payload := gjson.ParseBytes(body)
	return withAdaptiveStreamEffort(ctx, payload, adaptiveEffort(payload))
}

func (effort adaptiveStreamEffort) outbound(account *Account, model string, work adaptiveWorkload) (string, bool) {
	if effort.bucket == "" || effort.bucket == "unknown" {
		return "", false
	}
	// Other raw-body rewrites can change these dimensions after selection.
	// Continue to observe the actual outbound shape when it is attributable.
	if account.IsGrok() && (effort.flat != "" || work&adaptiveWorkDeclaredTools != 0) {
		return "", false
	}
	if rawChatHasOutputTokenClamp(account, model) {
		cap := ollamaCloudMaxTokensCap(account)
		if cap > 0 && effort.maxOutput > cap {
			return "", false
		}
	}
	path, mapped := glmOpenAIReasoningRewrite(model, effort.nested, effort.flat)
	if mapped != "" && (path == "reasoning.effort" || effort.nested == "") {
		_, suffix, _ := strings.Cut(effort.bucket, "/")
		return adaptiveEffortToken(mapped) + "/" + suffix, true
	}
	return effort.bucket, true
}

func adaptiveNativeSession(headers http.Header) bool {
	for name, values := range headers {
		if adaptiveNativeSessionHeader(name) {
			for _, value := range values {
				if strings.TrimSpace(value) != "" {
					return true
				}
			}
		}
	}
	return false
}

func adaptiveNativeSessionHeader(name string) bool {
	return strings.EqualFold(name, grokConversationIDHeader) ||
		strings.EqualFold(name, openCodeSessionHeader) || strings.EqualFold(name, codeBuddyConversationHeader)
}

func adaptiveMayAddNativeSession(account *Account) bool {
	if account.IsGrok() || (account.Type == AccountTypeAPIKey && shouldSendOpenCodeSessionHeader(account, account.GetOpenAIBaseURL())) {
		return true
	}
	for name := range account.GetHeaderOverrides() {
		if adaptiveNativeSessionHeader(name) {
			return true
		}
	}
	return false
}

// DeepSeek echoes these two sent models with a hyphen in the version.
// Keep the sent model in the stream-health key, including its rate suffix.
func adaptiveStreamModelMatches(account *Account, sent, echoed string) bool {
	echoed = normalizeOpenAIModelForUpstream(account, echoed)
	if strings.EqualFold(sent, echoed) {
		return true
	}
	if account.Platform == PlatformDeepseek {
		switch strings.ToLower(sent) {
		case "deepseek-v4.1-flash":
			return strings.EqualFold(echoed, "deepseek-v4-1-flash")
		case "deepseek-v4.1-flash-economy":
			return strings.EqualFold(echoed, "deepseek-v4-1-flash-economy")
		}
	}
	return false
}

func adaptiveStreamOperation(operation string) bool {
	return strings.HasPrefix(operation, "POST ") && strings.HasSuffix(operation, "/chat/completions")
}

type adaptiveSemanticDelta uint8

const (
	adaptiveNoDelta adaptiveSemanticDelta = iota
	adaptiveTextDelta
	adaptiveReasoningDelta
	adaptiveToolDelta
)

func adaptiveChatDelta(choices []gjson.Result) adaptiveSemanticDelta {
	var deltaType adaptiveSemanticDelta
	nonempty := func(value gjson.Result) bool { return value.Type == gjson.String && strings.TrimSpace(value.Str) != "" }
	for _, choice := range choices {
		delta := choice.Get("delta")
		if nonempty(delta.Get("function_call.arguments")) {
			return adaptiveToolDelta
		}
		for _, call := range delta.Get("tool_calls").Array() {
			if nonempty(call.Get("function.arguments")) {
				return adaptiveToolDelta
			}
		}
		if nonempty(delta.Get("content")) {
			deltaType = adaptiveTextDelta
		} else if deltaType == adaptiveNoDelta && (nonempty(delta.Get("reasoning_content")) || nonempty(delta.Get("reasoning"))) {
			deltaType = adaptiveReasoningDelta
		}
	}
	return deltaType
}

// The forwarding terminal detector tolerates interrupted streams for usage and
// client compatibility. Health requires affirmative success and keeps failures sticky.
type adaptiveChatHealthTerminal struct {
	expected, finished                     uint64
	invalidIndex, done, finalUsage, failed bool
}

func adaptiveChatExpectedChoices(n gjson.Result) uint64 {
	if !n.Exists() {
		return 1 // Chat Completions defaults to one choice.
	}
	count, err := strconv.Atoi(n.Raw)
	if n.Type != gjson.Number || err != nil || count < 1 || count > 64 {
		return 0
	}
	return (uint64(1) << count) - 1
}

func (t *adaptiveChatHealthTerminal) observe(payload gjson.Result, choices []gjson.Result) {
	for _, choice := range choices {
		index := choice.Get("index")
		i, err := strconv.Atoi(index.Raw)
		var bit uint64
		if index.Type == gjson.Number && err == nil && i >= 0 && i < 64 {
			bit = uint64(1) << i
		}
		if t.expected&bit == 0 {
			t.invalidIndex = true
		}
		reason := strings.ToLower(strings.TrimSpace(choice.Get("finish_reason").String()))
		switch reason {
		case "stop", "length", "tool_calls", "function_call":
			t.finished |= bit
		case "":
		default:
			t.failed = true
		}
	}
	if kind := payload.Get("type").String(); strings.EqualFold(kind, "error") || strings.EqualFold(kind, "response.failed") {
		t.failed = true
	}
	if payload.Get("error").Type != gjson.Null || strings.EqualFold(payload.Get("object").String(), "error") {
		t.failed = true
	}
	if len(choices) != 0 {
		t.finalUsage = false
	} else if payload.Get("choices").IsArray() {
		usage := payload.Get("usage")
		t.finalUsage = usage.IsObject() && (usage.Get("completion_tokens").Type == gjson.Number ||
			usage.Get("output_tokens").Type == gjson.Number || usage.Get("total_tokens").Type == gjson.Number)
	}
}

func (t adaptiveChatHealthTerminal) successful() bool {
	finishedAll := t.expected != 0 && !t.invalidIndex && t.finished == t.expected
	return !t.failed && (t.done || t.finalUsage || finishedAll)
}

type adaptiveStreamFacts struct {
	model             string
	tier              string
	effort            string
	outbound          adaptiveChatShape
	sent              bool
	terminal          bool
	aborted           bool
	firstOutputAt     time.Time
	firstDelta        adaptiveSemanticDelta
	clock             func() time.Time
	observedTier      upstreamResponseTierObservation
	unexpectedTools   bool
	transportRepeated bool
	expectedChoices   uint64
}

type adaptiveStreamKey struct {
	sample adaptiveSampleKey
	wire   string
	work   adaptiveWorkload
}

func observeAdaptiveStreamResult(ctx context.Context, o *adaptiveObservation, account *Account, result *OpenAIForwardResult, err error) {
	route, enabled := gatewayAdaptiveRoute(ctx)
	if !enabled || o == nil || !adaptiveStreamOperation(route.operation) || !o.streamOwner {
		return
	}
	health := &adaptiveTelemetryForContext(ctx).streamHealth
	o.mu.Lock()
	if o.streamObserved {
		o.mu.Unlock()
		return
	}
	o.streamObserved = true
	facts, first, calls, status := o.streamFacts, o.first, o.calls, o.status
	o.mu.Unlock()
	ingress, _ := ctx.Value(adaptiveWorkloadKey{}).(adaptiveChatShape)
	switch {
	case ctx.Err() != nil || facts.aborted || (result != nil && result.ClientDisconnect):
		health.count(streamCanceled)
	case calls > 1 || facts.transportRepeated:
		health.count(streamMultiAttempt)
	case route.transport != "sse" || !facts.sent || (result != nil && (!result.Stream || result.OpenAIWSMode)):
		health.count(streamUnknownWire)
	case route.nonText || ingress.nonText || facts.outbound.nonText:
		health.count(streamUnsupportedNonText)
	case ingress.work == adaptiveWorkUnknown:
		health.count(streamUnsupportedIngress)
	case facts.outbound.work == adaptiveWorkUnknown:
		health.count(streamUnsupportedOutbound)
	case calls != 1 || status < 200 || status >= 300 || err != nil || result == nil || !facts.terminal:
		health.count(streamNoTerminal)
	default:
		latency := facts.firstOutputAt.Sub(first)
		if first.IsZero() || facts.firstDelta == adaptiveNoDelta || latency <= 0 || latency > adaptiveStreamWindow {
			health.count(streamNoToken)
			return
		}
		observedTier, tierConflict := facts.observedTier.value()
		outboundTier := adaptiveTierBucket(adaptiveTier(facts.tier))
		if account == nil || facts.model == "" || len(facts.model) > 128 || facts.effort == "unknown" || outboundTier == "unknown" || tierConflict ||
			result.UpstreamResponseModelConflict || (result.UpstreamResponseModel != "" && !adaptiveStreamModelMatches(account, facts.model, result.UpstreamResponseModel)) ||
			(observedTier != "" && adaptiveTierBucket(adaptiveTier(observedTier)) != outboundTier &&
				!(account.IsOpenAIOAuthLike() && codexOAuthResponseTierIsNonAuthoritative(observedTier))) {
			health.count(streamMismatch)
			return
		}
		route.tier, route.effort = outboundTier, facts.effort
		work := facts.outbound.work
		if work != ingress.work {
			work |= ingress.work | adaptiveWorkRewritten
		}
		if facts.unexpectedTools {
			work |= adaptiveWorkUnexpectedTools
		}
		key := adaptiveStreamKey{adaptiveContextKey(ctx, route, account, facts.model), APIProtocolChatCompletions, work}
		health.observe(key, health.now(), latency)
		if ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress); ingress != nil {
			if lease := ingress.recovery.Load(); lease != nil && lease.streamKey == key && lease.acquired.Load() {
				health.count(streamProbeSuccess)
			}
		}
	}
}

type adaptiveStreamState struct {
	count  int
	slow   bool
	normal bool
}

func adaptiveStreamStatus(samples []adaptiveSample, now time.Time) adaptiveStreamState {
	state := adaptiveStreamState{}
	var slow, severe, recovered int
	var recoveryLatencies [10]time.Duration
	normal := recoveryLatencies[:0]
	for _, sample := range samples {
		if !sample.at.After(now.Add(-adaptiveStreamWindow)) || sample.at.After(now) {
			continue
		}
		points := adaptiveTTFTPoints(sample.latency)
		if state.slow {
			if points < 80 {
				recovered = 0
				normal = normal[:0]
				continue
			}
			recovered++
			normal = append(normal, sample.latency)
			if recovered == 3 {
				state.slow, state.count = false, recovered
				slow, severe, recovered = 0, 0, 0
			}
			continue
		}
		state.count++
		normal = append(normal, sample.latency)
		if points < 80 {
			slow++
		}
		if points == 0 {
			severe++
		} else {
			severe = 0
		}
		if severe >= 2 || slow >= 5 {
			state.slow = true
			normal = normal[:0]
		}
	}
	state.normal = !state.slow && len(normal) >= 3 && adaptiveTTFTPoints(adaptiveMedian(normal)) >= 80
	return state
}

type adaptiveStreamCounter uint8

const (
	streamSampled adaptiveStreamCounter = iota
	streamUnknownWire
	streamUnsupportedNonText
	streamUnsupportedIngress
	streamUnsupportedOutbound
	streamNoTerminal
	streamMultiAttempt
	streamCanceled
	streamNoToken
	streamMismatch
	streamQueued
	streamQueueFull
	streamFlushed
	streamFlushFailed
	streamRestored
	streamRestoreFailed
	streamCapacity
	streamBindingMiss
	streamBindingUnknown
	streamNoAlternate
	streamDemoted
	streamRecovered
	streamProbeReserved
	streamProbeBlocked
	streamProbeFailed
	streamProbeSuccess
	streamCounterCount
)

var adaptiveStreamCounterNames = [streamCounterCount]string{
	"sampled", "not_streaming_or_wire_unknown",
	"unsupported_workload_non_text", "unsupported_workload_ingress", "unsupported_workload_outbound", "non_2xx_or_no_terminal",
	"multi_attempt", "cancel_or_disconnect", "no_semantic_token_or_invalid_ttft", "model_or_tier_mismatch",
	"queued", "queue_full", "flushed", "flush_failed", "restored", "restore_failed", "capacity_truncated",
	"binding_miss", "binding_unknown", "no_comparable_alternate", "slow_demoted", "recovered",
	"probe_reserved", "probe_blocked", "probe_no_slot_or_recheck_failed", "probe_success",
}

type adaptiveStreamHealth struct {
	mu       sync.Mutex
	series   map[adaptiveStreamKey][]adaptiveSample
	ready    atomic.Bool
	history  *adaptiveStreamHistory
	counters [streamCounterCount]atomic.Uint64
	clock    func() time.Time
}

func (h *adaptiveStreamHealth) now() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now()
}

func (h *adaptiveStreamHealth) count(counter adaptiveStreamCounter) { h.counters[counter].Add(1) }

func (h *adaptiveStreamHealth) summarize() {
	var fields []slog.Attr
	var unsupported uint64
	for i, name := range adaptiveStreamCounterNames {
		if count := h.counters[i].Swap(0); count > 0 {
			fields = append(fields, slog.Uint64(name, count))
			if i >= int(streamUnsupportedNonText) && i <= int(streamUnsupportedOutbound) {
				unsupported += count
			}
		}
	}
	if unsupported > 0 {
		fields = append(fields, slog.Uint64("unsupported_workload", unsupported))
	}
	if len(fields) > 0 {
		slog.LogAttrs(context.Background(), slog.LevelInfo, "gateway.adaptive_stream_health", fields...)
	}
}

func (h *adaptiveStreamHealth) observe(key adaptiveStreamKey, now time.Time, latency time.Duration) {
	sample := adaptiveSample{at: now.Truncate(time.Microsecond), latency: latency, success: true}
	if h.history != nil {
		sample.id = h.history.nextID()
	}
	h.mu.Lock()
	before := adaptiveStreamStatus(h.series[key], now)
	h.insert(key, sample, now)
	after := adaptiveStreamStatus(h.series[key], now)
	h.mu.Unlock()
	h.count(streamSampled)
	if before.slow && after.normal {
		h.count(streamRecovered)
	}
	if h.history != nil {
		h.history.enqueue(h, adaptiveStreamEntry{key, sample})
	}
}

// insert is shared by live observations and restore, with the caller holding mu.
func (h *adaptiveStreamHealth) insert(key adaptiveStreamKey, sample adaptiveSample, now time.Time) {
	if !sample.at.After(now.Add(-adaptiveStreamWindow)) || sample.at.After(now) {
		return
	}
	if h.series == nil {
		h.series = make(map[adaptiveStreamKey][]adaptiveSample)
	}
	samples := h.series[key]
	if len(samples) == 0 && len(h.series) >= adaptiveCapacity {
		var oldestKey adaptiveStreamKey
		var oldest time.Time
		for existing, values := range h.series {
			at := values[len(values)-1].at
			if oldest.IsZero() || at.Before(oldest) {
				oldestKey, oldest = existing, at
			}
		}
		h.count(streamCapacity)
		if sample.at.Before(oldest) {
			return
		}
		delete(h.series, oldestKey)
	}
	samples = slices.DeleteFunc(samples, func(s adaptiveSample) bool { return !s.at.After(now.Add(-adaptiveStreamWindow)) })
	if sample.id != "" && slices.ContainsFunc(samples, func(s adaptiveSample) bool { return s.id == sample.id }) {
		return
	}
	samples = append(samples, sample)
	slices.SortStableFunc(samples, func(a, b adaptiveSample) int {
		if cmp := a.at.Compare(b.at); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.id, b.id)
	})
	if len(samples) > adaptiveStreamSamples {
		samples = slices.Delete(samples, 0, len(samples)-adaptiveStreamSamples)
	}
	h.series[key] = samples
}

type adaptiveStreamPermissionKey struct{}

type adaptiveBindingState uint8

const (
	adaptiveBindingUnknown adaptiveBindingState = iota
	adaptiveBindingMiss
	adaptiveBindingHit
)

func withAdaptiveStreamPermission(ctx context.Context, state adaptiveBindingState, eligible bool) context.Context {
	route, ok := gatewayAdaptiveRoute(ctx)
	shape, _ := ctx.Value(adaptiveWorkloadKey{}).(adaptiveChatShape)
	work := shape.work
	_, guardian := openAIGuardianParentAffinityFromContext(ctx)
	if !ok || !eligible || guardian || route.protected || route.transport != "sse" ||
		work == 0 || work & ^adaptiveWorkSortable != 0 {
		return ctx
	}
	health := &adaptiveTelemetryForContext(ctx).streamHealth
	switch state {
	case adaptiveBindingMiss:
		health.count(streamBindingMiss)
		return context.WithValue(ctx, adaptiveStreamPermissionKey{}, true)
	case adaptiveBindingUnknown:
		health.count(streamBindingUnknown)
	}
	return ctx
}

type adaptiveStreamChoice struct {
	accountID      int64
	key            adaptiveStreamKey
	evidence       bool
	requestedModel string
	rate           float64
}

func adaptiveStreamCandidateKey(ctx context.Context, route adaptiveRoute, account *Account, requestedModel string) (adaptiveStreamKey, bool) {
	shape, _ := ctx.Value(adaptiveWorkloadKey{}).(adaptiveChatShape)
	work := shape.work
	if work == 0 || requestedModel == "" {
		return adaptiveStreamKey{}, false
	}
	model := normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, requestedModel, ""))
	if model == "" || !synchronousNativeChatProtocol(account, model) {
		return adaptiveStreamKey{}, false
	}
	if work&adaptiveWorkCompat != 0 && adaptiveMayAddNativeSession(account) {
		return adaptiveStreamKey{}, false
	}
	effort, _ := ctx.Value(adaptiveStreamEffortKey{}).(adaptiveStreamEffort)
	var ok bool
	route.effort, ok = effort.outbound(account, model, work)
	if !ok {
		return adaptiveStreamKey{}, false
	}
	route.tier = adaptiveCandidateTier(ctx, route, account, model)
	if route.tier == "unknown" {
		return adaptiveStreamKey{}, false
	}
	return adaptiveStreamKey{adaptiveContextKey(ctx, route, account, model), APIProtocolChatCompletions, work}, true
}

func validAdaptiveStreamRate(account *Account) bool {
	rate := account.BillingRateMultiplier()
	return !math.IsNaN(rate) && !math.IsInf(rate, 0) && rate >= 0 &&
		(account.RateMultiplier == nil || *account.RateMultiplier >= 0)
}

func chooseAdaptiveStreamAccount[T any](ctx context.Context, pool []T, account func(T) *Account, peers adaptivePeerMatch,
	canProbe func(*Account) bool, requestedModel string, now time.Time, reserve func(adaptiveStreamKey) bool,
) adaptiveStreamChoice {
	health := &adaptiveTelemetryForContext(ctx).streamHealth
	if ctx.Value(adaptiveStreamPermissionKey{}) == nil || !health.ready.Load() || len(pool) < 2 {
		return adaptiveStreamChoice{}
	}
	route, _ := gatewayAdaptiveRoute(ctx)
	primary := account(pool[0])
	key, ok := adaptiveStreamCandidateKey(ctx, route, primary, requestedModel)
	if !ok || !validAdaptiveStreamRate(primary) {
		return adaptiveStreamChoice{}
	}
	health.mu.Lock()
	if !adaptiveStreamStatus(health.series[key], now).slow {
		health.mu.Unlock()
		return adaptiveStreamChoice{}
	}
	var unproven []adaptiveStreamKey
	for _, item := range pool[1:] {
		candidate := account(item)
		if !peers(primary, candidate) || !validAdaptiveStreamRate(candidate) ||
			primary.BillingRateMultiplier() != candidate.BillingRateMultiplier() {
			continue
		}
		candidateKey, ok := adaptiveStreamCandidateKey(ctx, route, candidate, requestedModel)
		if !ok || candidateKey.sample.route.tier != key.sample.route.tier {
			continue
		}
		state := adaptiveStreamStatus(health.series[candidateKey], now)
		if state.normal {
			health.mu.Unlock()
			health.count(streamDemoted)
			return adaptiveStreamChoice{accountID: candidate.ID, key: candidateKey, requestedModel: requestedModel, rate: primary.BillingRateMultiplier()}
		}
		if !state.slow && state.count < 3 && (canProbe == nil || canProbe(candidate)) {
			unproven = append(unproven, candidateKey)
		}
	}
	health.mu.Unlock()
	health.count(streamNoAlternate)
	if reserve != nil && len(unproven) > 0 {
		for _, candidateKey := range unproven {
			if reserve(candidateKey) {
				health.count(streamProbeReserved)
				return adaptiveStreamChoice{accountID: candidateKey.sample.accountID, key: candidateKey, evidence: true, requestedModel: requestedModel, rate: primary.BillingRateMultiplier()}
			}
		}
		health.count(streamProbeBlocked)
	}
	return adaptiveStreamChoice{}
}

func (choice adaptiveStreamChoice) failed(ctx context.Context) {
	if choice.evidence {
		adaptiveTelemetryForContext(ctx).streamHealth.count(streamProbeFailed)
		cancelUnacquiredAdaptiveProbe(ctx)
	}
}

func (choice adaptiveStreamChoice) matches(ctx context.Context, account *Account) bool {
	route, _ := gatewayAdaptiveRoute(ctx)
	key, ok := adaptiveStreamCandidateKey(ctx, route, account, choice.requestedModel)
	return ok && key == choice.key && validAdaptiveStreamRate(account) && account.BillingRateMultiplier() == choice.rate
}
