package service

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type detectionAccountRepo struct {
	AccountRepository
	fresh  *Account
	err    error
	writes int
}

func (r *detectionAccountRepo) GetByID(_ context.Context, _ int64) (*Account, error) {
	return r.fresh, r.err
}
func (r *detectionAccountRepo) IncrementDetectionQuotaUsed(context.Context, int64, float64) error {
	r.writes++
	return nil
}

func detectionTestAccount(platform string) *Account {
	return &Account{ID: 88, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, GroupIDs: []int64{1}}
}

func TestChannelDetectionAccountReadsAuthoritativeBindingQuotaAndRPM(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extra       map[string]any
		unbound     bool
		err         error
		routeChange func(*Account, *Account)
	}{
		{name: "rpm", extra: map[string]any{"base_rpm": 1}},
		{name: "daily", extra: map[string]any{"quota_daily_limit": 1, "quota_daily_used": 1, "quota_daily_start": time.Now().UTC().Format(time.RFC3339)}},
		{name: "weekly", extra: map[string]any{"quota_weekly_limit": 1, "quota_weekly_used": 1, "quota_weekly_start": time.Now().UTC().Format(time.RFC3339)}},
		{name: "total", extra: map[string]any{"quota_limit": 1, "quota_used": 1}},
		{name: "account unbound", unbound: true},
		{name: "upstream key rotated", routeChange: func(old, fresh *Account) {
			old.Credentials["api_key"], fresh.Credentials["api_key"] = "old-fixture-key", "new-fixture-key"
		}},
		{name: "upstream base URL changed", routeChange: func(old, fresh *Account) {
			old.Credentials["base_url"], fresh.Credentials["base_url"] = "https://old.invalid", "https://new.invalid"
		}},
		{name: "model remapped", routeChange: func(old, fresh *Account) {
			old.Credentials["model_mapping"] = map[string]any{"gpt-6.2-sol": "old-model"}
			fresh.Credentials["model_mapping"] = map[string]any{"gpt-6.2-sol": "new-model"}
		}},
		{name: "OpenAI passthrough changed", routeChange: func(old, fresh *Account) {
			old.Platform, fresh.Platform = PlatformOpenAI, PlatformOpenAI
			fresh.Extra = map[string]any{"openai_passthrough": true}
		}},
		{name: "OAuth relay address changed", routeChange: func(old, fresh *Account) {
			old.Type, fresh.Type = AccountTypeOAuth, AccountTypeOAuth
			old.Extra = map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://old.invalid"}
			fresh.Extra = map[string]any{"custom_base_url_enabled": true, "custom_base_url": "https://new.invalid"}
		}},
		{name: "proxy address changed without changing ID", routeChange: func(old, fresh *Account) {
			id := int64(1)
			old.ProxyID, fresh.ProxyID = &id, &id
			old.Proxy = &Proxy{ID: id, Protocol: "http", Host: "old.invalid", Port: 9000}
			fresh.Proxy = &Proxy{ID: id, Protocol: "http", Host: "new.invalid", Port: 9000}
		}},
		{name: "authoritative read fails", err: errors.New("fixture repository unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDetectionFixture(t)
			account := detectionTestAccount(PlatformAnthropic)
			fresh := detectionTestAccount(PlatformAnthropic)
			fresh.Extra = tc.extra
			if tc.routeChange != nil {
				account.Credentials, fresh.Credentials = map[string]any{}, map[string]any{}
				tc.routeChange(account, fresh)
			}
			if tc.unbound {
				fresh.GroupIDs = []int64{2}
			}
			if tc.name == "rpm" {
				account.Type, fresh.Type = AccountTypeOAuth, AccountTypeOAuth
			}
			repo := &detectionAccountRepo{fresh: fresh, err: tc.err}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			wire := &detectionLoopbackUpstream{client: server.Client(), url: server.URL}
			budget := &detectionRoundBudget{reserve: func(context.Context, *Group) (bool, error) {
				t.Error("ineligible account consumed a send")
				return true, nil
			}}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, ok := ChannelDetectionIngress(r)
				require.True(t, ok)
				ctx := withDetectionAccountScope(r.Context(), repo, account)
				upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"max_tokens":768}`))
				require.NoError(t, err)
				for range 2 {
					_, err = wire.Do(upstream, "", account.ID, 1)
					require.ErrorIs(t, err, errDetectionAccount)
				}
				w.WriteHeader(http.StatusBadGateway)
			})
			_, err := s.probe(context.Background(), handler, s.cfg.Groups[0], "fixture", 768, detectionTestCurrent, budget)
			require.ErrorIs(t, err, errDetectionAccount)
			require.Zero(t, calls.Load())
			require.Zero(t, repo.writes)
		})
	}
}

func TestChannelDetectionNeverUsesRevokedFallback(t *testing.T) {
	s := newDetectionFixture(t)
	original, fallback := s.groupRows[1], s.groupRows[2]
	original.ClaudeCodeOnly, original.FallbackGroupID = true, &fallback.ID
	repo := &mockGroupRepoForGateway{groups: s.groupRows}
	gateway := &GatewayService{groupRepo: repo}
	ordinary, _, err := gateway.resolveGatewayGroup(context.Background(), &original.ID)
	require.NoError(t, err)
	require.Equal(t, fallback.ID, ordinary.ID)
	original.ClaudeCodeOnly = false

	account := detectionTestAccount(PlatformAnthropic)
	account.GroupIDs = []int64{original.ID, fallback.ID}
	accountRepo := &detectionAccountRepo{fresh: account}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	wire := &detectionLoopbackUpstream{client: server.Client(), url: server.URL}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, ok := ChannelDetectionIngress(r)
		require.True(t, ok)
		original.ClaudeCodeOnly = true
		_, _, err := gateway.resolveGatewayGroup(r.Context(), &original.ID)
		require.ErrorIs(t, err, errDetectionDispatch)
		// Simulate an earlier selection returning the old group after the rule
		// is revoked. The account still belongs to both groups.
		original.FallbackGroupID, original.ClaudeCodeOnly = nil, false
		ctx := context.WithValue(r.Context(), ctxkey.Group, fallback)
		ctx = withDetectionAccountScope(ctx, accountRepo, account)
		upstream, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"max_tokens":768}`))
		require.NoError(t, err)
		_, err = wire.Do(upstream, "", account.ID, 1)
		require.ErrorIs(t, err, errDetectionDispatch)
		w.WriteHeader(http.StatusForbidden)
	})
	budget := &detectionRoundBudget{reserve: func(context.Context, *Group) (bool, error) {
		t.Error("cross-group route consumed a send")
		return true, nil
	}}
	_, err = s.probe(context.Background(), handler, s.cfg.Groups[0], "fixture", 768, detectionTestCurrent, budget)
	require.ErrorIs(t, err, errDetectionDispatch)
	require.Zero(t, calls.Load())
}

