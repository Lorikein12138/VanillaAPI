package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"bytes"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/modeltrace/trace"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"io"
	"net/http/httptest"
)

type detectionMemorySettings struct {
	mu     sync.Mutex
	values map[string]string
	owner  string
	sends  int
}

func (m *detectionMemorySettings) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		values[key] = m.values[key]
	}
	return values, nil
}

func (m *detectionMemorySettings) Set(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = value
	return nil
}

func (m *detectionMemorySettings) TryAcquire(_ context.Context, owner string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owner != "" {
		return false, nil
	}
	m.owner = owner
	return true, nil
}

func (m *detectionMemorySettings) Holds(_ context.Context, owner string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owner == owner, nil
}

func (m *detectionMemorySettings) Release(_ context.Context, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owner == owner {
		m.owner = ""
	}
	return nil
}

func (m *detectionMemorySettings) ReserveSend(_ context.Context, owner string, expected ChannelDetectionSnapshotGuard, _ int64, _, limit int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owner != owner || m.values[SettingKeyChannelDetectionFeature] != expected.Feature ||
		m.values[SettingKeyChannelDetectionConfig] != expected.Config || m.sends >= limit {
		return false, nil
	}
	m.sends++
	return true, nil
}

func (m *detectionMemorySettings) CompareAndSetSnapshot(_ context.Context, owner string, expected ChannelDetectionSnapshotGuard, next string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.owner != owner || m.values[SettingKeyChannelDetectionFeature] != expected.Feature ||
		m.values[SettingKeyChannelDetectionConfig] != expected.Config || m.values[SettingKeyChannelDetectionLatest] != expected.Snapshot {
		return false, nil
	}
	m.values[SettingKeyChannelDetectionLatest] = next
	return true, nil
}

type detectionTestGroups map[int64]*Group

func (g detectionTestGroups) GetGroup(_ context.Context, id int64) (*Group, error) {
	if group := g[id]; group != nil {
		return group, nil
	}
	return nil, ErrGroupNotFound
}

func (g detectionTestGroups) List(_ context.Context, _ pagination.PaginationParams) ([]Group, *pagination.PaginationResult, error) {
	groups := make([]Group, 0, len(g))
	for _, group := range g {
		groups = append(groups, *group)
	}
	return groups, &pagination.PaginationResult{Total: int64(len(groups))}, nil
}

func (detectionTestGroups) resolveAdminGroupModelAllowlist(context.Context, *Group) error { return nil }
func (detectionTestGroups) validateSimpleModeGroupAccess(*Group) error                    { return nil }

type detectionFixture struct {
	*ChannelDetection
	cfg       ChannelDetectionConfig
	storage   *detectionMemorySettings
	groupRows detectionTestGroups
}

func newDetectionFixture(t *testing.T) *detectionFixture {
	t.Helper()
	settings := &detectionMemorySettings{values: map[string]string{SettingKeyChannelDetectionFeature: "enable-one"}}
	groups := detectionTestGroups{
		1: {ID: 1, Name: "First", Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6.2-sol"}}},
		2: {ID: 2, Name: "Second", Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Models: []string{"claude-sonnet-5", "*"}}},
	}
	cfg := ChannelDetectionConfig{Enabled: true, IntervalMinutes: 60, Groups: []ChannelDetectionSelection{
		{GroupID: 1, Model: "gpt-6.2-sol"},
		{GroupID: 2, Model: "claude-sonnet-5"},
	}}
	svc := &ChannelDetection{
		settings: settings, groups: groups, directory: groups, coordinator: settings,
	}
	return &detectionFixture{ChannelDetection: svc, cfg: cfg, storage: settings, groupRows: groups}
}

func TestChannelDetectionValidationAndPause(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	config := s.cfg
	current, err := s.Config(ctx)
	require.NoError(t, err)
	require.Equal(t, ChannelDetectionConfig{IntervalMinutes: 60, Groups: []ChannelDetectionSelection{}}, current)
	_, err = s.PublicGroups(ctx)
	require.ErrorIs(t, err, errDetectionDisabled)

	models, err := s.ModelOptions(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, []string{"claude-sonnet-5"}, models)
	for _, invalid := range []ChannelDetectionSelection{
		{GroupID: 2, Model: "*"},
		{GroupID: 1, Model: "claude-sonnet-5"},
	} {
		require.ErrorIs(t, s.Save(ctx, ChannelDetectionConfig{Enabled: true, IntervalMinutes: 60, Groups: []ChannelDetectionSelection{invalid}}), errDetectionModel)
	}
	require.NoError(t, s.Save(ctx, config))

	s.groupRows[1].ModelAllowlist.Models = nil
	config.Enabled = false
	require.NoError(t, s.Save(ctx, config))
	saved, err := s.Config(ctx)
	require.NoError(t, err)
	require.Equal(t, config, saved)
	config.Enabled = true
	require.NoError(t, s.Save(ctx, config))
	config.Groups = nil
	config.Enabled = false
	require.NoError(t, s.Save(ctx, config))
}

