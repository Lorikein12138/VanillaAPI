package service

import (
	"cmp"
	"math"
	"slices"
	"strconv"
	"time"
)

const adaptiveScoreBucketsPerAccount = 8

type AdaptiveAccountScoresSnapshot struct {
	ObservedAt time.Time                     `json:"observed_at"`
	Scope      string                        `json:"scope"`
	V2Ready    bool                          `json:"v2_ready"`
	Items      []AdaptiveAccountScoresMember `json:"items"`
}

type AdaptiveAccountScoresMember struct {
	ID                     int64                     `json:"id"`
	Name                   string                    `json:"name"`
	Platform               string                    `json:"platform"`
	Type                   string                    `json:"type"`
	Status                 string                    `json:"status"`
	Schedulable            bool                      `json:"schedulable"`
	SchedulableNow         bool                      `json:"schedulable_now"`
	RateLimitResetAt       *time.Time                `json:"rate_limit_reset_at"`
	OverloadUntil          *time.Time                `json:"overload_until"`
	TempUnschedulableUntil *time.Time                `json:"temp_unschedulable_until"`
	ExpiresAt              *time.Time                `json:"expires_at"`
	AutoPauseOnExpired     bool                      `json:"auto_pause_on_expired"`
	OverallScore           *int                      `json:"overall_score"`
	OverallSampleCount     int                       `json:"overall_sample_count"`
	V1                     []AdaptiveAccountScoresV1 `json:"v1"`
	V2                     []AdaptiveAccountScoresV2 `json:"v2"`
	V1Truncated            bool                      `json:"v1_truncated"`
	V2Truncated            bool                      `json:"v2_truncated"`
}

type AdaptiveAccountScoreDimensions struct {
	Model           string `json:"model"`
	Operation       string `json:"operation"`
	Transport       string `json:"transport"`
	Tier            string `json:"tier"`
	Effort          string `json:"effort"`
	InputSize       int    `json:"input_size"`
	Duration        string `json:"duration,omitempty"`
	ThinkingLevel   string `json:"thinking_level,omitempty"`
	ThinkingEnabled bool   `json:"thinking_enabled"`
}

type AdaptiveAccountScoresV1 struct {
	AdaptiveAccountScoreDimensions
	Workload           string                      `json:"workload"`
	ScoreKind          string                      `json:"score_kind"`
	ScoreWindowMinutes int                         `json:"score_window_minutes"`
	Known              bool                        `json:"known"`
	SampleCount        int                         `json:"sample_count"`
	LatestSampleAt     *time.Time                  `json:"latest_sample_at"`
	Reliability        *float64                    `json:"reliability"`
	Latency            *int                        `json:"latency"`
	Health             *AdaptiveAccountScoreHealth `json:"health"`
}

type AdaptiveAccountScoreHealth struct {
	State        string     `json:"state"`
	UpdatedAt    time.Time  `json:"updated_at"`
	NextRetestAt *time.Time `json:"next_retest_at"`
	RecoveredAt  *time.Time `json:"recovered_at"`
}

type AdaptiveAccountScoresV2 struct {
	AdaptiveAccountScoreDimensions
	Wire                string     `json:"wire"`
	Workloads           []string   `json:"workloads"`
	State               string     `json:"state"`
	SampleCount         int        `json:"sample_count"`
	StateCount          int        `json:"state_count"`
	LatestSampleAt      *time.Time `json:"latest_sample_at"`
	LatestFirstOutputMs *int64     `json:"latest_first_output_ms"`
}

// AdaptiveAccountScores reads the same local telemetry used by adaptive routing.
// It never restores history or changes ranking, health, or exploration state.
func (s *SettingService) AdaptiveAccountScores(groupID int64, accounts []Account) AdaptiveAccountScoresSnapshot {
	telemetry := s.adaptiveTelemetry
	if telemetry == nil {
		telemetry = &gatewayAdaptiveTelemetry
	}
	return telemetry.accountScores(groupID, accounts, time.Now())
}

type adaptiveScoreMembership struct {
	position               int
	platform, kind, config string
}

type adaptiveScoreCandidate struct {
	key    adaptiveStreamKey
	rankAt time.Time
	count  int
	latest adaptiveSample
	v1     *adaptiveScoreV1Input
	v2     []adaptiveSample
}

