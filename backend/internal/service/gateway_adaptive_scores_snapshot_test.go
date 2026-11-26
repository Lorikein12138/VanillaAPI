package service

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAdaptiveAccountScoresSnapshotMatchesScoringAndGroups(t *testing.T) {
	now := time.Now()
	limited := now.Add(time.Hour)
	a := Account{ID: 11, Name: "shared", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		GroupIDs: []int64{1, 2}, Status: StatusActive, Schedulable: true, RateLimitResetAt: &limited,
		Credentials: map[string]any{"api_key": "unused-secret", "base_url": "https://fixture.invalid/v1"},
		Extra:       map[string]any{"metadata": "unused-internal-value"},
	}
	b := Account{ID: 12, Name: "empty", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		GroupIDs: []int64{1}, Status: StatusActive, Schedulable: true,
	}
	unbound := Account{ID: 13, Name: "unbound", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, GroupIDs: []int64{2}}
	tel := &adaptiveTelemetry{}
	route := adaptiveRoute{operation: "POST /v1/chat/completions", transport: "sse", effort: "low"}
	key := adaptiveKey(route, &a, "model-1")
	key.groupID = 1
	for i, latency := range []time.Duration{1, 1, 18, 26} {
		tel.addSample(key, adaptiveSample{at: now.Add(-time.Duration(4-i) * time.Minute), latency: latency * time.Second, success: i != 3})
	}
	sameModelOtherTier := key
	sameModelOtherTier.route.tier = "priority"
	sameModelOtherTier.route.effort = "high"
	tel.addSample(sameModelOtherTier, adaptiveSample{at: now.Add(-2 * time.Minute), latency: time.Second, success: true})
	general := key
	general.route.transport = "http"
	for i := range 4 {
		tel.addSample(general, adaptiveSample{at: now.Add(-time.Duration(i+1) * time.Minute), success: false})
	}
	expired := key
	expired.model = "retained-health"
	tel.addSample(expired, adaptiveSample{at: now.Add(-25 * time.Hour), success: false})
	tel.series[expired].health = adaptiveHealth{Last: now.Add(-25 * time.Hour), Fault: adaptiveUpstreamDegraded, RetryAt: limited}
	otherGroup := key
	otherGroup.groupID = 2
	otherGroup.model = "group-two-only"
	tel.addSample(otherGroup, adaptiveSample{at: now.Add(-time.Minute), latency: time.Second, success: true})
	for _, internal := range []adaptiveSampleKey{
		{accountID: a.ID, groupID: 0, platform: a.Platform, accountType: a.Type, config: key.config, model: "legacy-no-group"},
		{accountID: a.ID, groupID: 1, platform: a.Platform, accountType: a.Type, config: "internal-old-config", model: "old-config"},
		{accountID: a.ID, groupID: 1, platform: a.Platform, accountType: AccountTypeOAuth, config: key.config, model: "other-account-type"},
		{accountID: a.ID, groupID: 1, platform: PlatformGrok, accountType: a.Type, config: key.config, model: "other-platform"},
		{accountID: unbound.ID, groupID: 1, platform: unbound.Platform, accountType: unbound.Type, config: adaptiveAccountConfig(&unbound), model: "unbound-model"},
	} {
		sample := adaptiveSample{at: now.Add(-time.Minute), latency: time.Second, success: true}
		tel.addSample(internal, sample)
		tel.streamHealth.insert(adaptiveStreamKey{sample: internal, wire: APIProtocolChatCompletions, work: adaptiveWorkText}, sample, now)
	}
	streamKey := adaptiveStreamKey{sample: key, wire: APIProtocolChatCompletions, work: adaptiveWorkText}
	otherWork := streamKey
	otherWork.work |= adaptiveWorkDeclaredTools
	otherWireTier := adaptiveStreamKey{sample: sameModelOtherTier, wire: "other-wire", work: adaptiveWorkText | adaptiveWorkState}
	otherGroupStream := adaptiveStreamKey{sample: otherGroup, wire: APIProtocolChatCompletions, work: adaptiveWorkText}
	for i, seconds := range []time.Duration{32, 31, 2, 3, 4} {
		at := now.Add(-time.Duration(5-i) * time.Minute)
		tel.streamHealth.insert(streamKey, adaptiveSample{at: at, latency: seconds * time.Second, success: true}, now)
		if i < 2 {
			tel.streamHealth.insert(otherWork, adaptiveSample{at: at, latency: seconds * time.Second, success: true}, now)
		}
	}
	for _, k := range []adaptiveStreamKey{otherWireTier, otherGroupStream} {
		tel.streamHealth.insert(k, adaptiveSample{at: now.Add(-time.Minute), latency: time.Second, success: true}, now)
	}
	stale := streamKey
	stale.sample.model = "v2-expired"
	tel.streamHealth.series[stale] = []adaptiveSample{{at: now.Add(-adaptiveStreamWindow), latency: time.Second, success: true}}

	snapshot := tel.accountScores(1, []Account{a, b, unbound}, now)
	require.Equal(t, "current_instance", snapshot.Scope)
	require.False(t, snapshot.V2Ready)
	require.Len(t, snapshot.Items, 2)
	member := snapshot.Items[0]
	require.False(t, member.SchedulableNow)
	require.True(t, snapshot.Items[1].SchedulableNow)
	require.Empty(t, snapshot.Items[1].V1)
	require.Empty(t, snapshot.Items[1].V2)
	require.Len(t, member.V1, 4)
	require.Len(t, member.V2, 3)
	for _, bucket := range member.V1 {
		switch {
		case bucket.Model == expired.model:
			require.Zero(t, bucket.SampleCount)
			require.Nil(t, bucket.LatestSampleAt)
			require.Nil(t, bucket.Reliability)
			require.Equal(t, "verification_pending", bucket.Health.State)
			require.Equal(t, limited.UTC(), *bucket.Health.NextRetestAt)
		case bucket.Transport == "http":
			want := tel.score(general, now)
			require.True(t, bucket.Known)
			require.Equal(t, float64(want.reliability), *bucket.Reliability)
			require.Equal(t, 15, bucket.ScoreWindowMinutes)
			require.Nil(t, bucket.Latency)
		case bucket.Tier == "priority":
			require.Equal(t, "high", bucket.Effort)
			require.False(t, bucket.Known)
			require.Nil(t, bucket.Reliability)
			require.Nil(t, bucket.Latency)
		default:
			want := tel.score(key, now)
			require.Equal(t, "default", bucket.Tier)
			require.Equal(t, want.known, bucket.Known)
			require.Equal(t, float64(want.reliability)/100, *bucket.Reliability)
			require.Equal(t, 100-want.latency, *bucket.Latency)
			require.Equal(t, 4, bucket.SampleCount)
		}
	}
	for _, bucket := range member.V2 {
		switch {
		case bucket.Wire == "other-wire":
			require.Equal(t, "insufficient", bucket.State)
			require.Equal(t, "priority", bucket.Tier)
			require.Equal(t, []string{"text", "state"}, bucket.Workloads)
		case len(bucket.Workloads) == 2:
			want := adaptiveStreamStatus(tel.streamHealth.series[otherWork], now)
			require.True(t, want.slow)
			require.Equal(t, "slow", bucket.State)
			require.Equal(t, want.count, bucket.StateCount)
		default:
			want := adaptiveStreamStatus(tel.streamHealth.series[streamKey], now)
			require.True(t, want.normal)
			require.Equal(t, "normal", bucket.State)
			require.Equal(t, want.count, bucket.StateCount)
			require.Equal(t, 5, bucket.SampleCount)
			require.EqualValues(t, 4000, *bucket.LatestFirstOutputMs)
		}
	}
	tel.streamHealth.ready.Store(true)
	groupTwo := tel.accountScores(2, []Account{a, b}, now)
	require.True(t, groupTwo.V2Ready)
	require.Len(t, groupTwo.Items, 1)
	require.Equal(t, otherGroup.model, groupTwo.Items[0].V1[0].Model)
	require.Equal(t, otherGroup.model, groupTwo.Items[0].V2[0].Model)
	got, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.Contains(t, string(got), "\"reliability\":null")
	require.Contains(t, string(got), "\"v1\":[],\"v2\":[]")
	for _, private := range []string{"unused-secret", "unused-internal-value", "old-config", key.config, "group-two-only", "legacy-no-group", "unbound-model", "v2-expired"} {
		require.NotContains(t, string(got), private)
	}
}

