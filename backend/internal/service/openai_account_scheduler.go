package service

import (
	"container/heap"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"golang.org/x/sync/singleflight"
)

var (
	openAIProbeProcessTraceNamespace, openAIProbeProcessTraceCorrelationAvailable = openAIProbeTraceNamespaceFrom(rand.Reader)
	openAIProbeTraceInstanceSequence                                              atomic.Uint64
)

func openAIProbeTraceNamespaceFrom(source io.Reader) (string, bool) {
	var namespace [16]byte
	if source == nil {
		return "", false
	}
	if _, err := io.ReadFull(source, namespace[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(namespace[:]), true
}

const (
	openAIAccountScheduleLayerPreviousResponse = "previous_response_id"
	openAIAccountScheduleLayerGuardianParent   = "guardian_parent"
	openAIAccountScheduleLayerSessionSticky    = "session_hash"
	openAIAccountScheduleLayerLoadBalance      = "load_balance"
	openAIAdvancedSchedulerSettingKey          = "openai_advanced_scheduler_enabled"
)

const (
	openAIAdvancedSchedulerSettingCacheTTL    = 5 * time.Second
	openAIAdvancedSchedulerSettingDBTimeout   = 2 * time.Second
	openAIAccountHealthMinSamples             = int64(10)
	openAIAccountLegacyHealthMinSamples       = int64(3)
	openAIAccountHealthStaleAfter             = 10 * time.Minute
	openAIAccountLegacyProbeCooldown          = 30 * time.Second
	openAIAccountHealthProbeLease             = 30 * time.Second
	openAIAccountSmartTTFTThreshold           = 10 * time.Second
	openAIAccountSmartWindowSize              = 3
	openAIAccountSmartWindowDuration          = 10 * time.Minute
	openAIAccountSmartProbeBatchLimit         = 10
	openAIAccountSmartProbeBatchTimeout       = 10 * time.Minute
	openAIAccountRuntimeStatsReclaimInterval  = 15 * time.Minute
	openAIAccountRuntimeStatsReclaimIdle      = 30 * time.Minute
	openAIAccountRuntimeStatsReclaimBatchSize = 100
	openAIAccountRuntimeStatsReclaimTimeout   = 2 * time.Second
	// ponytail: cap probes added when cost ordering expands configured Top-K;
	// use bulk acquisition if a measured workload needs a higher ceiling.
	openAIAccountSelectionProbeLimit = 64
)

var openAIAccountHealthProbeCooldowns = [...]time.Duration{
	time.Minute,
	5 * time.Minute,
	10 * time.Minute,
	30 * time.Minute,
	time.Hour,
}

const (
	openAIQuotaHeadroomNeutralFactor           = 0.5
	openAIQuotaHeadroomSecondaryLowRemain      = 0.10
	openAIQuotaHeadroomSnapshotStaleAfter      = 8 * time.Hour
	openAIUpstreamCostNeutralFactor            = 0.5
	defaultOpenAIOAuthSchedulingRateMultiplier = 1.0
)

type cachedOpenAIAdvancedSchedulerSetting struct {
	lowUpstreamRatePriorityEnabled bool
	smartEnabled                   bool
	smartEnabledKnown              bool
	firstOutputTimeoutSeconds      int
	oauthSchedulingRateMultiplier  *float64
	enabled                        bool
	stickyWeightedEnabled          bool
	subscriptionPriorityEnabled    bool
	lbTopKOverride                 int
	weightOverrides                map[string]float64
	generation                     uint64
	expiresAt                      int64
}

type openAIAdvancedSchedulerRuntimeSettings struct {
	lowUpstreamRatePriorityEnabled bool
	smartEnabled                   bool
	smartEnabledKnown              bool
	firstOutputTimeoutSeconds      int
	oauthSchedulingRateMultiplier  *float64
	enabled                        bool
	stickyWeightedEnabled          bool
	subscriptionPriorityEnabled    bool
	lbTopKOverride                 int
	weightOverrides                map[string]float64
}

var openAIAdvancedSchedulerSettingCache atomic.Value // *cachedOpenAIAdvancedSchedulerSetting
var openAIAdvancedSchedulerSettingSF singleflight.Group
var openAIAdvancedSchedulerSettingCachePublishMu sync.Mutex
var openAIAdvancedSchedulerSettingGeneration atomic.Uint64
var openAIAdvancedSchedulerSettingWriteMu sync.Mutex

type openAIAdvancedSchedulerSettingsReadResult struct {
	generation uint64
	settings   openAIAdvancedSchedulerRuntimeSettings
}

func cachedOpenAIAdvancedSchedulerRuntimeSettings(cached *cachedOpenAIAdvancedSchedulerSetting) openAIAdvancedSchedulerRuntimeSettings {
	return openAIAdvancedSchedulerRuntimeSettings{
		lowUpstreamRatePriorityEnabled: cached.lowUpstreamRatePriorityEnabled,
		smartEnabled:                   cached.smartEnabled,
		smartEnabledKnown:              cached.smartEnabledKnown,
		firstOutputTimeoutSeconds:      cached.firstOutputTimeoutSeconds,
		oauthSchedulingRateMultiplier:  cached.oauthSchedulingRateMultiplier,
		enabled:                        cached.enabled,
		stickyWeightedEnabled:          cached.stickyWeightedEnabled,
		subscriptionPriorityEnabled:    cached.subscriptionPriorityEnabled,
		lbTopKOverride:                 cached.lbTopKOverride,
		weightOverrides:                cloneOpenAIAdvancedSchedulerWeightOverrides(cached.weightOverrides),
	}
}

func loadCurrentOpenAIAdvancedSchedulerSettingsCache() (*cachedOpenAIAdvancedSchedulerSetting, bool) {
	cached, ok := openAIAdvancedSchedulerSettingCache.Load().(*cachedOpenAIAdvancedSchedulerSetting)
	if !ok || cached == nil || cached.generation != openAIAdvancedSchedulerSettingGeneration.Load() || time.Now().UnixNano() >= cached.expiresAt {
		return nil, false
	}
	return cached, true
}

// invalidateOpenAIAdvancedSchedulerSettingsAfterWrite advances the local
// generation only after persistence succeeds, and makes every prior snapshot
// unusable before the writer rebuilds the complete post-write value.
func invalidateOpenAIAdvancedSchedulerSettingsAfterWrite() uint64 {
	openAIAdvancedSchedulerSettingCachePublishMu.Lock()
	generation := openAIAdvancedSchedulerSettingGeneration.Add(1)
	openAIAdvancedSchedulerSettingSF.Forget(openAIAdvancedSchedulerSettingKey)
	openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{generation: generation})
	openAIAdvancedSchedulerSettingCachePublishMu.Unlock()
	return generation
}

func publishOpenAIAdvancedSchedulerSettingsAfterWrite(settings *SystemSettings) {
	if settings == nil {
		return
	}
	openAIAdvancedSchedulerSettingCachePublishMu.Lock()
	generation := openAIAdvancedSchedulerSettingGeneration.Load()
	openAIAdvancedSchedulerSettingCache.Store(&cachedOpenAIAdvancedSchedulerSetting{
		lowUpstreamRatePriorityEnabled: settings.OpenAILowUpstreamRatePriorityEnabled,
		smartEnabled:                   settings.SmartSchedulingEnabled,
		smartEnabledKnown:              true,
		firstOutputTimeoutSeconds:      settings.OpenAIFirstOutputTimeoutSeconds,
		oauthSchedulingRateMultiplier:  settings.OpenAIOAuthSchedulingRateMultiplier,
		enabled:                        settings.OpenAIAdvancedSchedulerEnabled,
		stickyWeightedEnabled:          settings.OpenAIAdvancedSchedulerStickyWeightedEnabled,
		subscriptionPriorityEnabled:    settings.OpenAIAdvancedSchedulerSubscriptionPriorityEnabled,
		lbTopKOverride:                 parsePositiveIntOverride(settings.OpenAIAdvancedSchedulerLBTopK),
		weightOverrides: cloneOpenAIAdvancedSchedulerWeightOverrides(parseOpenAIAdvancedSchedulerWeightOverrides(map[string]string{
			SettingKeyOpenAIAdvancedSchedulerWeightPriority:         settings.OpenAIAdvancedSchedulerWeightPriority,
			SettingKeyOpenAIAdvancedSchedulerWeightLoad:             settings.OpenAIAdvancedSchedulerWeightLoad,
			SettingKeyOpenAIAdvancedSchedulerWeightQueue:            settings.OpenAIAdvancedSchedulerWeightQueue,
			SettingKeyOpenAIAdvancedSchedulerWeightErrorRate:        settings.OpenAIAdvancedSchedulerWeightErrorRate,
			SettingKeyOpenAIAdvancedSchedulerWeightTTFT:             settings.OpenAIAdvancedSchedulerWeightTTFT,
			SettingKeyOpenAIAdvancedSchedulerWeightReset:            settings.OpenAIAdvancedSchedulerWeightReset,
			SettingKeyOpenAIAdvancedSchedulerWeightQuotaHeadroom:    settings.OpenAIAdvancedSchedulerWeightQuotaHeadroom,
			SettingKeyOpenAIAdvancedSchedulerWeightUpstreamCost:     settings.OpenAIAdvancedSchedulerWeightUpstreamCost,
			SettingKeyOpenAIAdvancedSchedulerWeightPreviousResponse: settings.OpenAIAdvancedSchedulerWeightPreviousResponse,
			SettingKeyOpenAIAdvancedSchedulerWeightSessionSticky:    settings.OpenAIAdvancedSchedulerWeightSessionSticky,
		})),
		generation: generation,
		expiresAt:  time.Now().Add(openAIAdvancedSchedulerSettingCacheTTL).UnixNano(),
	})
	openAIAdvancedSchedulerSettingCachePublishMu.Unlock()
}

type OpenAIAccountScheduleRequest struct {
	GroupID                   *int64
	Platform                  string
	SessionHash               string
	StickyAccountID           int64
	GuardianParentAccountID   int64
	StickyPreviousAccountID   int64
	StickyWeighted            bool
	SubscriptionPriority      bool
	SmartHealthEnabled        bool
	SmartHealthProbeAccountID int64
	SmartHealthStickyEscaped  bool
	PreserveStickyBinding     bool
	// DisableStickyEscape keeps task-owner lookups on their account even when
	// generic sticky health or concurrency heuristics would prefer another.
	DisableStickyEscape     bool
	RequirePrivacySet       bool
	PreviousResponseID      string
	PreviousResponseCanMove bool
	UseUpstreamTokenCost    bool
	RequestedModel          string
	RequiredTransport       OpenAIUpstreamTransport
	RequiredCapability      OpenAIEndpointCapability
	RequiredImageCapability OpenAIImagesCapability
	// RequireCompact is only for legacy /responses/compact capability filtering
	// and compact_model_mapping; native remote compaction v2 leaves it false.
	RequireCompact bool
	ExcludedIDs    map[int64]struct{}
}

type OpenAIAccountScheduleDecision struct {
	Layer               string
	StickyPreviousHit   bool
	StickySessionHit    bool
	CandidateCount      int
	PrimaryCount        int
	DegradedCount       int
	TopK                int
	LatencyMs           int64
	LoadSkew            float64
	SelectedAccountID   int64
	SelectedAccountType string
	SelectedPriority    int
	SelectedHealth      string
	HealthEscape        bool
	FilterReasons       string
}

type OpenAIAccountSchedulerMetricsSnapshot struct {
	SelectTotal                    int64
	StickyPreviousHitTotal         int64
	StickySessionHitTotal          int64
	LoadBalanceSelectTotal         int64
	AccountSwitchTotal             int64
	SchedulerLatencyMsTotal        int64
	SchedulerLatencyMsAvg          float64
	StickyHitRatio                 float64
	AccountSwitchRate              float64
	LoadSkewAvg                    float64
	RuntimeStatsAccountCount       int
	SmartHealthDegradedEscapeTotal int64
	SmartHealthProbeTotal          int64
	SmartHealthAllDegradedTotal    int64
}

type OpenAIAccountScheduler interface {
	Select(ctx context.Context, req OpenAIAccountScheduleRequest) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error)
	ReportResult(accountID int64, success bool, firstTokenMs *int)
	ReportSwitch()
	SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot
}

type openAIAccountSchedulerMetrics struct {
	selectTotal                    atomic.Int64
	stickyPreviousHitTotal         atomic.Int64
	stickySessionHitTotal          atomic.Int64
	stickyHitTotal                 atomic.Int64
	loadBalanceSelectTotal         atomic.Int64
	accountSwitchTotal             atomic.Int64
	latencyMsTotal                 atomic.Int64
	loadSkewMilliTotal             atomic.Int64
	smartHealthDegradedEscapeTotal atomic.Int64
	smartHealthProbeTotal          atomic.Int64
	smartHealthAllDegradedTotal    atomic.Int64
}

type openAIAccountLoadPlan struct {
	allCandidates             []openAIAccountCandidateScore
	candidates                []openAIAccountCandidateScore
	probeCandidates           []openAIAccountCandidateScore
	staleSnapshotCompactRetry []openAIAccountCandidateScore
	selectionOrder            []openAIAccountCandidateScore
	candidateCount            int
	topK                      int
	loadSkew                  float64
	includeOverflowFallback   bool
}

type openAIAccountLoadSelectionAttempt struct {
	result              *AccountSelectionResult
	selectionOrder      []openAIAccountCandidateScore
	candidateCount      int
	topK                int
	loadSkew            float64
	compactBlocked      bool
	noCompactCandidates bool
	err                 error
}

func (m *openAIAccountSchedulerMetrics) recordSelect(decision OpenAIAccountScheduleDecision) {
	if m == nil {
		return
	}
	m.selectTotal.Add(1)
	m.latencyMsTotal.Add(decision.LatencyMs)
	m.loadSkewMilliTotal.Add(int64(math.Round(decision.LoadSkew * 1000)))
	if decision.StickyPreviousHit {
		m.stickyPreviousHitTotal.Add(1)
	}
	if decision.StickySessionHit {
		m.stickySessionHitTotal.Add(1)
	}
	if decision.StickyPreviousHit || decision.StickySessionHit {
		m.stickyHitTotal.Add(1)
	}
	if decision.Layer == openAIAccountScheduleLayerLoadBalance {
		m.loadBalanceSelectTotal.Add(1)
	}
}

func (m *openAIAccountSchedulerMetrics) recordSwitch() {
	if m == nil {
		return
	}
	m.accountSwitchTotal.Add(1)
}

func (m *openAIAccountSchedulerMetrics) recordSmartHealthDegradedEscape() {
	if m == nil {
		return
	}
	m.smartHealthDegradedEscapeTotal.Add(1)
}

func (m *openAIAccountSchedulerMetrics) recordSmartHealthProbe() {
	if m == nil {
		return
	}
	m.smartHealthProbeTotal.Add(1)
}

func (m *openAIAccountSchedulerMetrics) recordSmartHealthAllDegraded() {
	if m == nil {
		return
	}
	m.smartHealthAllDegradedTotal.Add(1)
}

type openAIAccountRuntimeStats struct {
	accounts                     sync.Map
	accountCount                 atomic.Int64
	probeSequence                atomic.Uint64
	probeTraceNamespace          string
	probeTraceNamespaceAvailable bool
	probeTraceInstanceID         uint64
	reclaimCursor                atomic.Int64
	maintenanceMu                sync.Mutex
	maintenanceStarted           bool
	maintenanceStopped           bool
	maintenanceCancel            context.CancelFunc
	maintenanceDone              chan struct{}
}

type openAIAccountHealthLevel uint32

const (
	openAIAccountHealthUnknown openAIAccountHealthLevel = iota
	openAIAccountHealthHealthy
	openAIAccountHealthDegraded
)

const (
	openAIAccountHealthReasonNone uint32 = iota
	openAIAccountHealthReasonErrorRate
	openAIAccountHealthReasonTTFT
)

type openAIAccountRuntimeStat struct {
	mu                    sync.Mutex
	probeTraceQueueMu     sync.Mutex
	probeTraceQueue       []openAIAccountProbeTraceEvent
	probeTraceDraining    bool
	lifecycleMu           sync.Mutex
	activeRefs            int64
	retiring              bool
	lastActivityAt        atomic.Int64
	errorRateEWMABits     atomic.Uint64
	ttftEWMABits          atomic.Uint64
	sampleCount           atomic.Int64
	lastObservedAt        atomic.Int64
	healthLevel           atomic.Uint32
	healthReason          atomic.Uint32
	nextProbeAt           atomic.Int64
	probeLeaseUntil       atomic.Int64
	probeActive           atomic.Bool
	smartMode             bool
	smartLevel            openAIAccountHealthLevel
	smartReason           uint32
	smartErrorWindow      []openAISmartTimedSample
	smartTTFTWindow       []openAISmartTimedSample
	smartLastObserved     time.Time
	probeID               uint64
	probeStartedAt        time.Time
	probeTraceID          string
	probeTraceStarted     bool
	probeTraceCorrelation string
	probeTraceGroupID     int64
	probeTraceHasGroupID  bool
	probeBatchOrdinal     int
	probeBatchStarted     time.Time
	probeBatchSamples     []openAISmartProbeSample
	backoffIndex          int
}

type openAIAccountProbeTraceEvent struct {
	accountID        int64
	groupID          int64
	hasGroupID       bool
	traceID          string
	correlation      string
	phase            string
	outcome          string
	reason           string
	batchOrdinal     int
	batchSampleCount int
	backoffStep      int
	nextProbeAt      int64
	healthLevel      string
}

func openAIProbeTraceEventLocked(
	stat *openAIAccountRuntimeStat,
	accountID int64,
	phase string,
	outcome string,
	reason string,
) openAIAccountProbeTraceEvent {
	healthLevel := openAIAccountHealthLevel(stat.healthLevel.Load())
	if stat.smartMode {
		healthLevel = stat.smartLevel
	}
	event := openAIAccountProbeTraceEvent{
		accountID:        accountID,
		groupID:          stat.probeTraceGroupID,
		hasGroupID:       stat.probeTraceHasGroupID,
		traceID:          stat.probeTraceID,
		correlation:      stat.probeTraceCorrelation,
		phase:            phase,
		outcome:          outcome,
		reason:           reason,
		batchOrdinal:     stat.probeBatchOrdinal,
		batchSampleCount: len(stat.probeBatchSamples),
		backoffStep:      stat.backoffIndex + 1,
		nextProbeAt:      stat.nextProbeAt.Load(),
		healthLevel:      openAIAccountHealthLevelName(healthLevel),
	}
	return event
}

func logOpenAIAccountProbeTrace(event openAIAccountProbeTraceEvent) {
	attrs := []any{
		"account_id", event.accountID,
		"phase", event.phase,
		"correlation", event.correlation,
	}
	if event.hasGroupID {
		attrs = append(attrs, "group_id", event.groupID)
	}
	if event.traceID != "" {
		attrs = append(attrs, "probe_trace_id", event.traceID)
	}
	if event.outcome != "" {
		attrs = append(attrs, "outcome", event.outcome)
	}
	if event.reason != "" {
		attrs = append(attrs, "reason", event.reason)
	}
	if event.batchOrdinal > 0 {
		attrs = append(attrs, "batch_ordinal", event.batchOrdinal)
	}
	attrs = append(attrs,
		"batch_sample_count", event.batchSampleCount,
		"backoff_step", event.backoffStep,
		"health_level", event.healthLevel,
	)
	if event.nextProbeAt > 0 {
		attrs = append(attrs, "next_probe_at", time.Unix(0, event.nextProbeAt).UTC())
	}
	slog.Info("openai_account_health_probe", attrs...)
}

// enqueueOpenAIAccountProbeTraceLocked appends an immutable event while stat.mu
// is held and atomically elects one synchronous drainer for this entry.
func enqueueOpenAIAccountProbeTraceLocked(
	stat *openAIAccountRuntimeStat,
	event openAIAccountProbeTraceEvent,
) bool {
	stat.probeTraceQueueMu.Lock()
	stat.probeTraceQueue = append(stat.probeTraceQueue, event)
	shouldDrain := !stat.probeTraceDraining
	if shouldDrain {
		stat.probeTraceDraining = true
	}
	stat.probeTraceQueueMu.Unlock()
	return shouldDrain
}

// drainOpenAIAccountProbeTraceQueue writes queued events in state-transition
// order without holding any per-entry lock while invoking the log handler.
func drainOpenAIAccountProbeTraceQueue(stat *openAIAccountRuntimeStat) {
	for {
		stat.probeTraceQueueMu.Lock()
		if len(stat.probeTraceQueue) == 0 {
			stat.probeTraceDraining = false
			stat.probeTraceQueueMu.Unlock()
			return
		}
		event := stat.probeTraceQueue[0]
		stat.probeTraceQueue[0] = openAIAccountProbeTraceEvent{}
		stat.probeTraceQueue = stat.probeTraceQueue[1:]
		stat.probeTraceQueueMu.Unlock()

		logOpenAIAccountProbeTrace(event)
	}
}

type openAISmartTimedSample struct {
	at    time.Time
	value float64
}

type openAISmartProbeSample struct {
	success  bool
	ttftMs   int
	hasTTFT  bool
	observed time.Time
}

type OpenAISmartHealthSnapshot struct {
	State       string     `json:"state"`
	Reason      string     `json:"reason"`
	NextProbeAt *time.Time `json:"next_probe_at"`
	BackoffStep int        `json:"backoff_step"`
}

type openAIAccountHealthAssessment struct {
	level     openAIAccountHealthLevel
	reason    uint32
	errorRate float64
	ttft      float64
	hasTTFT   bool
	samples   int64
}

func newOpenAIAccountRuntimeStats() *openAIAccountRuntimeStats {
	return &openAIAccountRuntimeStats{
		probeTraceNamespace:          openAIProbeProcessTraceNamespace,
		probeTraceNamespaceAvailable: openAIProbeProcessTraceCorrelationAvailable,
		probeTraceInstanceID:         openAIProbeTraceInstanceSequence.Add(1),
	}
}

// acquire pins a stat while a caller reads or mutates it so reclamation cannot
// detach an entry that still has an active user. Creating callers publish the
// first pin together with the map entry; read-only callers never create state.
func (s *openAIAccountRuntimeStats) acquire(
	accountID int64,
	create bool,
	refreshActivity bool,
) (*openAIAccountRuntimeStat, func(), bool) {
	if s == nil || accountID <= 0 {
		return nil, nil, false
	}

	for {
		value, exists := s.accounts.Load(accountID)
		if !exists {
			if !create {
				return nil, nil, false
			}

			now := time.Now().UnixNano()
			stat := &openAIAccountRuntimeStat{activeRefs: 1}
			stat.lastActivityAt.Store(now)
			stat.ttftEWMABits.Store(math.Float64bits(math.NaN()))
			actual, loaded := s.accounts.LoadOrStore(accountID, stat)
			if !loaded {
				s.accountCount.Add(1)
				return stat, func() { s.release(stat) }, true
			}
			value = actual
		}

		stat, ok := value.(*openAIAccountRuntimeStat)
		if !ok || stat == nil {
			return nil, nil, false
		}

		stat.lifecycleMu.Lock()
		if stat.retiring {
			stat.lifecycleMu.Unlock()
			if !create {
				return nil, nil, false
			}
			continue
		}
		current, mapped := s.accounts.Load(accountID)
		if !mapped || current != stat {
			stat.lifecycleMu.Unlock()
			if !create {
				return nil, nil, false
			}
			continue
		}
		stat.activeRefs++
		if refreshActivity {
			stat.lastActivityAt.Store(time.Now().UnixNano())
		}
		stat.lifecycleMu.Unlock()
		return stat, func() { s.release(stat) }, true
	}
}

func (s *openAIAccountRuntimeStats) release(stat *openAIAccountRuntimeStat) {
	if s == nil || stat == nil {
		return
	}
	stat.lifecycleMu.Lock()
	if stat.activeRefs <= 0 {
		stat.lifecycleMu.Unlock()
		panic("openai account runtime stat reference count underflow")
	}
	stat.activeRefs--
	stat.lifecycleMu.Unlock()
}