func detectionGoldenReply(t *testing.T) trace.Reply {
	t.Helper()
	data, err := os.ReadFile("testdata/channel_detection_modeltrace_sol.json")
	require.NoError(t, err)
	var reply trace.Reply
	require.NoError(t, json.Unmarshal(data, &reply))
	return reply
}

const testDetectionSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 480 360"><circle cx="50" cy="70" r="12" fill="#123456"/></svg>`

func TestChannelDetectionTickPersistsOnlyPublicEvidence(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	config := s.cfg
	require.NoError(t, s.Save(ctx, config))
	reply := detectionGoldenReply(t)
	bank, err := trace.EmbeddedBank()
	require.NoError(t, err)
	require.Equal(t, "match", categorizeDetectionFingerprint(bank, "gpt-6.2-sol", reply.Text, reply.ExpectedCount))
	require.Equal(t, "different", categorizeDetectionFingerprint(bank, "claude-sonnet-5", reply.Text, reply.ExpectedCount))
	require.Equal(t, "unattributed", categorizeDetectionFingerprint(bank, "unlisted-model", reply.Text, reply.ExpectedCount))

	sessions := make(map[string]bool)
	r := gin.New()
	r.POST("/v1/responses", func(c *gin.Context) {
		require.Empty(t, c.GetHeader("Authorization"))
		routing, ok := ChannelDetectionIngress(c.Request)
		require.True(t, ok)
		require.Zero(t, routing.ID)
		require.Zero(t, routing.UserID)
		require.Equal(t, int64(1), *routing.GroupID)
		session := c.GetHeader("Session_Id")
		require.NotEmpty(t, session)
		require.Equal(t, session, ChannelDetectionSessionID(c.Request.Context()))
		require.False(t, sessions[session])
		sessions[session] = true
		var body struct {
			Model string `json:"model"`
			Input string `json:"input"`
			Limit int    `json:"max_output_tokens"`
		}
		require.NoError(t, c.ShouldBindJSON(&body))
		require.Equal(t, "gpt-6.2-sol", body.Model)
		require.Positive(t, body.Limit)
		var a, b int
		var text string
		switch len(sessions) {
		case 1:
			_, err := fmt.Sscanf(body.Input, "A counter begins at %d. Repeat exactly %d", &a, &b)
			require.NoError(t, err)
			text = strconv.Itoa(detectionFinalCounter(a, b))
		case 2:
			text = "99999"
		case 3, 6:
			c.String(http.StatusBadGateway, "raw-internal-error")
			return
		case 4:
			text = reply.Text
		case 5:
			text = "I am gpt-6.2-sol"
		case 7:
			text = testDetectionSVG
		}
		c.JSON(http.StatusOK, gin.H{
			"model": "claimed-name-is-ignored", "status": "completed",
			"output": []gin.H{{"type": "message", "role": "assistant",
				"content": []gin.H{{"type": "output_text", "text": text}}}},
		})
	})

	s.tick(ctx, r)
	require.Len(t, sessions, 7)
	require.Empty(t, s.storage.owner)
	groups, err := s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	require.NotNil(t, groups[0].Sample)
	require.Equal(t, ChannelDetectionCounts{Passed: 1, Failed: 1, Errors: 1}, groups[0].Sample.Reasoning)
	require.Equal(t, ChannelDetectionCounts{Passed: 1, Errors: 1, Unattributed: 1}, groups[0].Sample.Fingerprint)
	require.Equal(t, "generated", groups[0].Sample.Pelican)
	require.Contains(t, groups[0].Sample.SVG, "<circle")
	require.Nil(t, groups[1].Sample)
	public, err := json.Marshal(groups)
	require.NoError(t, err)
	for _, hidden := range []string{"api_key", "test-only", "raw-internal-error", "claimed-name", "attempted_at", "enable-one"} {
		require.NotContains(t, string(public), hidden)
	}

	s.groupRows[1].ModelAllowlist.Models = nil
	s.groupRows[2].Status = StatusDisabled
	groups, err = s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Empty(t, groups)
	s.groupRows[1].ModelAllowlist.Models = []string{"gpt-6.2-sol"}
	require.NoError(t, s.settings.Set(ctx, SettingKeyChannelDetectionFeature, ""))
	_, err = s.PublicGroups(ctx)
	require.ErrorIs(t, err, errDetectionDisabled)
	require.NoError(t, s.settings.Set(ctx, SettingKeyChannelDetectionFeature, "enable-two"))
	groups, err = s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelDetectionPublicGroup{{Name: "First", Model: "gpt-6.2-sol"}}, groups)
}