type adaptiveScoreV1Input struct {
	key      adaptiveSampleKey
	position int
	samples  [adaptiveSamples]adaptiveSample
	health   adaptiveHealth
}

type adaptiveScoreV2Input struct {
	key      adaptiveStreamKey
	position int
	samples  [adaptiveStreamSamples]adaptiveSample
	count    int
}

// Fixed batches avoid resizing and copying a full snapshot under the lock.
// Empty or unmatched buckets allocate no capture storage.
func appendAdaptiveScoreInput[T any](batches *[][]T) *T {
	const batchSize = 32
	last := len(*batches) - 1
	if last < 0 || len((*batches)[last]) == batchSize {
		*batches = append(*batches, make([]T, 0, batchSize))
		last++
	}
	batch := (*batches)[last]
	batch = batch[:len(batch)+1]
	(*batches)[last] = batch
	return &batch[len(batch)-1]
}

type adaptiveScoreCandidateList struct {
	items     []adaptiveScoreCandidate
	truncated bool
}

type adaptiveAccountOverallScore struct {
	count          int
	weight, points float64
}

func (s *adaptiveAccountOverallScore) add(sample adaptiveSample, age time.Duration, textStream bool) {
	weight := math.Exp2(-float64(age) / float64(adaptiveHalfLife))
	s.count++
	s.weight += weight
	if sample.success {
		points := 100
		if textStream {
			points = adaptiveTTFTPoints(sample.latency)
		}
		s.points += weight * float64(points)
	}
}

func (l *adaptiveScoreCandidateList) add(candidate adaptiveScoreCandidate) {
	index, _ := slices.BinarySearchFunc(l.items, candidate, compareAdaptiveScoreCandidates)
	if len(l.items) == adaptiveScoreBucketsPerAccount {
		l.truncated = true
		if index == len(l.items) {
			return
		}
	} else {
		l.items = append(l.items, adaptiveScoreCandidate{})
	}
	copy(l.items[index+1:], l.items[index:])
	l.items[index] = candidate
}

func compareAdaptiveScoreCandidates(a, b adaptiveScoreCandidate) int {
	ar, br := a.key.sample.route, b.key.sample.route
	return cmp.Or(b.rankAt.Compare(a.rankAt), cmp.Compare(a.key.sample.model, b.key.sample.model),
		cmp.Compare(ar.operation, br.operation), cmp.Compare(ar.transport, br.transport),
		cmp.Compare(ar.tier, br.tier), cmp.Compare(ar.effort, br.effort), cmp.Compare(ar.inputSize, br.inputSize),
		cmp.Compare(ar.duration, br.duration), cmp.Compare(ar.thinkingLevel, br.thinkingLevel),
		cmp.Compare(strconv.FormatBool(ar.thinkingEnabled), strconv.FormatBool(br.thinkingEnabled)),
		cmp.Compare(strconv.FormatBool(ar.nonText), strconv.FormatBool(br.nonText)),
		cmp.Compare(a.key.wire, b.key.wire), cmp.Compare(a.key.work, b.key.work))
}

