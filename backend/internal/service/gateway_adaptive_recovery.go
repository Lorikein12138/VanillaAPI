package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

func adaptiveAccountConfig(account *Account) string {
	// Only the digest of routing configuration is kept. Runtime health and
	// last-used timestamps can change on every request.
	const responseSupport, responseMode, apiKeyWS, oauthWS = "openai_responses_supported", "openai_responses_mode",
		"openai_apikey_responses_websockets_v2_enabled", "openai_oauth_responses_websockets_v2_enabled"
	if account.ProxyID == nil && account.ParentAccountID == nil && account.Credentials["base_url"] == nil &&
		account.Extra[responseSupport] == nil && account.Extra[responseMode] == nil && account.Extra[apiKeyWS] == nil && account.Extra[oauthWS] == nil {
		return ""
	}
	var proxyID, parentID int64
	if account.ProxyID != nil {
		proxyID = *account.ProxyID
	}
	if account.ParentAccountID != nil {
		parentID = *account.ParentAccountID
	}
	digest := sha256.Sum256(fmt.Appendf(nil, "%d/%d/%q/%v/%v/%v/%v", proxyID, parentID, account.GetCredential("base_url"),
		account.Extra[responseSupport], account.Extra[responseMode], account.Extra[apiKeyWS], account.Extra[oauthWS]))
	return hex.EncodeToString(digest[:16])
}

func adaptiveReplayable(operation, transport string, payload gjson.Result) bool {
	if transport == "ws" || !payload.IsObject() || !strings.HasPrefix(operation, "POST ") {
		return false
	}
	// Only plain, text-only history with explicit turns qualifies. Tool outputs,
	// file handles, reasoning artifacts and provider-managed state are excluded.
	var field string
	switch {
	case strings.HasSuffix(operation, "/messages"), strings.HasSuffix(operation, "/chat/completions"):
		field = "messages"
	case strings.HasSuffix(operation, "/responses"):
		field = "input"
	case strings.HasSuffix(operation, ":generateContent"), strings.HasSuffix(operation, ":streamGenerateContent"):
		field = "contents"
	default:
		return false
	}
	safe := true
	payload.ForEach(func(key, value gjson.Result) bool {
		switch key.String() {
		case field, "model", "stream", "max_tokens", "max_output_tokens", "max_completion_tokens", "temperature", "top_p",
			"service_tier", "thinking", "reasoning", "reasoning_effort", "output_config", "generationConfig":
		case "system", "instructions":
			safe = value.Type == gjson.String
		default:
			safe = false
		}
		return safe
	})
	if !safe {
		return false
	}
	history := payload.Get(field)
	if !history.IsArray() {
		return false
	}
	entries := history.Array()
	if len(entries) < 3 {
		return false
	}
	var priorUser, explicitHistory bool
	for _, entry := range entries {
		role := entry.Get("role").String()
		switch role {
		case "user":
			priorUser = true
		case "assistant", "model":
			explicitHistory = explicitHistory || priorUser
		case "system", "developer":
		default:
			return false
		}
		entry.ForEach(func(key, value gjson.Result) bool {
			switch key.String() {
			case "role":
			case "type":
				safe = field == "input" && value.String() == "message"
			case "content":
				safe = field != "contents" && adaptivePlainContent(value)
			case "parts":
				safe = field == "contents" && adaptivePlainContent(value)
			default:
				safe = false
			}
			return safe
		})
		if !safe || (!entry.Get("content").Exists() && !entry.Get("parts").Exists()) {
			return false
		}
	}
	return explicitHistory && entries[len(entries)-1].Get("role").String() == "user"
}

func adaptivePlainContent(value gjson.Result) bool {
	if value.Type == gjson.String {
		return true
	}
	if !value.IsArray() || len(value.Array()) == 0 {
		return false
	}
	for _, part := range value.Array() {
		if part.Get("text").Type != gjson.String {
			return false
		}
		safe := true
		part.ForEach(func(key, value gjson.Result) bool {
			switch key.String() {
			case "text":
			case "type":
				safe = value.String() == "text" || value.String() == "input_text" || value.String() == "output_text"
			default:
				safe = false
			}
			return safe
		})
		if !safe {
			return false
		}
	}
	return true
}

type adaptiveRecoveryPool struct {
	lastSeen   time.Time
	arrivals   [100]time.Time
	next       int
	sinceProbe int
	inFlight   bool
}

type adaptiveRecovery struct {
	mu             sync.Mutex
	pools          map[adaptiveProbeScope]*adaptiveRecoveryPool
	active         map[adaptiveSampleKey]struct{}
	lastProbe      time.Time
	streamCooldown map[adaptiveStreamKey]time.Time
	pruneAt        time.Time
	inFlight       int
}