func TestChannelDetectionPrivateScopeAndWireBudget(t *testing.T) {
	s := newDetectionFixture(t)
	ctx := context.Background()
	budget := &detectionRoundBudget{reserve: func(context.Context, *Group) (bool, error) { return true, nil }}
	forged := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-6.2-sol"}`))
	forged.Header.Set("Authorization", "Bearer forged")
	forged.Header.Set("X-Expected-Group-ID", "1")
	_, ok := ChannelDetectionIngress(forged)
	require.False(t, ok)

	current := true
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		invalid := r.Clone(r.Context())
		invalid.URL.RawQuery = "internal=1"
		_, ok := ChannelDetectionIngress(invalid)
		require.False(t, ok)
		routing, ok := ChannelDetectionIngress(r)
		require.True(t, ok)
		require.Empty(t, routing.Key)
		_, ok = ChannelDetectionIngress(r)
		require.False(t, ok)

		account := detectionTestAccount(PlatformAnthropic)
		ctx := withDetectionAccountScope(r.Context(), &detectionAccountRepo{fresh: account}, account)
		out := func(path, body string) *http.Request {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://test.invalid"+path, bytes.NewBufferString(body))
			require.NoError(t, err)
			return req
		}
		require.ErrorIs(t, ReserveChannelDetectionSend(out("/v1/responses", `{ "input": "no cap" }`), account.ID), errDetectionOutputLimit)
		require.ErrorIs(t, ReserveChannelDetectionSend(out("/v1/messages", `{ "max_tokens":129 }`), account.ID), errDetectionOutputLimit)
		require.Zero(t, budget.sends)
		require.NoError(t, ReserveChannelDetectionSend(out("/v1/messages", `{ "max_tokens":128 }`), account.ID))
		require.NoError(t, ReserveChannelDetectionSend(out("/v1/responses", `{ "max_output_tokens":128, "store":false }`), account.ID))
		require.ErrorIs(t, ReserveChannelDetectionSend(out("/v1/responses", `{ "max_output_tokens":128 }`), account.ID), errDetectionBudget)
		require.Equal(t, 2, budget.sends)
		current = false
		require.False(t, ChannelDetectionDispatchAllowed(r.Context()))
		require.ErrorIs(t, ReserveChannelDetectionSend(out("/v1/responses", `{ "max_output_tokens":128 }`), account.ID), errDetectionDispatch)
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := s.probe(ctx, handler, s.cfg.Groups[0], "scope test", 128, func(context.Context) bool { return current }, budget)
	require.ErrorIs(t, err, errDetectionDispatch)
	require.True(t, ChannelDetectionDispatchAllowed(ctx))
}

func TestChannelDetectionReadsLegacyConfigurationWithoutPublishingLegacySample(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	const oldConfig = `{"enabled":true,"interval_minutes":60,"revision":"legacy","groups":[{"group_id":1,"model":"gpt-6.2-sol","api_key_id":999}]}`
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionConfig, oldConfig))
	legacy := channelDetectionSnapshot{
		Feature: "enable-one", Revision: "legacy",
		Samples:     map[int64]*ChannelDetectionSample{1: {Pelican: "generated", SVG: testDetectionSVG}},
		AttemptedAt: map[int64]time.Time{1: time.Now().UTC()},
	}
	raw, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NoError(t, s.storage.Set(ctx, SettingKeyChannelDetectionLatest, string(raw)))

	_, config, snapshot, err := s.state(ctx, true)
	require.NoError(t, err)
	require.Equal(t, oldConfig, config.stored)
	require.Contains(t, snapshot.AttemptedAt, int64(1))
	require.Equal(t, s.cfg.Groups[:1], config.Groups)
	public, err := s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Len(t, public, 1)
	require.Nil(t, public[0].Sample)

	input := config.ChannelDetectionConfig
	input.IntervalMinutes++
	require.NoError(t, s.Save(ctx, input))
	require.NotContains(t, s.storage.values[SettingKeyChannelDetectionConfig], "api_key_id")
}

