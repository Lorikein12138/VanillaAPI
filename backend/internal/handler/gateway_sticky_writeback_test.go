//go:build unit

package handler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type stickyWritebackContextKey struct{}

type stickyWritebackCall struct {
	err         error
	groupID     int64
	account     int64
	session     string
	ttl         time.Duration
	value       any
	deadline    time.Time
	replaceable []int64
}

type stickyWritebackCache struct {
	service.GatewayCache
	accountID int64
	// readErr is returned by the first lookup only, then cleared: the binding
	// lookup at request start fails while later reads see the key again.
	readErr     error
	claimErr    error
	beforeClaim func()
	finished    bool
	calls       []stickyWritebackCall
	lookups     []int64
}

func (s *stickyWritebackCache) ClaimSessionAccountID(ctx context.Context, groupID int64, session string, account int64, ttl time.Duration) (bool, error) {
	if hook := s.beforeClaim; hook != nil {
		s.beforeClaim = nil
		hook()
	}
	if s.claimErr != nil {
		err := s.claimErr
		s.claimErr = nil
		return false, err
	}
	missing := s.accountID == 0
	bound, err := s.CompareAndBindSessionAccountID(ctx, groupID, session, account, nil, ttl)
	return missing && bound, err
}

func (s *stickyWritebackCache) GetSessionAccountID(context.Context, int64, string) (int64, error) {
	s.lookups = append(s.lookups, s.accountID)
	if s.readErr != nil {
		err := s.readErr
		s.readErr = nil
		return 0, err
	}
	return s.accountID, nil
}

func (s *stickyWritebackCache) record(ctx context.Context, groupID int64, session string, account int64, ttl time.Duration, replaceable []int64) {
	deadline, _ := ctx.Deadline()
	s.calls = append(s.calls, stickyWritebackCall{
		err: ctx.Err(), groupID: groupID, account: account, session: session, ttl: ttl,
		value: ctx.Value(stickyWritebackContextKey{}), deadline: deadline,
		replaceable: append([]int64(nil), replaceable...),
	})
}