type detectionAntigravityTokenCache struct {
	antigravityCompatTokenCache
	reads int
}

func (c *detectionAntigravityTokenCache) GetAccessToken(context.Context, string) (string, error) {
	c.reads++
	return "", nil
}

func TestChannelDetectionAntigravityRejectsBeforeTokenAndProjectIO(t *testing.T) {
	var proxyCalls atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer proxyServer.Close()
	host, portText, err := net.SplitHostPort(proxyServer.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	account := newAntigravityCompatAccount(AccountTypeOAuth)
	proxy := &Proxy{ID: 1, Protocol: "http", Host: host, Port: port}
	account.ProxyID, account.Proxy = &proxy.ID, proxy
	account.Credentials["expires_at"] = strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	account.Credentials["refresh_token"] = "fixture-refresh-token"
	delete(account.Credentials, "project_id")
	accountRepo := &detectionAccountRepo{fresh: account}
	projectLookups := 0
	oauth := NewAntigravityOAuthService(&mockProxyRepoForOAuth{getByIDFunc: func(context.Context, int64) (*Proxy, error) {
		projectLookups++
		return proxy, nil
	}})
	t.Cleanup(oauth.Stop)
	cache := &detectionAntigravityTokenCache{}
	provider := NewAntigravityTokenProvider(accountRepo, cache, oauth)
	provider.SetRefreshAPI(NewOAuthRefreshAPI(accountRepo, cache), NewAntigravityTokenRefresher(oauth))
	wire := &detectionLoopbackUpstream{client: proxyServer.Client(), url: proxyServer.URL}
	svc := &AntigravityGatewayService{tokenProvider: provider, httpUpstream: wire}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), detectionBindingKey{}, &detectionBinding{})
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	_, err = svc.ForwardAsResponses(ctx, c, account, []byte(`{"model":"claude-sonnet-4-5","input":"local fixture"}`), nil)
	require.ErrorIs(t, err, errDetectionOutputLimit)
	require.Zero(t, cache.reads)
	require.Zero(t, projectLookups)
	require.Zero(t, proxyCalls.Load())
}

