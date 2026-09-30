//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestClassifyOpenAIAccountFailureAttribution(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		attributable bool
		reason       openAIAccountFailureAttribution
	}{
		{
			name:         "account authentication",
			err:          &UpstreamFailoverError{Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeAccount},
			attributable: true,
			reason:       openAIAccountFailureAuthentication,
		},
		{
			name: "structured account quota",
			err: newOpenAIUpstreamFailoverError(http.StatusTooManyRequests, nil,
				[]byte(`{"error":{"code":"insufficient_quota"}}`), "", false),
			attributable: true,
			reason:       openAIAccountFailureQuota,
		},
		{
			name: "structured account permission",
			err: newOpenAIUpstreamFailoverError(http.StatusForbidden, nil,
				[]byte(`{"error":{"code":"permission_denied"}}`), "", false),
			attributable: true,
			reason:       openAIAccountFailurePermission,
		},
		{
			name:         "account proxy authentication",
			err:          &UpstreamFailoverError{accountFailureAttribution: openAIAccountFailureProxyAuthentication},
			attributable: true,
			reason:       openAIAccountFailureProxyAuthentication,
		},
		{
			name:         "first output timeout",
			err:          &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, accountFailureAttribution: openAIAccountFailureFirstOutputTimeout},
			attributable: true,
			reason:       openAIAccountFailureFirstOutputTimeout,
		},
		{
			name:         "first output timeout structured wire shape",
			err:          &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, ResponseBody: []byte(`{"error":{"type":"first_output_timeout"}}`)},
			attributable: true,
			reason:       openAIAccountFailureFirstOutputTimeout,
		},
		{
			name:   "request scope overrides account auth stage",
			err:    &UpstreamFailoverError{Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeRequest},
			reason: openAIAccountFailureRequestScope,
		},
		{
			name:   "provider scope",
			err:    &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable, Scope: GatewayFailureScopeProvider},
			reason: openAIAccountFailureProviderScope,
		},
		{
			name:   "same account transient retry",
			err:    &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests, RetryableOnSameAccount: true},
			reason: openAIAccountFailureCapacity,
		},
		{
			name:   "request body too large overrides account scope",
			err:    newOpenAIUpstreamFailoverError(http.StatusRequestEntityTooLarge, nil, nil, "", false),
			reason: openAIAccountFailureRequestTooLarge,
		},
		{
			name:   "structured model not found",
			err:    newOpenAIUpstreamFailoverError(http.StatusBadRequest, nil, []byte(`{"error":{"code":"model_not_found"}}`), "", false),
			reason: openAIAccountFailureModelUnavailable,
		},
		{
			name:   "unknown structured model 404",
			err:    &UpstreamFailoverError{StatusCode: http.StatusNotFound, ResponseBody: []byte(`{"error":{"code":"model_not_found"}}`)},
			reason: openAIAccountFailureModelUnavailable,
		},
		{
			name:   "context window",
			err:    newOpenAIUpstreamFailoverError(http.StatusBadRequest, nil, []byte(`{"error":{"code":"context_length_exceeded"}}`), "", false),
			reason: openAIAccountFailureRequestInvalid,
		},
		{
			name:   "invalid parameter",
			err:    newOpenAIUpstreamFailoverError(http.StatusBadRequest, nil, []byte(`{"error":{"type":"invalid_request_error","param":"temperature"}}`), "", false),
			reason: openAIAccountFailureRequestInvalid,
		},
		{
			name:   "cyber policy",
			err:    newOpenAIUpstreamFailoverError(http.StatusBadGateway, nil, []byte(`{"error":{"code":"cyber_policy"}}`), "", false),
			reason: openAIAccountFailureRequestInvalid,
		},
		{
			name:   "local fast policy block",
			err:    &OpenAIFastBlockedError{Message: "local service tier restriction"},
			reason: openAIAccountFailureRequestInvalid,
		},
		{
			name:   "typed pass-through request result",
			err:    withOpenAIAccountFailureAttribution(errors.New("pass-through error"), openAIAccountFailureRequestInvalid),
			reason: openAIAccountFailureRequestInvalid,
		},
		{
			name:   "provider capacity",
			err:    newOpenAIUpstreamFailoverError(http.StatusServiceUnavailable, nil, []byte(`{"error":{"code":"server_is_overloaded"}}`), "", false),
			reason: openAIAccountFailureCapacity,
		},
		{
			name:   "silent refusal",
			err:    &UpstreamFailoverError{StatusCode: http.StatusBadGateway, ResponseBody: openAISilentRefusalErrorBody()},
			reason: openAIAccountFailureSilentRefusal,
		},
		{
			name:   "provider 503 without account evidence",
			err:    &UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable},
			reason: openAIAccountFailureProviderResponse,
		},
		{
			name:   "ordinary 429 without quota evidence",
			err:    &UpstreamFailoverError{StatusCode: http.StatusTooManyRequests},
			reason: openAIAccountFailureUnknown,
		},
		{
			name:   "ambiguous error",
			err:    errors.New("upstream request failed"),
			reason: openAIAccountFailureUnknown,
		},
		{
			name:   "failure without an error object",
			reason: openAIAccountFailureNoObservedError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyOpenAIAccountFailure(tt.err)
			require.Equal(t, tt.attributable, got.attributable)
			require.Equal(t, tt.reason, got.reason)
			if failoverErr, ok := tt.err.(*UpstreamFailoverError); ok {
				require.Equal(t, tt.attributable, failoverErr.ShouldReportAccountScheduleFailure())
			}
		})
	}
}