func TestAdaptiveAccountScoresSnapshotBoundedAndDeterministic(t *testing.T) {
	tel, accounts, now := fullAdaptiveScoresTestTelemetry()
	first := tel.accountScores(1, accounts, now)
	require.Len(t, tel.series, adaptiveCapacity)
	require.Len(t, tel.streamHealth.series, adaptiveCapacity)
	require.Len(t, first.Items, 50)
	for _, member := range first.Items {
		require.True(t, member.V1Truncated)
		require.True(t, member.V2Truncated)
		require.Len(t, member.V1, 8)
		require.Len(t, member.V2, 8)
		for i := 1; i < len(member.V1); i++ {
			require.Less(t, member.V1[i-1].Model, member.V1[i].Model)
			require.Less(t, member.V2[i-1].Model, member.V2[i].Model)
		}
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { assertAdaptiveScoresEqual(t, first, tel.accountScores(1, accounts, now)) })
	}
	wg.Wait()
}

func TestAdaptiveAccountScoresOverallUsesAllCurrentV1Samples(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	accounts := []Account{
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, GroupIDs: []int64{1, 2}},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, GroupIDs: []int64{1}},
		{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, GroupIDs: []int64{1}},
	}
	telemetry := &adaptiveTelemetry{}
	text := adaptiveRoute{operation: "POST /v1/responses", transport: "sse"}
	general := adaptiveRoute{operation: "POST /v1/embeddings", transport: "http"}
	keyFor := func(account int, route adaptiveRoute, model string) adaptiveSampleKey {
		key := adaptiveKey(route, &accounts[account], model)
		key.groupID = 1
		return key
	}
	add := func(key adaptiveSampleKey, age, latency time.Duration, success bool) {
		telemetry.addSample(key, adaptiveSample{at: now.Add(-age), latency: latency, success: success})
	}
	for i := range adaptiveScoreBucketsPerAccount {
		add(keyFor(0, text, fmt.Sprintf("recent-%02d", i)), 0, time.Second, true)
	}
	add(keyFor(0, text, "moderate"), 6*time.Hour, 15*time.Second, true)
	add(keyFor(0, text, "slow"), 6*time.Hour, 25*time.Second, true)
	add(keyFor(0, general, "unary"), 12*time.Hour, 45*time.Second, true)
	add(keyFor(0, general, "failure"), 6*time.Hour, 0, false)

	add(keyFor(0, text, "exactly-expired"), adaptiveWindow, 0, false)
	add(keyFor(0, text, "expired"), adaptiveWindow+time.Second, 0, false)
	add(keyFor(0, text, "future"), -time.Second, 0, false)
	oldConfig := keyFor(0, text, "old-configuration")
	oldConfig.config = "previous-config"
	add(oldConfig, 0, 0, false)
	otherGroup := keyFor(0, general, "group-two")
	otherGroup.groupID = 2
	for i := range adaptiveMinimum {
		add(otherGroup, time.Duration(i)*time.Second, time.Second, true)
		zero := keyFor(1, text, "zero")
		add(zero, time.Duration(i)*time.Second, 45*time.Second, i != 0)
		if i < adaptiveMinimum-1 {
			add(keyFor(2, text, "insufficient"), time.Duration(i)*time.Second, time.Second, true)
		}
	}
	v2 := adaptiveStreamKey{sample: keyFor(0, text, "recent-00"), wire: APIProtocolChatCompletions, work: adaptiveWorkText}
	telemetry.streamHealth.insert(v2, adaptiveSample{at: now, latency: 45 * time.Second, success: true}, now)

	rows := telemetry.accountScores(1, accounts, now).Items
	require.Len(t, rows, 3)
	require.Len(t, rows[0].V1, adaptiveScoreBucketsPerAccount)
	require.True(t, rows[0].V1Truncated)
	require.Len(t, rows[0].V2, 1)
	require.Equal(t, 12, rows[0].OverallSampleCount)
	// 8*100 + (80+40)*0.5 + 100*0.25; the failure contributes weight 0.5.
	require.Equal(t, 91, *rows[0].OverallScore)
	require.Equal(t, adaptiveMinimum, rows[1].OverallSampleCount)
	require.NotNil(t, rows[1].OverallScore)
	require.Zero(t, *rows[1].OverallScore)
	require.Equal(t, adaptiveMinimum-1, rows[2].OverallSampleCount)
	require.Nil(t, rows[2].OverallScore)

	secondGroup := telemetry.accountScores(2, accounts, now).Items
	require.Len(t, secondGroup, 1)
	require.Equal(t, adaptiveMinimum, secondGroup[0].OverallSampleCount)
	require.Equal(t, 100, *secondGroup[0].OverallScore)
	encoded, err := json.Marshal(rows)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "\"overall_score\":0,\"overall_sample_count\":4")
	require.Contains(t, string(encoded), "\"overall_score\":null,\"overall_sample_count\":3")
}