func TestChannelDetectionPricingFailureStopsRound(t *testing.T) {
	s := newDetectionFixture(t)
	account := detectionTestAccount(PlatformAnthropic)
	account.Extra = map[string]any{"quota_daily_limit": 1}
	repo := &detectionAccountRepo{fresh: account}
	svc := &GatewayService{accountRepo: repo, billingService: NewBillingService(&config.Config{}, nil)}
	budget := &detectionRoundBudget{reserve: func(context.Context, *Group) (bool, error) { return true, nil }}
	questions := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		questions++
		routing, ok := ChannelDetectionIngress(r)
		require.True(t, ok)
		ctx := withDetectionAccountScope(r.Context(), repo, account)
		out, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://fixture.invalid/v1/messages", strings.NewReader(`{"max_tokens":768}`))
		require.NoError(t, err)
		require.NoError(t, ReserveChannelDetectionSend(out, account.ID))
		svc.RecordDetectionAccountUsage(ctx, account, routing, &ForwardResult{
			Model: "fixture-unknown-unpriced-model", Usage: ClaudeUsage{InputTokens: 10, OutputTokens: 5},
		}, ChannelUsageFields{}, time.Now())
		w.WriteHeader(http.StatusOK)
	})
	sample := s.runSample(context.Background(), handler, s.cfg.Groups[0], detectionTestCurrent, budget)
	require.Equal(t, 1, questions)
	require.Equal(t, 1, budget.sends)
	require.Zero(t, repo.writes)
	require.Equal(t, "error", sample.Pelican)
}

func TestChannelDetectionClaudeCodeOnlyGroupEligibility(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	first, second := s.groupRows[1], s.groupRows[2]
	first.ClaudeCodeOnly, first.FallbackGroupID = true, &second.ID
	options, err := s.GroupOptions(ctx)
	require.NoError(t, err)
	require.Contains(t, options, ChannelDetectionGroup{ID: first.ID, Name: first.Name, Active: true, ClaudeCodeOnly: true})
	models, err := s.ModelOptions(ctx, first.ID)
	require.ErrorIs(t, err, errDetectionModel)
	require.Empty(t, models)
	require.ErrorIs(t, s.Save(ctx, s.cfg), errDetectionModel)

	first.ClaudeCodeOnly = false
	require.NoError(t, s.Save(ctx, s.cfg))
	feature, config, latest, err := s.state(ctx, true)
	require.NoError(t, err)
	latest.Feature, latest.Revision = feature, config.Revision
	latest.Samples[first.ID] = &ChannelDetectionSample{Pelican: "generated", SVG: testDetectionSVG}
	raw, err := json.Marshal(latest)
	require.NoError(t, err)
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionLatest, string(raw)))
	results, err := s.PublicGroups(ctx)
	require.NoError(t, err)
	require.NotNil(t, results[0].Sample)

	first.ClaudeCodeOnly = true
	persisted, err := s.Config(ctx)
	require.NoError(t, err)
	require.Contains(t, persisted.Groups, s.cfg.Groups[0])
	results, err = s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelDetectionPublicGroup{{Name: second.Name, Model: s.cfg.Groups[1].Model}}, results)
	s.tickAndWait(ctx, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		binding := detectionBindingFromContext(r.Context())
		require.Equal(t, second.ID, binding.selection.GroupID)
	}))
	persisted.Groups = persisted.Groups[1:]
	require.NoError(t, s.Save(ctx, persisted))
	require.ErrorIs(t, s.Save(ctx, s.cfg), errDetectionModel)
}