func TestOpenAIAccountRuntimeStats_NeutralFailureDoesNotMutateSmartHealth(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	const accountID int64 = 88101
	now := time.Unix(1780000000, 0)
	stat := openAIAccountRuntimeStatForTest(t, stats, accountID)
	stat.errorRateEWMABits.Store(math.Float64bits(0.4))
	stat.ttftEWMABits.Store(math.Float64bits(250))
	stat.sampleCount.Store(7)
	stat.lastObservedAt.Store(now.UnixNano())
	stat.mu.Lock()
	stat.smartMode = true
	stat.smartLevel = openAIAccountHealthDegraded
	stat.smartReason = openAIAccountHealthReasonErrorRate
	stat.smartErrorWindow = []openAISmartTimedSample{{at: now.Add(-time.Second), value: 1}, {at: now, value: 0}}
	stat.smartTTFTWindow = []openAISmartTimedSample{{at: now.Add(-time.Second), value: 250}}
	stat.smartLastObserved = now
	stat.nextProbeAt.Store(now.Add(time.Minute).UnixNano())
	stat.probeBatchStarted = now
	stat.probeBatchSamples = []openAISmartProbeSample{{success: true, ttftMs: 180, hasTTFT: true, observed: now}}
	stat.probeID = 17
	stat.probeStartedAt = now
	stat.probeLeaseUntil.Store(now.Add(openAIAccountHealthProbeLease).UnixNano())
	stat.probeActive.Store(true)
	stat.mu.Unlock()

	beforeErrorEWMA := stat.errorRateEWMABits.Load()
	beforeTTFTEWMA := stat.ttftEWMABits.Load()
	beforeSampleCount := stat.sampleCount.Load()
	beforeLastObserved := stat.lastObservedAt.Load()
	beforeNextProbe := stat.nextProbeAt.Load()
	neutralTTFT := 900
	for i := 0; i < 3; i++ {
		stats.reportAtForRequest(accountID, false, &neutralTTFT, now.Add(time.Duration(i+1)*time.Second), openAIStickyEscapeConfig{}, false, true, 17, false)
	}

	require.Equal(t, beforeErrorEWMA, stat.errorRateEWMABits.Load())
	require.Equal(t, beforeTTFTEWMA, stat.ttftEWMABits.Load())
	require.Equal(t, beforeSampleCount, stat.sampleCount.Load())
	require.Equal(t, beforeLastObserved, stat.lastObservedAt.Load())
	require.Equal(t, beforeNextProbe, stat.nextProbeAt.Load())
	stat.mu.Lock()
	require.Equal(t, openAIAccountHealthDegraded, stat.smartLevel)
	require.Equal(t, now, stat.smartLastObserved)
	require.Len(t, stat.smartErrorWindow, 2)
	require.Len(t, stat.smartTTFTWindow, 1)
	require.Len(t, stat.probeBatchSamples, 1)
	require.Zero(t, stat.probeID, "the matching neutral probe lease is released")
	require.False(t, stat.probeActive.Load())
	require.Zero(t, stat.probeLeaseUntil.Load())
	stat.mu.Unlock()
}