type openAIAccountRuntimeStatReclaimCandidate struct {
	accountID int64
	stat      *openAIAccountRuntimeStat
}

func isOpenAIAccountRuntimeStatIdle(stat *openAIAccountRuntimeStat, now time.Time) bool {
	if stat == nil {
		return false
	}
	stat.lifecycleMu.Lock()
	defer stat.lifecycleMu.Unlock()
	if stat.retiring || stat.activeRefs != 0 {
		return false
	}
	lastActivity := stat.lastActivityAt.Load()
	return lastActivity > 0 && now.Sub(time.Unix(0, lastActivity)) >= openAIAccountRuntimeStatsReclaimIdle
}

func appendLowestOpenAIAccountRuntimeStatCandidate(
	candidates []openAIAccountRuntimeStatReclaimCandidate,
	candidate openAIAccountRuntimeStatReclaimCandidate,
) []openAIAccountRuntimeStatReclaimCandidate {
	if len(candidates) < openAIAccountRuntimeStatsReclaimBatchSize {
		return append(candidates, candidate)
	}
	largest := 0
	for index := 1; index < len(candidates); index++ {
		if candidates[index].accountID > candidates[largest].accountID {
			largest = index
		}
	}
	if candidate.accountID < candidates[largest].accountID {
		candidates[largest] = candidate
	}
	return candidates
}

func (s *openAIAccountRuntimeStats) reclaimCandidates(now time.Time) []openAIAccountRuntimeStatReclaimCandidate {
	if s == nil {
		return nil
	}
	cursor := s.reclaimCursor.Load()
	afterCursor := make([]openAIAccountRuntimeStatReclaimCandidate, 0, openAIAccountRuntimeStatsReclaimBatchSize)
	wrapped := make([]openAIAccountRuntimeStatReclaimCandidate, 0, openAIAccountRuntimeStatsReclaimBatchSize)
	s.accounts.Range(func(key, value any) bool {
		accountID, ok := key.(int64)
		if !ok || accountID <= 0 {
			return true
		}
		stat, ok := value.(*openAIAccountRuntimeStat)
		if !ok || !isOpenAIAccountRuntimeStatIdle(stat, now) {
			return true
		}
		candidate := openAIAccountRuntimeStatReclaimCandidate{accountID: accountID, stat: stat}
		wrapped = appendLowestOpenAIAccountRuntimeStatCandidate(wrapped, candidate)
		if accountID > cursor {
			afterCursor = appendLowestOpenAIAccountRuntimeStatCandidate(afterCursor, candidate)
		}
		return true
	})

	selected := afterCursor
	if len(selected) == 0 {
		selected = wrapped
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].accountID < selected[j].accountID })
	if len(selected) == 0 {
		s.reclaimCursor.Store(0)
		return nil
	}
	return selected
}

func (s *openAIAccountRuntimeStats) retireReclaimCandidate(
	candidate openAIAccountRuntimeStatReclaimCandidate,
	exists bool,
	now time.Time,
) bool {
	stat := candidate.stat
	if stat == nil {
		return false
	}
	stat.lifecycleMu.Lock()
	defer stat.lifecycleMu.Unlock()
	if stat.retiring || stat.activeRefs != 0 {
		return false
	}
	mapped, ok := s.accounts.Load(candidate.accountID)
	if !ok || mapped != stat {
		return false
	}
	lastActivity := stat.lastActivityAt.Load()
	if lastActivity <= 0 || now.Sub(time.Unix(0, lastActivity)) < openAIAccountRuntimeStatsReclaimIdle {
		return false
	}

	stat.retiring = true
	stat.mu.Lock()
	probeInFlight := stat.probeID != 0 || stat.probeActive.Load() || stat.probeLeaseUntil.Load() != 0
	probeBatchInFlight := !stat.probeBatchStarted.IsZero() || len(stat.probeBatchSamples) != 0
	degraded := openAIAccountHealthLevel(stat.healthLevel.Load()) == openAIAccountHealthDegraded ||
		stat.smartLevel == openAIAccountHealthDegraded
	stat.mu.Unlock()
	if probeInFlight || probeBatchInFlight || (exists && degraded) {
		stat.retiring = false
		return false
	}
	if !s.accounts.CompareAndDelete(candidate.accountID, stat) {
		stat.retiring = false
		return false
	}
	s.accountCount.Add(-1)
	return true
}

// reclaimIdleBatch verifies a bounded batch against the account repository.
// Repository failures and malformed results keep the complete batch for retry.
func (s *openAIAccountRuntimeStats) reclaimIdleBatch(ctx context.Context, repo AccountRepository, now time.Time) int {
	if s == nil || repo == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	candidates := s.reclaimCandidates(now)
	if len(candidates) == 0 {
		return 0
	}
	ids := make([]int64, 0, len(candidates))
	requested := make(map[int64]struct{}, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.accountID)
		requested[candidate.accountID] = struct{}{}
	}

	queryCtx, cancel := context.WithTimeout(ctx, openAIAccountRuntimeStatsReclaimTimeout)
	accounts, err := repo.GetByIDs(queryCtx, ids)
	cancel()
	if err != nil {
		slog.Warn("openai_account_runtime_stats_reclaim_skipped",
			"operation", "get_by_ids",
			"candidate_count", len(ids),
		)
		return 0
	}
	existing := make(map[int64]struct{}, len(accounts))
	for _, account := range accounts {
		if account == nil {
			slog.Warn("openai_account_runtime_stats_reclaim_skipped",
				"operation", "validate_get_by_ids_result",
				"candidate_count", len(ids),
				"returned_count", len(accounts),
			)
			return 0
		}
		if _, ok := requested[account.ID]; !ok {
			slog.Warn("openai_account_runtime_stats_reclaim_skipped",
				"operation", "validate_get_by_ids_result",
				"candidate_count", len(ids),
				"returned_count", len(accounts),
			)
			return 0
		}
		if _, duplicate := existing[account.ID]; duplicate {
			slog.Warn("openai_account_runtime_stats_reclaim_skipped",
				"operation", "validate_get_by_ids_result",
				"candidate_count", len(ids),
				"returned_count", len(accounts),
			)
			return 0
		}
		existing[account.ID] = struct{}{}
	}
	s.reclaimCursor.Store(candidates[len(candidates)-1].accountID)

	deleted := 0
	for _, candidate := range candidates {
		_, accountExists := existing[candidate.accountID]
		if s.retireReclaimCandidate(candidate, accountExists, now) {
			deleted++
		}
	}
	return deleted
}

func (s *openAIAccountRuntimeStats) startReclamation(parent context.Context, repo AccountRepository) {
	if s == nil || parent == nil || repo == nil || parent.Err() != nil {
		return
	}
	s.maintenanceMu.Lock()
	if s.maintenanceStarted || s.maintenanceStopped || parent.Err() != nil {
		s.maintenanceMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.maintenanceStarted = true
	s.maintenanceCancel = cancel
	s.maintenanceDone = done
	s.maintenanceMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(openAIAccountRuntimeStatsReclaimInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				s.reclaimIdleBatch(ctx, repo, now)
			}
		}
	}()
}

func (s *openAIAccountRuntimeStats) stopReclamation() {
	if s == nil {
		return
	}
	s.maintenanceMu.Lock()
	s.maintenanceStopped = true
	cancel := s.maintenanceCancel
	done := s.maintenanceDone
	s.maintenanceMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func updateEWMAAtomic(target *atomic.Uint64, sample float64, alpha float64) {
	for {
		oldBits := target.Load()
		oldValue := math.Float64frombits(oldBits)
		newValue := alpha*sample + (1-alpha)*oldValue
		if target.CompareAndSwap(oldBits, math.Float64bits(newValue)) {
			return
		}
	}
}

func defaultOpenAIAccountHealthThresholds() openAIStickyEscapeConfig {
	return openAIStickyEscapeConfig{ttftMs: 15000, errorRate: 0.5}
}

func classifyOpenAIAccountHealth(
	errorRate float64,
	ttft float64,
	hasTTFT bool,
	sampleCount int64,
	thresholds openAIStickyEscapeConfig,
) (openAIAccountHealthLevel, uint32) {
	if sampleCount < openAIAccountLegacyHealthMinSamples || math.IsNaN(errorRate) || math.IsInf(errorRate, 0) {
		return openAIAccountHealthUnknown, openAIAccountHealthReasonNone
	}
	if errorRate > thresholds.errorRate {
		return openAIAccountHealthDegraded, openAIAccountHealthReasonErrorRate
	}
	if hasTTFT && (math.IsNaN(ttft) || math.IsInf(ttft, 0)) {
		return openAIAccountHealthUnknown, openAIAccountHealthReasonNone
	}
	if hasTTFT && ttft > thresholds.ttftMs {
		return openAIAccountHealthDegraded, openAIAccountHealthReasonTTFT
	}
	return openAIAccountHealthHealthy, openAIAccountHealthReasonNone
}

func openAIAccountHealthReasonName(reason uint32) string {
	switch reason {
	case openAIAccountHealthReasonErrorRate:
		return "error_rate"
	case openAIAccountHealthReasonTTFT:
		return "ttft"
	default:
		return ""
	}
}

func (s *openAIAccountRuntimeStats) report(accountID int64, success bool, firstTokenMs *int) {
	s.reportAt(accountID, success, firstTokenMs, time.Now(), defaultOpenAIAccountHealthThresholds(), false)
}

func (s *openAIAccountRuntimeStats) reportAt(
	accountID int64,
	success bool,
	firstTokenMs *int,
	now time.Time,
	thresholds openAIStickyEscapeConfig,
	logHealth bool,
) {
	s.reportAtForRequest(accountID, success, firstTokenMs, now, thresholds, logHealth, false, 0, success)
}

func (s *openAIAccountRuntimeStats) reportAtForRequest(
	accountID int64,
	success bool,
	firstTokenMs *int,
	now time.Time,
	thresholds openAIStickyEscapeConfig,
	logHealth bool,
	smartEnabled bool,
	probeID uint64,
	attributable bool,
) {
	if s == nil || accountID <= 0 {
		return
	}
	const alpha = 0.2
	stat, release, ok := s.acquire(accountID, true, true)
	if !ok {
		return
	}
	defer func() {
		if release != nil {
			release()
		}
	}()

	errorSample := 1.0
	if success {
		errorSample = 0.0
	}
	updateEWMAAtomic(&stat.errorRateEWMABits, errorSample, alpha)

	if firstTokenMs != nil && *firstTokenMs > 0 {
		ttft := float64(*firstTokenMs)
		ttftBits := math.Float64bits(ttft)
		for {
			oldBits := stat.ttftEWMABits.Load()
			oldValue := math.Float64frombits(oldBits)
			if math.IsNaN(oldValue) {
				if stat.ttftEWMABits.CompareAndSwap(oldBits, ttftBits) {
					break
				}
				continue
			}
			newValue := alpha*ttft + (1-alpha)*oldValue
			if stat.ttftEWMABits.CompareAndSwap(oldBits, math.Float64bits(newValue)) {
				break
			}
		}
	}

	sampleCount := stat.sampleCount.Add(1)
	stat.lastObservedAt.Store(now.UnixNano())
	if smartEnabled {
		stat.mu.Lock()
		oldLevel := stat.smartLevel
		stat.smartMode = true
		if attributable {
			s.appendSmartHealthObservationLocked(stat, success, firstTokenMs, now)
		}
		matchedProbe := probeID != 0 && stat.probeID == probeID
		traceStarted := matchedProbe && stat.probeTraceStarted
		batchSamplesBefore := len(stat.probeBatchSamples)
		tracePhase := "finish"
		traceOutcome := ""
		traceReason := ""
		if matchedProbe {
			if traceStarted {
				traceOutcome, traceReason = openAIProbeTraceOutcome(stat, success, firstTokenMs, now, attributable)
				if !attributable {
					tracePhase = "cancel"
				}
			}
			stat.probeID = 0
			stat.probeLeaseUntil.Store(0)
			stat.probeActive.Store(false)
			stat.probeStartedAt = time.Time{}
			if attributable {
				s.finishSmartProbeSampleLocked(stat, success, firstTokenMs, now)
			}
		} else if !success && attributable && stat.smartLevel == openAIAccountHealthDegraded && len(stat.probeBatchSamples) > 0 {
			// A normal attributable failure during a recovery batch invalidates the
			// cached recovery evidence, but does not advance the backoff ladder.
			s.rescheduleSmartProbeBatchLocked(stat, now, false)
		}
		s.recomputeSmartHealthLocked(stat, now, oldLevel)
		newLevel := stat.smartLevel
		newReason := stat.smartReason
		shouldDrainProbeTrace := false
		if traceStarted {
			event := openAIProbeTraceEventLocked(stat, accountID, tracePhase, traceOutcome, traceReason)
			event.batchSampleCount = batchSamplesBefore
			if traceOutcome == "qualified" || traceOutcome == "unqualified" {
				event.batchSampleCount++
			}
			shouldDrainProbeTrace = enqueueOpenAIAccountProbeTraceLocked(stat, event)
			clearOpenAIProbeTraceLocked(stat)
		}
		stat.mu.Unlock()
		release()
		release = nil
		if shouldDrainProbeTrace {
			drainOpenAIAccountProbeTraceQueue(stat)
		}
		if logHealth && oldLevel != openAIAccountHealthDegraded && newLevel == openAIAccountHealthDegraded {
			errorRate, ttft, hasTTFT := s.smartHealthWindowValues(accountID, now)
			if !hasTTFT {
				ttft = math.NaN()
			}
			slog.Info("openai_account_health_changed",
				"account_id", accountID,
				"from", openAIAccountHealthLevelName(oldLevel),
				"to", openAIAccountHealthLevelName(newLevel),
				"reason", openAIAccountHealthReasonName(newReason),
				"error_rate", errorRate,
				"ttft_ms", ttft,
				"sample_count", sampleCount,
			)
		}
		return
	}

	stat.mu.Lock()
	oldLevel := openAIAccountHealthLevel(stat.healthLevel.Load())
	// The legacy S=false path intentionally preserves its historical EWMA behavior.
	ttftValue := math.Float64frombits(stat.ttftEWMABits.Load())
	newLevel, newReason := classifyOpenAIAccountHealth(
		clamp01(math.Float64frombits(stat.errorRateEWMABits.Load())),
		ttftValue,
		!math.IsNaN(ttftValue),
		sampleCount,
		thresholds,
	)
	if oldLevel != openAIAccountHealthDegraded {
		stat.healthLevel.Store(uint32(newLevel))
		stat.healthReason.Store(newReason)
		if newLevel == openAIAccountHealthDegraded {
			stat.nextProbeAt.Store(now.Add(openAIAccountLegacyProbeCooldown).UnixNano())
		}
	}
	stat.mu.Unlock()
	release()
	release = nil
	if logHealth && oldLevel != openAIAccountHealthDegraded && newLevel == openAIAccountHealthDegraded {
		slog.Info("openai_account_health_changed",
			"account_id", accountID,
			"from", openAIAccountHealthLevelName(oldLevel),
			"to", openAIAccountHealthLevelName(newLevel),
			"reason", openAIAccountHealthReasonName(newReason),
			"error_rate", clamp01(math.Float64frombits(stat.errorRateEWMABits.Load())),
			"ttft_ms", math.Float64frombits(stat.ttftEWMABits.Load()),
			"sample_count", sampleCount,
		)
	}
}

func openAIProbeTraceOutcome(
	stat *openAIAccountRuntimeStat,
	success bool,
	firstTokenMs *int,
	now time.Time,
	attributable bool,
) (string, string) {
	if !attributable {
		return "inconclusive", "not_attributable"
	}
	if !stat.probeBatchStarted.IsZero() && now.Sub(stat.probeBatchStarted) > openAIAccountSmartProbeBatchTimeout {
		return "inconclusive", "batch_timeout"
	}
	if !success {
		return "unqualified", "account_error"
	}
	if stat.smartReason == openAIAccountHealthReasonTTFT &&
		(firstTokenMs == nil || *firstTokenMs <= 0) {
		return "inconclusive", "ttft"
	}
	if reason := openAIAccountHealthReasonName(stat.smartReason); reason != "" {
		return "qualified", reason
	}
	return "qualified", "none"
}

func clearOpenAIProbeTraceLocked(stat *openAIAccountRuntimeStat) {
	stat.probeTraceID = ""
	stat.probeTraceStarted = false
	stat.probeTraceCorrelation = ""
	stat.probeTraceGroupID = 0
	stat.probeTraceHasGroupID = false
	stat.probeBatchOrdinal = 0
}

func (s *openAIAccountRuntimeStats) appendSmartHealthObservationLocked(stat *openAIAccountRuntimeStat, success bool, firstTokenMs *int, now time.Time) {
	s.pruneSmartHealthWindowsLocked(stat, now)
	errorValue := 0.0
	if !success {
		errorValue = 1
	}
	stat.smartErrorWindow = appendBoundedSmartSample(stat.smartErrorWindow, openAISmartTimedSample{at: now, value: errorValue})
	if firstTokenMs != nil && *firstTokenMs > 0 {
		stat.smartTTFTWindow = appendBoundedSmartSample(stat.smartTTFTWindow, openAISmartTimedSample{at: now, value: float64(*firstTokenMs)})
	}
	stat.smartLastObserved = now
}

func appendBoundedSmartSample(samples []openAISmartTimedSample, sample openAISmartTimedSample) []openAISmartTimedSample {
	samples = append(samples, sample)
	if len(samples) > openAIAccountSmartWindowSize {
		samples = append([]openAISmartTimedSample(nil), samples[len(samples)-openAIAccountSmartWindowSize:]...)
	}
	return samples
}

func (s *openAIAccountRuntimeStats) pruneSmartHealthWindowsLocked(stat *openAIAccountRuntimeStat, now time.Time) {
	cutoff := now.Add(-openAIAccountSmartWindowDuration)
	keepFresh := func(samples []openAISmartTimedSample) []openAISmartTimedSample {
		first := 0
		for first < len(samples) && samples[first].at.Before(cutoff) {
			first++
		}
		return append([]openAISmartTimedSample(nil), samples[first:]...)
	}
	stat.smartErrorWindow = keepFresh(stat.smartErrorWindow)
	stat.smartTTFTWindow = keepFresh(stat.smartTTFTWindow)
}

func smartTTFTEWMA(samples []openAISmartTimedSample) float64 {
	if len(samples) == 0 {
		return math.NaN()
	}
	value := samples[0].value
	for _, sample := range samples[1:] {
		value = 0.2*sample.value + 0.8*value
	}
	return value
}

func (s *openAIAccountRuntimeStats) recomputeSmartHealthLocked(stat *openAIAccountRuntimeStat, now time.Time, previous openAIAccountHealthLevel) {
	s.pruneSmartHealthWindowsLocked(stat, now)
	if stat.smartLevel == openAIAccountHealthDegraded {
		return
	}
	if stat.smartLastObserved.IsZero() || now.Sub(stat.smartLastObserved) > openAIAccountSmartWindowDuration {
		stat.smartLevel = openAIAccountHealthUnknown
		stat.smartReason = openAIAccountHealthReasonNone
		return
	}
	errorsMature := len(stat.smartErrorWindow) >= openAIAccountSmartWindowSize
	ttftMature := len(stat.smartTTFTWindow) >= openAIAccountSmartWindowSize
	if errorsMature {
		failures := 0
		for _, sample := range stat.smartErrorWindow {
			if sample.value > 0 {
				failures++
			}
		}
		if failures >= 2 {
			stat.smartLevel = openAIAccountHealthDegraded
			stat.smartReason = openAIAccountHealthReasonErrorRate
		}
	}
	ttft := smartTTFTEWMA(stat.smartTTFTWindow)
	if stat.smartLevel != openAIAccountHealthDegraded && ttftMature && ttft > float64(openAIAccountSmartTTFTThreshold.Milliseconds()) {
		stat.smartLevel = openAIAccountHealthDegraded
		stat.smartReason = openAIAccountHealthReasonTTFT
	}
	if stat.smartLevel == openAIAccountHealthDegraded && previous != openAIAccountHealthDegraded {
		stat.backoffIndex = 0
		stat.probeBatchStarted = time.Time{}
		stat.probeBatchSamples = nil
		stat.nextProbeAt.Store(now.Add(openAIAccountHealthProbeCooldowns[0]).UnixNano())
		return
	}
	if errorsMature && ttftMature && stat.smartLevel != openAIAccountHealthDegraded {
		stat.smartLevel = openAIAccountHealthHealthy
		stat.smartReason = openAIAccountHealthReasonNone
		return
	}
	stat.smartLevel = openAIAccountHealthUnknown
	stat.smartReason = openAIAccountHealthReasonNone
}

func (s *openAIAccountRuntimeStats) finishSmartProbeSampleLocked(stat *openAIAccountRuntimeStat, success bool, firstTokenMs *int, now time.Time) {
	if stat.probeBatchStarted.IsZero() {
		stat.probeBatchStarted = now
	}
	if now.Sub(stat.probeBatchStarted) > openAIAccountSmartProbeBatchTimeout {
		s.rescheduleSmartProbeBatchLocked(stat, now, false)
		return
	}
	probeSample := openAISmartProbeSample{success: success, observed: now}
	if success && firstTokenMs != nil && *firstTokenMs > 0 {
		probeSample.ttftMs = *firstTokenMs
		probeSample.hasTTFT = true
	}
	if success && stat.smartReason == openAIAccountHealthReasonTTFT && !probeSample.hasTTFT {
		s.rescheduleSmartProbeBatchLocked(stat, now, false)
		return
	}
	stat.probeBatchSamples = append(stat.probeBatchSamples, probeSample)
	failures, successes, ttftCount := 0, 0, 0
	ttftSum := 0
	for _, sample := range stat.probeBatchSamples {
		if sample.success {
			successes++
			if sample.hasTTFT {
				ttftSum += sample.ttftMs
				ttftCount++
			}
		} else {
			failures++
		}
	}
	if failures >= 2 || (stat.smartReason == openAIAccountHealthReasonTTFT && ttftCount > 0 &&
		ttftSum > int(openAIAccountSmartTTFTThreshold.Milliseconds())*openAIAccountSmartProbeBatchLimit) {
		s.rescheduleSmartProbeBatchLocked(stat, now, true)
		return
	}
	if len(stat.probeBatchSamples) < openAIAccountSmartProbeBatchLimit {
		return
	}
	passed := failures <= 1
	if stat.smartReason == openAIAccountHealthReasonTTFT && ttftCount > 0 &&
		float64(ttftSum)/float64(ttftCount) > float64(openAIAccountSmartTTFTThreshold.Milliseconds()) {
		passed = false
	}
	if stat.smartReason == openAIAccountHealthReasonTTFT && ttftCount != successes {
		passed = false
	}
	if !passed {
		s.rescheduleSmartProbeBatchLocked(stat, now, true)
		return
	}
	stat.smartErrorWindow = nil
	stat.smartTTFTWindow = nil
	for _, sample := range stat.probeBatchSamples {
		errorValue := 0.0
		if !sample.success {
			errorValue = 1
		}
		stat.smartErrorWindow = appendBoundedSmartSample(stat.smartErrorWindow, openAISmartTimedSample{at: sample.observed, value: errorValue})
		if sample.success && sample.hasTTFT {
			stat.smartTTFTWindow = appendBoundedSmartSample(stat.smartTTFTWindow, openAISmartTimedSample{at: sample.observed, value: float64(sample.ttftMs)})
		}
	}
	stat.smartLastObserved = now
	stat.smartLevel = openAIAccountHealthUnknown
	stat.smartReason = openAIAccountHealthReasonNone
	stat.backoffIndex = 0
	stat.nextProbeAt.Store(0)
	stat.probeBatchStarted = time.Time{}
	stat.probeBatchSamples = nil
	s.recomputeSmartHealthLocked(stat, now, openAIAccountHealthDegraded)
}

func (s *openAIAccountRuntimeStats) rescheduleSmartProbeBatchLocked(stat *openAIAccountRuntimeStat, now time.Time, advance bool) {
	stat.probeBatchStarted = time.Time{}
	stat.probeBatchSamples = nil
	if advance && stat.backoffIndex < len(openAIAccountHealthProbeCooldowns)-1 {
		stat.backoffIndex++
	}
	stat.nextProbeAt.Store(now.Add(openAIAccountHealthProbeCooldowns[stat.backoffIndex]).UnixNano())
}

func (s *openAIAccountRuntimeStats) smartHealthWindowValues(accountID int64, now time.Time) (float64, float64, bool) {
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return 0, 0, false
	}
	defer release()
	stat.mu.Lock()
	defer stat.mu.Unlock()
	s.pruneSmartHealthWindowsLocked(stat, now)
	errorRate := 0.0
	if len(stat.smartErrorWindow) >= openAIAccountSmartWindowSize {
		for _, sample := range stat.smartErrorWindow {
			errorRate += sample.value
		}
		errorRate /= float64(len(stat.smartErrorWindow))
	}
	ttft := smartTTFTEWMA(stat.smartTTFTWindow)
	return errorRate, ttft, len(stat.smartTTFTWindow) >= openAIAccountSmartWindowSize
}