type adaptiveRecoveryLease struct {
	recovery  *adaptiveRecovery
	pool      *adaptiveRecoveryPool
	key       adaptiveSampleKey
	streamKey adaptiveStreamKey
	acquired  atomic.Bool
	observed  atomic.Bool
	once      sync.Once
}

func (l *adaptiveRecoveryLease) finish() {
	l.once.Do(func() {
		l.recovery.mu.Lock()
		defer l.recovery.mu.Unlock()
		l.recovery.inFlight--
		l.pool.inFlight = false
		delete(l.recovery.active, l.key)
	})
}

func (r *adaptiveRecovery) opportunity(ctx context.Context, ingress *adaptiveIngress, scope adaptiveProbeScope, now time.Time) func(adaptiveSampleKey) bool {
	reserve := r.opportunityWithStream(ctx, ingress, scope, now)
	if reserve == nil {
		return nil
	}
	return func(key adaptiveSampleKey) bool { return reserve(key, adaptiveStreamKey{}) }
}

func (r *adaptiveRecovery) opportunityWithStream(ctx context.Context, ingress *adaptiveIngress, scope adaptiveProbeScope, now time.Time) func(adaptiveSampleKey, adaptiveStreamKey) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !now.Before(r.pruneAt) {
		for key, pool := range r.pools {
			if !pool.inFlight && now.Sub(pool.lastSeen) >= time.Hour {
				delete(r.pools, key)
			}
		}
		for key, until := range r.streamCooldown {
			if !now.Before(until) {
				delete(r.streamCooldown, key)
			}
		}
		r.pruneAt = now.Add(time.Minute)
	}
	pool := r.pools[scope]
	if pool == nil {
		if len(r.pools) >= adaptiveCapacity {
			return nil
		}
		if r.pools == nil {
			r.pools = make(map[adaptiveProbeScope]*adaptiveRecoveryPool)
		}
		pool = &adaptiveRecoveryPool{}
		r.pools[scope] = pool
	}
	pool.lastSeen = now
	if ingress.counted.CompareAndSwap(false, true) {
		pool.arrivals[pool.next] = now
		pool.next = (pool.next + 1) % len(pool.arrivals)
		pool.sinceProbe = min(pool.sinceProbe+1, len(pool.arrivals))
	}
	return func(key adaptiveSampleKey, streamKey adaptiveStreamKey) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, sameHealthKeyInFlight := r.active[key]
		if streamKey.wire != "" {
			if until, exists := r.streamCooldown[streamKey]; (exists && now.Before(until)) || (!exists && len(r.streamCooldown) >= adaptiveCapacity) {
				return false
			}
		}
		busyPool := now.Sub(pool.arrivals[pool.next]) < time.Minute
		if ctx.Err() != nil || pool.inFlight || sameHealthKeyInFlight || r.inFlight >= 8 || now.Sub(r.lastProbe) < time.Second ||
			(busyPool && pool.sinceProbe < len(pool.arrivals)) ||
			!ingress.explored.CompareAndSwap(false, true) {
			return false
		}
		if streamKey.wire != "" {
			if r.streamCooldown == nil {
				r.streamCooldown = make(map[adaptiveStreamKey]time.Time)
			}
			r.streamCooldown[streamKey] = now.Add(time.Minute)
		}
		lease := &adaptiveRecoveryLease{recovery: r, pool: pool, key: key, streamKey: streamKey}
		ingress.recovery.Store(lease)
		if r.active == nil {
			r.active = make(map[adaptiveSampleKey]struct{})
		}
		r.active[key] = struct{}{}
		pool.inFlight = true
		r.inFlight++
		r.lastProbe = now
		context.AfterFunc(ctx, func() {
			ingress.recovery.CompareAndSwap(lease, nil)
			lease.finish()
		})
		return true
	}
}

func cancelUnacquiredAdaptiveProbe(ctx context.Context) {
	ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
	if ingress == nil {
		return
	}
	lease := ingress.recovery.Load()
	if lease != nil && !lease.acquired.Load() && ingress.recovery.CompareAndSwap(lease, nil) {
		lease.finish()
	}
}

func adaptiveTrackSlot(ctx context.Context, accountID int64, result *AcquireResult, err error) (*AcquireResult, error) {
	ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress)
	if ingress == nil {
		return result, err
	}
	lease := ingress.recovery.Load()
	if lease == nil || lease.acquired.Load() {
		return result, err
	}
	if lease.key.accountID != accountID || err != nil || result == nil || !result.Acquired || ctx.Err() != nil {
		if ingress.recovery.CompareAndSwap(lease, nil) {
			lease.finish()
		}
		return result, err
	}
	lease.acquired.Store(true)
	lease.recovery.mu.Lock()
	lease.pool.sinceProbe = 0
	lease.recovery.mu.Unlock()
	release := result.ReleaseFunc
	result.ReleaseFunc = func() {
		if release != nil {
			release()
		}
		ingress.recovery.CompareAndSwap(lease, nil)
		lease.finish()
	}
	return result, err
}