func TestReportOpenAIAccountScheduleResult_NeutralFailuresDoNotAddSmartSamples(t *testing.T) {
	schedulerService := &OpenAIGatewayService{
		rateLimitService:   newIsolatedOpenAISchedulerModeRateLimitServiceForTest(t, true, false),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	account := &Account{ID: 88105, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	scheduler, ok := schedulerService.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, account.ID)
	stat.errorRateEWMABits.Store(math.Float64bits(0.35))
	stat.ttftEWMABits.Store(math.Float64bits(320))
	stat.sampleCount.Store(8)
	lastObservedAt := time.Unix(1780001000, 0)
	stat.lastObservedAt.Store(lastObservedAt.UnixNano())
	beforeErrorEWMA := stat.errorRateEWMABits.Load()
	beforeTtftEWMA := stat.ttftEWMABits.Load()
	beforeSampleCount := stat.sampleCount.Load()
	beforeLastObservedAt := stat.lastObservedAt.Load()
	neutralErrors := []error{
		nil,
		&UpstreamFailoverError{StatusCode: http.StatusServiceUnavailable},
		errors.New("upstream request failed"),
	}
	for _, observedErr := range neutralErrors {
		schedulerService.ReportOpenAIAccountScheduleResult(account, "model", false, nil, observedErr)
	}
	for _, observedErr := range neutralErrors[1:] {
		schedulerService.ObserveOpenAIAccountHealthFailure(context.Background(), account, observedErr)
	}

	require.Equal(t, beforeErrorEWMA, stat.errorRateEWMABits.Load())
	require.Equal(t, beforeTtftEWMA, stat.ttftEWMABits.Load())
	require.Equal(t, beforeSampleCount, stat.sampleCount.Load())
	require.Equal(t, beforeLastObservedAt, stat.lastObservedAt.Load())
	stat.mu.Lock()
	require.Empty(t, stat.smartErrorWindow)
	require.Empty(t, stat.smartTTFTWindow)
	require.NotEqual(t, openAIAccountHealthDegraded, stat.smartLevel)
	stat.mu.Unlock()
}

func TestOpenAIAccountRuntimeStats_NeutralProbeReleaseOnlyMatchesProbeID(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	const accountID int64 = 88102
	now := time.Unix(1780000000, 0)
	stat := openAIAccountRuntimeStatForTest(t, stats, accountID)
	stat.mu.Lock()
	stat.probeID = 29
	stat.probeActive.Store(true)
	stat.probeLeaseUntil.Store(now.Add(time.Minute).UnixNano())
	stat.mu.Unlock()

	stats.reportAtForRequest(accountID, false, nil, now.Add(time.Second), openAIStickyEscapeConfig{}, false, true, 28, false)
	stat.mu.Lock()
	require.Equal(t, uint64(29), stat.probeID, "a late neutral result cannot release a newer lease")
	require.True(t, stat.probeActive.Load())
	stat.mu.Unlock()

	stats.reportAtForRequest(accountID, false, nil, now.Add(2*time.Second), openAIStickyEscapeConfig{}, false, true, 29, false)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	require.False(t, stat.probeActive.Load())
	stat.mu.Unlock()
}

func TestOpenAIAccountRuntimeStats_NeutralFailurePreservesLegacyObservation(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	const accountID int64 = 88103
	now := time.Unix(1780000000, 0)
	stats.reportAtForRequest(accountID, false, nil, now, openAIStickyEscapeConfig{}, false, false, 0, false)
	stat := openAIAccountRuntimeStatForTest(t, stats, accountID)
	require.EqualValues(t, 1, stat.sampleCount.Load())
	require.InDelta(t, 0.2, math.Float64frombits(stat.errorRateEWMABits.Load()), 1e-12)
}

func TestReportOpenAIAccountScheduleResult_ImagesUserErrorPreservesLegacyButIsNeutralInSmartMode(t *testing.T) {
	imageErr := &OpenAIImagesUpstreamError{StatusCode: http.StatusBadRequest, ErrorType: "invalid_request_error", Code: "invalid_value", Message: "invalid image request"}
	account := &Account{ID: 88104, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	legacyService := newIsolatedSmartSchedulerTestServiceForTest(t, []Account{*account}, schedulerTestConcurrencyCache{})
	legacyService.rateLimitService = newIsolatedOpenAISchedulerModeRateLimitServiceForTest(t, false, true)
	legacyService.ReportOpenAIAccountScheduleResult(account, "image-model", true, nil, imageErr)
	legacyScheduler, ok := legacyService.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	legacyStat := openAIAccountRuntimeStatForTest(t, legacyScheduler.stats, account.ID)
	require.EqualValues(t, 1, legacyStat.sampleCount.Load())
	require.InDelta(t, 0, math.Float64frombits(legacyStat.errorRateEWMABits.Load()), 1e-12, "S=false retains the historical success observation for completed client errors")

	smartAccount := *account
	smartAccount.ID++
	smartService := newIsolatedSmartSchedulerTestServiceForTest(t, []Account{smartAccount}, schedulerTestConcurrencyCache{})
	smartService.ReportOpenAIAccountScheduleResult(&smartAccount, "image-model", true, nil, imageErr)
	smartScheduler, ok := smartService.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	smartStat := openAIAccountRuntimeStatForTest(t, smartScheduler.stats, smartAccount.ID)
	require.Zero(t, smartStat.sampleCount.Load(), "S=true treats the same request error as neutral")
	smartStat.mu.Lock()
	require.Empty(t, smartStat.smartErrorWindow)
	require.Empty(t, smartStat.smartTTFTWindow)
	smartStat.mu.Unlock()
}

func TestOpenAIAccountFailureEvidenceEquivalentSources(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		code, typ string
		reason    openAIAccountFailureAttribution
	}{
		{"permission", 403, "permission_denied", "invalid_request_error", openAIAccountFailurePermission},
		{"permissions", 403, "insufficient_permissions", "invalid_request_error", openAIAccountFailurePermission},
		{"generic401", 401, "", "invalid_request_error", openAIAccountFailureAuthentication},
		{"parameter401", 401, "invalid_parameter", "invalid_request_error", openAIAccountFailureRequestInvalid},
		{"context401", 401, "context_length_exceeded", "", openAIAccountFailureRequestInvalid},
		{"model401", 401, "model_not_found", "", openAIAccountFailureModelUnavailable},
		{"capacity401", 401, "server_is_overloaded", "", openAIAccountFailureCapacity},
		{"cyber401", 401, "cyber_policy", "", openAIAccountFailureRequestInvalid},
		{"conflicting", 403, "permission_denied", "invalid_parameter", openAIAccountFailureConflictingEvidence},
		{"conflicting_accounts", 403, "permission_denied", "invalid_api_key", openAIAccountFailureConflictingEvidence},
		{"size", 413, "permission_denied", "", openAIAccountFailureRequestTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"error": map[string]string{"code": tc.code, "type": tc.typ}})
			require.NoError(t, err)
			for name, source := range map[string]error{
				"annotated": newOpenAIUpstreamFailoverError(tc.status, nil, body, "", false),
				"direct":    &UpstreamFailoverError{StatusCode: tc.status, ResponseBody: body},
				"images":    &OpenAIImagesUpstreamError{StatusCode: tc.status, Code: tc.code, ErrorType: tc.typ},
			} {
				t.Run(name, func(t *testing.T) {
					got := classifyOpenAIAccountFailure(source)
					require.Equal(t, tc.reason, got.reason)
					require.Equal(t, tc.reason.isAccountFailure(), got.attributable)
				})
			}
			for _, scope := range []GatewayFailureScope{GatewayFailureScopeRequest, GatewayFailureScopeProvider} {
				source := newOpenAIUpstreamFailoverError(tc.status, nil, body, "", false)
				source.Scope = scope
				require.False(t, classifyOpenAIAccountFailure(source).attributable)
			}
		})
	}
}