func (s *openAIAccountRuntimeStats) snapshot(accountID int64) (errorRate float64, ttft float64, hasTTFT bool) {
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return 0, 0, false
	}
	defer release()
	errorRate = clamp01(math.Float64frombits(stat.errorRateEWMABits.Load()))
	ttftValue := math.Float64frombits(stat.ttftEWMABits.Load())
	if math.IsNaN(ttftValue) {
		return errorRate, 0, false
	}
	return errorRate, ttftValue, true
}

func openAIAccountHealthLevelName(level openAIAccountHealthLevel) string {
	switch level {
	case openAIAccountHealthHealthy:
		return "healthy"
	case openAIAccountHealthDegraded:
		return "degraded"
	default:
		return "unknown"
	}
}

func (s *openAIAccountRuntimeStats) health(accountID int64, now time.Time, thresholds openAIStickyEscapeConfig) openAIAccountHealthAssessment {
	assessment := openAIAccountHealthAssessment{level: openAIAccountHealthUnknown}
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return assessment
	}
	defer release()
	stat.mu.Lock()
	if stat.smartMode {
		s.recomputeSmartHealthLocked(stat, now, stat.smartLevel)
		assessment.level = stat.smartLevel
		assessment.reason = stat.smartReason
		assessment.samples = int64(len(stat.smartErrorWindow))
		if len(stat.smartErrorWindow) >= openAIAccountSmartWindowSize {
			for _, sample := range stat.smartErrorWindow {
				assessment.errorRate += sample.value
			}
			assessment.errorRate /= float64(len(stat.smartErrorWindow))
		}
		assessment.hasTTFT = len(stat.smartTTFTWindow) >= openAIAccountSmartWindowSize
		assessment.ttft = smartTTFTEWMA(stat.smartTTFTWindow)
		if !assessment.hasTTFT {
			assessment.ttft = 0
		}
		stat.mu.Unlock()
		return assessment
	}
	stat.mu.Unlock()
	assessment.errorRate, assessment.ttft, assessment.hasTTFT = s.snapshot(accountID)
	assessment.samples = stat.sampleCount.Load()
	level := openAIAccountHealthLevel(stat.healthLevel.Load())
	assessment.reason = stat.healthReason.Load()
	if level == openAIAccountHealthDegraded {
		assessment.level = level
		return assessment
	}
	lastObservedAt := stat.lastObservedAt.Load()
	if assessment.samples < openAIAccountLegacyHealthMinSamples || lastObservedAt <= 0 {
		return assessment
	}
	last := time.Unix(0, lastObservedAt)
	if !now.Before(last) && now.Sub(last) > openAIAccountHealthStaleAfter {
		return assessment
	}
	classified, reason := classifyOpenAIAccountHealth(assessment.errorRate, assessment.ttft, assessment.hasTTFT, assessment.samples, thresholds)
	assessment.level = classified
	assessment.reason = reason
	return assessment
}

func (s *openAIAccountRuntimeStats) tryAcquireProbeLease(accountID int64, now time.Time) bool {
	if s == nil {
		return false
	}
	return s.tryAcquireProbeLeaseWithID(accountID, now, s.probeSequence.Add(1))
}

func (s *openAIAccountRuntimeStats) tryAcquireProbeLeaseWithID(accountID int64, now time.Time, probeID uint64) bool {
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return false
	}
	defer release()
	stat.mu.Lock()
	defer stat.mu.Unlock()
	level := openAIAccountHealthLevel(stat.healthLevel.Load())
	if stat.smartMode {
		level = stat.smartLevel
	}
	if level != openAIAccountHealthDegraded || stat.probeID != 0 {
		return false
	}
	nowNanos := now.UnixNano()
	if stat.smartMode && !stat.probeBatchStarted.IsZero() && now.Sub(stat.probeBatchStarted) > openAIAccountSmartProbeBatchTimeout {
		s.rescheduleSmartProbeBatchLocked(stat, now, false)
	}
	continuingBatch := stat.smartMode && !stat.probeBatchStarted.IsZero() &&
		len(stat.probeBatchSamples) > 0 && now.Sub(stat.probeBatchStarted) <= openAIAccountSmartProbeBatchTimeout
	if !continuingBatch {
		if next := stat.nextProbeAt.Load(); next > nowNanos {
			return false
		}
	}
	// The 30 second lease deadline remains observable for compatibility, but an
	// in-flight request owns the lease until its result or ReleaseFunc arrives.
	stat.probeLeaseUntil.Store(now.Add(openAIAccountHealthProbeLease).UnixNano())
	stat.probeID = probeID
	stat.probeStartedAt = now
	stat.probeActive.Store(true)
	return true
}

func (s *openAIAccountRuntimeStats) markProbeSlotAcquired(
	accountID int64,
	probeID uint64,
	now time.Time,
	groupIDs ...*int64,
) {
	if s == nil || probeID == 0 {
		return
	}
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return
	}
	shouldDrainProbeTrace := false
	stat.mu.Lock()
	if stat.probeID == probeID && stat.probeBatchStarted.IsZero() {
		stat.probeBatchStarted = now
	}
	if stat.probeID == probeID && !stat.probeTraceStarted {
		stat.probeTraceID = ""
		stat.probeTraceCorrelation = "unavailable"
		if s.probeTraceNamespaceAvailable {
			stat.probeTraceID = fmt.Sprintf("%s-%d-%d", s.probeTraceNamespace, s.probeTraceInstanceID, probeID)
			stat.probeTraceCorrelation = "available"
		}
		stat.probeTraceStarted = true
		stat.probeBatchOrdinal = len(stat.probeBatchSamples) + 1
		stat.probeTraceGroupID = 0
		stat.probeTraceHasGroupID = false
		if len(groupIDs) > 0 && groupIDs[0] != nil {
			stat.probeTraceGroupID = *groupIDs[0]
			stat.probeTraceHasGroupID = true
		}
		traceEvent := openAIProbeTraceEventLocked(
			stat,
			accountID,
			"start",
			"",
			openAIAccountHealthReasonName(stat.smartReason),
		)
		shouldDrainProbeTrace = enqueueOpenAIAccountProbeTraceLocked(stat, traceEvent)
	}
	stat.mu.Unlock()
	release()
	if shouldDrainProbeTrace {
		drainOpenAIAccountProbeTraceQueue(stat)
	}
}

func (s *openAIAccountRuntimeStats) probeEligible(accountID int64, now time.Time) bool {
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return false
	}
	defer release()
	stat.mu.Lock()
	defer stat.mu.Unlock()
	level := openAIAccountHealthLevel(stat.healthLevel.Load())
	if stat.smartMode {
		level = stat.smartLevel
		if !stat.probeBatchStarted.IsZero() && now.Sub(stat.probeBatchStarted) > openAIAccountSmartProbeBatchTimeout {
			s.rescheduleSmartProbeBatchLocked(stat, now, false)
		}
		if level == openAIAccountHealthDegraded && stat.probeID == 0 &&
			!stat.probeBatchStarted.IsZero() && len(stat.probeBatchSamples) > 0 &&
			now.Sub(stat.probeBatchStarted) <= openAIAccountSmartProbeBatchTimeout {
			return true
		}
	}
	if level != openAIAccountHealthDegraded || stat.probeID != 0 {
		return false
	}
	nowNanos := now.UnixNano()
	return stat.nextProbeAt.Load() <= nowNanos
}

func (s *openAIAccountRuntimeStats) probeOpportunityOrder(accountID int64, now time.Time) (continuing bool, nextAt int64) {
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return false, 0
	}
	defer release()
	stat.mu.Lock()
	defer stat.mu.Unlock()
	if stat.smartMode && stat.probeID == 0 && !stat.probeBatchStarted.IsZero() && now.Sub(stat.probeBatchStarted) > openAIAccountSmartProbeBatchTimeout {
		s.rescheduleSmartProbeBatchLocked(stat, now, false)
	}
	continuing = stat.smartMode && stat.probeID == 0 && !stat.probeBatchStarted.IsZero() &&
		len(stat.probeBatchSamples) > 0 && now.Sub(stat.probeBatchStarted) <= openAIAccountSmartProbeBatchTimeout
	return continuing, stat.nextProbeAt.Load()
}

func (s *openAIAccountRuntimeStats) cancelProbeLease(accountID int64, now time.Time) {
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return
	}
	defer release()
	stat.mu.Lock()
	probeID := stat.probeID
	stat.mu.Unlock()
	s.cancelProbeLeaseForID(accountID, probeID, now)
}

func (s *openAIAccountRuntimeStats) cancelProbeLeaseForID(accountID int64, probeID uint64, now time.Time) {
	if probeID == 0 {
		return
	}
	stat, release, ok := s.acquire(accountID, false, true)
	if !ok {
		return
	}
	shouldDrainProbeTrace := false
	stat.mu.Lock()
	if stat.probeID == probeID {
		batchSamplesBefore := len(stat.probeBatchSamples)
		batchTimedOut := stat.smartMode && !stat.probeBatchStarted.IsZero() &&
			now.Sub(stat.probeBatchStarted) > openAIAccountSmartProbeBatchTimeout
		reason := "not_attributable"
		if batchTimedOut {
			reason = "batch_timeout"
		}
		stat.probeID = 0
		stat.probeStartedAt = time.Time{}
		stat.probeActive.Store(false)
		stat.probeLeaseUntil.Store(0)
		if batchTimedOut {
			s.rescheduleSmartProbeBatchLocked(stat, now, false)
		}
		if stat.probeTraceStarted {
			event := openAIProbeTraceEventLocked(stat, accountID, "cancel", "inconclusive", reason)
			event.batchSampleCount = batchSamplesBefore
			shouldDrainProbeTrace = enqueueOpenAIAccountProbeTraceLocked(stat, event)
		}
		clearOpenAIProbeTraceLocked(stat)
	}
	stat.mu.Unlock()
	release()
	if shouldDrainProbeTrace {
		drainOpenAIAccountProbeTraceQueue(stat)
	}
}

func (s *openAIAccountRuntimeStats) smartHealthSnapshots(accountIDs []int64, now time.Time) map[int64]OpenAISmartHealthSnapshot {
	if s == nil || len(accountIDs) == 0 {
		return nil
	}
	result := make(map[int64]OpenAISmartHealthSnapshot)
	for _, accountID := range accountIDs {
		stat, release, ok := s.acquire(accountID, false, false)
		if !ok {
			continue
		}
		func() {
			defer release()
			stat.mu.Lock()
			defer stat.mu.Unlock()
			if stat.probeID == 0 && !stat.probeBatchStarted.IsZero() && now.Sub(stat.probeBatchStarted) > openAIAccountSmartProbeBatchTimeout {
				s.rescheduleSmartProbeBatchLocked(stat, now, false)
			}
			if !stat.smartMode || stat.smartLevel != openAIAccountHealthDegraded {
				return
			}
			snapshot := OpenAISmartHealthSnapshot{
				Reason:      openAIAccountHealthReasonName(stat.smartReason),
				BackoffStep: stat.backoffIndex + 1,
			}
			switch {
			case stat.probeID != 0:
				snapshot.State = "probing"
			case (!stat.probeBatchStarted.IsZero() && len(stat.probeBatchSamples) > 0 && now.Sub(stat.probeBatchStarted) <= openAIAccountSmartProbeBatchTimeout) || stat.nextProbeAt.Load() <= now.UnixNano():
				snapshot.State = "ready"
			default:
				snapshot.State = "cooldown"
				nextProbeAt := time.Unix(0, stat.nextProbeAt.Load()).UTC()
				snapshot.NextProbeAt = &nextProbeAt
			}
			result[accountID] = snapshot
		}()
	}
	return result
}

func (s *openAIAccountRuntimeStats) size() int {
	if s == nil {
		return 0
	}
	return int(s.accountCount.Load())
}

type defaultOpenAIAccountScheduler struct {
	service                *OpenAIGatewayService
	metrics                openAIAccountSchedulerMetrics
	stats                  *openAIAccountRuntimeStats
	grokFreeQuotaGateCache sync.Map // key: int64(accountID), value: grokFreeQuotaGateCacheEntry
}

type openAISelectionProbeBudget struct {
	acquires  int
	rechecks  int
	attempted map[int64]struct{}
	limited   bool
}

func newOpenAISelectionProbeBudget() *openAISelectionProbeBudget {
	return &openAISelectionProbeBudget{attempted: make(map[int64]struct{})}
}

func (b *openAISelectionProbeBudget) enableLimit() {
	if b != nil {
		b.limited = true
	}
}

func (b *openAISelectionProbeBudget) recordAcquire(accountID int64) bool {
	if b == nil {
		return false
	}
	if !b.limited {
		return true
	}
	if b.acquires >= openAIAccountSelectionProbeLimit {
		return false
	}
	if b.attempted == nil {
		b.attempted = make(map[int64]struct{})
	}
	b.acquires++
	b.attempted[accountID] = struct{}{}
	return true
}

func (b *openAISelectionProbeBudget) recordRecheck() bool {
	if b == nil {
		return false
	}
	if !b.limited {
		return true
	}
	if b.rechecks >= openAIAccountSelectionProbeLimit {
		return false
	}
	b.rechecks++
	return true
}

func (b *openAISelectionProbeBudget) acquireExhausted() bool {
	return b != nil && b.limited && b.acquires >= openAIAccountSelectionProbeLimit
}

func (b *openAISelectionProbeBudget) wasAttempted(accountID int64) bool {
	if b == nil {
		return false
	}
	_, ok := b.attempted[accountID]
	return ok
}

type openAIStickyEscapeConfig struct {
	enabled   bool
	ttftMs    float64
	errorRate float64
}

func newDefaultOpenAIAccountScheduler(service *OpenAIGatewayService, stats *openAIAccountRuntimeStats) OpenAIAccountScheduler {
	if stats == nil {
		stats = newOpenAIAccountRuntimeStats()
	}
	return &defaultOpenAIAccountScheduler{
		service: service,
		stats:   stats,
	}
}

func (s *defaultOpenAIAccountScheduler) Select(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (selection *AccountSelectionResult, decision OpenAIAccountScheduleDecision, err error) {
	if s != nil && s.service != nil && s.service.openAIGroupRequiresPrivacySet(ctx, req.GroupID) {
		req.RequirePrivacySet = true
	}
	start := time.Now()
	healthEscapeReason := ""
	// 命名返回值保证 defer 写入的耗时同时返回给调用方。
	defer func() {
		decision.LatencyMs = time.Since(start).Milliseconds()
		decision.HealthEscape = healthEscapeReason != ""
		if selection != nil && selection.Account != nil {
			decision.SelectedPriority = openAIAccountSchedulingPriority(selection.Account)
			if req.SmartHealthEnabled {
				selectedHealth := openAIAccountHealthUnknown
				if s.stats != nil {
					selectedHealth = s.stats.health(selection.Account.ID, time.Now(), s.service.openAIStickyEscapeConfig()).level
				}
				decision.SelectedHealth = openAIAccountHealthLevelName(selectedHealth)
			}
		}
		if req.SmartHealthEnabled && decision.SelectedHealth == "" {
			decision.SelectedHealth = openAIAccountHealthLevelName(openAIAccountHealthUnknown)
		}
		s.metrics.recordSelect(decision)
		s.logOpenAIAccountScheduleDecision(req, selection, decision, err, healthEscapeReason)
	}()

	previousResponseID := strings.TrimSpace(req.PreviousResponseID)
	if previousResponseID != "" && NormalizeOpenAICompatiblePlatform(req.Platform) == PlatformOpenAI &&
		(!req.StickyWeighted || !req.PreviousResponseCanMove) {
		selection, err := s.service.selectAccountByPreviousResponseIDForCapability(
			ctx,
			req.GroupID,
			previousResponseID,
			req.RequestedModel,
			req.ExcludedIDs,
			req.RequiredCapability,
			req.RequireCompact,
		)
		if err != nil {
			return nil, decision, err
		}
		if selection != nil && selection.Account != nil {
			compatible, _ := s.isAccountRequestCompatibleReason(ctx, selection.Account, req)
			hasGroupMetadata := len(selection.Account.GroupIDs) > 0 || len(selection.Account.AccountGroups) > 0
			groupCompatible := !hasGroupMetadata || openAIStickyAccountMatchesGroup(selection.Account, req.GroupID)
			if hasGroupMetadata && s.service != nil {
				groupCompatible = s.service.openAIAccountMatchesSchedulingGroup(selection.Account, req.GroupID)
			}
			if !groupCompatible ||
				!compatible || !s.isAccountTransportCompatible(selection.Account, req.RequiredTransport) {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil && req.SmartHealthEnabled && s.stats != nil {
			assessment := s.stats.health(selection.Account.ID, time.Now(), s.service.openAIStickyEscapeConfig())
			if assessment.level == openAIAccountHealthDegraded {
				healthEscapeReason = openAIAccountHealthReasonName(assessment.reason)
				s.metrics.recordSmartHealthDegradedEscape()
				slog.Info("openai_account_health_previous_response_escape",
					"account_id", selection.Account.ID,
					"reason", healthEscapeReason,
					"error_rate", assessment.errorRate,
					"ttft_ms", assessment.ttft,
					"sample_count", assessment.samples,
				)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				selection = nil
			}
		}
		if selection != nil && selection.Account != nil {
			decision.Layer = openAIAccountScheduleLayerPreviousResponse
			decision.StickyPreviousHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			if req.SessionHash != "" {
				_ = s.service.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, selection.Account.ID)
			}
			return selection, decision, nil
		}
	}

	if req.GuardianParentAccountID > 0 {
		parentReq := req
		parentReq.StickyAccountID = req.GuardianParentAccountID
		parentReq.PreserveStickyBinding = true
		selection, _, err := s.selectBySessionHash(ctx, parentReq)
		if err != nil {
			return nil, decision, err
		}
		if selection != nil && selection.Account != nil {
			decision.Layer = openAIAccountScheduleLayerGuardianParent
			decision.StickySessionHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			return selection, decision, nil
		}
	}

	if !req.StickyWeighted {
		stickyHealthEscapeReason := ""
		selection, escapedSticky, err := s.selectBySessionHash(ctx, req, &stickyHealthEscapeReason)
		if err != nil {
			return nil, decision, err
		}
		if selection != nil && selection.Account != nil {
			decision.Layer = openAIAccountScheduleLayerSessionSticky
			decision.StickySessionHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			return selection, decision, nil
		}
		if escapedSticky {
			healthEscapeReason = stickyHealthEscapeReason
			req.PreserveStickyBinding = true
			req.SmartHealthStickyEscaped = true
		}
	}

	selection, candidateCount, topK, loadSkew, err := s.selectByLoadBalance(ctx, req, &decision)
	decision.Layer = openAIAccountScheduleLayerLoadBalance
	decision.CandidateCount = candidateCount
	decision.TopK = topK
	decision.LoadSkew = loadSkew
	if err != nil {
		return nil, decision, err
	}
	if selection != nil && selection.Account != nil {
		decision.SelectedAccountID = selection.Account.ID
		decision.SelectedAccountType = selection.Account.Type
		if req.StickyWeighted {
			if req.StickyPreviousAccountID > 0 && selection.Account.ID == req.StickyPreviousAccountID {
				decision.StickyPreviousHit = true
			}
			if req.StickyAccountID > 0 && selection.Account.ID == req.StickyAccountID {
				decision.StickySessionHit = true
			}
		}
	}
	return selection, decision, nil
}

func (s *defaultOpenAIAccountScheduler) logOpenAIAccountScheduleDecision(
	req OpenAIAccountScheduleRequest,
	selection *AccountSelectionResult,
	decision OpenAIAccountScheduleDecision,
	err error,
	healthEscapeReason string,
) {
	if !req.SmartHealthEnabled {
		return
	}

	reason := "selected"
	if healthEscapeReason != "" {
		reason = healthEscapeReason
	} else if err != nil {
		switch {
		case errors.Is(err, ErrNoAvailableCompactAccounts):
			reason = "no_compact_candidate"
		case errors.Is(err, ErrNoAvailableAccounts):
			reason = "no_available_account"
		default:
			reason = "selection_error"
		}
	}
	filterReasons := decision.FilterReasons
	if filterReasons == "" && err != nil {
		filterReasons = reason
	}
	candidateCount := decision.CandidateCount
	if total := decision.PrimaryCount + decision.DegradedCount; total > 0 {
		candidateCount = total
	}
	selectedAccountID := decision.SelectedAccountID
	if selection != nil && selection.Account != nil {
		selectedAccountID = selection.Account.ID
	}

	attrs := []any{
		"group_id", derefGroupID(req.GroupID),
		"platform", NormalizeOpenAICompatiblePlatform(req.Platform),
		"model", normalizeOpenAISchedulingModelForLog(req.RequestedModel),
		"candidate_count", candidateCount,
		"primary_count", decision.PrimaryCount,
		"degraded_count", decision.DegradedCount,
		"top_k", decision.TopK,
		"selection_layer", decision.Layer,
		"selected_account_id", selectedAccountID,
		"selected_priority", decision.SelectedPriority,
		"selected_health", decision.SelectedHealth,
		"sticky_hit", decision.StickyPreviousHit || decision.StickySessionHit,
		"health_escape", decision.HealthEscape,
		"filter_reasons", filterReasons,
		"reason", reason,
	}
	if selection != nil && selection.Account != nil && selection.Account.smartHealthProbeID != 0 && s.stats != nil {
		traceID, correlation, found := s.stats.probeTraceFor(selection.Account.ID, selection.Account.smartHealthProbeID)
		if found {
			if traceID != "" {
				attrs = append(attrs, "probe_trace_id", traceID)
			}
			attrs = append(attrs, "correlation", correlation)
		}
	}
	slog.Info("openai_account_schedule_decision", attrs...)
}

func (s *openAIAccountRuntimeStats) probeTraceFor(accountID int64, probeID uint64) (string, string, bool) {
	if s == nil || probeID == 0 {
		return "", "", false
	}
	stat, release, ok := s.acquire(accountID, false, false)
	if !ok {
		return "", "", false
	}
	defer release()
	stat.mu.Lock()
	defer stat.mu.Unlock()
	if stat.probeID != probeID || !stat.probeTraceStarted {
		return "", "", false
	}
	return stat.probeTraceID, stat.probeTraceCorrelation, true
}

func normalizeOpenAISchedulingModelForLog(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if len(model) > 128 {
		return model[:128]
	}
	return model
}

func (s *defaultOpenAIAccountScheduler) selectBySessionHash(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	healthEscapeReasonOutput ...*string,
) (*AccountSelectionResult, bool, error) {
	var healthEscapeReasonTarget *string
	if len(healthEscapeReasonOutput) > 0 {
		healthEscapeReasonTarget = healthEscapeReasonOutput[0]
		if healthEscapeReasonTarget != nil {
			*healthEscapeReasonTarget = ""
		}
	}
	sessionHash := strings.TrimSpace(req.SessionHash)
	if sessionHash == "" || s == nil || s.service == nil || s.service.cache == nil {
		return nil, false, nil
	}

	accountID := req.StickyAccountID
	clearBinding := func() {
		if !req.PreserveStickyBinding {
			_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, sessionHash)
		}
	}
	if accountID <= 0 {
		var err error
		accountID, err = s.service.getStickySessionAccountID(ctx, req.GroupID, sessionHash)
		if err != nil || accountID <= 0 {
			return nil, false, nil
		}
	}
	if accountID <= 0 {
		return nil, false, nil
	}
	if req.ExcludedIDs != nil {
		if _, excluded := req.ExcludedIDs[accountID]; excluded {
			return nil, false, nil
		}
	}

	account, err := s.service.getSchedulableAccount(ctx, accountID)
	if err != nil || account == nil {
		clearBinding()
		return nil, false, nil
	}
	if shouldClearStickySession(account, req.RequestedModel) || account.Platform != NormalizeOpenAICompatiblePlatform(req.Platform) || !account.IsOpenAICompatible() || !account.IsSchedulable() {
		clearBinding()
		return nil, false, nil
	}
	if !s.isAccountRequestCompatible(ctx, account, req) {
		return nil, false, nil
	}
	if !s.isAccountTransportCompatible(account, req.RequiredTransport) {
		clearBinding()
		return nil, false, nil
	}
	account = s.service.recheckSelectedOpenAIAccountFromDB(ctx, account, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability)
	if account == nil || !s.service.openAIAccountMatchesSchedulingGroup(account, req.GroupID) || !s.isAccountRequestCompatible(ctx, account, req) || !s.isAccountTransportCompatible(account, req.RequiredTransport) {
		clearBinding()
		return nil, false, nil
	}
	// Free-tier soft gate: sticky session must not pin an over-quota free OAuth account.
	// Admin QueryQuota / import probes do not use this path.
	if account != nil && len(s.filterGrokFreeQuotaAccounts(ctx, []Account{*account})) == 0 {
		clearBinding()
		return nil, false, nil
	}
	// Team+model cool: sticky must not pin a sibling under the same team 429 window.
	now := time.Now()
	upstreamModel := canonicalOpenAIAccountSchedulingModel(account, req.RequestedModel)
	if account != nil && isGrokTeamModelRateLimited(account, upstreamModel, now) {
		clearBinding()
		return nil, false, nil
	}
	if account != nil && isGrokModelQuotaBlocked(account.ID, upstreamModel, now) {
		clearBinding()
		return nil, false, nil
	}
	escapeCfg := s.service.openAIStickyEscapeConfig()
	if req.SmartHealthEnabled && s.stats != nil {
		assessment := s.stats.health(accountID, now, escapeCfg)
		if assessment.level == openAIAccountHealthDegraded && !req.DisableStickyEscape {
			healthEscapeReason := openAIAccountHealthReasonName(assessment.reason)
			if healthEscapeReasonTarget != nil {
				*healthEscapeReasonTarget = healthEscapeReason
			}
			s.metrics.recordSmartHealthDegradedEscape()
			slog.Info("openai_account_health_sticky_escape",
				"account_id", accountID,
				"reason", healthEscapeReason,
				"error_rate", assessment.errorRate,
				"ttft_ms", assessment.ttft,
				"sample_count", assessment.samples,
			)
			return nil, true, nil
		}
	} else if reason, errorRate, ttft, shouldEscape := s.shouldEscapeStickyAccount(accountID, escapeCfg); shouldEscape && !req.DisableStickyEscape {
		slog.Info("sticky_escape_triggered",
			"account_id", accountID,
			"reason", reason,
			"error_rate", errorRate,
			"ttft", ttft,
		)
		return nil, true, nil
	}
	result, acquireErr := s.service.tryAcquireAccountSlot(ctx, accountID, account.Concurrency)
	if acquireErr != nil && req.DisableStickyEscape {
		return nil, false, acquireErr
	}
	if acquireErr == nil && result != nil && result.Acquired {
		if !req.PreserveStickyBinding {
			_ = s.service.refreshStickySessionTTL(ctx, req.GroupID, sessionHash, s.service.openAIWSSessionStickyTTL())
		}
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account:     account,
			Acquired:    true,
			ReleaseFunc: result.ReleaseFunc,
		}), false, nil
	}

	cfg := s.service.schedulingConfig()
	// WaitPlan.MaxConcurrency 使用 Concurrency（非 EffectiveLoadFactor），因为 WaitPlan 控制的是 Redis 实际并发槽位等待。
	if s.service.concurrencyService != nil {
		if escapeCfg.enabled && !req.DisableStickyEscape && acquireErr == nil && result != nil && !result.Acquired {
			errorRate, ttft, _ := s.stats.snapshot(accountID)
			slog.Info("sticky_escape_triggered",
				"account_id", accountID,
				"reason", "concurrency_full",
				"error_rate", errorRate,
				"ttft", ttft,
			)
			return nil, true, nil
		}
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account: account,
			WaitPlan: &AccountWaitPlan{
				AccountID:      accountID,
				MaxConcurrency: account.Concurrency,
				Timeout:        cfg.StickySessionWaitTimeout,
				MaxWaiting:     cfg.StickySessionMaxWaiting,
			},
		}), false, nil
	}
	return nil, false, nil
}