func (s *stickyWritebackCache) SetSessionAccountID(ctx context.Context, groupID int64, session string, account int64, ttl time.Duration) error {
	if s.finished {
		s.record(ctx, groupID, session, account, ttl, nil)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.accountID = account
	return nil
}

// CompareAndBindSessionAccountID mirrors the Redis compare-and-bind script: a
// key already holding the account only gets its TTL refreshed, a missing key or
// one holding an account this request failed over from is overwritten, anything
// else was rebound by another request and is left alone.
func (s *stickyWritebackCache) CompareAndBindSessionAccountID(ctx context.Context, groupID int64, session string, account int64, replaceable []int64, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.accountID == account {
		if s.finished {
			s.record(ctx, groupID, session, account, ttl, replaceable)
		}
		return true, nil
	}
	if s.accountID == 0 || slices.Contains(replaceable, s.accountID) {
		if s.finished {
			s.record(ctx, groupID, session, account, ttl, replaceable)
		}
		s.accountID = account
		return true, nil
	}
	return false, nil
}

func (s *stickyWritebackCache) RefreshSessionTTL(context.Context, int64, string, time.Duration) error {
	return nil
}

func (s *stickyWritebackCache) DeleteSessionAccountID(context.Context, int64, string) error {
	s.accountID = 0
	return nil
}

type stickyWritebackBody struct {
	io.Reader
	finish func()
}

func (b *stickyWritebackBody) Close() error {
	b.finish()
	return nil
}

// stickyWritebackUpstream answers per account with a freshly built response, so
// a failover row can fail on the first account and serve on the second.
type stickyWritebackUpstream struct {
	service.HTTPUpstream
	responses map[int64]func() *http.Response
	seen      []int64
}

func (u *stickyWritebackUpstream) Do(req *http.Request, proxy string, accountID int64, limit int) (*http.Response, error) {
	return u.DoWithTLS(req, proxy, accountID, limit, nil)
}

func (u *stickyWritebackUpstream) DoWithTLS(_ *http.Request, _ string, accountID int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.seen = append(u.seen, accountID)
	build, ok := u.responses[accountID]
	if !ok {
		return nil, fmt.Errorf("no upstream response for account %d", accountID)
	}
	return build(), nil
}

func TestGatewayMessagesStickyWritebackAfterForward(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		name := "buffered"
		if stream {
			name = "streaming"
		}
		for _, tt := range []struct {
			name   string
			bound  int64
			cancel bool
			fail   bool
			// failover makes account 9401 fail with a 500 and account 9499 serve,
			// mirroring a request that switches accounts mid-flight.
			failover bool
			// profitGate installs the group profit gate, which makes the request
			// write the admitted account eagerly and skip a different binding.
			profitGate bool
			// readErr fails the binding lookup at request start.
			readErr         bool
			claimErr        bool
			bindBeforeClaim int64
			during          func(*stickyWritebackCache)
			// rebindOnServe runs during on the serving forward instead of the
			// failing one.
			rebindOnServe bool
			wantSeen      []int64
			// wantWrites counts the terminal writes; reads and admission-time
			// writes made before the forward are not counted.
			wantWrites       int
			wantFinal        int64
			checkFinal       bool
			wantSecondLookup int64
		}{
			{name: "new binding", wantWrites: 1},
			{name: "new binding after disconnect", cancel: true, wantWrites: 1},
			{name: "refresh binding", bound: 9401, wantWrites: 1},
			{name: "refresh after disconnect", bound: 9401, cancel: true, wantWrites: 1},
			{name: "different binding", bound: 9499, cancel: true},
			{name: "forward failed", cancel: true, fail: true},
			{
				name: "rebound during forward", bound: 9401, checkFinal: true, wantFinal: 9499, wantSecondLookup: 9499,
				during: func(c *stickyWritebackCache) { c.accountID = 9499 },
			},
			{
				name: "bound by another request during forward", checkFinal: true, wantFinal: 9499, wantSecondLookup: 9499,
				during: func(c *stickyWritebackCache) { c.accountID = 9499 },
			},
			{
				name: "deleted during forward", bound: 9401, wantWrites: 1, checkFinal: true, wantFinal: 9401,
				during: func(c *stickyWritebackCache) { c.accountID = 0 },
			},
			{
				name: "rebound away and back during forward", bound: 9401, wantWrites: 1, checkFinal: true, wantFinal: 9401,
				during: func(c *stickyWritebackCache) { c.accountID = 9499; c.accountID = 9401 },
			},
			{
				// K1: the key starts missing, the request writes the admitted
				// account A, A fails over to B. The key must end on B instead of
				// keeping the account that just failed.
				name: "profit gate missing binding failover", failover: true, profitGate: true,
				wantSeen: []int64{9401, 9499}, wantWrites: 1, checkFinal: true, wantFinal: 9499, wantSecondLookup: 9499,
			},
			{
				name: "profit gate failed start lookup failover", failover: true, profitGate: true, readErr: true,
				wantSeen: []int64{9401, 9499}, wantWrites: 1, checkFinal: true, wantFinal: 9499, wantSecondLookup: 9499,
			},
			{
				name:     "failed first admission must not authorize replacing the same account from a concurrent request",
				failover: true, profitGate: true, claimErr: true,
				wantSeen: []int64{9401, 9499}, checkFinal: true, wantFinal: 9401,
				during: func(c *stickyWritebackCache) { c.accountID = 9401 },
			},
			{
				name:     "concurrent binding before admission veto must not authorize replacing the attempted account",
				failover: true, profitGate: true, bindBeforeClaim: 9499, rebindOnServe: true,
				wantSeen: []int64{9401, 9499}, checkFinal: true, wantFinal: 9401,
				during: func(c *stickyWritebackCache) { c.accountID = 9401 },
			},
			{
				name:     "refreshing a concurrent same-account binding does not confer ownership",
				failover: true, profitGate: true, bindBeforeClaim: 9401,
				wantSeen: []int64{9401, 9499}, checkFinal: true, wantFinal: 9401,
			},
			{
				name: "profit gate bound to the failed account", bound: 9401, failover: true, profitGate: true,
				wantSeen: []int64{9401, 9499}, wantWrites: 0, checkFinal: true, wantFinal: 9401, wantSecondLookup: 9401,
			},
			{
				// Another request rebinds to a third account while the failover
				// already serves B: the third account was never tried here, so the
				// terminal write leaves it alone and writes nothing.
				name: "profit gate rebound by another request during failover", failover: true, profitGate: true, rebindOnServe: true,
				wantSeen: []int64{9401, 9499}, checkFinal: true, wantFinal: 9455,
				during: func(c *stickyWritebackCache) { c.accountID = 9455 },
			},
			{
				// Without a profit gate the scheduler binds before every forward,
				// so a failed account found in the key was bound by another request
				// and is kept.
				name: "rebound to the failed account during failover", failover: true, rebindOnServe: true,
				wantSeen: []int64{9401, 9499}, checkFinal: true, wantFinal: 9401,
				during: func(c *stickyWritebackCache) { c.accountID = 9401 },
			},
		} {
			t.Run(name+"/"+tt.name, func(t *testing.T) {
				groupID := int64(9400)
				group := &service.Group{
					ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive,
					RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard,
					ProfitControlEnabled: tt.profitGate, ProfitMinMargin: 0.5,
				}
				newAccount := func(id int64, priority int, schedulable bool) *service.Account {
					rate := 0.1
					return &service.Account{
						ID: id, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
						Status: service.StatusActive, Schedulable: schedulable, Concurrency: 1, Priority: priority,
						RateMultiplier: &rate,
						Credentials:    map[string]any{"api_key": "test-key"},
						Extra:          map[string]any{"anthropic_passthrough": true},
						AccountGroups:  []service.AccountGroup{{AccountID: id, GroupID: groupID}},
					}
				}
				account := newAccount(9401, 0, true)
				other := newAccount(9499, 1, tt.failover)
				// third is only bound by the other request; it only has to resolve
				// as a known account.
				third := newAccount(9455, 2, true)
				servedID := account.ID
				if tt.failover {
					servedID = other.ID
				}
				wantCalls := 1
				if tt.failover {
					wantCalls = 2
				}
				cache := &stickyWritebackCache{accountID: tt.bound}
				if tt.readErr {
					cache.readErr = fmt.Errorf("sticky cache read failed")
				}
				if tt.claimErr {
					cache.claimErr = errors.New("sticky admission failed")
				}
				if tt.bindBeforeClaim > 0 {
					cache.beforeClaim = func() { cache.accountID = tt.bindBeforeClaim }
				}
				ctx := context.WithValue(context.Background(), stickyWritebackContextKey{}, "request-scoped value")
				ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				t.Cleanup(cancel)
				responseBody := `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4.5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
				contentType := "application/json"
				if stream {
					responseBody = compatStreamUsagePartialStream + compatStreamUsageCompletion
					contentType = "text/event-stream"
				}
				status := http.StatusOK
				if tt.fail {
					status = http.StatusBadRequest
					responseBody = `{"type":"error","error":{"type":"invalid_request_error","message":"invalid input"}}`
				}
				duringRan := false
				runDuring := func() {
					if duringRan || tt.during == nil {
						return
					}
					duringRan = true
					tt.during(cache)
				}
				finish := func() {
					// Forward closes its upstream body on return. Bound the concurrent
					// rebind and cancel precisely after that work, before the handler's
					// terminal sticky write.
					cache.finished = true
					runDuring()
					if tt.cancel {
						cancel()
					}
				}
				respond := func(status int, contentType, body string, bodyFinish func()) func() *http.Response {
					return func() *http.Response {
						return &http.Response{
							StatusCode: status,
							Header:     http.Header{"Content-Type": []string{contentType}},
							Body:       &stickyWritebackBody{Reader: strings.NewReader(body), finish: bodyFinish},
						}
					}
				}
				responses := map[int64]func() *http.Response{}
				if tt.failover {
					// By default the failing attempt's body closes before the next
					// account is selected, so a rebind there sits inside the
					// failover; rebindOnServe moves it into the serving forward.
					failingFinish := runDuring
					if tt.rebindOnServe {
						failingFinish = func() {}
					}
					responses[account.ID] = respond(http.StatusInternalServerError, "application/json", `{"type":"error","error":{"type":"api_error","message":"upstream boom"}}`, failingFinish)
					responses[other.ID] = respond(status, contentType, responseBody, finish)
				} else {
					responses[account.ID] = respond(status, contentType, responseBody, finish)
				}
				upstream := &stickyWritebackUpstream{responses: responses}
				cfg := &config.Config{RunMode: config.RunModeSimple}
				snapshot := service.NewSchedulerSnapshotService(&fakeSchedulerCache{accounts: []*service.Account{account, other, third}}, nil, nil, nil, nil)
				gw := service.NewGatewayService(
					nil, &fakeGroupRepo{group: group}, nil, nil, nil, nil, nil, cache, cfg,
					snapshot, nil, service.NewBillingService(cfg, nil), nil, nil, nil, upstream,
					&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				)
				billingCache := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
				t.Cleanup(billingCache.Stop)
				h := &GatewayHandler{
					gatewayService: gw, billingCacheService: billingCache, cfg: cfg, maxAccountSwitches: 1,
					concurrencyHelper: NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0),
				}
				requestBody := `{"model":"claude-sonnet-4.5","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"sticky writeback test"}]}`
				if stream {
					requestBody = strings.Replace(requestBody, `"stream":false`, `"stream":true`, 1)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody)).WithContext(ctx)
				c.Request.Header.Set("Content-Type", "application/json")
				key := &service.APIKey{ID: 9402, UserID: 9403, GroupID: &groupID, Group: group, User: &service.User{ID: 9403, Balance: 100}}
				c.Set(string(middleware.ContextKeyAPIKey), key)
				c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9403})

				h.Messages(c)

				require.Equal(t, wantCalls, len(upstream.seen))
				if tt.wantSeen != nil {
					require.Equal(t, tt.wantSeen, upstream.seen)
				} else {
					require.Equal(t, servedID, upstream.seen[len(upstream.seen)-1])
				}
				require.True(t, cache.finished)
				require.Equal(t, status, rec.Code)
				if tt.cancel {
					require.ErrorIs(t, ctx.Err(), context.Canceled)
				}
				require.Len(t, cache.calls, tt.wantWrites)
				for _, call := range cache.calls {
					require.NoError(t, call.err, "a completed upstream request must retain its terminal sticky write")
					require.Equal(t, groupID, call.groupID)
					require.Equal(t, servedID, call.account)
					require.NotEmpty(t, call.session)
					require.Positive(t, call.ttl)
					require.Equal(t, "request-scoped value", call.value)
					require.False(t, call.deadline.IsZero(), "the detached cache write must have a deadline")
					require.WithinDuration(t, time.Now(), call.deadline, 5*time.Second)
				}
				if tt.checkFinal {
					require.Equal(t, tt.wantFinal, cache.accountID, "the terminal write must only cover values this request may have written")
				}
				if tt.wantSecondLookup != 0 {
					nextLookup := len(cache.lookups)
					rec2 := httptest.NewRecorder()
					c2, _ := gin.CreateTestContext(rec2)
					c2.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody)).WithContext(context.WithValue(context.Background(), stickyWritebackContextKey{}, "request-scoped value"))
					c2.Request.Header.Set("Content-Type", "application/json")
					c2.Set(string(middleware.ContextKeyAPIKey), key)
					c2.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9403})

					h.Messages(c2)

					require.Equal(t, status, rec2.Code)
					require.Equal(t, tt.wantSecondLookup, cache.lookups[nextLookup], "a later request must observe the terminal binding")
				}
			})
		}
	}
}

