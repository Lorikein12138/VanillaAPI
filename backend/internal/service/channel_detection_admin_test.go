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
			if detectionBindingFromContext(req.Context()).selection.GroupID == 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
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
		latest.Proofs = map[int64]ChannelDetectionProof{1: {SampleID: "previous-sample"}}
		data, err := json.Marshal(latest)
		require.NoError(t, err)
		require.NoError(t, s.storage.Set(context.Background(), SettingKeyChannelDetectionLatest, string(data)))

		request, disconnect := context.WithCancel(context.Background())
		sampleID, err := s.Trigger(request, 1)
		require.NoError(t, err)
		require.NotEmpty(t, sampleID)
		<-started
		disconnect()
		_, err = s.Trigger(context.Background(), 1)
		require.ErrorIs(t, err, ErrDetectionBusy)
		s.tick(context.Background())
		cards, err := s.AdminGroups(context.Background())
		require.NoError(t, err)
		require.Equal(t, "running", cards[0].TriggerState)
		require.Equal(t, "idle", cards[1].TriggerState)
		_, err = s.Trigger(context.Background(), 2)
		require.NoError(t, err)
		synctest.Wait()
		require.Equal(t, "previous-sample", cards[0].SampleID)

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
		require.Equal(t, 1, questionCalls)
		require.Equal(t, "gpt-6.2-sol", checkedModel)
		cards, err = s.AdminGroups(context.Background())
		require.NoError(t, err)
		require.Equal(t, "no_send", cards[0].TriggerState)
		require.True(t, oldSample.CheckedAt.Equal(cards[0].Sample.CheckedAt))
		require.Equal(t, "previous-sample", cards[0].SampleID)
		require.Nil(t, cards[0].Confirmation)
		require.NoError(t, s.Stop(context.Background()))
	})
}

type detectionShutdownCAS struct {
	*detectionMemorySettings
	committed  chan struct{}
	resume     chan struct{}
	beforeUndo func(ChannelDetectionSnapshotGuard) error
	calls      int
}

func (m *detectionShutdownCAS) CompareAndSetSnapshot(ctx context.Context, id int64, owner string, guard ChannelDetectionSnapshotGuard, next string) (bool, error) {
	m.calls++
	switch m.calls {
	case 1:
		applied, err := m.detectionMemorySettings.CompareAndSetSnapshot(ctx, id, owner, guard, next)
		if applied {
			close(m.committed)
			<-m.resume
		}
		return applied, err
	case 2:
		if err := m.beforeUndo(guard); err != nil {
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return m.detectionMemorySettings.CompareAndSetSnapshot(ctx, id, owner, guard, next)
}

func TestChannelDetectionShutdownRetractsCommittedAdmission(t *testing.T) {
	for _, previouslyAttempted := range []bool{true, false} {
		name := "first_attempt"
		if previouslyAttempted {
			name = "prior_attempt"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				s := newDetectionFixture(t)
				coord := &detectionShutdownCAS{
					detectionMemorySettings: s.storage, committed: make(chan struct{}), resume: make(chan struct{}),
				}
				s.coordinator = coord
				gatewayCalls := 0
				s.Start(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { gatewayCalls++ }))
				synctest.Wait() // Let startup tick see the disabled plan.
				require.NoError(t, s.Save(ctx, s.cfg))
				_, _, latest, err := s.state(ctx, true)
				require.NoError(t, err)
				prior := time.Now().UTC().Add(-time.Hour)
				if previouslyAttempted {
					latest.AttemptedAt[1] = prior
				}
				latest.Samples[1] = &ChannelDetectionSample{CheckedAt: prior, Pelican: "generated", SVG: testDetectionSVG}
				latest.Proofs = map[int64]ChannelDetectionProof{1: {SampleID: "existing"}}
				latest.NoSend = map[int64]bool{1: true}
				latest.RunningByGroup[2] = channelDetectionRun{GroupID: 2, Owner: "other-owner", SampleID: "other-run"}
				raw, err := json.Marshal(latest)
				require.NoError(t, err)
				require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionLatest, string(raw)))

				coord.beforeUndo = func(guard ChannelDetectionSnapshotGuard) error {
					// An independent publication wins the snapshot CAS while
					// the retracting worker still holds only its own lease.
					var other channelDetectionSnapshot
					if err := json.Unmarshal([]byte(guard.Snapshot), &other); err != nil {
						return err
					}
					delete(other.RunningByGroup, 2)
					other.Proofs[2] = ChannelDetectionProof{SampleID: "other-completed"}
					other.Samples[2] = &ChannelDetectionSample{CheckedAt: time.Now().UTC(), Pelican: "generated", SVG: testDetectionSVG}
					other.AttemptedAt[2] = time.Now().UTC()
					data, err := json.Marshal(other)
					if err != nil {
						return err
					}
					return s.storage.Set(ctx, SettingKeyChannelDetectionLatest, string(data))
				}
				type triggerResult struct {
					id  string
					err error
				}
				triggered := make(chan triggerResult, 1)
				go func() {
					id, err := s.Trigger(ctx, 1)
					triggered <- triggerResult{id, err}
				}()
				select {
				case <-coord.committed:
				case result := <-triggered:
					t.Fatalf("trigger returned before startup CAS: %+v", result)
				}
				stopped := make(chan error, 1)
				go func() { stopped <- s.Stop(ctx) }()
				synctest.Wait()
				require.ErrorIs(t, s.worker.Err(), context.Canceled)
				close(coord.resume)

				result := <-triggered
				require.ErrorIs(t, result.err, ErrDetectionBusy)
				require.Empty(t, result.id)
				require.NoError(t, <-stopped)
				require.Zero(t, gatewayCalls)
				require.Equal(t, 3, coord.calls, "startup CAS plus conflicting and retried undo")
				active, err := coord.ActiveLeases(ctx)
				require.NoError(t, err)
				require.Empty(t, active.Groups)
				_, _, after, err := s.state(ctx, true)
				require.NoError(t, err)
				require.Empty(t, after.RunningByGroup)
				if previouslyAttempted {
					require.Equal(t, prior, after.AttemptedAt[1])
				} else {
					require.NotContains(t, after.AttemptedAt, int64(1))
				}
				require.True(t, after.NoSend[1])
				require.Equal(t, latest.Samples[1], after.Samples[1])
				require.Equal(t, latest.Proofs[1], after.Proofs[1])
				require.Equal(t, "other-completed", after.Proofs[2].SampleID)
				require.NotNil(t, after.Samples[2])
				require.Contains(t, after.AttemptedAt, int64(2))
			})
		})
	}
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