func openAIStickyAccountMatchesGroup(account *Account, groupID *int64) bool {
	if account == nil {
		return false
	}
	if groupID == nil {
		return len(account.AccountGroups) == 0 && len(account.GroupIDs) == 0
	}
	for _, accountGroupID := range account.GroupIDs {
		if accountGroupID == *groupID {
			return true
		}
	}
	for _, accountGroup := range account.AccountGroups {
		if accountGroup.GroupID == *groupID {
			return true
		}
	}
	return false
}

func openAIAccountSchedulingPriority(account *Account) int {
	if account == nil {
		return 0
	}
	return account.Priority
}

func (s *defaultOpenAIAccountScheduler) shouldEscapeStickyAccount(accountID int64, cfg openAIStickyEscapeConfig) (reason string, errorRate float64, ttft float64, shouldEscape bool) {
	if !cfg.enabled || s == nil || s.stats == nil || accountID <= 0 {
		return "", 0, 0, false
	}
	errorRate, ttft, hasTTFT := s.stats.snapshot(accountID)
	if hasTTFT && ttft > cfg.ttftMs {
		return "ttft", errorRate, ttft, true
	}
	if errorRate > cfg.errorRate {
		return "error_rate", errorRate, ttft, true
	}
	return "", errorRate, ttft, false
}

type openAIAccountCandidateScore struct {
	account      *Account
	loadInfo     *AccountLoadInfo
	loadKnown    bool
	score        float64
	priority     int
	errorRate    float64
	ttft         float64
	hasTTFT      bool
	health       openAIAccountHealthLevel
	healthReason uint32
	healthProbe  bool
}

type openAIAccountCandidateHeap []openAIAccountCandidateScore

func (h openAIAccountCandidateHeap) Len() int {
	return len(h)
}

func (h openAIAccountCandidateHeap) Less(i, j int) bool {
	// 最小堆根节点保存“最差”候选，便于 O(log k) 维护 topK。
	return isOpenAIAccountCandidateBetter(h[j], h[i])
}

func (h openAIAccountCandidateHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
}

func (h *openAIAccountCandidateHeap) Push(x any) {
	candidate, ok := x.(openAIAccountCandidateScore)
	if !ok {
		panic("openAIAccountCandidateHeap: invalid element type")
	}
	*h = append(*h, candidate)
}

func (h *openAIAccountCandidateHeap) Pop() any {
	old := *h
	n := len(old)
	last := old[n-1]
	*h = old[:n-1]
	return last
}

func isOpenAIAccountCandidateBetter(left openAIAccountCandidateScore, right openAIAccountCandidateScore) bool {
	if left.score != right.score {
		return left.score > right.score
	}
	if left.account.Priority != right.account.Priority {
		return left.account.Priority < right.account.Priority
	}
	if left.loadInfo.LoadRate != right.loadInfo.LoadRate {
		return left.loadInfo.LoadRate < right.loadInfo.LoadRate
	}
	if left.loadInfo.WaitingCount != right.loadInfo.WaitingCount {
		return left.loadInfo.WaitingCount < right.loadInfo.WaitingCount
	}
	return left.account.ID < right.account.ID
}

func selectTopKOpenAICandidates(candidates []openAIAccountCandidateScore, topK int) []openAIAccountCandidateScore {
	if len(candidates) == 0 {
		return nil
	}
	if topK <= 0 {
		topK = 1
	}
	if topK >= len(candidates) {
		ranked := append([]openAIAccountCandidateScore(nil), candidates...)
		sort.Slice(ranked, func(i, j int) bool {
			return isOpenAIAccountCandidateBetter(ranked[i], ranked[j])
		})
		return ranked
	}

	best := make(openAIAccountCandidateHeap, 0, topK)
	for _, candidate := range candidates {
		if len(best) < topK {
			heap.Push(&best, candidate)
			continue
		}
		if isOpenAIAccountCandidateBetter(candidate, best[0]) {
			best[0] = candidate
			heap.Fix(&best, 0)
		}
	}

	ranked := make([]openAIAccountCandidateScore, len(best))
	copy(ranked, best)
	sort.Slice(ranked, func(i, j int) bool {
		return isOpenAIAccountCandidateBetter(ranked[i], ranked[j])
	})
	return ranked
}

type openAISelectionRNG struct {
	state uint64
}

func newOpenAISelectionRNG(seed uint64) openAISelectionRNG {
	if seed == 0 {
		seed = 0x9e3779b97f4a7c15
	}
	return openAISelectionRNG{state: seed}
}

func (r *openAISelectionRNG) nextUint64() uint64 {
	// xorshift64*
	x := r.state
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.state = x
	return x * 2685821657736338717
}

func (r *openAISelectionRNG) nextFloat64() float64 {
	// [0,1)
	return float64(r.nextUint64()>>11) / (1 << 53)
}

func deriveOpenAISelectionSeed(req OpenAIAccountScheduleRequest) uint64 {
	hasher := fnv.New64a()
	writeValue := func(value string) {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return
		}
		_, _ = hasher.Write([]byte(trimmed))
		_, _ = hasher.Write([]byte{0})
	}

	writeValue(req.SessionHash)
	writeValue(req.PreviousResponseID)
	writeValue(req.RequestedModel)
	if req.GroupID != nil {
		_, _ = hasher.Write([]byte(strconv.FormatInt(*req.GroupID, 10)))
	}

	seed := hasher.Sum64()
	// 对“无会话锚点”的纯负载均衡请求引入时间熵，避免固定命中同一账号。
	if strings.TrimSpace(req.SessionHash) == "" && strings.TrimSpace(req.PreviousResponseID) == "" {
		seed ^= uint64(time.Now().UnixNano())
	}
	if seed == 0 {
		seed = uint64(time.Now().UnixNano()) ^ 0x9e3779b97f4a7c15
	}
	return seed
}

func buildOpenAIWeightedSelectionOrder(
	candidates []openAIAccountCandidateScore,
	req OpenAIAccountScheduleRequest,
) []openAIAccountCandidateScore {
	if len(candidates) <= 1 {
		return append([]openAIAccountCandidateScore(nil), candidates...)
	}

	pool := append([]openAIAccountCandidateScore(nil), candidates...)
	weights := make([]float64, len(pool))
	minScore := pool[0].score
	for i := 1; i < len(pool); i++ {
		if pool[i].score < minScore {
			minScore = pool[i].score
		}
	}
	for i := range pool {
		// 将 top-K 分值平移到正区间，避免“单一最高分账号”长期垄断。
		weight := (pool[i].score - minScore) + 1.0
		if math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
			weight = 1.0
		}
		weights[i] = weight
	}

	order := make([]openAIAccountCandidateScore, 0, len(pool))
	rng := newOpenAISelectionRNG(deriveOpenAISelectionSeed(req))
	for len(pool) > 0 {
		total := 0.0
		for _, w := range weights {
			total += w
		}

		selectedIdx := 0
		if total > 0 {
			r := rng.nextFloat64() * total
			acc := 0.0
			for i, w := range weights {
				acc += w
				if r <= acc {
					selectedIdx = i
					break
				}
			}
		} else {
			selectedIdx = int(rng.nextUint64() % uint64(len(pool)))
		}

		order = append(order, pool[selectedIdx])
		pool = append(pool[:selectedIdx], pool[selectedIdx+1:]...)
		weights = append(weights[:selectedIdx], weights[selectedIdx+1:]...)
	}
	return order
}

func (s *defaultOpenAIAccountScheduler) buildOpenAIAccountLoadPlan(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	filtered []*Account,
	loadMap map[int64]*AccountLoadInfo,
) openAIAccountLoadPlan {
	allCandidates := make([]openAIAccountCandidateScore, 0, len(filtered))
	for _, account := range filtered {
		loadInfo, loadKnown := loadMap[account.ID]
		if !loadKnown || loadInfo == nil {
			loadInfo = &AccountLoadInfo{AccountID: account.ID}
			loadKnown = false
		}
		errorRate, ttft, hasTTFT := 0.0, 0.0, false
		if s.stats != nil {
			errorRate, ttft, hasTTFT = s.stats.snapshot(account.ID)
		}
		health := openAIAccountHealthUnknown
		healthReason := uint32(openAIAccountHealthReasonNone)
		healthProbe := false
		if req.SmartHealthEnabled && s.stats != nil {
			assessment := s.stats.health(account.ID, time.Now(), s.service.openAIStickyEscapeConfig())
			health = assessment.level
			healthReason = assessment.reason
			healthProbe = req.SmartHealthProbeAccountID == account.ID
		}
		allCandidates = append(allCandidates, openAIAccountCandidateScore{
			account:      account,
			loadInfo:     loadInfo,
			loadKnown:    loadKnown,
			errorRate:    errorRate,
			ttft:         ttft,
			hasTTFT:      hasTTFT,
			health:       health,
			healthReason: healthReason,
			healthProbe:  healthProbe,
		})
	}

	candidates := allCandidates
	staleSnapshotCompactRetry := make([]openAIAccountCandidateScore, 0, len(allCandidates))
	if req.RequireCompact {
		candidates = make([]openAIAccountCandidateScore, 0, len(allCandidates))
		for _, candidate := range allCandidates {
			if openAICompactSupportTier(candidate.account) == 0 {
				staleSnapshotCompactRetry = append(staleSnapshotCompactRetry, candidate)
				continue
			}
			candidates = append(candidates, candidate)
		}
	}

	plan := openAIAccountLoadPlan{
		allCandidates:             allCandidates,
		candidates:                candidates,
		staleSnapshotCompactRetry: staleSnapshotCompactRetry,
		candidateCount:            len(candidates),
	}
	if len(candidates) == 0 {
		plan.selectionOrder = s.buildOpenAISelectionOrder(req, plan)
		return plan
	}

	minPriority, maxPriority := openAIAccountSchedulingPriority(candidates[0].account), openAIAccountSchedulingPriority(candidates[0].account)
	maxWaiting := 1
	loadRateSum := 0.0
	loadRateSumSquares := 0.0
	minTTFT, maxTTFT := 0.0, 0.0
	hasTTFTSample := false
	for i := range candidates {
		candidate := &candidates[i]
		candidate.priority = openAIAccountSchedulingPriority(candidate.account)
		if candidate.priority < minPriority {
			minPriority = candidate.priority
		}
		if candidate.priority > maxPriority {
			maxPriority = candidate.priority
		}
		if candidate.loadInfo.WaitingCount > maxWaiting {
			maxWaiting = candidate.loadInfo.WaitingCount
		}
		if candidate.hasTTFT && candidate.ttft > 0 {
			if !hasTTFTSample {
				minTTFT, maxTTFT = candidate.ttft, candidate.ttft
				hasTTFTSample = true
			} else {
				if candidate.ttft < minTTFT {
					minTTFT = candidate.ttft
				}
				if candidate.ttft > maxTTFT {
					maxTTFT = candidate.ttft
				}
			}
		}
		loadRate := float64(candidate.loadInfo.LoadRate)
		loadRateSum += loadRate
		loadRateSumSquares += loadRate * loadRate
	}
	plan.loadSkew = calcLoadSkewByMoments(loadRateSum, loadRateSumSquares, len(candidates))

	weights := s.service.openAIWSSchedulerWeightsForRequest(ctx)
	now := time.Now()
	upstreamCostFactors := map[int64]float64(nil)
	if req.UseUpstreamTokenCost && weights.UpstreamCost > 0 {
		accounts := make([]*Account, 0, len(candidates))
		for _, candidate := range candidates {
			accounts = append(accounts, candidate.account)
		}
		upstreamCostFactors = openAIUpstreamCostFactors(accounts, now, s.service.openAIOAuthSchedulingRateMultiplier(ctx))
		for _, factor := range upstreamCostFactors {
			if factor != openAIUpstreamCostNeutralFactor {
				plan.includeOverflowFallback = true
				break
			}
		}
	}

	// Reset 因子（use-it-or-lose-it）：优先读取 Codex 5h 重置时间，回退到会话窗口。
	// 剩余时间越短 → 因子越接近 1（越早重置越优先用尽）。无活跃窗口的账号因子为 0。
	// 仅在 weights.Reset > 0 时计算，默认关闭不影响原有行为。
	minResetRemaining, maxResetRemaining := 0.0, 0.0
	hasResetSample := false
	if weights.Reset > 0 {
		for _, candidate := range candidates {
			end, ok := openAISchedulingResetWindowEnd(candidate.account, now)
			if !ok {
				continue
			}
			remaining := end.Sub(now).Seconds()
			if !hasResetSample {
				minResetRemaining, maxResetRemaining = remaining, remaining
				hasResetSample = true
				continue
			}
			if remaining < minResetRemaining {
				minResetRemaining = remaining
			}
			if remaining > maxResetRemaining {
				maxResetRemaining = remaining
			}
		}
	}

	for i := range candidates {
		item := &candidates[i]
		priorityFactor := 1.0
		if maxPriority > minPriority {
			priorityFactor = 1 - float64(item.priority-minPriority)/float64(maxPriority-minPriority)
		}
		loadFactor := 1 - clamp01(float64(item.loadInfo.LoadRate)/100.0)
		queueFactor := 1 - clamp01(float64(item.loadInfo.WaitingCount)/float64(maxWaiting))
		errorFactor := 1 - clamp01(item.errorRate)
		ttftFactor := 0.5
		if req.SmartHealthEnabled && item.health == openAIAccountHealthUnknown {
			errorFactor = 0.5
		}
		if item.hasTTFT && hasTTFTSample && maxTTFT > minTTFT {
			if !req.SmartHealthEnabled || item.health != openAIAccountHealthUnknown {
				ttftFactor = 1 - clamp01((item.ttft-minTTFT)/(maxTTFT-minTTFT))
			}
		}
		resetFactor := 0.0
		if weights.Reset > 0 && hasResetSample {
			if end, ok := openAISchedulingResetWindowEnd(item.account, now); ok {
				if maxResetRemaining > minResetRemaining {
					resetFactor = 1 - clamp01((end.Sub(now).Seconds()-minResetRemaining)/(maxResetRemaining-minResetRemaining))
				} else {
					// 所有有窗口的账号剩余时间相同：一律给满分，让其优于无窗口账号。
					resetFactor = 1
				}
			}
		}
		quotaHeadroomFactor := 0.0
		if weights.QuotaHeadroom > 0 {
			quotaHeadroomFactor = openAIQuotaHeadroomFactor(item.account, now)
		}
		upstreamCostFactor := openAIUpstreamCostNeutralFactor
		if factor, ok := upstreamCostFactors[item.account.ID]; ok {
			upstreamCostFactor = factor
		}

		item.score = weights.Priority*priorityFactor +
			weights.Load*loadFactor +
			weights.Queue*queueFactor +
			weights.ErrorRate*errorFactor +
			weights.TTFT*ttftFactor +
			weights.Reset*resetFactor +
			weights.QuotaHeadroom*quotaHeadroomFactor +
			weights.UpstreamCost*(upstreamCostFactor-openAIUpstreamCostNeutralFactor)
		if req.StickyWeighted {
			if req.PreviousResponseCanMove && req.StickyPreviousAccountID > 0 && item.account.ID == req.StickyPreviousAccountID {
				item.score += weights.Previous
			}
			if req.StickyAccountID > 0 && item.account.ID == req.StickyAccountID {
				item.score += weights.SessionSticky
			}
		}
	}
	if req.SmartHealthEnabled && req.SmartHealthProbeAccountID > 0 {
		primaryCandidates := make([]openAIAccountCandidateScore, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.healthProbe {
				plan.probeCandidates = append(plan.probeCandidates, candidate)
				continue
			}
			primaryCandidates = append(primaryCandidates, candidate)
		}
		candidates = primaryCandidates
	}
	plan.candidates = candidates

	plan.topK = s.service.openAIWSLBTopKForRequest(ctx)
	if plan.topK > len(candidates) {
		plan.topK = len(candidates)
	}
	if plan.topK <= 0 {
		plan.topK = 1
	}

	plan.selectionOrder = s.buildOpenAISelectionOrder(req, plan)
	if len(plan.probeCandidates) > 0 {
		probePlan := plan
		probePlan.candidates = plan.probeCandidates
		probePlan.probeCandidates = nil
		probePlan.topK = 1
		probeOrder := s.buildOpenAISelectionOrder(req, probePlan)
		seen := make(map[int64]struct{}, len(probeOrder)+len(plan.selectionOrder))
		ordered := make([]openAIAccountCandidateScore, 0, len(probeOrder)+len(plan.selectionOrder))
		for _, candidate := range probeOrder {
			if candidate.account == nil {
				continue
			}
			if _, exists := seen[candidate.account.ID]; exists {
				continue
			}
			seen[candidate.account.ID] = struct{}{}
			ordered = append(ordered, candidate)
		}
		for _, candidate := range plan.selectionOrder {
			if candidate.account == nil {
				continue
			}
			if _, exists := seen[candidate.account.ID]; exists {
				continue
			}
			seen[candidate.account.ID] = struct{}{}
			ordered = append(ordered, candidate)
		}
		plan.selectionOrder = ordered
	}
	return plan
}

func (s *defaultOpenAIAccountScheduler) buildOpenAISelectionOrder(
	req OpenAIAccountScheduleRequest,
	plan openAIAccountLoadPlan,
) []openAIAccountCandidateScore {
	buildSelectionOrder := func(pool []openAIAccountCandidateScore, topK int, includeOverflowFallback bool) []openAIAccountCandidateScore {
		if len(pool) == 0 || topK <= 0 {
			return nil
		}
		groupTopK := topK
		if groupTopK > len(pool) {
			groupTopK = len(pool)
		}
		ranked := selectTopKOpenAICandidates(pool, groupTopK)
		var primary []openAIAccountCandidateScore
		if req.StickyWeighted {
			for _, stickyID := range []int64{req.StickyPreviousAccountID, req.StickyAccountID} {
				if stickyID <= 0 {
					continue
				}
				for i, candidate := range ranked {
					if candidate.account != nil && candidate.account.ID == stickyID {
						primary = append([]openAIAccountCandidateScore{candidate}, ranked[:i]...)
						primary = append(primary, ranked[i+1:]...)
						break
					}
				}
				if len(primary) > 0 {
					break
				}
			}
		}
		if len(primary) == 0 {
			primary = buildOpenAIWeightedSelectionOrder(ranked, req)
		}
		if !includeOverflowFallback || groupTopK >= len(pool) {
			return primary
		}

		selected := make(map[int64]struct{}, len(primary))
		for _, candidate := range primary {
			selected[candidate.account.ID] = struct{}{}
		}
		overflow := make([]openAIAccountCandidateScore, 0, len(pool)-len(primary))
		for _, candidate := range pool {
			if _, ok := selected[candidate.account.ID]; !ok {
				overflow = append(overflow, candidate)
			}
		}
		sort.Slice(overflow, func(i, j int) bool {
			return isOpenAIAccountCandidateBetter(overflow[i], overflow[j])
		})
		return append(primary, overflow...)
	}
	buildPriorityFirstSelectionOrder := func(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
		if len(pool) == 0 || plan.topK <= 0 {
			return nil
		}
		byPriority := make(map[int][]openAIAccountCandidateScore)
		priorities := make([]int, 0, len(pool))
		for _, candidate := range pool {
			priority := candidate.priority
			if candidate.account != nil {
				priority = openAIAccountSchedulingPriority(candidate.account)
			}
			if _, exists := byPriority[priority]; !exists {
				priorities = append(priorities, priority)
			}
			byPriority[priority] = append(byPriority[priority], candidate)
		}
		sort.Ints(priorities)

		ordered := make([]openAIAccountCandidateScore, 0, len(pool))
		for _, priority := range priorities {
			group := byPriority[priority]
			// TopK and weighted selection remain available as tie-breaking inside
			// one priority. Every same-priority candidate stays in the retry tail,
			// so a full slot cannot cause a larger priority to jump the queue.
			ordered = append(ordered, buildSelectionOrder(group, plan.topK, true)...)
		}
		return ordered
	}

	if req.RequireCompact {
		supported := make([]openAIAccountCandidateScore, 0, len(plan.candidates))
		unknown := make([]openAIAccountCandidateScore, 0, len(plan.candidates))
		for _, candidate := range plan.candidates {
			switch openAICompactSupportTier(candidate.account) {
			case 2:
				supported = append(supported, candidate)
			case 1:
				unknown = append(unknown, candidate)
			}
		}
		selectionOrder := make([]openAIAccountCandidateScore, 0, len(plan.allCandidates))
		if req.SmartHealthEnabled {
			selectionOrder = append(selectionOrder, buildPriorityFirstSelectionOrder(supported)...)
			selectionOrder = append(selectionOrder, buildPriorityFirstSelectionOrder(unknown)...)
		} else {
			selectionOrder = append(selectionOrder, buildSelectionOrder(supported, plan.topK, plan.includeOverflowFallback)...)
			selectionOrder = append(selectionOrder, buildSelectionOrder(unknown, plan.topK, plan.includeOverflowFallback)...)
		}
		if len(plan.staleSnapshotCompactRetry) > 0 && s.service.schedulerSnapshot != nil {
			selectionOrder = append(selectionOrder, sortOpenAICompactRetryCandidates(plan.staleSnapshotCompactRetry)...)
		}
		return selectionOrder
	}

	if req.SmartHealthEnabled {
		return buildPriorityFirstSelectionOrder(plan.candidates)
	}
	return buildSelectionOrder(plan.candidates, plan.topK, plan.includeOverflowFallback)
}