func TestOpenAIAccountPermissionReportAndObserveCountOnce(t *testing.T) {
	for _, code := range []string{"permission_denied", "insufficient_permissions"} {
		t.Run(code, func(t *testing.T) {
			rate := newIsolatedOpenAISchedulerModeRateLimitServiceForTest(t, true, true)
			encoded, err := json.Marshal(OpenAIAPIKeyHealthBreakerSettings{Enabled: true, WindowMinutes: 1, FailureThreshold: 3, CooldownMinutes: 5})
			require.NoError(t, err)
			rate.settingService = NewSettingService(&openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
				SettingKeySmartSchedulingEnabled: "true", openAIAdvancedSchedulerSettingKey: "true",
				SettingKeyOpenAIAPIKeyHealthBreakerSettings: string(encoded),
			}}, &config.Config{})
			cache := &openAIAPIKeyHealthCacheStub{}
			rate.accountRepo = &openAIAPIKeyHealthAccountRepo{}
			rate.SetOpenAIAPIKeyHealthCache(cache)
			svc := &OpenAIGatewayService{rateLimitService: rate, openaiAccountStats: newOpenAIAccountRuntimeStats()}
			account := openAIHealthPoolAccount()
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "invalid_request_error", "code": code}})
			source := newOpenAIUpstreamFailoverError(403, nil, body, "", false)
			svc.ReportOpenAIAccountScheduleResult(account, "model", false, nil, source)
			svc.ObserveOpenAIAccountHealthFailure(context.Background(), account, source)
			svc.ReportOpenAIAccountScheduleResult(account, "model", false, nil, source)
			scheduler := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
			stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, account.ID)
			require.EqualValues(t, 1, stat.sampleCount.Load())
			require.Equal(t, 1, cache.recordCalls)
			stat.mu.Lock()
			require.Len(t, stat.smartErrorWindow, 1)
			stat.mu.Unlock()
		})
	}
}