type stickyFallbackGroups struct {
	service.GroupRepository
	groups map[int64]*service.Group
}

func (r stickyFallbackGroups) GetByIDLite(_ context.Context, groupID int64) (*service.Group, error) {
	if group := r.groups[groupID]; group != nil {
		return group, nil
	}
	return nil, service.ErrGroupNotFound
}

type stickyFallbackScheduler struct {
	*fakeSchedulerCache
	byGroup map[int64][]*service.Account
}

func (s stickyFallbackScheduler) GetSnapshot(_ context.Context, bucket service.SchedulerBucket) ([]*service.Account, bool, error) {
	return s.byGroup[bucket.GroupID], true, nil
}

type stickyFallbackCache struct {
	service.GatewayCache
	groups map[int64]*stickyWritebackCache
}

func (s stickyFallbackCache) GetSessionAccountID(ctx context.Context, groupID int64, key string) (int64, error) {
	return s.groups[groupID].GetSessionAccountID(ctx, groupID, key)
}

func (s stickyFallbackCache) SetSessionAccountID(ctx context.Context, groupID int64, key string, account int64, ttl time.Duration) error {
	return s.groups[groupID].SetSessionAccountID(ctx, groupID, key, account, ttl)
}

func (s stickyFallbackCache) ClaimSessionAccountID(ctx context.Context, groupID int64, key string, account int64, ttl time.Duration) (bool, error) {
	return s.groups[groupID].ClaimSessionAccountID(ctx, groupID, key, account, ttl)
}