func sortOpenAICompactRetryCandidates(pool []openAIAccountCandidateScore) []openAIAccountCandidateScore {
	if len(pool) == 0 {
		return nil
	}
	ordered := append([]openAIAccountCandidateScore(nil), pool...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.account.Priority != b.account.Priority {
			return a.account.Priority < b.account.Priority
		}
		if a.loadInfo.LoadRate != b.loadInfo.LoadRate {
			return a.loadInfo.LoadRate < b.loadInfo.LoadRate
		}
		if a.loadInfo.WaitingCount != b.loadInfo.WaitingCount {
			return a.loadInfo.WaitingCount < b.loadInfo.WaitingCount
		}
		switch {
		case a.account.LastUsedAt == nil && b.account.LastUsedAt != nil:
			return true
		case a.account.LastUsedAt != nil && b.account.LastUsedAt == nil:
			return false
		case a.account.LastUsedAt == nil && b.account.LastUsedAt == nil:
			return false
		default:
			return a.account.LastUsedAt.Before(*b.account.LastUsedAt)
		}
	})
	return ordered
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAISelectionOrder(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	selectionOrder []openAIAccountCandidateScore,
) (*AccountSelectionResult, bool, error) {
	budget := newOpenAISelectionProbeBudget()
	budget.enableLimit()
	return s.tryAcquireOpenAISelectionOrderWithBudget(ctx, req, selectionOrder, budget)
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAISelectionOrderWithBudget(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	selectionOrder []openAIAccountCandidateScore,
	budget *openAISelectionProbeBudget,
) (*AccountSelectionResult, bool, error) {
	compactBlocked := false
	release := func(result *AcquireResult) {
		if result != nil && result.ReleaseFunc != nil {
			result.ReleaseFunc()
		}
	}
	for i := 0; i < len(selectionOrder); i++ {
		candidate := selectionOrder[i]
		if candidate.account == nil {
			continue
		}
		probe := candidate.healthProbe && req.SmartHealthEnabled
		probeID := uint64(0)
		if probe {
			probeID = s.stats.probeSequence.Add(1)
			if !s.stats.tryAcquireProbeLeaseWithID(candidate.account.ID, time.Now(), probeID) {
				continue
			}
		}
		cancelProbe := func() {
			if probe {
				s.stats.cancelProbeLeaseForID(candidate.account.ID, probeID, time.Now())
			}
		}
		if candidate.loadKnown && candidate.account.Concurrency > 0 &&
			candidate.loadInfo.CurrentConcurrency >= candidate.account.Concurrency {
			cancelProbe()
			continue
		}

		result, attempted, acquireErr := s.tryAcquireOpenAIAccountSlot(ctx, candidate.account.ID, candidate.account.Concurrency, budget)
		if !attempted {
			cancelProbe()
			break
		}
		if acquireErr != nil {
			cancelProbe()
			if probe {
				continue
			}
			return nil, compactBlocked, acquireErr
		}
		if result == nil || !result.Acquired {
			cancelProbe()
			continue
		}

		fresh := s.service.resolveFreshSchedulableOpenAIAccount(ctx, candidate.account, req.Platform, req.RequestedModel, false, req.RequiredCapability)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			release(result)
			cancelProbe()
			continue
		}
		if !s.consumeOpenAISelectionDBRecheck(budget) {
			release(result)
			cancelProbe()
			break
		}
		fresh = s.service.recheckSelectedOpenAIAccountFromDB(ctx, fresh, req.GroupID, req.Platform, req.RequestedModel, false, req.RequiredCapability)
		if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
			release(result)
			cancelProbe()
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(fresh) == 0 {
			compactBlocked = true
			release(result)
			cancelProbe()
			continue
		}

		if fresh.Concurrency != candidate.account.Concurrency {
			release(result)
			result, attempted, acquireErr = s.tryAcquireOpenAIAccountSlot(ctx, fresh.ID, fresh.Concurrency, budget)
			if !attempted {
				cancelProbe()
				continue
			}
			if acquireErr != nil {
				cancelProbe()
				if probe {
					continue
				}
				return nil, compactBlocked, acquireErr
			}
			if result == nil || !result.Acquired {
				cancelProbe()
				continue
			}
		}
		if req.SessionHash != "" && !req.PreserveStickyBinding && !probe {
			_ = s.service.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, fresh.ID)
		}
		if probe {
			freshCopy := *fresh
			freshCopy.smartHealthProbeID = probeID
			fresh = &freshCopy
			s.stats.markProbeSlotAcquired(fresh.ID, probeID, time.Now(), req.GroupID)
			s.metrics.recordSmartHealthProbe()
		}
		releaseFunc := result.ReleaseFunc
		if probe {
			baseRelease := releaseFunc
			var releaseOnce sync.Once
			releaseFunc = func() {
				releaseOnce.Do(func() {
					if baseRelease != nil {
						baseRelease()
					}
					s.stats.cancelProbeLeaseForID(fresh.ID, probeID, time.Now())
				})
			}
		}
		return attachSelectionProfitGate(ctx, &AccountSelectionResult{
			Account:     fresh,
			Acquired:    true,
			ReleaseFunc: releaseFunc,
		}), compactBlocked, nil
	}
	return nil, compactBlocked, nil
}

func (s *defaultOpenAIAccountScheduler) tryAcquireOpenAIAccountSlot(
	ctx context.Context,
	accountID int64,
	maxConcurrency int,
	budget *openAISelectionProbeBudget,
) (*AcquireResult, bool, error) {
	if s.service.concurrencyService != nil && maxConcurrency > 0 && !budget.recordAcquire(accountID) {
		return nil, false, nil
	}
	result, err := s.service.tryAcquireAccountSlot(ctx, accountID, maxConcurrency)
	return result, true, err
}

func (s *defaultOpenAIAccountScheduler) consumeOpenAISelectionDBRecheck(budget *openAISelectionProbeBudget) bool {
	if s.service.schedulerSnapshot == nil || s.service.accountRepo == nil {
		return true
	}
	return budget.recordRecheck()
}

func (s *defaultOpenAIAccountScheduler) tryFallbackToWeightedSticky(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
) (*AccountSelectionResult, error) {
	if !req.StickyWeighted {
		return nil, nil
	}
	for _, accountID := range []int64{req.StickyPreviousAccountID, req.StickyAccountID} {
		if accountID <= 0 {
			continue
		}
		if req.ExcludedIDs != nil {
			if _, excluded := req.ExcludedIDs[accountID]; excluded {
				continue
			}
		}
		account, err := s.service.getSchedulableAccount(ctx, accountID)
		if err != nil || account == nil {
			continue
		}
		if !s.isAccountRequestCompatible(ctx, account, req) || !s.isAccountTransportCompatible(account, req.RequiredTransport) {
			continue
		}
		account = s.service.recheckSelectedOpenAIAccountFromDB(ctx, account, req.GroupID, req.Platform, req.RequestedModel, req.RequireCompact, req.RequiredCapability)
		if account == nil {
			if accountID == req.StickyAccountID && strings.TrimSpace(req.SessionHash) != "" {
				_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, req.SessionHash)
			}
			continue
		}
		if !s.service.openAIAccountMatchesSchedulingGroup(account, req.GroupID) {
			if accountID == req.StickyAccountID && strings.TrimSpace(req.SessionHash) != "" {
				_ = s.service.deleteStickySessionAccountID(ctx, req.GroupID, req.SessionHash)
			}
			continue
		}
		if !s.isAccountRequestCompatible(ctx, account, req) || !s.isAccountTransportCompatible(account, req.RequiredTransport) {
			continue
		}
		if req.RequireCompact && openAICompactSupportTier(account) == 0 {
			continue
		}
		if req.SmartHealthEnabled && s.stats != nil {
			assessment := s.stats.health(account.ID, time.Now(), s.service.openAIStickyEscapeConfig())
			if assessment.level == openAIAccountHealthDegraded {
				s.metrics.recordSmartHealthDegradedEscape()
				slog.Info("openai_account_health_weighted_sticky_escape",
					"account_id", account.ID,
					"reason", openAIAccountHealthReasonName(assessment.reason),
					"error_rate", assessment.errorRate,
					"ttft_ms", assessment.ttft,
					"sample_count", assessment.samples,
				)
				continue
			}
		}
		// Keep weighted sticky fallback subject to the same free-tier gate as the
		// normal and sticky selection paths. Otherwise an over-quota free account
		// could be reintroduced after the primary candidate pass.
		if len(s.filterGrokFreeQuotaAccounts(ctx, []Account{*account})) == 0 {
			continue
		}
		upstreamModel := canonicalOpenAIAccountSchedulingModel(account, req.RequestedModel)
		now := time.Now()
		if isGrokTeamModelRateLimited(account, upstreamModel, now) ||
			isGrokModelQuotaBlocked(account.ID, upstreamModel, now) {
			continue
		}
		result, acquireErr := s.service.tryAcquireAccountSlot(ctx, account.ID, account.Concurrency)
		if acquireErr != nil {
			return nil, acquireErr
		}
		if result != nil && result.Acquired {
			if req.SessionHash != "" && !req.PreserveStickyBinding {
				_ = s.service.bindOpenAIStickySessionDuringSelection(ctx, req.GroupID, req.SessionHash, account.ID)
			}
			return attachSelectionProfitGate(ctx, &AccountSelectionResult{
				Account:     account,
				Acquired:    true,
				ReleaseFunc: result.ReleaseFunc,
			}), nil
		}
		if s.service.concurrencyService != nil {
			cfg := s.service.schedulingConfig()
			return attachSelectionProfitGate(ctx, &AccountSelectionResult{
				Account: account,
				WaitPlan: &AccountWaitPlan{
					AccountID:      account.ID,
					MaxConcurrency: account.Concurrency,
					Timeout:        cfg.StickySessionWaitTimeout,
					MaxWaiting:     cfg.StickySessionMaxWaiting,
				},
			}), nil
		}
	}
	return nil, nil
}

// openAISelectionFilterStats counts why candidates were dropped by the
// selectByLoadBalance initial filter. Historically these exclusions were
// silent (debug logs at best), so a "no available accounts" failure with
// excluded_account_count=0 was undiagnosable from the error alone (#4599).
// The reasons map is lazily allocated: on the happy path (nothing filtered
// out, or an account is eventually selected) no extra allocation happens.
type openAISelectionFilterStats struct {
	pool    int
	reasons map[string]int
}

func (s *openAISelectionFilterStats) exclude(reason string) {
	if s.reasons == nil {
		s.reasons = make(map[string]int, 4)
	}
	s.reasons[reason]++
}

// summary renders deterministic exclusion statistics for scheduling error
// messages, e.g. "pool=3, filtered: model_not_supported=2 quota_auto_pause_7d=1".
// Reasons are sorted lexicographically so the output is stable for tests and
// log aggregation. extra, when non-empty, is appended as a trailing marker.
func (s openAISelectionFilterStats) summary(extra string) string {
	var b strings.Builder
	_, _ = b.WriteString("pool=")
	_, _ = b.WriteString(strconv.Itoa(s.pool))
	if len(s.reasons) > 0 {
		reasons := make([]string, 0, len(s.reasons))
		for reason := range s.reasons {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		_, _ = b.WriteString(", filtered:")
		for _, reason := range reasons {
			_, _ = b.WriteString(" ")
			_, _ = b.WriteString(reason)
			_, _ = b.WriteString("=")
			_, _ = b.WriteString(strconv.Itoa(s.reasons[reason]))
		}
	}
	if extra != "" {
		_, _ = b.WriteString(", ")
		_, _ = b.WriteString(extra)
	}
	return b.String()
}

func (s *defaultOpenAIAccountScheduler) selectByLoadBalance(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	decisionState ...*OpenAIAccountScheduleDecision,
) (*AccountSelectionResult, int, int, float64, error) {
	var decision *OpenAIAccountScheduleDecision
	if len(decisionState) > 0 {
		decision = decisionState[0]
	}
	budget := newOpenAISelectionProbeBudget()
	accounts, err := s.service.listSchedulableAccounts(ctx, req.GroupID, req.Platform)
	if err != nil {
		if decision != nil {
			decision.FilterReasons = "account_list_error"
		}
		return nil, 0, 0, 0, err
	}
	if len(accounts) == 0 {
		if decision != nil {
			decision.FilterReasons = "pool=0, reason=no_schedulable_accounts"
		}
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary(""))
	}
	// Local free-tier soft gate on the Grok scheduling path only (not admin probe).
	accounts = s.filterGrokFreeQuotaAccounts(ctx, accounts)
	if len(accounts) == 0 {
		if decision != nil {
			decision.FilterReasons = "pool=0, reason=grok_free_quota_soft_gate"
		}
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary("grok_free_quota_soft_gate"))
	}
	// Team+model rate-limit cool: siblings of a 429'd team skip the hot model.
	if req.Platform == PlatformGrok {
		now := time.Now()
		filtered := filterGrokTeamModelRateLimitedAccounts(accounts, req.RequestedModel, now)
		if len(filtered) == 0 && len(accounts) > 0 {
			if decision != nil {
				decision.FilterReasons = "pool=" + strconv.Itoa(len(accounts)) + ", reason=grok_team_model_rate_limit"
			}
			return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary("grok_team_model_rate_limit"))
		}
		if filtered != nil {
			accounts = filtered
		}
		// Per-account model free-usage soft-block (other models stay eligible).
		modelFiltered := filterGrokModelQuotaBlockedAccounts(accounts, req.RequestedModel, now)
		if len(modelFiltered) == 0 && len(accounts) > 0 {
			if decision != nil {
				decision.FilterReasons = "pool=" + strconv.Itoa(len(accounts)) + ", reason=grok_model_quota_block"
			}
			return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, openAISelectionFilterStats{}.summary("grok_model_quota_block"))
		}
		accounts = modelFiltered
	}

	// require_privacy_set: 获取分组配置。GetByID 会聚合账号计数，选号不能走它。
	var schedGroup *Group
	if req.GroupID != nil && s.service.schedulerSnapshot != nil {
		schedGroup, _ = s.service.schedulerSnapshot.GetGroupByIDLite(ctx, *req.GroupID)
	}

	filterStats := openAISelectionFilterStats{pool: len(accounts)}
	filtered := make([]*Account, 0, len(accounts))
	loadReq := make([]AccountWithConcurrency, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if req.ExcludedIDs != nil {
			if _, excluded := req.ExcludedIDs[account.ID]; excluded {
				filterStats.exclude("excluded")
				continue
			}
		}
		if !account.IsSchedulable() {
			filterStats.exclude("not_schedulable")
			continue
		}
		if account.Platform != NormalizeOpenAICompatiblePlatform(req.Platform) || !account.IsOpenAICompatible() {
			filterStats.exclude("platform_mismatch")
			continue
		}
		if s.service.isOpenAIAccountRequestRuntimeBlocked(account, req.RequestedModel) {
			filterStats.exclude("runtime_blocked")
			continue
		}
		// require_privacy_set is a group-scoped eligibility gate. Do not mutate the
		// shared account: another group may intentionally allow accounts whose
		// upstream privacy setting has not been confirmed.
		if schedGroup != nil && schedGroup.RequirePrivacySet && !account.IsPrivacySet() {
			filterStats.exclude("privacy_not_set")
			continue
		}
		if compatible, reason := s.isAccountRequestCompatibleReason(ctx, account, req); !compatible {
			filterStats.exclude(reason)
			continue
		}
		if !s.isAccountTransportCompatible(account, req.RequiredTransport) {
			filterStats.exclude("transport_incompatible")
			continue
		}
		filtered = append(filtered, account)
		loadReq = append(loadReq, AccountWithConcurrency{
			ID:             account.ID,
			MaxConcurrency: account.EffectiveLoadFactor(),
		})
	}
	if decision != nil {
		decision.FilterReasons = filterStats.summary("")
	}
	if len(filtered) == 0 {
		return nil, 0, 0, 0, noAvailableOpenAISelectionError(req.RequestedModel, false, filterStats.summary(""))
	}

	loadMap := map[int64]*AccountLoadInfo{}
	if s.service.concurrencyService != nil {
		if batchLoad, loadErr := s.service.concurrencyService.GetAccountsLoadBatch(ctx, loadReq); loadErr == nil {
			loadMap = batchLoad
		}
	}

	if req.SmartHealthEnabled {
		primary, degraded := s.splitSmartHealthCandidates(req, filtered)
		if decision != nil {
			decision.PrimaryCount = len(primary)
			decision.DegradedCount = len(degraded)
		}
		hasPrimary := len(primary) > 0
		allDegraded := !hasPrimary && len(degraded) > 0
		if hasPrimary {
			filtered = primary
		} else if allDegraded {
			filtered = degraded
			s.metrics.recordSmartHealthAllDegraded()
		}
		probe := s.selectSmartHealthProbeCandidate(req, degraded, loadMap)
		if probe != nil {
			if hasPrimary {
				filtered = append(filtered, probe)
			}
			req.SmartHealthProbeAccountID = probe.ID
		}
		if allDegraded {
			probeOpportunity := "unavailable"
			if probe != nil {
				probeOpportunity = "eligible"
			}
			slog.Info("openai_account_health_all_degraded",
				"candidate_count", len(degraded),
				"probe_opportunity", probeOpportunity,
			)
		}
	}

	if req.SubscriptionPriority && !req.SmartHealthEnabled {
		subscriptionAccounts, regularAccounts := partitionOpenAIChatGPTSubscriptionAccounts(filtered)
		if req.SmartHealthEnabled && req.SmartHealthProbeAccountID > 0 {
			probeSubscription := false
			keptSubscriptions := subscriptionAccounts[:0]
			for _, account := range subscriptionAccounts {
				if account != nil && account.ID == req.SmartHealthProbeAccountID {
					probeSubscription = true
					regularAccounts = append(regularAccounts, account)
					continue
				}
				keptSubscriptions = append(keptSubscriptions, account)
			}
			if probeSubscription {
				subscriptionAccounts = keptSubscriptions
			}
		}
		if len(subscriptionAccounts) > 0 {
			attempt := s.trySelectByLoadBalancePool(ctx, req, subscriptionAccounts, loadMap, budget)
			if attempt.err != nil && (!attempt.noCompactCandidates || len(regularAccounts) <= 0) {
				return nil, attempt.candidateCount, attempt.topK, attempt.loadSkew, attempt.err
			}
			if attempt.result != nil {
				return attempt.result, attempt.candidateCount, attempt.topK, attempt.loadSkew, nil
			}
			if len(regularAccounts) > 0 {
				regularAttempt := s.trySelectByLoadBalancePool(ctx, req, regularAccounts, loadMap, budget)
				if regularAttempt.err != nil && !regularAttempt.noCompactCandidates {
					return nil, regularAttempt.candidateCount, regularAttempt.topK, regularAttempt.loadSkew, regularAttempt.err
				}
				if regularAttempt.result != nil {
					return regularAttempt.result, regularAttempt.candidateCount, regularAttempt.topK, regularAttempt.loadSkew, nil
				}
				var result *AccountSelectionResult
				candidateCount, topK, loadSkew := regularAttempt.candidateCount, regularAttempt.topK, regularAttempt.loadSkew
				fallbackErr := regularAttempt.err
				if regularAttempt.err == nil {
					result, candidateCount, topK, loadSkew, fallbackErr = s.finishLoadBalanceSelectionFallback(ctx, req, regularAttempt, budget, filterStats)
					if fallbackErr == nil && result != nil {
						return result, candidateCount, topK, loadSkew, nil
					}
				}
				// 常规池既无法获取也无法排队（含仅剩不支持 compact 的候选）时，
				// 回退到订阅池的等待计划：busy-but-waitable 的订阅账号不应因常规池存在
				// 而被丢弃，否则开启订阅优先反而让本可排队成功的请求硬失败。
				subResult, subCandidateCount, subTopK, subLoadSkew, subErr := s.finishLoadBalanceSelectionFallback(ctx, req, attempt, budget, filterStats)
				if subErr == nil && subResult != nil {
					return subResult, subCandidateCount, subTopK, subLoadSkew, nil
				}
				return result, candidateCount, topK, loadSkew, fallbackErr
			}
			return s.finishLoadBalanceSelectionFallback(ctx, req, attempt, budget, filterStats)
		}
	}

	attempt := s.trySelectByLoadBalancePool(ctx, req, filtered, loadMap, budget)
	if attempt.err != nil {
		return nil, attempt.candidateCount, attempt.topK, attempt.loadSkew, attempt.err
	}
	if attempt.result != nil {
		return attempt.result, attempt.candidateCount, attempt.topK, attempt.loadSkew, nil
	}
	return s.finishLoadBalanceSelectionFallback(ctx, req, attempt, budget, filterStats)
}

func (s *defaultOpenAIAccountScheduler) splitSmartHealthCandidates(
	req OpenAIAccountScheduleRequest,
	accounts []*Account,
) (primary []*Account, degraded []*Account) {
	if !req.SmartHealthEnabled || s == nil || s.stats == nil {
		return accounts, nil
	}
	thresholds := s.service.openAIStickyEscapeConfig()
	for _, account := range accounts {
		if account == nil {
			continue
		}
		assessment := s.stats.health(account.ID, time.Now(), thresholds)
		if assessment.level == openAIAccountHealthDegraded {
			degraded = append(degraded, account)
			continue
		}
		primary = append(primary, account)
	}
	return primary, degraded
}