func TestChannelDetectionLeaseAndConfigurationChange(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	config := s.cfg
	require.NoError(t, s.Save(ctx, config))
	started := make(chan struct{})
	canceled := make(chan struct{})
	done := make(chan struct{})
	gateway := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		upstream, release := detachUpstreamContext(r.Context())
		defer release()
		<-upstream.Done()
		close(canceled)
	})
	go func() {
		defer close(done)
		s.tick(ctx, gateway)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	s.tick(ctx, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("overlapping probe") }))
	_, _, snapshot, err := s.state(ctx, true)
	require.NoError(t, err)
	require.Contains(t, snapshot.AttemptedAt, int64(1))
	require.Empty(t, snapshot.Samples)
	config.Enabled = false
	require.NoError(t, s.Save(ctx, config))
	select {
	case <-canceled:
	case <-time.After(6 * time.Second):
		t.Fatal("configuration change did not cancel upstream")
	}
	<-done
	config.Enabled = true
	require.NoError(t, s.Save(ctx, config))
	groups, err := s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Nil(t, groups[0].Sample)
}

func TestChannelDetectionCaptureAndSVG(t *testing.T) {
	s := newDetectionFixture(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte(strings.Repeat("x", detectionCaptureLimit+1)))
		require.ErrorIs(t, err, errDetectionResponse)
		require.ErrorIs(t, r.Context().Err(), context.Canceled)
	})
	_, err := s.probe(context.Background(), h, s.cfg.Groups[0], "capture test", 128, detectionTestCurrent, &detectionRoundBudget{})
	require.ErrorIs(t, err, errDetectionResponse)
	require.Contains(t, sanitizeDetectionSVG("```svg\n"+testDetectionSVG+"\n```"), "<circle")
	require.Contains(t, sanitizeDetectionSVG(`<svg><circle cx="25%" cy="7e1" r=".5"/><polyline points="0,0 10,10 0,20"/></svg>`), "<polyline")
	for _, value := range []string{
		`<svg><script>alert(1)</script></svg>`,
		`<svg><image href="https://example.invalid"/></svg>`,
		`<svg><path d="M 0 0" onclick="alert(1)"/></svg>`,
		`<svg><circle r="1" fill="url(https://example.invalid)"/></svg>`,
		`<svg><rect width="1" style="fill:url(//example.invalid)"/></svg>`,
		`<svg><foreignObject><p>bad</p></foreignObject></svg>`,
		`<svg><circle xmlns="https://example.invalid" r="1"/></svg>`,
		`<!DOCTYPE svg [<!ENTITY x SYSTEM "file:///secret">]><svg><g/></svg>`,
		`<svg><circle r="9999999"/></svg>`,
		`<svg><circle r="1,2"/></svg>`,
		`<svg><circle r="1 2"/></svg>`,
		`<svg><circle r="1..2"/></svg>`,
		`<svg><circle r="-1"/></svg>`,
		`<svg><rect width="50%%"/></svg>`,
		testDetectionSVG + testDetectionSVG,
	} {
		require.Empty(t, sanitizeDetectionSVG(value), value)
	}
}

func detectionTestCurrent(context.Context) bool { return true }

func TestChannelDetectionRevalidatesBeforeEveryDispatch(t *testing.T) {
	for _, kind := range []string{"manual allowlist", "automatic allowlist", "disabled group", "lease"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s := newDetectionFixture(t)
			s.groupRows[1].ModelAllowlist.Enabled = kind == "automatic allowlist"
			require.NoError(t, s.Save(ctx, s.cfg))
			attempts := 0
			gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				require.Equal(t, 1, attempts, "a second billable dispatch was made")
				w.WriteHeader(http.StatusBadGateway)
				switch kind {
				case "manual allowlist", "automatic allowlist":
					s.groupRows[1].ModelAllowlist.Models = nil
				case "disabled group":
					s.groupRows[1].Status = StatusDisabled
				case "lease":
					s.storage.mu.Lock()
					s.storage.owner = "successor"
					s.storage.mu.Unlock()
				}
			})
			s.tick(ctx, gateway)
			require.Equal(t, 1, attempts)
			_, _, latest, err := s.state(ctx, true)
			require.NoError(t, err)
			require.Empty(t, latest.Samples)
			require.Contains(t, latest.AttemptedAt, int64(1))
		})
	}
}