func observeAdaptiveAttempt(ctx context.Context, route adaptiveRoute, account *Account, model string, now time.Time, latency time.Duration, success bool) {
	key := adaptiveContextKey(ctx, route, account, model)
	probe := false
	if ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress); ingress != nil {
		if lease := ingress.recovery.Load(); lease != nil && lease.streamKey.wire == "" && lease.key == key && lease.acquired.Load() {
			if !lease.observed.CompareAndSwap(false, true) {
				return
			}
			probe = true
		}
	}
	adaptiveTelemetryForContext(ctx).observeAttempt(key, now, latency, success, probe)
}

type adaptiveSoftStickyKey struct{}
type adaptiveStickyNeedsSlotKey struct{}

func withAdaptiveSoftSticky(ctx context.Context, owner *Account) context.Context {
	return context.WithValue(ctx, adaptiveSoftStickyKey{}, owner)
}

type adaptiveStickyTrialKey struct{}

func adaptiveStickyTrialContext(ctx context.Context, owner *Account, model func(*Account) string, peers adaptivePeerMatch) context.Context {
	if owner == nil || !adaptiveStickyEscape(ctx, owner, model(owner)) {
		return nil
	}
	route, _ := gatewayAdaptiveRoute(ctx)
	ownerModel := model(owner)
	ownerRoute := route
	ownerRoute.tier = adaptiveCandidateTier(ctx, ownerRoute, owner, ownerModel)
	ownerKey := adaptiveContextKey(ctx, ownerRoute, owner, ownerModel)
	telemetry := adaptiveTelemetryForContext(ctx)
	allowed := func(candidate *Account) bool {
		if candidate == nil || candidate.ID == owner.ID || !peers(owner, candidate) {
			return false
		}
		if eligible, _ := ctx.Value(adaptiveEligibilityKey{}).(func(*Account) bool); eligible != nil && !eligible(candidate) {
			return false
		}
		candidateModel := model(candidate)
		candidateRoute := route
		candidateRoute.tier = adaptiveCandidateTier(ctx, candidateRoute, candidate, candidateModel)
		if ownerRoute.tier == "unknown" || ownerRoute.tier != candidateRoute.tier {
			return false
		}
		candidateKey := adaptiveContextKey(ctx, candidateRoute, candidate, candidateModel)
		now := time.Now()
		telemetry.mu.Lock()
		defer telemetry.mu.Unlock()
		a, b := telemetry.score(ownerKey, now), telemetry.score(candidateKey, now)
		if b.health.Fault != adaptiveHealthy || a.health.Passed > 0 {
			return false
		}
		return a.health.Fault == adaptiveUpstreamDegraded ||
			(a.health.Fault == adaptiveLatencyDegraded && adaptiveProvenFaster(a, b, ownerModel == candidateModel))
	}
	return context.WithValue(withoutAdaptiveExploration(ctx), adaptiveStickyTrialKey{}, allowed)
}

func adaptiveStickyTrialAllows(ctx context.Context, candidate *Account) bool {
	allowed, _ := ctx.Value(adaptiveStickyTrialKey{}).(func(*Account) bool)
	return allowed == nil || allowed(candidate)
}

func acquiredAdaptiveStickyAlternative(ctx context.Context, selection *AccountSelectionResult, err error) bool {
	if err == nil && selection != nil && selection.Acquired && adaptiveStickyTrialAllows(ctx, selection.Account) {
		return true
	}
	if selection != nil && selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	return false
}

func adaptiveHardContinuity(ctx context.Context) bool {
	route, enabled := gatewayAdaptiveRoute(ctx)
	return enabled && (route.protected || route.transport == "ws")
}

func adaptiveGatewayStickyEscape(ctx context.Context, account *Account, model string) bool {
	return ctx.Value(adaptiveStickyNeedsSlotKey{}) == nil && account != nil &&
		adaptiveStickyEscape(ctx, account, adaptiveMappedModel(ctx, model)(account))
}

func adaptiveStickyEscape(ctx context.Context, account *Account, model string) bool {
	route, enabled := gatewayAdaptiveRoute(ctx)
	if !enabled || route.protected || !route.replayable || account == nil {
		return false
	}
	model = adaptiveSelectionModel(ctx, model)
	route.tier = adaptiveCandidateTier(ctx, route, account, model)
	key := adaptiveContextKey(ctx, route, account, model)
	telemetry := adaptiveTelemetryForContext(ctx)
	telemetry.mu.Lock()
	defer telemetry.mu.Unlock()
	series := telemetry.series[key]
	return series != nil && series.health.Fault != adaptiveHealthy && series.health.Passed == 0 &&
		time.Since(series.health.Last) < adaptiveWindow
}