func (s *defaultOpenAIAccountScheduler) selectSmartHealthProbeCandidate(
	req OpenAIAccountScheduleRequest,
	degraded []*Account,
	loadMap map[int64]*AccountLoadInfo,
) *Account {
	if !req.SmartHealthEnabled || s == nil || s.stats == nil || len(degraded) == 0 {
		return nil
	}
	if req.GuardianParentAccountID > 0 || (!req.StickyWeighted && req.StickyAccountID > 0 && !req.SmartHealthStickyEscaped) {
		return nil
	}
	now := time.Now()
	type opportunity struct {
		account    *Account
		continuing bool
		nextAt     int64
	}
	opportunities := make([]opportunity, 0, len(degraded))
	for _, account := range degraded {
		if account == nil || !s.stats.probeEligible(account.ID, now) {
			continue
		}
		if load, known := loadMap[account.ID]; known && load != nil && account.Concurrency > 0 && load.CurrentConcurrency >= account.Concurrency {
			continue
		}
		continuing, nextAt := s.stats.probeOpportunityOrder(account.ID, now)
		opportunities = append(opportunities, opportunity{account: account, continuing: continuing, nextAt: nextAt})
	}
	sort.Slice(opportunities, func(i, j int) bool {
		if opportunities[i].continuing != opportunities[j].continuing {
			return opportunities[i].continuing
		}
		if opportunities[i].nextAt != opportunities[j].nextAt {
			return opportunities[i].nextAt < opportunities[j].nextAt
		}
		return opportunities[i].account.ID < opportunities[j].account.ID
	})
	if len(opportunities) == 0 {
		return nil
	}
	return opportunities[0].account
}

func partitionOpenAIChatGPTSubscriptionAccounts(accounts []*Account) ([]*Account, []*Account) {
	subscriptionAccounts := make([]*Account, 0, len(accounts))
	regularAccounts := make([]*Account, 0, len(accounts))
	for _, account := range accounts {
		if account != nil && account.IsOpenAIChatGPTSubscription() {
			subscriptionAccounts = append(subscriptionAccounts, account)
			continue
		}
		regularAccounts = append(regularAccounts, account)
	}
	return subscriptionAccounts, regularAccounts
}

func (s *defaultOpenAIAccountScheduler) trySelectByLoadBalancePool(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	filtered []*Account,
	loadMap map[int64]*AccountLoadInfo,
	budget *openAISelectionProbeBudget,
) openAIAccountLoadSelectionAttempt {
	plan := s.buildOpenAIAccountLoadPlan(ctx, req, filtered, loadMap)
	if openAICostOverflowExpanded(req, plan) {
		budget.enableLimit()
	}
	attempt := openAIAccountLoadSelectionAttempt{
		selectionOrder: plan.selectionOrder,
		candidateCount: plan.candidateCount,
		topK:           plan.topK,
		loadSkew:       plan.loadSkew,
	}
	if req.RequireCompact && len(plan.candidates) == 0 && len(plan.staleSnapshotCompactRetry) == 0 {
		attempt.noCompactCandidates = true
		attempt.err = ErrNoAvailableCompactAccounts
		return attempt
	}
	if req.RequireCompact && len(attempt.selectionOrder) == 0 && s.service.schedulerSnapshot == nil {
		attempt.noCompactCandidates = true
		attempt.err = ErrNoAvailableCompactAccounts
		return attempt
	}
	if len(attempt.selectionOrder) == 0 {
		attempt.compactBlocked = req.RequireCompact && len(plan.allCandidates) > 0
		return attempt
	}

	result, compactBlocked, acquireErr := s.tryAcquireOpenAISelectionOrderWithBudget(ctx, req, attempt.selectionOrder, budget)
	attempt.compactBlocked = compactBlocked
	if acquireErr != nil {
		attempt.err = acquireErr
		return attempt
	}
	if result != nil {
		attempt.result = result
		return attempt
	}

	if s.service.concurrencyService != nil && !budget.acquireExhausted() {
		loadReq := buildOpenAIAccountLoadRequest(filtered)
		if freshLoadMap, loadErr := s.service.concurrencyService.GetAccountsLoadBatchFresh(ctx, loadReq); loadErr == nil {
			freshPlan := s.buildOpenAIAccountLoadPlan(ctx, req, filtered, freshLoadMap)
			if openAICostOverflowExpanded(req, freshPlan) {
				budget.enableLimit()
			}
			if len(freshPlan.selectionOrder) > 0 {
				freshResult, freshCompactBlocked, freshAcquireErr := s.tryAcquireOpenAISelectionOrderWithBudget(ctx, req, freshPlan.selectionOrder, budget)
				if freshAcquireErr != nil {
					attempt.err = freshAcquireErr
					return attempt
				}
				if freshResult != nil {
					attempt.result = freshResult
					attempt.selectionOrder = freshPlan.selectionOrder
					attempt.candidateCount = freshPlan.candidateCount
					attempt.topK = freshPlan.topK
					attempt.loadSkew = freshPlan.loadSkew
					return attempt
				}
				attempt.compactBlocked = attempt.compactBlocked || freshCompactBlocked
				attempt.selectionOrder = freshPlan.selectionOrder
				attempt.candidateCount = freshPlan.candidateCount
				attempt.topK = freshPlan.topK
				attempt.loadSkew = freshPlan.loadSkew
			}
		}
	}

	return attempt
}

func openAICostOverflowExpanded(req OpenAIAccountScheduleRequest, plan openAIAccountLoadPlan) bool {
	if !plan.includeOverflowFallback || plan.topK <= 0 {
		return false
	}
	if !req.RequireCompact {
		return len(plan.candidates) > plan.topK
	}
	supported, unknown := 0, 0
	for _, candidate := range plan.candidates {
		switch openAICompactSupportTier(candidate.account) {
		case 2:
			supported++
		case 1:
			unknown++
		}
	}
	return supported > plan.topK || unknown > plan.topK
}

func buildOpenAIAccountLoadRequest(accounts []*Account) []AccountWithConcurrency {
	loadReq := make([]AccountWithConcurrency, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		loadReq = append(loadReq, AccountWithConcurrency{
			ID:             account.ID,
			MaxConcurrency: account.EffectiveLoadFactor(),
		})
	}
	return loadReq
}

func (s *defaultOpenAIAccountScheduler) finishLoadBalanceSelectionFallback(
	ctx context.Context,
	req OpenAIAccountScheduleRequest,
	attempt openAIAccountLoadSelectionAttempt,
	budget *openAISelectionProbeBudget,
	filterStats openAISelectionFilterStats,
) (*AccountSelectionResult, int, int, float64, error) {
	candidateCount := attempt.candidateCount
	topK := attempt.topK
	loadSkew := attempt.loadSkew

	if len(attempt.selectionOrder) == 0 {
		return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, attempt.compactBlocked, filterStats.summary("selection_order_empty"))
	}

	if stickyFallback, stickyErr := s.tryFallbackToWeightedSticky(ctx, req); stickyErr != nil {
		return nil, candidateCount, topK, loadSkew, stickyErr
	} else if stickyFallback != nil {
		return stickyFallback, candidateCount, topK, loadSkew, nil
	}

	cfg := s.service.schedulingConfig()
	compactBlocked := attempt.compactBlocked
	// WaitPlan.MaxConcurrency 使用 Concurrency（非 EffectiveLoadFactor），因为 WaitPlan 控制的是 Redis 实际并发槽位等待。
	passes := 1
	if budget != nil && budget.limited {
		passes = 4
	}
	for pass := 0; pass < passes; pass++ {
		wantAttempted := pass == 1 || pass == 3
		wantKnownFull := pass >= 2
		for _, candidate := range attempt.selectionOrder {
			if candidate.account == nil {
				continue
			}
			if budget != nil && budget.limited {
				knownFull := candidate.loadKnown && candidate.account.Concurrency > 0 &&
					candidate.loadInfo.CurrentConcurrency >= candidate.account.Concurrency
				if budget.wasAttempted(candidate.account.ID) != wantAttempted || knownFull != wantKnownFull {
					continue
				}
			}
			fresh := s.service.resolveFreshSchedulableOpenAIAccount(ctx, candidate.account, req.Platform, req.RequestedModel, false, req.RequiredCapability)
			if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
				continue
			}
			if !s.consumeOpenAISelectionDBRecheck(budget) {
				return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, compactBlocked, filterStats.summary("selection_order_exhausted"))
			}
			fresh = s.service.recheckSelectedOpenAIAccountFromDB(ctx, fresh, req.GroupID, req.Platform, req.RequestedModel, false, req.RequiredCapability)
			if fresh == nil || !s.isAccountTransportCompatible(fresh, req.RequiredTransport) || !s.isAccountRequestCompatible(ctx, fresh, req) {
				continue
			}
			if req.RequireCompact && openAICompactSupportTier(fresh) == 0 {
				compactBlocked = true
				continue
			}
			return attachSelectionProfitGate(ctx, &AccountSelectionResult{
				Account: fresh,
				WaitPlan: &AccountWaitPlan{
					AccountID:      fresh.ID,
					MaxConcurrency: fresh.Concurrency,
					Timeout:        cfg.FallbackWaitTimeout,
					MaxWaiting:     cfg.FallbackMaxWaiting,
				},
			}), candidateCount, topK, loadSkew, nil
		}
	}

	return nil, candidateCount, topK, loadSkew, noAvailableOpenAISelectionError(req.RequestedModel, compactBlocked, filterStats.summary("selection_order_exhausted"))
}

func (s *defaultOpenAIAccountScheduler) isAccountTransportCompatible(account *Account, requiredTransport OpenAIUpstreamTransport) bool {
	if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
		return true
	}
	if s == nil || s.service == nil {
		return false
	}
	return s.service.isOpenAIAccountTransportCompatible(account, requiredTransport)
}

func (s *defaultOpenAIAccountScheduler) lookupShadowParentAccount(ctx context.Context, id int64) *Account {
	if s == nil || s.service == nil {
		return nil
	}
	if s.service.schedulerSnapshot != nil {
		if account, err := s.service.schedulerSnapshot.GetAccount(ctx, id); err == nil && account != nil {
			return account
		}
	}
	if s.service.accountRepo == nil {
		return nil
	}
	account, _ := s.service.accountRepo.GetByID(ctx, id)
	return account
}

func (s *defaultOpenAIAccountScheduler) isAccountRequestCompatible(ctx context.Context, account *Account, req OpenAIAccountScheduleRequest) bool {
	compatible, _ := s.isAccountRequestCompatibleReason(ctx, account, req)
	return compatible
}

// isAccountRequestCompatibleReason reports whether the account can serve the
// request, and when it cannot, names the veto point. The reason feeds
// openAISelectionFilterStats so that "no available accounts" errors state why
// each candidate was dropped instead of failing silently (#4599).
func (s *defaultOpenAIAccountScheduler) isAccountRequestCompatibleReason(ctx context.Context, account *Account, req OpenAIAccountScheduleRequest) (bool, string) {
	if account == nil {
		return false, "account_nil"
	}
	if req.RequirePrivacySet && !account.IsPrivacySet() {
		return false, "privacy_not_set"
	}
	if s != nil && s.service != nil && s.service.isOpenAIAccountRequestRuntimeBlocked(account, req.RequestedModel) {
		return false, "runtime_blocked"
	}
	if s != nil && s.service != nil && s.service.isOpenAIProxyStreamQuarantined(ctx, account) {
		return false, "proxy_stream_quarantined"
	}
	// Quota auto-pause must be evaluated during the initial filter too. Without it the
	// TopK candidate pool can be filled with paused accounts and the later fresh/DB
	// rechecks won't reach healthy accounts that fell outside TopK — manifesting as
	// "no available accounts" even though healthy ones exist.
	if paused, decision := shouldAutoPauseOpenAIAccountByQuota(ctx, account); paused {
		if decision.reason != "" {
			return false, decision.reason
		}
		reason := "quota_auto_pause"
		if decision.window != "" {
			reason += "_" + decision.window
		}
		return false, reason
	}
	// 母账号健康联动：影子账号的凭据来自母账号，母账号不可调度时影子也不应被选中。
	// Parent-health gate: shadow borrows the parent's credentials; an unschedulable
	// parent must block the shadow across all scheduler paths.
	if !parentHealthyForShadow(account, func(id int64) *Account {
		return s.lookupShadowParentAccount(ctx, id)
	}) {
		return false, "shadow_parent_unhealthy"
	}
	if req.RequestedModel != "" && !account.IsModelSupported(req.RequestedModel) {
		return false, "model_not_supported"
	}
	if req.GroupID != nil && s != nil && s.service != nil &&
		s.service.needsUpstreamChannelRestrictionCheck(ctx, req.GroupID) &&
		s.service.isUpstreamModelRestrictedByChannel(ctx, *req.GroupID, account, req.RequestedModel, req.RequireCompact) {
		return false, "channel_upstream_restricted"
	}
	if !accountSupportsOpenAICapabilities(account, req.RequiredCapability, req.RequiredImageCapability) {
		return false, "capability_mismatch"
	}
	// 分组利润控制：不合格账号在候选过滤与抢槽后终检阶段即被排除，
	// 排序/评分/粘性/熔断只在合格账号之间工作；named reason 进入 filter stats。
	if vetoed, reason := openAIProfitControlVetoReason(ctx, account); vetoed {
		return false, reason
	}
	return true, ""
}

func (s *defaultOpenAIAccountScheduler) ReportResult(accountID int64, success bool, firstTokenMs *int) {
	s.reportAccountResult(&Account{ID: accountID}, success, firstTokenMs, success || !success)
}

func (s *defaultOpenAIAccountScheduler) reportAccountResult(account *Account, success bool, firstTokenMs *int, attributable bool) {
	if s == nil || s.stats == nil || account == nil || account.ID <= 0 {
		return
	}
	smartEnabled := s.service != nil && s.service.isSmartSchedulingEnabled(context.Background())
	if !smartEnabled && account.smartHealthProbeID != 0 {
		s.stats.cancelProbeLeaseForID(account.ID, account.smartHealthProbeID, time.Now())
		return
	}
	thresholds := defaultOpenAIAccountHealthThresholds()
	if s.service != nil {
		thresholds = s.service.openAIStickyEscapeConfig()
	}
	s.stats.reportAtForRequest(account.ID, success, firstTokenMs, time.Now(), thresholds, smartEnabled, smartEnabled, account.smartHealthProbeID, attributable)
}

func (s *defaultOpenAIAccountScheduler) ReportSwitch() {
	if s == nil {
		return
	}
	s.metrics.recordSwitch()
}

func (s *defaultOpenAIAccountScheduler) SnapshotMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	if s == nil {
		return OpenAIAccountSchedulerMetricsSnapshot{}
	}

	selectTotal := s.metrics.selectTotal.Load()
	prevHit := s.metrics.stickyPreviousHitTotal.Load()
	sessionHit := s.metrics.stickySessionHitTotal.Load()
	stickyHit := s.metrics.stickyHitTotal.Load()
	switchTotal := s.metrics.accountSwitchTotal.Load()
	latencyTotal := s.metrics.latencyMsTotal.Load()
	loadSkewTotal := s.metrics.loadSkewMilliTotal.Load()

	snapshot := OpenAIAccountSchedulerMetricsSnapshot{
		SelectTotal:                    selectTotal,
		StickyPreviousHitTotal:         prevHit,
		StickySessionHitTotal:          sessionHit,
		LoadBalanceSelectTotal:         s.metrics.loadBalanceSelectTotal.Load(),
		AccountSwitchTotal:             switchTotal,
		SchedulerLatencyMsTotal:        latencyTotal,
		RuntimeStatsAccountCount:       s.stats.size(),
		SmartHealthDegradedEscapeTotal: s.metrics.smartHealthDegradedEscapeTotal.Load(),
		SmartHealthProbeTotal:          s.metrics.smartHealthProbeTotal.Load(),
		SmartHealthAllDegradedTotal:    s.metrics.smartHealthAllDegradedTotal.Load(),
	}
	if selectTotal > 0 {
		snapshot.SchedulerLatencyMsAvg = float64(latencyTotal) / float64(selectTotal)
		snapshot.StickyHitRatio = float64(stickyHit) / float64(selectTotal)
		snapshot.AccountSwitchRate = float64(switchTotal) / float64(selectTotal)
		snapshot.LoadSkewAvg = float64(loadSkewTotal) / 1000 / float64(selectTotal)
	}
	return snapshot
}

func (s *OpenAIGatewayService) openAIAdvancedSchedulerSettingRepo() SettingRepository {
	if s == nil || s.rateLimitService == nil || s.rateLimitService.settingService == nil {
		return nil
	}
	return s.rateLimitService.settingService.settingRepo
}

func (s *OpenAIGatewayService) openAIAdvancedSchedulerRuntimeSettings(ctx context.Context) openAIAdvancedSchedulerRuntimeSettings {
	return s.openAIAdvancedSchedulerRuntimeSettingsWithFlightRegistered(ctx, nil)
}

// openAIAdvancedSchedulerRuntimeSettingsWithFlightRegistered lets tests
// synchronize after a caller has registered with singleflight. Production
// callers pass no callback and follow the same cache/read path.
func (s *OpenAIGatewayService) openAIAdvancedSchedulerRuntimeSettingsWithFlightRegistered(
	ctx context.Context,
	flightRegistered func(),
) openAIAdvancedSchedulerRuntimeSettings {
	for {
		if cached, ok := loadCurrentOpenAIAdvancedSchedulerSettingsCache(); ok {
			return cachedOpenAIAdvancedSchedulerRuntimeSettings(cached)
		}

		resultCh := openAIAdvancedSchedulerSettingSF.DoChan(openAIAdvancedSchedulerSettingKey, func() (any, error) {
			if cached, ok := loadCurrentOpenAIAdvancedSchedulerSettingsCache(); ok {
				return openAIAdvancedSchedulerSettingsReadResult{
					generation: cached.generation,
					settings:   cachedOpenAIAdvancedSchedulerRuntimeSettings(cached),
				}, nil
			}

			generation := openAIAdvancedSchedulerSettingGeneration.Load()
			lowUpstreamRatePriorityEnabled := false
			smartEnabled := false
			firstOutputTimeoutSeconds := 30
			oauthSchedulingRateMultiplier := parseOpenAIOAuthSchedulingRateMultiplier(nil)
			enabled := false
			stickyWeightedEnabled := false
			subscriptionPriorityEnabled := false
			lbTopKOverride := 0
			weightOverrides := map[string]float64{}
			smartEnabledKnown := false
			if repo := s.openAIAdvancedSchedulerSettingRepo(); repo != nil {
				dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAIAdvancedSchedulerSettingDBTimeout)
				defer cancel()

				if values, err := repo.GetMultiple(dbCtx, openAIAdvancedSchedulerRuntimeSettingKeys()); err == nil {
					smartEnabledKnown = true
					lowUpstreamRatePriorityEnabled = strings.EqualFold(strings.TrimSpace(values[SettingKeyOpenAILowUpstreamRatePriorityEnabled]), "true")
					smartEnabled = strings.EqualFold(strings.TrimSpace(values[SettingKeySmartSchedulingEnabled]), "true")
					firstOutputTimeoutSeconds = parseOpenAIFirstOutputTimeoutSeconds(values[SettingKeyOpenAIFirstOutputTimeoutSeconds], settingValuePresent(values, SettingKeyOpenAIFirstOutputTimeoutSeconds))
					oauthSchedulingRateMultiplier = parseOpenAIOAuthSchedulingRateMultiplier(values)
					enabled = strings.EqualFold(strings.TrimSpace(values[openAIAdvancedSchedulerSettingKey]), "true")
					stickyWeightedEnabled = strings.EqualFold(strings.TrimSpace(values[SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled]), "true")
					subscriptionPriorityEnabled = strings.EqualFold(strings.TrimSpace(values[SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled]), "true")
					lbTopKOverride = parsePositiveIntOverride(values[SettingKeyOpenAIAdvancedSchedulerLBTopK])
					weightOverrides = parseOpenAIAdvancedSchedulerWeightOverrides(values)
				} else {
					// 批量读取失败时逐键降级，覆盖全部键（含 TopK/权重），避免只加载布尔开关
					// 而静默丢弃管理员配置的覆盖值；降级状态会被缓存一个 TTL，必须留痕。
					slog.Warn("openai_advanced_scheduler_settings_batch_load_failed", "error", err)
					fallbackValues := make(map[string]string)
					for _, key := range openAIAdvancedSchedulerRuntimeSettingKeys() {
						if value, valueErr := repo.GetValue(dbCtx, key); valueErr == nil {
							fallbackValues[key] = value
							if key == SettingKeySmartSchedulingEnabled {
								smartEnabledKnown = true
							}
						}
					}
					lowUpstreamRatePriorityEnabled = strings.EqualFold(strings.TrimSpace(fallbackValues[SettingKeyOpenAILowUpstreamRatePriorityEnabled]), "true")
					smartEnabled = strings.EqualFold(strings.TrimSpace(fallbackValues[SettingKeySmartSchedulingEnabled]), "true")
					firstOutputTimeoutSeconds = parseOpenAIFirstOutputTimeoutSeconds(fallbackValues[SettingKeyOpenAIFirstOutputTimeoutSeconds], settingValuePresent(fallbackValues, SettingKeyOpenAIFirstOutputTimeoutSeconds))
					oauthSchedulingRateMultiplier = parseOpenAIOAuthSchedulingRateMultiplier(fallbackValues)
					enabled = strings.EqualFold(strings.TrimSpace(fallbackValues[openAIAdvancedSchedulerSettingKey]), "true")
					stickyWeightedEnabled = strings.EqualFold(strings.TrimSpace(fallbackValues[SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled]), "true")
					subscriptionPriorityEnabled = strings.EqualFold(strings.TrimSpace(fallbackValues[SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled]), "true")
					lbTopKOverride = parsePositiveIntOverride(fallbackValues[SettingKeyOpenAIAdvancedSchedulerLBTopK])
					weightOverrides = parseOpenAIAdvancedSchedulerWeightOverrides(fallbackValues)
				}
			}

			cached := &cachedOpenAIAdvancedSchedulerSetting{
				lowUpstreamRatePriorityEnabled: lowUpstreamRatePriorityEnabled,
				smartEnabled:                   smartEnabled,
				smartEnabledKnown:              smartEnabledKnown,
				firstOutputTimeoutSeconds:      firstOutputTimeoutSeconds,
				oauthSchedulingRateMultiplier:  oauthSchedulingRateMultiplier,
				enabled:                        enabled,
				stickyWeightedEnabled:          stickyWeightedEnabled,
				subscriptionPriorityEnabled:    subscriptionPriorityEnabled,
				lbTopKOverride:                 lbTopKOverride,
				weightOverrides:                cloneOpenAIAdvancedSchedulerWeightOverrides(weightOverrides),
				generation:                     generation,
				expiresAt:                      time.Now().Add(openAIAdvancedSchedulerSettingCacheTTL).UnixNano(),
			}

			openAIAdvancedSchedulerSettingCachePublishMu.Lock()
			currentGeneration := openAIAdvancedSchedulerSettingGeneration.Load()
			if generation == currentGeneration {
				openAIAdvancedSchedulerSettingCache.Store(cached)
				openAIAdvancedSchedulerSettingCachePublishMu.Unlock()
				return openAIAdvancedSchedulerSettingsReadResult{
					generation: generation,
					settings:   cachedOpenAIAdvancedSchedulerRuntimeSettings(cached),
				}, nil
			}
			if current, ok := loadCurrentOpenAIAdvancedSchedulerSettingsCache(); ok {
				openAIAdvancedSchedulerSettingCachePublishMu.Unlock()
				return openAIAdvancedSchedulerSettingsReadResult{
					generation: current.generation,
					settings:   cachedOpenAIAdvancedSchedulerRuntimeSettings(current),
				}, nil
			}
			openAIAdvancedSchedulerSettingCachePublishMu.Unlock()
			return openAIAdvancedSchedulerSettingsReadResult{
				generation: generation,
				settings:   cachedOpenAIAdvancedSchedulerRuntimeSettings(cached),
			}, nil
		})

		if flightRegistered != nil {
			flightRegistered()
		}
		result := <-resultCh
		readResult, ok := result.Val.(openAIAdvancedSchedulerSettingsReadResult)
		if !ok {
			return openAIAdvancedSchedulerRuntimeSettings{}
		}
		if readResult.generation != openAIAdvancedSchedulerSettingGeneration.Load() {
			continue
		}
		return readResult.settings
	}
}

func (s *OpenAIGatewayService) isOpenAIAdvancedSchedulerEnabled(ctx context.Context) bool {
	return s.openAIAdvancedSchedulerRuntimeSettings(ctx).enabled
}

