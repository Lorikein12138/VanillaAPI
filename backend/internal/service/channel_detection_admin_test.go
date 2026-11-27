package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChannelDetectionManualRoundUsesLeaseAndWorkerContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newDetectionFixture(t)
		started := make(chan struct{})
		resume := make(chan struct{})
		var questionCalls int
		var checkedModel string
		s.Start(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			questionCalls++
			checkedModel = detectionBindingFromContext(req.Context()).selection.Model
			if questionCalls == 1 {
				close(started)
				<-resume
			}
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		synctest.Wait() // the first automatic tick saw the plan disabled
		require.NoError(t, s.Save(context.Background(), s.cfg))
		_, config, latest, err := s.state(context.Background(), true)
		require.NoError(t, err)
		for _, group := range config.Groups {
			latest.AttemptedAt[group.GroupID] = time.Now()
		}
		latest.Samples[1] = &ChannelDetectionSample{CheckedAt: time.Now(), Pelican: "error"}
		oldSample := *latest.Samples[1]
		data, err := json.Marshal(latest)
		require.NoError(t, err)
		require.NoError(t, s.storage.Set(context.Background(), SettingKeyChannelDetectionLatest, string(data)))

		request, disconnect := context.WithCancel(context.Background())
		sampleID, err := s.Trigger(request, 1)
		require.NoError(t, err)
		require.NotEmpty(t, sampleID)
		<-started
		disconnect()
		_, err = s.Trigger(context.Background(), 2)
		require.ErrorIs(t, err, ErrDetectionBusy)
		s.tick(context.Background(), s.gateway)
		cards, err := s.AdminGroups(context.Background())
		require.NoError(t, err)
		require.Equal(t, "running", cards[0].TriggerState)
		require.Equal(t, "busy", cards[1].TriggerState)
		require.Empty(t, cards[0].SampleID, "the previous sample predates private evidence")

		public, err := s.PublicGroups(context.Background())
		require.NoError(t, err)
		publicJSON, err := json.Marshal(public)
		require.NoError(t, err)
		require.NotContains(t, string(publicJSON), "trigger_state")
		require.NotContains(t, string(publicJSON), "sample_id")
		require.NotContains(t, string(publicJSON), "scope_version")
		require.NotContains(t, string(publicJSON), "account_id")
		close(resume)
		synctest.Wait()
		require.Equal(t, 7, questionCalls)
		require.Equal(t, "gpt-6.2-sol", checkedModel)
		cards, err = s.AdminGroups(context.Background())
		require.NoError(t, err)
		require.Equal(t, "no_send", cards[0].TriggerState)
		require.True(t, oldSample.CheckedAt.Equal(cards[0].Sample.CheckedAt))
		require.Nil(t, cards[0].Confirmation)
		require.NoError(t, s.Stop(context.Background()))
	})
}

func TestChannelDetectionEvidenceActualHTTPResponseAndWrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/close-after-write" {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fixture"}}
	budget := &detectionRoundBudget{}
	for _, path := range []string{"/non-2xx", "/close-after-write"} {
		req, err := http.NewRequest(http.MethodPost, server.URL+path, nil)
		require.NoError(t, err)
		req.Close = true
		req.Header.Set("Authorization", "Bearer fixture")
		observed, settle := observeDetectionAttempt(req, budget, account)
		resp, err := client.Do(observed)
		settle(resp)
		if path == "/non-2xx" {
			require.NoError(t, err)
			require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
			require.NoError(t, resp.Body.Close())
		} else {
			require.Error(t, err)
			require.Nil(t, resp)
		}
	}
	proof, entered := budget.proof(channelDetectionRun{SampleID: "test", Source: "manual"})
	require.True(t, entered)
	require.Equal(t, 2, proof.Sends)
	require.True(t, DetectionAccountMatchesProof(account, proof))
	proof.Version = 1
	require.False(t, DetectionAccountMatchesProof(account, proof))
}

func TestChannelDetectionEvidenceRejectsAmbiguity(t *testing.T) {
	newRequest := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "http://fixture.test/v1/responses", nil)
		setHeaderRaw(req.Header, "authorization", "Bearer fixture")
		return req
	}
	credentials := map[string]any{"access_token": "fixture"}
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: credentials}
	for _, tc := range []struct {
		name      string
		second    *Account
		uncertain bool
		valid     bool
	}{
		{name: "consistent", second: account, valid: true},
		{name: "different account", second: &Account{ID: 2, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: credentials}},
		{name: "identity changed", second: &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture", "refresh_token": "updated"}}},
		{name: "shadow", second: &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: credentials, ParentAccountID: new(int64)}},
		{name: "unknown and late callback", second: account, uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := &detectionRoundBudget{}
			_, settle := observeDetectionAttempt(newRequest(), budget, account)
			settle(&http.Response{StatusCode: http.StatusBadGateway})
			settle(&http.Response{StatusCode: http.StatusOK}) // a physical call settles once
			observed, settle := observeDetectionAttempt(newRequest(), budget, tc.second)
			trace := httptrace.ContextClientTrace(observed.Context())
			if tc.uncertain {
				trace.WroteRequest(httptrace.WroteRequestInfo{Err: context.Canceled})
				settle(nil)
				trace.WroteRequest(httptrace.WroteRequestInfo{})
			} else {
				trace.WroteRequest(httptrace.WroteRequestInfo{})
				settle(nil)
			}
			proof, entered := budget.proof(channelDetectionRun{})
			require.True(t, entered)
			if tc.valid {
				require.Equal(t, 2, proof.Sends)
				require.Equal(t, account.ID, proof.AccountID)
			} else {
				require.Zero(t, proof.Sends)
				require.Zero(t, proof.AccountID)
			}
		})
	}
	proof, entered := (&detectionRoundBudget{sends: 1}).proof(channelDetectionRun{})
	require.False(t, entered, "a debit alone is not a transport attempt")
	require.Zero(t, proof.AccountID)
}

func TestChannelDetectionEvidenceRequiresActualWireCredentials(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "fixture-db"}}
	for _, tc := range []struct {
		name      string
		tokens    []string
		wantSends int
	}{
		{name: "database token", tokens: []string{"fixture-db"}, wantSends: 1},
		{name: "cached token", tokens: []string{"fixture-cache"}},
		{name: "rotation during sample", tokens: []string{"fixture-cache", "fixture-db"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := &detectionRoundBudget{}
			for _, token := range tc.tokens {
				req := httptest.NewRequest(http.MethodPost, "http://fixture.test/v1/messages", nil)
				setHeaderRaw(req.Header, "authorization", "Bearer "+token)
				_, settle := observeDetectionAttempt(req, budget, account)
				settle(&http.Response{StatusCode: http.StatusOK})
			}
			proof, entered := budget.proof(channelDetectionRun{})
			require.True(t, entered)
			require.Equal(t, tc.wantSends, proof.Sends)
			encoded, err := json.Marshal(proof)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "fixture-db")
			require.NotContains(t, string(encoded), "fixture-cache")
		})
	}
	req := httptest.NewRequest(http.MethodPost, "http://fixture.test/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer fixture-db")
	addHeaderRaw(req.Header, "authorization", "Bearer fixture-cache")
	require.False(t, detectionWireCredentialMatches(req, account), "two differently cased auth headers remain ambiguous")
}