func assertAdaptiveScoresEqual(t *testing.T, want, got AdaptiveAccountScoresSnapshot) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Error("concurrent snapshot changed rows or ordering")
	}
}

func BenchmarkAdaptiveAccountScoresSnapshot(b *testing.B) {
	tel, accounts, now := fullAdaptiveScoresTestTelemetry()
	b.ReportAllocs()
	b.SetParallelism(1)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = tel.accountScores(1, accounts, now)
		}
	})
}

func fullAdaptiveScoresTestTelemetry() (*adaptiveTelemetry, []Account, time.Time) {
	now := time.Now()
	accounts := make([]Account, 50)
	for i := range accounts {
		accounts[i] = Account{ID: int64(i + 1), Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			GroupIDs: []int64{1}, Status: StatusActive, Schedulable: true}
	}
	tel := &adaptiveTelemetry{series: make(map[adaptiveSampleKey]*adaptiveSeries, adaptiveCapacity)}
	tel.streamHealth.series = make(map[adaptiveStreamKey][]adaptiveSample, adaptiveCapacity)
	for i := range adaptiveCapacity {
		account := &accounts[i%len(accounts)]
		key := adaptiveKey(adaptiveRoute{operation: "POST /v1/chat/completions", transport: "sse"}, account, fmt.Sprintf("model-%04d", i))
		key.groupID = 1
		series := &adaptiveSeries{}
		stream := make([]adaptiveSample, 10)
		for position := range adaptiveSamples {
			sample := adaptiveSample{at: now.Add(-time.Duration(adaptiveSamples-position) * time.Second), latency: 2 * time.Second, success: true}
			series.samples[position] = sample
			if position < len(stream) {
				stream[position] = sample
				series.successes[position] = sample
			}
		}
		tel.series[key] = series
		tel.streamHealth.series[adaptiveStreamKey{sample: key, wire: APIProtocolChatCompletions, work: adaptiveWorkText}] = stream
	}
	return tel, accounts, now
}