func (s stickyFallbackCache) CompareAndBindSessionAccountID(ctx context.Context, groupID int64, key string, account int64, replaceable []int64, ttl time.Duration) (bool, error) {
	return s.groups[groupID].CompareAndBindSessionAccountID(ctx, groupID, key, account, replaceable, ttl)
}

func (s stickyFallbackCache) RefreshSessionTTL(ctx context.Context, groupID int64, key string, ttl time.Duration) error {
	return s.groups[groupID].RefreshSessionTTL(ctx, groupID, key, ttl)
}

func (s stickyFallbackCache) DeleteSessionAccountID(ctx context.Context, groupID int64, key string) error {
	return s.groups[groupID].DeleteSessionAccountID(ctx, groupID, key)
}

func TestGatewayMessagesStickyWritebackAcrossFallbackGroups(t *testing.T) {
	const originID, fallbackID, xID, yID = int64(1), int64(2), int64(9401), int64(9499)
	for _, tt := range []struct {
		name              string
		concurrentBinding int64
		want              int64
		writes            int
	}{
		{"missing key can be written", 0, yID, 1},
		{"serving account refreshes", yID, yID, 1},
		{"original group account rebound by another request survives", xID, xID, 0},
		{"other third-party binding survives", 9455, 9455, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			origin := &service.Group{ID: originID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive,
				RateMultiplier: 1, SubscriptionType: service.SubscriptionTypeStandard, ProfitControlEnabled: true, ProfitMinMargin: 0.5}
			fallback := *origin
			fallback.ID = fallbackID
			origin.FallbackGroupIDOnInvalidRequest = new(fallbackID)
			x := &service.Account{ID: xID, Platform: service.PlatformAntigravity, Type: service.AccountTypeOAuth,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1, RateMultiplier: new(0.1),
				Credentials: map[string]any{"access_token": "fixture", "project_id": "fixture",
					"model_mapping": map[string]any{"claude-sonnet-4.5": "claude-sonnet-4-5"}},
				Extra: map[string]any{"mixed_scheduling": true}, AccountGroups: []service.AccountGroup{{AccountID: xID, GroupID: originID}}}
			y := &service.Account{ID: yID, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1, RateMultiplier: new(0.1),
				Credentials: map[string]any{"api_key": "fixture"}, Extra: map[string]any{"anthropic_passthrough": true},
				AccountGroups: []service.AccountGroup{{AccountID: yID, GroupID: fallbackID}}}
			first, second := &stickyWritebackCache{}, &stickyWritebackCache{}
			cache := stickyFallbackCache{groups: map[int64]*stickyWritebackCache{originID: first, fallbackID: second}}
			upstream := &stickyWritebackUpstream{responses: map[int64]func() *http.Response{
				xID: func() *http.Response {
					return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Prompt is too long"}}`))}
				},
				yID: func() *http.Response {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
						Body: &stickyWritebackBody{Reader: strings.NewReader(`{"id":"msg_1","type":"message","model":"claude-sonnet-4.5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`),
							finish: func() { second.accountID, second.finished = tt.concurrentBinding, true }}}
				},
			}}
			scheduler := stickyFallbackScheduler{fakeSchedulerCache: &fakeSchedulerCache{accounts: []*service.Account{x, y}},
				byGroup: map[int64][]*service.Account{originID: {x}, fallbackID: {y}}}
			cfg := &config.Config{RunMode: config.RunModeSimple}
			snapshot := service.NewSchedulerSnapshotService(scheduler, nil, nil, nil, nil)
			gw := service.NewGatewayService(nil, stickyFallbackGroups{groups: map[int64]*service.Group{originID: origin, fallbackID: &fallback}},
				nil, nil, nil, nil, nil, cache, cfg, snapshot, nil, service.NewBillingService(cfg, nil), nil, nil, nil, upstream,
				&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			billing := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
			t.Cleanup(billing.Stop)
			h := &GatewayHandler{gatewayService: gw, billingCacheService: billing, cfg: cfg, maxAccountSwitches: 1,
				antigravityGatewayService: service.NewAntigravityGatewayService(nil, cache, nil, service.NewAntigravityTokenProvider(nil, nil, nil), nil, upstream, nil, nil),
				concurrencyHelper:         NewConcurrencyHelper(service.NewConcurrencyService(&fakeConcurrencyCache{}), SSEPingFormatClaude, 0)}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","max_tokens":16,"messages":[{"role":"user","content":"fallback request"}]}`))
			c.Request.Header.Set("Content-Type", "application/json")
			key := &service.APIKey{ID: 9402, UserID: 9403, GroupID: new(originID), Group: origin, User: &service.User{ID: 9403, Balance: 100}}
			c.Set(string(middleware.ContextKeyAPIKey), key)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 9403})
			h.Messages(c)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, []int64{xID, yID}, upstream.seen)
			require.Equal(t, xID, first.accountID, "origin group acquired its own binding")
			require.Equal(t, tt.want, second.accountID)
			require.Len(t, second.calls, tt.writes)
			for _, call := range second.calls {
				require.Equal(t, fallbackID, call.groupID)
				require.Empty(t, call.replaceable)
			}
		})
	}
}