type openAIProductionAttributionCase struct {
	name      string
	status    int
	code, typ string
	headers   http.Header
	reason    openAIAccountFailureAttribution
}

func openAIProductionAttributionCases() []openAIProductionAttributionCase {
	quotaHeaders := http.Header{}
	quotaHeaders.Set("x-codex-primary-used-percent", "100")
	return []openAIProductionAttributionCase{
		{"402_parameter", 402, "invalid_parameter", "", nil, openAIAccountFailureRequestInvalid},
		{"429_conflicting", 429, "insufficient_quota", "invalid_parameter", nil, openAIAccountFailureConflictingEvidence},
		{"402_model", 402, "model_not_found", "", nil, openAIAccountFailureModelUnavailable},
		{"429_context", 429, "context_length_exceeded", "", nil, openAIAccountFailureRequestInvalid},
		{"402_cyber", 402, "cyber_policy", "", nil, openAIAccountFailureRequestInvalid},
		{"429_capacity", 429, "server_is_overloaded", "", nil, openAIAccountFailureCapacity},
		{"quota_body", 429, "insufficient_quota", "", nil, openAIAccountFailureQuota},
		{"quota_402", 402, "", "", nil, openAIAccountFailureQuota},
		{"quota_header", 429, "", "", quotaHeaders, openAIAccountFailureQuota},
		{"quota_header_parameter", 429, "invalid_parameter", "", quotaHeaders, openAIAccountFailureRequestInvalid},
		{"ordinary_429", 429, "", "", nil, openAIAccountFailureUnknown},
		{"permission_403", 403, "permission_denied", "invalid_request_error", nil, openAIAccountFailurePermission},
		{"permissions_403", 403, "insufficient_permissions", "invalid_request_error", nil, openAIAccountFailurePermission},
		{"generic_401", 401, "", "invalid_request_error", nil, openAIAccountFailureAuthentication},
		{"parameter_401", 401, "invalid_parameter", "", nil, openAIAccountFailureRequestInvalid},
		{"413", 413, "insufficient_quota", "", nil, openAIAccountFailureRequestTooLarge},
	}
}

func openAIProductionAttributionBody(tc openAIProductionAttributionCase) []byte {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": tc.code, "type": tc.typ}})
	return body
}

func newOpenAIProductionAttributionSource(svc *OpenAIGatewayService, account *Account, tc openAIProductionAttributionCase, source string) error {
	body := openAIProductionAttributionBody(tc)
	switch source {
	case "service":
		return svc.newOpenAIAccountFailoverError(account, tc.status, tc.headers, body, "", false, false)
	case "classification_headers":
		public := http.Header{"X-Request-Id": []string{"public-request"}}
		// The public headers deliberately suggest quota even when the semantic
		// classification headers are nil (the successful-stream 429 case).
		public.Set("x-codex-primary-used-percent", "100")
		return svc.newOpenAIAccountFailoverErrorWithClassificationHeaders(account, tc.status, public, tc.headers, body, "", false, false)
	case "pure":
		return newOpenAIUpstreamFailoverError(tc.status, tc.headers, body, "", false)
	case "direct":
		return &UpstreamFailoverError{StatusCode: tc.status, ResponseHeaders: tc.headers.Clone(), ResponseBody: body}
	case "images":
		return &OpenAIImagesUpstreamError{StatusCode: tc.status, Code: tc.code, ErrorType: tc.typ}
	default:
		panic("unknown test source")
	}
}