func (s *OpenAIGatewayService) isSmartSchedulingEnabled(ctx context.Context) bool {
	return s.openAIAdvancedSchedulerRuntimeSettings(ctx).smartEnabled
}

func (s *OpenAIGatewayService) isOpenAILowUpstreamRatePriorityEnabled(ctx context.Context) bool {
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	return !settings.smartEnabled && !settings.enabled && settings.lowUpstreamRatePriorityEnabled
}

func (s *OpenAIGatewayService) openAIOAuthSchedulingRateMultiplier(ctx context.Context) *float64 {
	return s.openAIAdvancedSchedulerRuntimeSettings(ctx).oauthSchedulingRateMultiplier
}

func (s *OpenAIGatewayService) isOpenAIAdvancedSchedulerStickyWeightedEnabled(ctx context.Context) bool {
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	return settings.enabled && settings.stickyWeightedEnabled
}

func (s *OpenAIGatewayService) isOpenAIAdvancedSchedulerSubscriptionPriorityEnabled(ctx context.Context) bool {
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	return settings.enabled && settings.subscriptionPriorityEnabled
}

func openAIAdvancedSchedulerRuntimeSettingKeys() []string {
	keys := []string{
		SettingKeyOpenAILowUpstreamRatePriorityEnabled,
		SettingKeySmartSchedulingEnabled,
		SettingKeyOpenAIFirstOutputTimeoutSeconds,
		SettingKeyOpenAIOAuthSchedulingRateMultiplier,
		openAIAdvancedSchedulerSettingKey,
		SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled,
		SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled,
		SettingKeyOpenAIAdvancedSchedulerLBTopK,
	}
	for _, spec := range openAIAdvancedSchedulerWeightOverrideSpecs() {
		keys = append(keys, spec.key)
	}
	return keys
}

type openAIAdvancedSchedulerWeightOverrideSpec struct {
	key  string
	name string
}

func openAIAdvancedSchedulerWeightOverrideSpecs() []openAIAdvancedSchedulerWeightOverrideSpec {
	return []openAIAdvancedSchedulerWeightOverrideSpec{
		{key: SettingKeyOpenAIAdvancedSchedulerWeightPriority, name: "priority"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightLoad, name: "load"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightQueue, name: "queue"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightErrorRate, name: "error_rate"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightTTFT, name: "ttft"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightReset, name: "reset"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightQuotaHeadroom, name: "quota_headroom"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightUpstreamCost, name: "upstream_cost"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightPreviousResponse, name: "previous_response"},
		{key: SettingKeyOpenAIAdvancedSchedulerWeightSessionSticky, name: "session_sticky"},
	}
}

func parsePositiveIntOverride(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func parseOpenAIAdvancedSchedulerWeightOverrides(values map[string]string) map[string]float64 {
	overrides := map[string]float64{}
	for _, spec := range openAIAdvancedSchedulerWeightOverrideSpecs() {
		raw := strings.TrimSpace(values[spec.key])
		if raw == "" {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		overrides[spec.name] = value
	}
	return overrides
}

func cloneOpenAIAdvancedSchedulerWeightOverrides(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (s *OpenAIGatewayService) getOpenAIAccountScheduler(ctx context.Context) OpenAIAccountScheduler {
	if s == nil {
		return nil
	}
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	if !settings.smartEnabled && !settings.enabled {
		return nil
	}
	s.openaiSchedulerOnce.Do(func() {
		if s.openaiAccountStats == nil {
			s.openaiAccountStats = newOpenAIAccountRuntimeStats()
		}
		if s.openaiScheduler == nil {
			s.openaiScheduler = newDefaultOpenAIAccountScheduler(s, s.openaiAccountStats)
		}
		s.openaiAccountStatsPublished.Store(s.openaiAccountStats)
		s.openaiAccountStats.startReclamation(s.openaiAccountStatsMaintenanceCtx, s.accountRepo)
	})
	return s.openaiScheduler
}

func resetOpenAIAdvancedSchedulerSettingCacheForTest() {
	invalidateOpenAIAdvancedSchedulerSettingsAfterWrite()
}

func (s *OpenAIGatewayService) SelectAccountWithScheduler(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requireCompact bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	return s.selectAccountWithScheduler(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, "", "", requireCompact, PlatformOpenAI, false, true)
}

// SelectAccountWithSchedulerForCapability 按能力要求调度账号。
// previousResponseCanMove 表示首包 input 可自行重建工具续链，previous_response_id 允许跨账号迁移
// （粘性加权模式下改为加权偏好而非硬粘连）。
func (s *OpenAIGatewayService) SelectAccountWithSchedulerForCapability(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requireCompact bool,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
	platformOverride ...string,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	platform := PlatformOpenAI
	if len(platformOverride) > 0 {
		platform = platformOverride[0]
	}
	return s.selectAccountWithScheduler(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, "", requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
}

func (s *OpenAIGatewayService) SelectAccountWithSchedulerForImages(
	ctx context.Context,
	groupID *int64,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredCapability OpenAIImagesCapability,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	selection, decision, err := s.selectAccountWithScheduler(ctx, groupID, "", sessionHash, requestedModel, excludedIDs, OpenAIUpstreamTransportHTTPSSE, "", requiredCapability, false, PlatformOpenAI, false, false)
	if err == nil && selection != nil && selection.Account != nil {
		return selection, decision, nil
	}
	// 如果要求 native 能力（如指定了模型）但没有可用的 APIKey 账号，回退到 basic（OAuth 账号）
	if requiredCapability == OpenAIImagesCapabilityNative {
		return s.selectAccountWithScheduler(ctx, groupID, "", sessionHash, requestedModel, excludedIDs, OpenAIUpstreamTransportHTTPSSE, "", OpenAIImagesCapabilityBasic, false, PlatformOpenAI, false, false)
	}
	return selection, decision, err
}

// selectAccountWithScheduler wraps selectAccountWithSchedulerOnce with a
// fail-open second pass for the proxy stream circuit (#5056): when the only
// reason no account is available is that every candidate sits behind a
// quarantined proxy, the quarantine must degrade to a preference instead of
// zeroing out capacity. The retry re-runs the exact same selection with the
// quarantine checks bypassed, so healthy proxies always win the first pass
// and quarantined ones only serve when nothing else can.
func (s *OpenAIGatewayService) selectAccountWithScheduler(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	selection, decision, err := s.selectAccountWithSchedulerOnce(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
	if err == nil || openAIProxyStreamQuarantineBypassed(ctx) {
		return selection, decision, err
	}
	if !errors.Is(err, ErrNoAvailableAccounts) && !errors.Is(err, ErrNoAvailableCompactAccounts) {
		return selection, decision, err
	}
	// The circuit only ever quarantines PlatformOpenAI accounts.
	if NormalizeOpenAICompatiblePlatform(platform) != PlatformOpenAI {
		return selection, decision, err
	}
	blocked := s.getOpenAIProxyStreamCircuit().activeBlockCount(time.Now())
	if blocked == 0 {
		return selection, decision, err
	}
	s.logOpenAIProxyStreamQuarantineFailOpen(requestedModel, blocked)
	return s.selectAccountWithSchedulerOnce(withOpenAIProxyStreamQuarantineBypass(ctx), groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform, previousResponseCanMove, useUpstreamTokenCost)
}

type openAIGroupPrivacyRequirementContextKey struct{}

type openAIGroupPrivacyRequirement struct {
	groupID  int64
	required bool
}

func (s *OpenAIGatewayService) withOpenAIGroupPrivacyRequirement(ctx context.Context, groupID *int64) context.Context {
	return context.WithValue(ctx, openAIGroupPrivacyRequirementContextKey{}, openAIGroupPrivacyRequirement{
		groupID:  derefGroupID(groupID),
		required: s.loadOpenAIGroupRequiresPrivacySet(ctx, groupID),
	})
}

func (s *OpenAIGatewayService) openAIGroupRequiresPrivacySet(ctx context.Context, groupID *int64) bool {
	if cached, ok := ctx.Value(openAIGroupPrivacyRequirementContextKey{}).(openAIGroupPrivacyRequirement); ok && cached.groupID == derefGroupID(groupID) {
		return cached.required
	}
	return s.loadOpenAIGroupRequiresPrivacySet(ctx, groupID)
}

func (s *OpenAIGatewayService) loadOpenAIGroupRequiresPrivacySet(ctx context.Context, groupID *int64) bool {
	if s == nil || groupID == nil || s.schedulerSnapshot == nil {
		return false
	}
	// GetByID 会聚合账号计数；隐私开关只在分组行上。
	group, err := s.schedulerSnapshot.GetGroupByIDLite(ctx, *groupID)
	if err != nil {
		return true
	}
	return group != nil && group.RequirePrivacySet
}

// applyLegacySelectionDecision 把非高级调度路径的选号结果回填到决策：记录选中账号，
// 命中会话粘性时标记 session_hash 层，否则保持 load_balance。
func applyLegacySelectionDecision(decision *OpenAIAccountScheduleDecision, selection *AccountSelectionResult) {
	if decision == nil || selection == nil || selection.Account == nil {
		return
	}
	decision.SelectedAccountID = selection.Account.ID
	decision.SelectedAccountType = selection.Account.Type
	if selection.stickySessionHit {
		decision.Layer = openAIAccountScheduleLayerSessionSticky
		decision.StickySessionHit = true
	}
}

// selectLegacyAccountByPreviousResponse 在非高级调度路径按 previous_response_id 命中持有该响应的账号，
// 先按请求模型做渠道限制检查，再复用高级调度器 previous_response 层的账号兼容校验
// （分组、隐私、运行期封禁、代理隔离、上游模型渠道限制、传输与能力）；未命中返回 (nil, false, nil)。
func (s *OpenAIGatewayService) selectLegacyAccountByPreviousResponse(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
) (*AccountSelectionResult, bool, error) {
	if strings.TrimSpace(previousResponseID) == "" || platform != PlatformOpenAI {
		return nil, false, nil
	}
	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		return nil, false, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}
	selection, err := s.selectAccountByPreviousResponseIDForCapability(ctx, groupID, previousResponseID, requestedModel, excludedIDs, requiredCapability, requireCompact)
	if err != nil {
		return nil, false, err
	}
	if selection == nil || selection.Account == nil {
		return nil, false, nil
	}
	account := selection.Account
	scheduler := &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
	compatible, _ := scheduler.isAccountRequestCompatibleReason(ctx, account, OpenAIAccountScheduleRequest{
		GroupID:                 groupID,
		Platform:                platform,
		RequestedModel:          requestedModel,
		RequiredTransport:       requiredTransport,
		RequiredCapability:      requiredCapability,
		RequiredImageCapability: requiredImageCapability,
		RequireCompact:          requireCompact,
		ExcludedIDs:             excludedIDs,
		RequirePrivacySet:       s.openAIGroupRequiresPrivacySet(ctx, groupID),
	})
	if !s.openAIAccountMatchesSchedulingGroup(account, groupID) || !compatible || !scheduler.isAccountTransportCompatible(account, requiredTransport) {
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		return nil, false, nil
	}
	if sessionHash != "" {
		_ = s.bindOpenAIStickySessionDuringSelection(ctx, groupID, sessionHash, account.ID)
	}
	return selection, true, nil
}

func (s *OpenAIGatewayService) selectAccountWithSchedulerOnce(
	ctx context.Context,
	groupID *int64,
	previousResponseID string,
	sessionHash string,
	requestedModel string,
	excludedIDs map[int64]struct{},
	requiredTransport OpenAIUpstreamTransport,
	requiredCapability OpenAIEndpointCapability,
	requiredImageCapability OpenAIImagesCapability,
	requireCompact bool,
	platform string,
	previousResponseCanMove bool,
	useUpstreamTokenCost bool,
) (*AccountSelectionResult, OpenAIAccountScheduleDecision, error) {
	ctx = s.withOpenAIQuotaAutoPauseContext(ctx)
	ctx = s.withOpenAIGroupPrivacyRequirement(ctx, groupID)
	// 分组利润控制：唯一文本调度入口的防御性装门。handler 文本
	// 入口已在请求开始经 WithOpenAIRequestPricingContext 装门并固定 pricingAt，
	// 此处对同分组门直接复用（failover 重入阈值稳定），仅为不经 handler 装配的
	// 内部调用兜底。图片/视频调度不在利润门范围：requiredImageCapability 非空的
	// Images 调度不装门；其他使用 Responses 能力的文本请求（包括原生远程压缩）
	// 仍须装门。其余媒体路径通过 WithOpenAIProfitControlSuppressed 显式跳过。
	if requiredImageCapability == "" {
		ctx = s.withOpenAIProfitControlGate(ctx, groupID)
	}
	platform = NormalizeOpenAICompatiblePlatform(platform)
	decision := OpenAIAccountScheduleDecision{}
	runtimeSettings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	preserveGuardianParentBinding := preserveOpenAIGuardianParentBinding(ctx, sessionHash)
	guardianParentAccountID := int64(0)
	if strings.TrimSpace(previousResponseID) == "" {
		guardianParentAccountID = s.resolveOpenAIGuardianParentAccountID(ctx, groupID)
	}
	scheduler := s.getOpenAIAccountScheduler(ctx)
	if scheduler == nil {
		decision.Layer = openAIAccountScheduleLayerLoadBalance
		if selection, hit, err := s.selectLegacyAccountByPreviousResponse(ctx, groupID, previousResponseID, sessionHash, requestedModel, excludedIDs, requiredTransport, requiredCapability, requiredImageCapability, requireCompact, platform); err != nil {
			return nil, decision, err
		} else if hit {
			decision.Layer = openAIAccountScheduleLayerPreviousResponse
			decision.StickyPreviousHit = true
			decision.SelectedAccountID = selection.Account.ID
			decision.SelectedAccountType = selection.Account.Type
			return selection, decision, nil
		}
		if guardianParentAccountID > 0 {
			if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
				return nil, decision, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
			}
			fallbackScheduler := &defaultOpenAIAccountScheduler{service: s, stats: newOpenAIAccountRuntimeStats()}
			selection, _, err := fallbackScheduler.selectBySessionHash(ctx, OpenAIAccountScheduleRequest{
				GroupID:                 groupID,
				Platform:                platform,
				SessionHash:             sessionHash,
				StickyAccountID:         guardianParentAccountID,
				PreserveStickyBinding:   true,
				RequestedModel:          requestedModel,
				RequiredTransport:       requiredTransport,
				RequiredCapability:      requiredCapability,
				RequiredImageCapability: requiredImageCapability,
				RequireCompact:          requireCompact,
				ExcludedIDs:             excludedIDs,
				RequirePrivacySet:       s.openAIGroupRequiresPrivacySet(ctx, groupID),
			})
			if err != nil {
				return nil, decision, err
			}
			if selection != nil && selection.Account != nil {
				decision.Layer = openAIAccountScheduleLayerGuardianParent
				decision.StickySessionHit = true
				decision.SelectedAccountID = selection.Account.ID
				decision.SelectedAccountType = selection.Account.Type
				return selection, decision, nil
			}
		}
		legacySessionHash := sessionHash
		if preserveGuardianParentBinding {
			legacySessionHash = ""
		}
		if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
			effectiveExcludedIDs := cloneExcludedAccountIDs(excludedIDs)
			for {
				selection, err := s.selectAccountWithLoadAwareness(ctx, groupID, platform, legacySessionHash, requestedModel, effectiveExcludedIDs, requireCompact, requiredCapability, useUpstreamTokenCost)
				if err != nil {
					return nil, decision, err
				}
				if selection == nil || selection.Account == nil {
					return selection, decision, nil
				}
				if accountSupportsOpenAICapabilities(selection.Account, requiredCapability, requiredImageCapability) {
					applyLegacySelectionDecision(&decision, selection)
					return selection, decision, nil
				}
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				if effectiveExcludedIDs == nil {
					effectiveExcludedIDs = make(map[int64]struct{})
				}
				if _, exists := effectiveExcludedIDs[selection.Account.ID]; exists {
					return nil, decision, ErrNoAvailableAccounts
				}
				effectiveExcludedIDs[selection.Account.ID] = struct{}{}
			}
		}

		effectiveExcludedIDs := cloneExcludedAccountIDs(excludedIDs)
		for {
			selection, err := s.selectAccountWithLoadAwareness(ctx, groupID, platform, legacySessionHash, requestedModel, effectiveExcludedIDs, requireCompact, requiredCapability, useUpstreamTokenCost)
			if err != nil {
				return nil, decision, err
			}
			if selection == nil || selection.Account == nil {
				return selection, decision, nil
			}
			if s.isOpenAIAccountTransportCompatible(selection.Account, requiredTransport) &&
				accountSupportsOpenAICapabilities(selection.Account, requiredCapability, requiredImageCapability) {
				applyLegacySelectionDecision(&decision, selection)
				return selection, decision, nil
			}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
			if effectiveExcludedIDs == nil {
				effectiveExcludedIDs = make(map[int64]struct{})
			}
			if _, exists := effectiveExcludedIDs[selection.Account.ID]; exists {
				return nil, decision, ErrNoAvailableAccounts
			}
			effectiveExcludedIDs[selection.Account.ID] = struct{}{}
		}
	}

	if s.checkChannelPricingRestriction(ctx, groupID, requestedModel) {
		slog.Warn("channel pricing restriction blocked request",
			"group_id", derefGroupID(groupID),
			"model", requestedModel)
		return nil, decision, fmt.Errorf("%w supporting model: %s (channel pricing restriction)", ErrNoAvailableAccounts, requestedModel)
	}

	var stickyAccountID int64
	if sessionHash != "" && s.cache != nil {
		if accountID, err := s.getStickySessionAccountID(ctx, groupID, sessionHash); err == nil && accountID > 0 {
			stickyAccountID = accountID
		}
	}
	stickyWeighted := s.isOpenAIAdvancedSchedulerStickyWeightedEnabled(ctx)
	subscriptionPriority := s.isOpenAIAdvancedSchedulerSubscriptionPriorityEnabled(ctx)
	stickyPreviousAccountID := int64(0)
	if stickyWeighted && previousResponseCanMove && strings.TrimSpace(previousResponseID) != "" && platform == PlatformOpenAI {
		stickyPreviousAccountID = s.ResolveAccountIDByPreviousResponseIDForScheduler(ctx, groupID, previousResponseID, requestedModel, excludedIDs, requiredCapability, requireCompact)
	}

	return scheduler.Select(ctx, OpenAIAccountScheduleRequest{
		GroupID:                 groupID,
		Platform:                platform,
		SessionHash:             sessionHash,
		StickyAccountID:         stickyAccountID,
		GuardianParentAccountID: guardianParentAccountID,
		StickyPreviousAccountID: stickyPreviousAccountID,
		StickyWeighted:          stickyWeighted,
		SubscriptionPriority:    subscriptionPriority,
		SmartHealthEnabled:      runtimeSettings.smartEnabled,
		PreserveStickyBinding:   preserveGuardianParentBinding,
		RequirePrivacySet:       s.openAIGroupRequiresPrivacySet(ctx, groupID),
		PreviousResponseID:      previousResponseID,
		PreviousResponseCanMove: previousResponseCanMove,
		UseUpstreamTokenCost:    useUpstreamTokenCost,
		RequestedModel:          requestedModel,
		RequiredTransport:       requiredTransport,
		RequiredCapability:      requiredCapability,
		RequiredImageCapability: requiredImageCapability,
		RequireCompact:          requireCompact,
		ExcludedIDs:             excludedIDs,
	})
}

func accountSupportsOpenAICapabilities(account *Account, requiredCapability OpenAIEndpointCapability, requiredImageCapability OpenAIImagesCapability) bool {
	if account == nil {
		return false
	}
	return account.SupportsOpenAIEndpointCapability(requiredCapability) &&
		account.SupportsOpenAIImageCapability(requiredImageCapability)
}

func cloneExcludedAccountIDs(excludedIDs map[int64]struct{}) map[int64]struct{} {
	if len(excludedIDs) == 0 {
		return nil
	}
	cloned := make(map[int64]struct{}, len(excludedIDs))
	for id := range excludedIDs {
		cloned[id] = struct{}{}
	}
	return cloned
}

func (s *OpenAIGatewayService) isOpenAIAccountTransportCompatible(account *Account, requiredTransport OpenAIUpstreamTransport) bool {
	if requiredTransport == OpenAIUpstreamTransportAny || requiredTransport == OpenAIUpstreamTransportHTTPSSE {
		return true
	}
	if s == nil || account == nil {
		return false
	}
	if requiredTransport == OpenAIUpstreamTransportResponsesWebsocketV2Ingress {
		if s.cfg == nil || !s.cfg.Gateway.OpenAIWS.ModeRouterV2Enabled {
			return s.getOpenAIWSProtocolResolver().Resolve(account).Transport == OpenAIUpstreamTransportResponsesWebsocketV2
		}
		mode := account.ResolveOpenAIResponsesWebSocketV2Mode(s.cfg.Gateway.OpenAIWS.IngressModeDefault)
		switch mode {
		case OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge, OpenAIWSIngressModeShared, OpenAIWSIngressModeDedicated:
			return true
		default:
			return false
		}
	}
	return s.getOpenAIWSProtocolResolver().Resolve(account).Transport == requiredTransport
}

func (s *OpenAIGatewayService) ReportOpenAIAccountScheduleResult(account *Account, model string, success bool, firstTokenMs *int, observedErr ...error) bool {
	if account == nil {
		return false
	}
	accountID := account.ID
	healthTripped := false
	clientCanceled := !success && len(observedErr) > 0 && errors.Is(observedErr[0], context.Canceled)
	attributable := success
	if !success {
		attributable = false
		if !clientCanceled && len(observedErr) > 0 && observedErr[0] != nil {
			var failoverErr *UpstreamFailoverError
			if errors.As(observedErr[0], &failoverErr) {
				attributable = failoverErr.ShouldReportAccountScheduleFailure() && failoverErr.claimAccountScheduleFailureReport()
			} else {
				attributable = true
			}
		}
	}
	if s != nil && s.rateLimitService != nil {
		if success {
			s.rateLimitService.ObserveOpenAIAPIKeyHealthSuccess(context.Background(), account)
		} else if attributable {
			healthTripped = s.rateLimitService.ObserveOpenAIAPIKeyHealthFailure(context.Background(), account, observedErr[0])
		}
	}
	if success && s != nil {
		s.openaiOAuth429RetryStartedAt.Delete(accountID)
		s.clearOpenAIAccountModelTransientState(accountID, normalizeOpenAIAccountModelTransientModel(model))
	}
	var scheduler OpenAIAccountScheduler
	if s != nil {
		scheduler = s.getOpenAIAccountScheduler(context.Background())
	}
	if scheduler == nil {
		if s != nil && account.smartHealthProbeID != 0 {
			if stats := s.openaiAccountStatsPublished.Load(); stats != nil {
				stats.cancelProbeLeaseForID(accountID, account.smartHealthProbeID, time.Now())
			}
		}
		return healthTripped
	}
	if reporter, ok := scheduler.(*defaultOpenAIAccountScheduler); ok {
		reporter.reportAccountResult(account, success, firstTokenMs, attributable)
	} else if account.smartHealthProbeID == 0 {
		scheduler.ReportResult(accountID, success, firstTokenMs)
	}
	return healthTripped
}

// ObserveOpenAIAccountHealthFailure records failures that cannot reach the
// scheduler-result path, for example after semantic response bytes were sent.
func (s *OpenAIGatewayService) ObserveOpenAIAccountHealthFailure(ctx context.Context, account *Account, observedErr error) bool {
	if s == nil || account == nil || observedErr == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if errors.Is(observedErr, context.Canceled) || (ctx != nil && errors.Is(ctx.Err(), context.Canceled)) {
		if stats := s.openaiAccountStatsPublished.Load(); stats != nil && account.smartHealthProbeID != 0 {
			stats.cancelProbeLeaseForID(account.ID, account.smartHealthProbeID, time.Now())
		}
		return false
	}
	attributable := true
	var failoverErr *UpstreamFailoverError
	if errors.As(observedErr, &failoverErr) {
		attributable = failoverErr.ShouldReportAccountScheduleFailure() && failoverErr.claimAccountScheduleFailureReport()
	}
	healthTripped := false
	if attributable && s.rateLimitService != nil {
		healthTripped = s.rateLimitService.ObserveOpenAIAPIKeyHealthFailure(ctx, account, observedErr)
	}
	smartEnabled := s.isSmartSchedulingEnabled(ctx)
	if !smartEnabled && account.smartHealthProbeID == 0 {
		return healthTripped
	}
	stats := s.openaiAccountStatsPublished.Load()
	if !smartEnabled {
		if stats != nil {
			stats.cancelProbeLeaseForID(account.ID, account.smartHealthProbeID, time.Now())
		}
		return healthTripped
	}
	if stats == nil {
		return healthTripped
	}
	thresholds := s.openAIStickyEscapeConfig()
	stats.reportAtForRequest(account.ID, false, nil, time.Now(), thresholds, true, true, account.smartHealthProbeID, attributable)
	return healthTripped
}

func (s *OpenAIGatewayService) RecordOpenAIAccountSwitch() {
	scheduler := s.getOpenAIAccountScheduler(context.Background())
	if scheduler == nil {
		return
	}
	scheduler.ReportSwitch()
}

func (s *OpenAIGatewayService) SnapshotOpenAIAccountSchedulerMetrics() OpenAIAccountSchedulerMetricsSnapshot {
	scheduler := s.getOpenAIAccountScheduler(context.Background())
	if scheduler == nil {
		return OpenAIAccountSchedulerMetricsSnapshot{}
	}
	return scheduler.SnapshotMetrics()
}

// SmartHealthSnapshot returns read-only in-process health state for applicable
// accounts on the current page. It never initializes the scheduler or runtime
// statistics; an unavailable/disabled setting intentionally yields no fields.
func (s *OpenAIGatewayService) SmartHealthSnapshot(ctx context.Context, accounts []*Account) map[int64]OpenAISmartHealthSnapshot {
	if s == nil || len(accounts) == 0 {
		return nil
	}
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	if !settings.smartEnabledKnown || !settings.smartEnabled {
		return nil
	}
	stats := s.openaiAccountStatsPublished.Load()
	if stats == nil {
		return nil
	}
	accountIDs := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account != nil && account.IsOpenAICompatible() {
			accountIDs = append(accountIDs, account.ID)
		}
	}
	return stats.smartHealthSnapshots(accountIDs, time.Now())
}

func (s *OpenAIGatewayService) openAIWSSessionStickyTTL() time.Duration {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return openaiStickySessionTTL
}

func (s *OpenAIGatewayService) openAIWSLBTopK() int {
	if s != nil && s.cfg != nil && s.cfg.Gateway.OpenAIWS.LBTopK > 0 {
		return s.cfg.Gateway.OpenAIWS.LBTopK
	}
	return 7
}

func (s *OpenAIGatewayService) openAIWSLBTopKForRequest(ctx context.Context) int {
	base := s.openAIWSLBTopK()
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	// DB 覆盖值与 stickyWeighted/subscriptionPriority 一样受总开关门控：
	// 关闭高级调度器后所有调用方（含管理页分数快照）都应回到配置/默认行为。
	if !settings.enabled {
		return base
	}
	if settings.lbTopKOverride > 0 {
		return settings.lbTopKOverride
	}
	return base
}

func (s *OpenAIGatewayService) openAIStickyEscapeConfig() openAIStickyEscapeConfig {
	if s != nil && s.cfg != nil {
		cfg := s.cfg.Gateway.OpenAIScheduler
		enabled := cfg.StickyEscapeEnabled
		if !enabled && cfg.StickyEscapeTTFTMs == 0 && cfg.StickyEscapeErrorRate == 0 {
			enabled = true
		}
		ttftMs := float64(cfg.StickyEscapeTTFTMs)
		if ttftMs <= 0 {
			ttftMs = 15000
		}
		errorRate := cfg.StickyEscapeErrorRate
		if errorRate < 0 || errorRate > 1 {
			errorRate = 0.5
		}
		if errorRate == 0 && cfg.StickyEscapeTTFTMs == 0 && cfg.StickyEscapeErrorRate == 0 {
			errorRate = 0.5
		}
		return openAIStickyEscapeConfig{
			enabled:   enabled,
			ttftMs:    ttftMs,
			errorRate: errorRate,
		}
	}
	return openAIStickyEscapeConfig{
		enabled:   true,
		ttftMs:    15000,
		errorRate: 0.5,
	}
}

func (s *OpenAIGatewayService) openAIWSSchedulerWeights() GatewayOpenAIWSSchedulerScoreWeightsView {
	if s != nil && s.cfg != nil {
		return GatewayOpenAIWSSchedulerScoreWeightsView{
			Priority:      s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority,
			Load:          s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load,
			Queue:         s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue,
			ErrorRate:     s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate,
			TTFT:          s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT,
			Reset:         s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Reset,
			QuotaHeadroom: s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.QuotaHeadroom,
			UpstreamCost:  s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.UpstreamCost,
			Previous:      s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.PreviousResponse,
			SessionSticky: s.cfg.Gateway.OpenAIWS.SchedulerScoreWeights.SessionSticky,
		}
	}
	return GatewayOpenAIWSSchedulerScoreWeightsView{
		Priority:      1.0,
		Load:          1.0,
		Queue:         0.7,
		ErrorRate:     0.8,
		TTFT:          0.5,
		Reset:         0.0,
		QuotaHeadroom: 0.0,
		UpstreamCost:  0.0,
		Previous:      5.0,
		SessionSticky: 3.0,
	}
}

func (s *OpenAIGatewayService) openAIWSSchedulerWeightsForRequest(ctx context.Context) GatewayOpenAIWSSchedulerScoreWeightsView {
	weights := s.openAIWSSchedulerWeights()
	settings := s.openAIAdvancedSchedulerRuntimeSettings(ctx)
	// 同 openAIWSLBTopKForRequest：总开关关闭时不应用 DB 覆盖值。
	if !settings.enabled {
		return weights
	}
	overridden := applyOpenAIAdvancedSchedulerWeightOverrides(weights, settings.weightOverrides)
	if !overridden.configWeights().IsValid() {
		return weights
	}
	return overridden
}

func applyOpenAIAdvancedSchedulerWeightOverrides(
	weights GatewayOpenAIWSSchedulerScoreWeightsView,
	overrides map[string]float64,
) GatewayOpenAIWSSchedulerScoreWeightsView {
	for key, value := range overrides {
		switch key {
		case "priority":
			weights.Priority = value
		case "load":
			weights.Load = value
		case "queue":
			weights.Queue = value
		case "error_rate":
			weights.ErrorRate = value
		case "ttft":
			weights.TTFT = value
		case "reset":
			weights.Reset = value
		case "quota_headroom":
			weights.QuotaHeadroom = value
		case "upstream_cost":
			weights.UpstreamCost = value
		case "previous_response":
			weights.Previous = value
		case "session_sticky":
			weights.SessionSticky = value
		}
	}
	return weights
}

type GatewayOpenAIWSSchedulerScoreWeightsView struct {
	Priority  float64
	Load      float64
	Queue     float64
	ErrorRate float64
	TTFT      float64
	// Reset 倾向「会话窗口最早重置」的账号；0 表示关闭（默认）。
	Reset         float64
	QuotaHeadroom float64
	UpstreamCost  float64
	Previous      float64
	SessionSticky float64
}

func (w GatewayOpenAIWSSchedulerScoreWeightsView) configWeights() config.GatewayOpenAIWSSchedulerScoreWeights {
	return config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority:         w.Priority,
		Load:             w.Load,
		Queue:            w.Queue,
		ErrorRate:        w.ErrorRate,
		TTFT:             w.TTFT,
		Reset:            w.Reset,
		QuotaHeadroom:    w.QuotaHeadroom,
		UpstreamCost:     w.UpstreamCost,
		PreviousResponse: w.Previous,
		SessionSticky:    w.SessionSticky,
	}
}