func TestChannelDetectionRevalidatesAfterGatewayAuth(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	require.NoError(t, s.Save(ctx, s.cfg))
	s.groupRows[1].ModelAllowlist.Enabled = false
	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.groupRows[1].ModelAllowlist.Models = nil
		require.False(t, ChannelDetectionDispatchAllowed(r.Context()))
		require.ErrorIs(t, r.Context().Err(), context.Canceled)
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := s.probe(ctx, gateway, s.cfg.Groups[0], "queued", 128, func(ctx context.Context) bool {
		_, err := s.probeGroup(ctx, s.cfg.Groups[0])
		return err == nil
	}, &detectionRoundBudget{})
	require.ErrorIs(t, err, errDetectionDispatch)
	require.True(t, ChannelDetectionDispatchAllowed(ctx), "ordinary requests have no internal marker")
}

// detectionLoopbackUpstream runs the real request builder and converter, and
// verifies the wire cap at the same interface used by the production transport.
type detectionLoopbackUpstream struct {
	client *http.Client
	url    string
}

func (u *detectionLoopbackUpstream) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	if err := ReserveChannelDetectionSend(req, accountID); err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	parsed, err := http.NewRequest(req.Method, u.url+req.URL.Path, nil)
	if err != nil {
		return nil, err
	}
	req.URL = parsed.URL
	return u.client.Do(req)
}

func (u *detectionLoopbackUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

func TestChannelDetectionOpenAIAndAnthropicWireForwarding(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformAnthropic, PlatformGemini} {
		t.Run(platform, func(t *testing.T) {
			s := newDetectionFixture(t)
			group := s.groupRows[1]
			group.Platform = platform
			model := s.cfg.Groups[0].Model
			var calls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if got := gjson.GetBytes(body, "model").String(); got != "upstream-model" {
					t.Errorf("mapped model = %q", got)
				}
				if platform == PlatformOpenAI {
					if r.URL.Path != "/v1/responses" || gjson.GetBytes(body, "max_output_tokens").Int() != 768 {
						t.Errorf("unexpected native wire request: %s %s", r.URL.Path, body)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_probe","status":"completed","model":"upstream-model","usage":{"input_tokens":10,"output_tokens":5},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
					return
				}
				if r.URL.Path != "/v1/messages" || gjson.GetBytes(body, "max_tokens").Int() != 768 {
					t.Errorf("unexpected Anthropic wire request: %s %s", r.URL.Path, body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				events := []string{
					`{"type":"message_start","message":{"id":"msg_probe","model":"upstream-model","content":[],"usage":{"input_tokens":10}}}`,
					`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
					`{"type":"content_block_stop","index":0}`,
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
					`{"type":"message_stop"}`,
				}
				for _, event := range events {
					_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", gjson.Get(event, "type").String(), event)
				}
			}))
			defer server.Close()

			upstream := &detectionLoopbackUpstream{client: server.Client(), url: server.URL}
			account := detectionTestAccount(platform)
			account.Credentials = map[string]any{
				"api_key": "fixture-upstream-key", "model_mapping": map[string]any{model: "upstream-model"},
			}
			repo := &detectionAccountRepo{fresh: account}
			if platform == PlatformOpenAI {
				account.Extra = map[string]any{
					openai_compat.ExtraKeyResponsesMode:      string(openai_compat.ResponsesSupportModeAuto),
					openai_compat.ExtraKeyResponsesSupported: true,
				}
			}
			gateway := gin.New()
			gateway.POST("/v1/responses", func(c *gin.Context) {
				key, ok := ChannelDetectionIngress(c.Request)
				require.True(t, ok)
				require.Empty(t, key.Key)
				require.Zero(t, key.ID)
				body, err := io.ReadAll(c.Request.Body)
				require.NoError(t, err)
				requestmodel.ResetRequestBody(c.Request, body)
				ctx := c.Request.Context()
				if platform == PlatformOpenAI {
					svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, accountRepo: repo}
					_, err = svc.Forward(ctx, c, account, body)
				} else {
					svc := &GatewayService{cfg: &config.Config{}, httpUpstream: upstream, accountRepo: repo}
					_, err = svc.ForwardAsResponses(ctx, c, account, body, nil)
				}
				if err != nil {
					c.Status(http.StatusBadGateway)
					if platform != PlatformGemini {
						t.Errorf("forwarding error: %v", err)
					}
				}
			})
			budget := &detectionRoundBudget{reserve: func(context.Context, *Group) (bool, error) { return true, nil }}
			answer, err := s.probe(context.Background(), gateway, s.cfg.Groups[0], "hello", 768, detectionTestCurrent, budget)
			if platform == PlatformGemini {
				require.ErrorIs(t, err, errDetectionOutputLimit)
				require.Zero(t, calls)
				return
			}
			require.NoError(t, err)
			require.Equal(t, "ok", answer)
			require.Equal(t, 1, calls)
			require.Equal(t, 1, budget.sends)
		})
	}
}