func TestOpenAIProductionFactoriesPreserveAttributionSources(t *testing.T) {
	for _, tc := range openAIProductionAttributionCases() {
		t.Run(tc.name, func(t *testing.T) {
			account := openAIHealthPoolAccount()
			svc := &OpenAIGatewayService{}
			for _, source := range []string{"service", "classification_headers", "pure", "direct", "images"} {
				if source == "images" && len(tc.headers) > 0 {
					continue
				}
				t.Run(source, func(t *testing.T) {
					err := newOpenAIProductionAttributionSource(svc, account, tc, source)
					got := classifyOpenAIAccountFailure(err)
					require.Equal(t, tc.reason, got.reason)
					require.Equal(t, tc.reason.isAccountFailure(), got.attributable)
					if fo, ok := err.(*UpstreamFailoverError); ok {
						require.Equal(t, tc.status, fo.StatusCode)
						require.Equal(t, openAIProductionAttributionBody(tc), fo.ResponseBody)
						if source == "classification_headers" {
							require.Equal(t, "public-request", fo.ResponseHeaders.Get("X-Request-Id"))
							require.Equal(t, "100", fo.ResponseHeaders.Get("x-codex-primary-used-percent"))
						} else {
							require.Equal(t, tc.headers, fo.ResponseHeaders)
						}
						for _, scope := range []GatewayFailureScope{GatewayFailureScopeRequest, GatewayFailureScopeProvider} {
							scoped := newOpenAIProductionAttributionSource(svc, account, tc, source).(*UpstreamFailoverError)
							scoped.Scope = scope
							require.False(t, classifyOpenAIAccountFailure(scoped).attributable)
						}
					}
				})
			}
		})
	}
}

func TestOpenAIProductionFactoryReportObserveCounts(t *testing.T) {
	for _, tc := range openAIProductionAttributionCases() {
		for _, source := range []string{"service", "classification_headers"} {
			for _, scope := range []GatewayFailureScope{"", GatewayFailureScopeRequest, GatewayFailureScopeProvider} {
				t.Run(tc.name+"/"+source+"/"+string(scope), func(t *testing.T) {
					rate := newIsolatedOpenAISchedulerModeRateLimitServiceForTest(t, true, true)
					encoded, err := json.Marshal(OpenAIAPIKeyHealthBreakerSettings{Enabled: true, WindowMinutes: 1, FailureThreshold: 3, CooldownMinutes: 5})
					require.NoError(t, err)
					rate.settingService = NewSettingService(&openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
						SettingKeySmartSchedulingEnabled: "true", openAIAdvancedSchedulerSettingKey: "true",
						SettingKeyOpenAIAPIKeyHealthBreakerSettings: string(encoded),
					}}, &config.Config{})
					rate.accountRepo = &openAIAPIKeyHealthAccountRepo{}
					cache := &openAIAPIKeyHealthCacheStub{}
					rate.SetOpenAIAPIKeyHealthCache(cache)
					svc := &OpenAIGatewayService{rateLimitService: rate, openaiAccountStats: newOpenAIAccountRuntimeStats()}
					account := openAIHealthPoolAccount()
					err = newOpenAIProductionAttributionSource(svc, account, tc, source)
					fo := err.(*UpstreamFailoverError)
					fo.Scope = scope
					scheduler := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
					stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, account.ID)
					beforeError, beforeTTFT := stat.errorRateEWMABits.Load(), stat.ttftEWMABits.Load()
					beforeObserved := stat.lastObservedAt.Load()
					svc.ReportOpenAIAccountScheduleResult(account, "model", false, nil, err)
					svc.ObserveOpenAIAccountHealthFailure(context.Background(), account, err)
					svc.ReportOpenAIAccountScheduleResult(account, "model", false, nil, err)
					want := 0
					if scope == "" && tc.reason.isAccountFailure() {
						want = 1
					}
					require.EqualValues(t, want, stat.sampleCount.Load())
					require.Equal(t, want, cache.recordCalls)
					if want == 0 {
						require.Equal(t, beforeError, stat.errorRateEWMABits.Load())
						require.Equal(t, beforeTTFT, stat.ttftEWMABits.Load())
						require.Equal(t, beforeObserved, stat.lastObservedAt.Load())
					}
					stat.mu.Lock()
					require.Len(t, stat.smartErrorWindow, want)
					require.Empty(t, stat.smartTTFTWindow)
					stat.mu.Unlock()
				})
			}
		}
	}
}