type OpenAIAccountSchedulerScoreSnapshot struct {
	BaseScore             float64
	StickyScore           float64
	StickyScoreInfinity   bool
	StickyWeightedEnabled bool
}

func (s *RateLimitService) BuildOpenAIAccountSchedulerScoreSnapshot(
	ctx context.Context,
	accounts []*Account,
	loadMap map[int64]*AccountLoadInfo,
) map[int64]OpenAIAccountSchedulerScoreSnapshot {
	gateway := &OpenAIGatewayService{cfg: nil, rateLimitService: s}
	if s != nil {
		gateway.cfg = s.cfg
	}
	return buildOpenAIAccountSchedulerScoreSnapshot(
		accounts,
		loadMap,
		gateway.openAIWSSchedulerWeightsForRequest(ctx),
		gateway.isOpenAIAdvancedSchedulerStickyWeightedEnabled(ctx),
		gateway.openAIOAuthSchedulingRateMultiplier(ctx),
	)
}

func BuildOpenAIAccountSchedulerScoreSnapshot(
	accounts []*Account,
	loadMap map[int64]*AccountLoadInfo,
) map[int64]OpenAIAccountSchedulerScoreSnapshot {
	gateway := &OpenAIGatewayService{}
	return buildOpenAIAccountSchedulerScoreSnapshot(accounts, loadMap, gateway.openAIWSSchedulerWeights(), false, parseOpenAIOAuthSchedulingRateMultiplier(nil))
}

func buildOpenAIAccountSchedulerScoreSnapshot(
	accounts []*Account,
	loadMap map[int64]*AccountLoadInfo,
	weights GatewayOpenAIWSSchedulerScoreWeightsView,
	stickyWeightedEnabled bool,
	oauthSchedulingRateMultiplier *float64,
) map[int64]OpenAIAccountSchedulerScoreSnapshot {
	if len(accounts) == 0 {
		return nil
	}
	candidates := make([]openAIAccountCandidateScore, 0, len(accounts))
	for _, account := range accounts {
		if account == nil {
			continue
		}
		loadInfo := loadMap[account.ID]
		if loadInfo == nil {
			loadInfo = &AccountLoadInfo{AccountID: account.ID}
		}
		candidates = append(candidates, openAIAccountCandidateScore{
			account:   account,
			loadInfo:  loadInfo,
			errorRate: 0,
			ttft:      0,
			hasTTFT:   false,
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	minPriority, maxPriority := openAIAccountSchedulingPriority(candidates[0].account), openAIAccountSchedulingPriority(candidates[0].account)
	maxWaiting := 1
	for i := range candidates {
		candidate := &candidates[i]
		candidate.priority = openAIAccountSchedulingPriority(candidate.account)
		if candidate.priority < minPriority {
			minPriority = candidate.priority
		}
		if candidate.priority > maxPriority {
			maxPriority = candidate.priority
		}
		if candidate.loadInfo.WaitingCount > maxWaiting {
			maxWaiting = candidate.loadInfo.WaitingCount
		}
	}

	minResetRemaining, maxResetRemaining := 0.0, 0.0
	hasResetSample := false
	now := time.Now()
	upstreamCostFactors := map[int64]float64(nil)
	if weights.UpstreamCost > 0 {
		accounts := make([]*Account, 0, len(candidates))
		for _, candidate := range candidates {
			accounts = append(accounts, candidate.account)
		}
		upstreamCostFactors = openAIUpstreamCostFactors(accounts, now, oauthSchedulingRateMultiplier)
	}
	if weights.Reset > 0 {
		for _, candidate := range candidates {
			end, ok := openAISchedulingResetWindowEnd(candidate.account, now)
			if !ok {
				continue
			}
			remaining := end.Sub(now).Seconds()
			if !hasResetSample {
				minResetRemaining, maxResetRemaining = remaining, remaining
				hasResetSample = true
				continue
			}
			if remaining < minResetRemaining {
				minResetRemaining = remaining
			}
			if remaining > maxResetRemaining {
				maxResetRemaining = remaining
			}
		}
	}

	result := make(map[int64]OpenAIAccountSchedulerScoreSnapshot, len(candidates))
	for _, candidate := range candidates {
		priorityFactor := 1.0
		if maxPriority > minPriority {
			priorityFactor = 1 - float64(candidate.priority-minPriority)/float64(maxPriority-minPriority)
		}
		loadFactor := 1 - clamp01(float64(candidate.loadInfo.LoadRate)/100.0)
		queueFactor := 1 - clamp01(float64(candidate.loadInfo.WaitingCount)/float64(maxWaiting))
		errorFactor := 1.0
		ttftFactor := 0.5
		resetFactor := 0.0
		if weights.Reset > 0 && hasResetSample {
			if end, ok := openAISchedulingResetWindowEnd(candidate.account, now); ok {
				if maxResetRemaining > minResetRemaining {
					resetFactor = 1 - clamp01((end.Sub(now).Seconds()-minResetRemaining)/(maxResetRemaining-minResetRemaining))
				} else {
					resetFactor = 1
				}
			}
		}
		quotaHeadroomFactor := 0.0
		if weights.QuotaHeadroom > 0 {
			quotaHeadroomFactor = openAIQuotaHeadroomFactor(candidate.account, now)
		}
		upstreamCostFactor := openAIUpstreamCostNeutralFactor
		if factor, ok := upstreamCostFactors[candidate.account.ID]; ok {
			upstreamCostFactor = factor
		}
		baseScore := weights.Priority*priorityFactor +
			weights.Load*loadFactor +
			weights.Queue*queueFactor +
			weights.ErrorRate*errorFactor +
			weights.TTFT*ttftFactor +
			weights.Reset*resetFactor +
			weights.QuotaHeadroom*quotaHeadroomFactor +
			weights.UpstreamCost*(upstreamCostFactor-openAIUpstreamCostNeutralFactor)
		score := OpenAIAccountSchedulerScoreSnapshot{
			BaseScore:             baseScore,
			StickyWeightedEnabled: stickyWeightedEnabled,
			StickyScoreInfinity:   !stickyWeightedEnabled,
		}
		if stickyWeightedEnabled {
			score.StickyScore = baseScore + weights.Previous + weights.SessionSticky
		}
		result[candidate.account.ID] = score
	}
	return result
}

func openAIUpstreamCostFactors(accounts []*Account, now time.Time, oauthSchedulingRateMultiplier *float64) map[int64]float64 {
	type rateSample struct {
		accountID int64
		rate      float64
	}

	factors := make(map[int64]float64, len(accounts))
	samples := make([]rateSample, 0, len(accounts))
	eligibleCount := 0
	for _, account := range accounts {
		if account == nil {
			continue
		}
		factors[account.ID] = openAIUpstreamCostNeutralFactor
		if !account.IsOpenAIApiKey() && !account.IsOpenAIOAuthLike() {
			continue
		}
		eligibleCount++
		if rate, ok := openAISchedulingRate(account, now, oauthSchedulingRateMultiplier); ok {
			samples = append(samples, rateSample{accountID: account.ID, rate: rate})
		}
	}
	if len(samples) < 2 || eligibleCount == 0 {
		return factors
	}

	allEqual := true
	positiveLogs := make([]float64, 0, len(samples))
	for i, sample := range samples {
		if i > 0 && sample.rate != samples[0].rate {
			allEqual = false
		}
		if sample.rate > 0 {
			positiveLogs = append(positiveLogs, math.Log(sample.rate))
		}
	}
	if allEqual || len(positiveLogs) == 0 {
		return factors
	}

	sort.Float64s(positiveLogs)
	middle := len(positiveLogs) / 2
	medianLog := positiveLogs[middle]
	if len(positiveLogs)%2 == 0 {
		medianLog = (positiveLogs[middle-1] + positiveLogs[middle]) / 2
	}
	center := math.Exp(medianLog)
	if center <= 0 || math.IsNaN(center) || math.IsInf(center, 0) {
		return factors
	}

	coverage := float64(len(samples)) / float64(eligibleCount)
	for _, sample := range samples {
		rawFactor := 1.0
		if sample.rate > 0 {
			rawFactor = 1 / (1 + sample.rate/center)
		}
		factors[sample.accountID] = clamp01(openAIUpstreamCostNeutralFactor + coverage*(rawFactor-openAIUpstreamCostNeutralFactor))
	}
	return factors
}

type openAILegacyUpstreamRateOrder struct {
	enabled bool
	rates   map[int64]float64
}

func newOpenAILegacyUpstreamRateOrder(accounts []*Account, now time.Time, oauthSchedulingRateMultiplier *float64) openAILegacyUpstreamRateOrder {
	rates := make(map[int64]float64, len(accounts))
	var first float64
	distinct := false
	for _, account := range accounts {
		if account == nil {
			continue
		}
		// 与 openAIUpstreamCostFactors 使用同一道平台门控：只有 OpenAI 平台账号
		// 的倍率参与 legacy 低倍率优先排序。上游自报倍率来自中转方，不能让它对
		// 其他平台的调度产生影响——否则自报低价即可吸走流量，而实际结算走本地倍率。
		if !account.IsOpenAIApiKey() && !account.IsOpenAIOAuthLike() {
			continue
		}
		rate, ok := openAISchedulingRate(account, now, oauthSchedulingRateMultiplier)
		if !ok {
			continue
		}
		if len(rates) == 0 {
			first = rate
		} else if rate != first {
			distinct = true
		}
		rates[account.ID] = rate
	}
	return openAILegacyUpstreamRateOrder{enabled: len(rates) >= 2 && distinct, rates: rates}
}

func openAISchedulingRate(account *Account, now time.Time, oauthSchedulingRateMultiplier *float64) (float64, bool) {
	if account == nil || (!account.IsOpenAIApiKey() && !account.IsOpenAIOAuthLike()) {
		return 0, false
	}
	if account.IsOpenAIOAuthLike() {
		if rate := oauthSchedulingRateMultiplier; rate != nil && *rate >= 0 && !math.IsNaN(*rate) && !math.IsInf(*rate, 0) {
			return *rate, true
		}
	} else if rate, ok := openAIFreshUpstreamBillingRate(account, now); ok {
		return rate, true
	}
	rate := account.BillingRateMultiplier()
	return rate, !math.IsNaN(rate) && !math.IsInf(rate, 0)
}

// compare returns -1 when a should be selected before b, 1 when b should be
// selected first, and 0 when the rate signal does not distinguish them.
func (o openAILegacyUpstreamRateOrder) compare(a, b *Account) int {
	if !o.enabled || a == nil || b == nil {
		return 0
	}
	aRate, aKnown := o.rates[a.ID]
	bRate, bKnown := o.rates[b.ID]
	if aKnown != bKnown {
		if aKnown {
			return -1
		}
		return 1
	}
	if !aKnown || aRate == bRate {
		return 0
	}
	if aRate < bRate {
		return -1
	}
	return 1
}

func openAIFreshUpstreamBillingRate(account *Account, now time.Time) (float64, bool) {
	if !isUpstreamBillingProbeAccount(account) {
		return 0, false
	}
	snapshot := decodeUpstreamBillingProbeSnapshot(account.Extra)
	if snapshot == nil || (snapshot.Status != UpstreamBillingProbeStatusOK && snapshot.Status != UpstreamBillingProbeStatusFailed) ||
		snapshot.ReceivedAt == nil || snapshot.ReceivedAt.IsZero() {
		return 0, false
	}
	receivedAt := *snapshot.ReceivedAt
	freshUntil := snapshot.FreshUntil
	if freshUntil == nil && snapshot.Status == UpstreamBillingProbeStatusOK {
		interval := snapshot.NextProbeAt.Sub(receivedAt)
		if interval > 0 {
			freshUntil = probeTimePtr(receivedAt.Add(2 * interval))
		}
	}
	if freshUntil == nil || !freshUntil.After(receivedAt) || now.Before(receivedAt) || now.After(*freshUntil) {
		return 0, false
	}
	return upstreamBillingRateAt(snapshot.Data, now)
}

func openAIQuotaHeadroomFactor(account *Account, now time.Time) float64 {
	if account == nil || len(account.Extra) == 0 || openAIQuotaHeadroomSnapshotStale(account.Extra, now) {
		return openAIQuotaHeadroomNeutralFactor
	}
	window5h, window7d := openAICanonicalQuotaWindows(account.Extra, now)
	if !window7d.hasUsed || window7d.reset {
		return openAIQuotaHeadroomNeutralFactor
	}

	factor := 1 - clamp01(window7d.usedPercent/100)
	if window5h.hasUsed && !window5h.reset {
		remaining := 1 - clamp01(window5h.usedPercent/100)
		if remaining < openAIQuotaHeadroomSecondaryLowRemain {
			factor *= openAIQuotaHeadroomNeutralFactor
		}
	}
	return factor
}

type openAICanonicalQuotaWindow struct {
	usedPercent float64
	hasUsed     bool
	reset       bool
}

// 规范字段优先；历史原始字段复用写入端 Normalize 的窗口分类，不能固定把 primary 当作 7d。
func openAICanonicalQuotaWindows(extra map[string]any, now time.Time) (window5h, window7d openAICanonicalQuotaWindow) {
	if used, ok := resolveAccountExtraNumber(extra, "codex_5h_used_percent"); ok {
		window5h = openAICanonicalQuotaWindow{usedPercent: used, hasUsed: true, reset: openAIQuotaWindowReset(extra, "5h", now)}
	}
	if used, ok := resolveAccountExtraNumber(extra, "codex_7d_used_percent"); ok {
		window7d = openAICanonicalQuotaWindow{usedPercent: used, hasUsed: true, reset: openAIQuotaWindowReset(extra, "7d", now)}
	}
	if window5h.hasUsed && window7d.hasUsed {
		return window5h, window7d
	}

	snapshot := &OpenAICodexUsageSnapshot{}
	if used, ok := resolveAccountExtraNumber(extra, "codex_primary_used_percent"); ok {
		snapshot.PrimaryUsedPercent = &used
	}
	if used, ok := resolveAccountExtraNumber(extra, "codex_secondary_used_percent"); ok {
		snapshot.SecondaryUsedPercent = &used
	}
	if minutes := parseExtraInt(extra["codex_primary_window_minutes"]); minutes > 0 {
		snapshot.PrimaryWindowMinutes = &minutes
	}
	if minutes := parseExtraInt(extra["codex_secondary_window_minutes"]); minutes > 0 {
		snapshot.SecondaryWindowMinutes = &minutes
	}
	normalized := snapshot.Normalize()
	if normalized == nil {
		return window5h, window7d
	}
	fromRaw := func(used *float64) openAICanonicalQuotaWindow {
		if used == nil {
			return openAICanonicalQuotaWindow{}
		}
		// Normalize 保留原始字段指针，据此配对同一个窗口的用量和重置时间。
		window := "secondary"
		if used == snapshot.PrimaryUsedPercent {
			window = "primary"
		}
		return openAICanonicalQuotaWindow{usedPercent: *used, hasUsed: true, reset: openAIQuotaWindowReset(extra, window, now)}
	}
	if !window5h.hasUsed {
		window5h = fromRaw(normalized.Used5hPercent)
	}
	if !window7d.hasUsed {
		window7d = fromRaw(normalized.Used7dPercent)
	}
	return window5h, window7d
}

func openAISchedulingResetWindowEnd(account *Account, now time.Time) (time.Time, bool) {
	if account == nil {
		return time.Time{}, false
	}
	if end, ok := openAICodexWindowResetAt(account.Extra, "5h"); ok && now.Before(end) {
		return end, true
	}
	if end := account.SessionWindowEnd; end != nil && now.Before(*end) {
		return *end, true
	}
	return time.Time{}, false
}

func openAIQuotaHeadroomSnapshotStale(extra map[string]any, now time.Time) bool {
	updatedRaw, ok := extra["codex_usage_updated_at"]
	if !ok {
		return true
	}
	updatedAt, err := parseTime(fmt.Sprint(updatedRaw))
	if err != nil {
		return true
	}
	return now.Sub(updatedAt) >= openAIQuotaHeadroomSnapshotStaleAfter
}

func clamp01(value float64) float64 {
	switch {
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}

func calcLoadSkewByMoments(sum float64, sumSquares float64, count int) float64 {
	if count <= 1 {
		return 0
	}
	mean := sum / float64(count)
	variance := sumSquares/float64(count) - mean*mean
	if variance < 0 {
		variance = 0
	}
	return math.Sqrt(variance)
}
