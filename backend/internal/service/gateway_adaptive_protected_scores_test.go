package service

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type protectedScoreTestStore struct{ adaptiveHistoryTestStore }

func (s protectedScoreTestStore) ReadScoreHistory(ctx context.Context, after, through time.Time) ([]AdaptiveHistoryRecord, error) {
	return s.Read(ctx, after, through)
}

func (s protectedScoreTestStore) MergeScoreHistory(ctx context.Context, cutoff time.Time, batch []AdaptiveHistoryRecord) error {
	return s.Merge(ctx, cutoff, batch)
}

type firstReadDelay struct {
	io.Reader
	delayed bool
}

func (r *firstReadDelay) Read(p []byte) (int, error) {
	if !r.delayed {
		r.delayed = true
		time.Sleep(4 * time.Millisecond)
	}
	return r.Reader.Read(p)
}

func TestGatewayAdaptiveProtectedScoresUseValidatedForwardAndRestore(t *testing.T) {
	ctx := context.Background()
	const group = int64(72)
	account, _, _ := streamHealthFixture(group)
	account.GroupIDs = append(account.GroupIDs, group+1)
	var records []AdaptiveHistoryRecord
	store := protectedScoreTestStore{adaptiveHistoryTestStore{
		read: func(context.Context, time.Time, time.Time) ([]AdaptiveHistoryRecord, error) {
			return append(append([]AdaptiveHistoryRecord{}, records...), records[0]), nil
		},
		merge: func(_ context.Context, cutoff time.Time, batch []AdaptiveHistoryRecord) error {
			require.WithinDuration(t, time.Now().Add(-24*time.Hour), cutoff, time.Minute)
			records = append(records, batch...)
			return nil
		},
	}}
	telemetry := &adaptiveTelemetry{}
	telemetry.attachAdaptiveScoreHistory(store)
	t.Cleanup(telemetry.scoreHistory.stopAccepting)
	upstream := &adaptiveStreamTestUpstream{}
	svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
	forward := func(body string, protected bool) {
		t.Helper()
		requestCtx := streamHealthContext(ctx, telemetry, group, body)
		route, enabled := gatewayAdaptiveRoute(requestCtx)
		require.True(t, enabled)
		require.Equal(t, protected, route.protected)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(requestCtx)
		upstream.resp = &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": {"text/event-stream"}},
			Body: io.NopCloser(&firstReadDelay{Reader: strings.NewReader(
				"data: {\"model\":\"deepseek-v4-1-flash-economy\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
					"data: [DONE]\n\n")}),
		}
		res, err := svc.ForwardAsChatCompletions(requestCtx, c, &account, []byte(body), "", "")
		require.NoError(t, err)
		require.NotNil(t, res.FirstTokenMs)
		require.Equal(t, streamHealthEconomyModel, gjson.GetBytes(upstream.lastBody, "model").Str)
	}

	protected := `{"conversation":"existing-session",` + streamHealthBody[1:]
	for range 4 {
		forward(protected, true)
	}
	require.Empty(t, telemetry.series)
	require.Len(t, telemetry.scoreSeries, 1)
	forward(streamHealthBody, false)
	require.Len(t, telemetry.series, 1)
	now := time.Now()
	member := telemetry.accountScores(group, []Account{account}, now).Items[0]
	require.Equal(t, 5, member.OverallSampleCount, "V2 overlaps ordinary and score-only observations")
	require.Equal(t, 100, *member.OverallScore)
	require.Len(t, member.V1, 1)
	require.Equal(t, 1, member.V1[0].SampleCount)
	require.Len(t, member.V2, 2)

	require.NoError(t, telemetry.scoreHistory.flush(ctx, telemetry, false))
	require.Len(t, records, 4)
	for _, record := range records {
		var data adaptiveStoredSample
		require.NoError(t, json.Unmarshal([]byte(record.Payload), &data))
		require.True(t, data.ScoreOnly)
		require.Equal(t, group, data.GroupID)
		require.Equal(t, account.ID, data.AccountID)
		require.Equal(t, adaptiveAccountConfig(&account), data.Config)
		require.Equal(t, streamHealthEconomyModel, data.Model)
		require.Empty(t, record.StateKey)
	}
	restored := &adaptiveTelemetry{}
	restored.attachAdaptiveScoreHistory(store)
	t.Cleanup(restored.scoreHistory.stopAccepting)
	restartedAt := now.Add(time.Hour)
	require.NoError(t, restored.scoreHistory.restore(ctx, restored, restartedAt))
	require.Empty(t, restored.series)
	require.Empty(t, restored.streamHealth.series)
	member = restored.accountScores(group, []Account{account}, restartedAt).Items[0]
	require.Equal(t, 4, member.OverallSampleCount)
	require.Equal(t, 100, *member.OverallScore)
	require.Empty(t, member.V1)
	require.Empty(t, member.V2)

	ordinary := &adaptiveTelemetry{}
	ordinary.attachAdaptiveHistory(store.adaptiveHistoryTestStore)
	t.Cleanup(ordinary.history.stopAccepting)
	require.NoError(t, ordinary.history.restore(ctx, ordinary, restartedAt))
	require.Empty(t, ordinary.series, "even an incorrectly stored score record cannot become routing evidence")
	require.Empty(t, ordinary.scoreSeries)

	require.Zero(t, restored.accountScores(group+1, []Account{account}, restartedAt).Items[0].OverallSampleCount)
	changed := account
	changed.Credentials = maps.Clone(account.Credentials)
	changed.Credentials["base_url"] = "https://other-upstream.example"
	require.Zero(t, restored.accountScores(group, []Account{changed}, restartedAt).Items[0].OverallSampleCount)
	require.Zero(t, restored.accountScores(group, []Account{account}, now.Add(25*time.Hour)).Items[0].OverallSampleCount)
}

func TestGatewayAdaptiveDetectionProbeSkipsAdaptiveIngress(t *testing.T) {
	telemetry := &adaptiveTelemetry{}
	ctx := streamHealthContext(context.Background(), telemetry, 72, streamHealthBody)
	_, enabled := gatewayAdaptiveRoute(ctx)
	require.True(t, enabled)

	probe := context.WithValue(context.Background(), detectionBindingKey{}, detectionBinding{})
	ctx = streamHealthContext(probe, telemetry, 72, streamHealthBody)
	_, enabled = gatewayAdaptiveRoute(ctx)
	require.False(t, enabled)
	_, observation := beginAdaptiveObservation(ctx)
	require.Nil(t, observation)
	require.Empty(t, telemetry.series)
	require.Empty(t, telemetry.scoreSeries)
	require.Empty(t, telemetry.streamHealth.series)
}