func (t *adaptiveTelemetry) accountScores(groupID int64, accounts []Account, now time.Time) AdaptiveAccountScoresSnapshot {
	result := AdaptiveAccountScoresSnapshot{
		ObservedAt: now.UTC(), Scope: "current_instance", V2Ready: t.streamHealth.ready.Load(),
		Items: make([]AdaptiveAccountScoresMember, 0, len(accounts)),
	}
	members := make(map[int64]adaptiveScoreMembership, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		if groupID <= 0 || !slices.Contains(account.GroupIDs, groupID) {
			continue
		}
		members[account.ID] = adaptiveScoreMembership{len(result.Items), account.Platform, account.Type, adaptiveAccountConfig(account)}
		result.Items = append(result.Items, AdaptiveAccountScoresMember{
			ID: account.ID, Name: account.Name, Platform: account.Platform, Type: account.Type, Status: account.Status,
			Schedulable: account.Schedulable, SchedulableNow: account.IsSchedulable(), RateLimitResetAt: account.RateLimitResetAt,
			OverloadUntil: account.OverloadUntil, TempUnschedulableUntil: account.TempUnschedulableUntil,
			ExpiresAt: account.ExpiresAt, AutoPauseOnExpired: account.AutoPauseOnExpired,
			V1: []AdaptiveAccountScoresV1{}, V2: []AdaptiveAccountScoresV2{},
		})
	}
	if len(members) == 0 {
		return result
	}
	match := func(key adaptiveSampleKey) (int, bool) {
		member, ok := members[key.accountID]
		return member.position, ok && key.groupID == groupID && key.platform == member.platform &&
			key.accountType == member.kind && key.config == member.config
	}
	v1 := make([]adaptiveScoreCandidateList, len(result.Items))
	v2 := make([]adaptiveScoreCandidateList, len(result.Items))
	overall := make([]adaptiveAccountOverallScore, len(result.Items))
	var v1Inputs [][]adaptiveScoreV1Input
	t.mu.Lock()
	for key, series := range t.series {
		if pos, ok := match(key); ok {
			*appendAdaptiveScoreInput(&v1Inputs) = adaptiveScoreV1Input{key, pos, series.samples, series.health}
		}
	}
	t.mu.Unlock()

	var v2Inputs [][]adaptiveScoreV2Input
	t.streamHealth.mu.Lock()
	for key, samples := range t.streamHealth.series {
		if pos, ok := match(key.sample); ok && len(samples) > 0 {
			input := appendAdaptiveScoreInput(&v2Inputs)
			input.key, input.position = key, pos
			input.count = copy(input.samples[:], samples)
		}
	}
	t.streamHealth.mu.Unlock()

	healthCutoff := now.Add(-adaptiveHealthRetention)
	for _, batch := range v1Inputs {
		for i := range batch {
			input := &batch[i]
			var count int
			var latest adaptiveSample
			textStream := adaptiveTextStream(input.key.route)
			for _, sample := range input.samples {
				age := now.Sub(sample.at)
				if age < 0 || age >= adaptiveWindow {
					continue
				}
				count++
				if !sample.at.Before(latest.at) {
					latest = sample
				}
				// The overview covers every eligible v1 sample, including buckets
				// below the detail limit. v2 can observe the same attempts.
				overall[input.position].add(sample, age, textStream)
			}
			rankAt := latest.at
			if input.health.Last.After(healthCutoff) && !input.health.Last.After(now) && input.health.Last.After(rankAt) {
				rankAt = input.health.Last
			}
			if !rankAt.IsZero() {
				v1[input.position].add(adaptiveScoreCandidate{key: adaptiveStreamKey{sample: input.key}, rankAt: rankAt, count: count, latest: latest, v1: input})
			}
		}
	}

	v2Cutoff := now.Add(-adaptiveStreamWindow)
	for _, batch := range v2Inputs {
		for i := range batch {
			input := &batch[i]
			samples := input.samples[:input.count]
			count, latest := adaptiveScoreSampleCoverage(samples, v2Cutoff, now)
			if count > 0 {
				v2[input.position].add(adaptiveScoreCandidate{key: input.key, rankAt: latest.at, count: count, latest: latest, v2: samples})
			}
		}
	}

	for i := range result.Items {
		member := &result.Items[i]
		summary := overall[i]
		member.OverallSampleCount = summary.count
		if summary.count >= adaptiveMinimum {
			score := int(math.Round(summary.points / summary.weight))
			member.OverallScore = &score
		}
		member.V1Truncated, member.V2Truncated = v1[i].truncated, v2[i].truncated
		for _, candidate := range v1[i].items {
			member.V1 = append(member.V1, candidate.snapshotV1(now))
		}
		for _, candidate := range v2[i].items {
			member.V2 = append(member.V2, candidate.snapshotV2(now))
		}
	}
	return result
}

func adaptiveScoreSampleCoverage(samples []adaptiveSample, cutoff, now time.Time) (count int, latest adaptiveSample) {
	for _, sample := range samples {
		if sample.at.After(cutoff) && !sample.at.After(now) {
			count++
			if !sample.at.Before(latest.at) {
				latest = sample
			}
		}
	}
	return count, latest
}