func TestOpenAIProductionFactoryKeepsOAuthRetryAndPublicFields(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 99401, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	public := http.Header{"Retry-After": []string{"2"}, "X-Request-Id": []string{"public-request"}}
	public.Set("x-codex-primary-used-percent", "100")
	tc := openAIProductionAttributionCase{status: 429}
	body := openAIProductionAttributionBody(tc)
	before := time.Now()
	fo := svc.newOpenAIAccountFailoverErrorWithClassificationHeaders(account, 429, public, nil, body, "", false, false)
	require.True(t, fo.RetryableOnSameAccount)
	require.False(t, fo.RequestScopedTransient)
	require.WithinDuration(t, before.Add(openAIOAuth429RetryWindow), fo.SameAccountRetryDeadline, time.Second)
	require.InDelta(t, 2*time.Second, fo.SameAccountRetryDelay, float64(time.Second))
	require.Equal(t, public, fo.ResponseHeaders)
	require.Equal(t, body, fo.ResponseBody)
	require.Empty(t, fo.Scope)
	require.Zero(t, fo.ClientStatusCode)
	require.Empty(t, fo.ClientMessage)
	require.False(t, classifyOpenAIAccountFailure(fo).attributable)
}

func TestOpenAIAttributionFixtureLifecycleRestoresFirstOutputTimeout(t *testing.T) {
	isolateOpenAISchedulerSettingsForTest(t)
	t.Run("owner publishes smart thirty", func(t *testing.T) {
		rate := newIsolatedOpenAISchedulerModeRateLimitServiceForTest(t, true, true)
		svc := &OpenAIGatewayService{rateLimitService: rate}
		require.Equal(t, 30*time.Second, svc.openAIFirstOutputTimeoutForRequest(context.Background(), nil, ""))
		cached, ok := loadCurrentOpenAIAdvancedSchedulerSettingsCache()
		require.True(t, ok)
		require.True(t, cached.smartEnabled)
		require.Equal(t, 30, cached.firstOutputTimeoutSeconds)
	})
	_, ok := loadCurrentOpenAIAdvancedSchedulerSettingsCache()
	require.False(t, ok, "the completed owner must leave no valid old snapshot")
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	require.Equal(t, time.Second, svc.openAIFirstOutputTimeoutForRequest(context.Background(), nil, ""))
}

func TestOpenAIAttributionFixtureLateRefreshCannotRepublishOldGeneration(t *testing.T) {
	isolateOpenAISchedulerSettingsForTest(t)
	release := make(chan struct{})
	var once sync.Once
	repo := &schedulerCacheTestSettingsRepo{
		values:         schedulerCacheTestSettingsValues(true, true, "2", "1", "1"),
		blockBatchCall: 1, batchStarted: make(chan struct{}), releaseBatch: release,
	}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: NewSettingService(repo, &config.Config{})}}
	done := make(chan struct{})
	result := make(chan openAIAdvancedSchedulerRuntimeSettings, 1)
	// LIFO cleanup joins the blocked worker before the owner's cache reset.
	t.Cleanup(func() { once.Do(func() { close(release) }); <-done })
	go func() { defer close(done); result <- svc.openAIAdvancedSchedulerRuntimeSettings(context.Background()) }()
	<-repo.batchStarted
	oldGeneration := openAIAdvancedSchedulerSettingGeneration.Load()
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	_, ok := loadCurrentOpenAIAdvancedSchedulerSettingsCache()
	require.False(t, ok)
	fresh := &OpenAIGatewayService{cfg: &config.Config{}}
	fresh.cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 1
	require.Equal(t, time.Second, fresh.openAIFirstOutputTimeoutForRequest(context.Background(), nil, ""))
	generation := openAIAdvancedSchedulerSettingGeneration.Load()
	require.Greater(t, generation, oldGeneration)
	once.Do(func() { close(release) })
	settings := <-result
	require.False(t, settings.smartEnabled)
	<-done
	cached, ok := loadCurrentOpenAIAdvancedSchedulerSettingsCache()
	require.True(t, ok)
	require.Equal(t, generation, cached.generation)
	require.False(t, cached.smartEnabled)
	require.Equal(t, time.Second, fresh.openAIFirstOutputTimeoutForRequest(context.Background(), nil, ""))
}