func adaptiveScoreDimensions(key adaptiveSampleKey) AdaptiveAccountScoreDimensions {
	route := key.route
	tier := route.tier
	if tier == "" {
		tier = "default"
	}
	return AdaptiveAccountScoreDimensions{
		Model: key.model, Operation: route.operation, Transport: route.transport, Tier: tier, Effort: route.effort,
		InputSize: route.inputSize, Duration: route.duration, ThinkingLevel: route.thinkingLevel, ThinkingEnabled: route.thinkingEnabled,
	}
}

func (c adaptiveScoreCandidate) snapshotV1(now time.Time) AdaptiveAccountScoresV1 {
	key := c.key.sample
	score := scoreAdaptiveSamples(c.v1.samples[:], key.route, now)
	score.health = adaptiveHealthAt(c.v1.health, now)
	window := adaptiveWindow
	if score.recent {
		window = adaptiveRecentWindow
	}
	result := AdaptiveAccountScoresV1{
		AdaptiveAccountScoreDimensions: adaptiveScoreDimensions(key),
		Workload:                       "text_or_unspecified", ScoreKind: "general", ScoreWindowMinutes: int(window.Minutes()),
		Known: score.known, SampleCount: c.count, LatestSampleAt: adaptiveScoreTime(c.latest.at),
	}
	if adaptiveTextStream(key.route) {
		result.Workload, result.ScoreKind = "text", "text_stream"
	} else if key.route.nonText {
		result.Workload = "non_text"
	}
	if score.known {
		reliability, latency := float64(score.reliability), score.latency
		if result.ScoreKind == "text_stream" {
			reliability /= 100
			latency = 100 - latency
		}
		result.Reliability = &reliability
		if score.latency != math.MaxInt {
			result.Latency = &latency
		}
	}
	if !score.health.Last.IsZero() && !score.health.Last.After(now) {
		health := score.health
		state := "healthy"
		switch {
		case health.Fault == adaptiveLatencyDegraded:
			state = "latency_degraded"
		case health.Fault == adaptiveUpstreamDegraded:
			state = "upstream_degraded"
		case health.Verify:
			state = "verification_pending"
		case !health.RecoveredAt.IsZero():
			state = "recovered"
		}
		result.Health = &AdaptiveAccountScoreHealth{
			State: state, UpdatedAt: health.Last.UTC(), NextRetestAt: adaptiveScoreTime(health.RetryAt),
			RecoveredAt: adaptiveScoreTime(health.RecoveredAt),
		}
	}
	return result
}

func (c adaptiveScoreCandidate) snapshotV2(now time.Time) AdaptiveAccountScoresV2 {
	status := adaptiveStreamStatus(c.v2, now)
	state := "insufficient"
	if status.slow {
		state = "slow"
	} else if status.normal {
		state = "normal"
	}
	firstOutput := c.latest.latency.Milliseconds()
	return AdaptiveAccountScoresV2{
		AdaptiveAccountScoreDimensions: adaptiveScoreDimensions(c.key.sample), Wire: c.key.wire,
		Workloads: adaptiveScoreWorkloads(c.key.work), State: state, StateCount: status.count,
		SampleCount: c.count, LatestSampleAt: adaptiveScoreTime(c.latest.at), LatestFirstOutputMs: &firstOutput,
	}
}

func adaptiveScoreWorkloads(work adaptiveWorkload) []string {
	out := make([]string, 0, 4)
	for _, item := range []struct {
		bit   adaptiveWorkload
		label string
	}{
		{adaptiveWorkText, "text"}, {adaptiveWorkDeclaredTools, "declared_tools"}, {adaptiveWorkToolHistory, "tool_history"},
		{adaptiveWorkState, "state"}, {adaptiveWorkUnexpectedTools, "unexpected_tools"}, {adaptiveWorkRewritten, "rewritten"},
		{adaptiveWorkInstructions, "instructions"}, {adaptiveWorkToolStream, "tool_stream"},
		{adaptiveWorkToolStreamTrue, "tool_stream_true"}, {adaptiveWorkReasoningText, "reasoning_text"},
		{adaptiveWorkCache, "cache"}, {adaptiveWorkReasoningOpaque, "reasoning_opaque"},
	} {
		if work&item.bit != 0 {
			out = append(out, item.label)
		}
	}
	return out
}

func adaptiveScoreTime(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	utc := at.UTC()
	return &utc
}
