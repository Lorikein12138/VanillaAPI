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

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/modeltrace/trace"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type detectionMemorySettings struct {
	mu     sync.Mutex
	values map[string]string
	owner  string
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

type detectionTestKeys struct {
	rows        map[int64]*APIKey
	secretReads int
}

func (k *detectionTestKeys) GetChannelDetectionKey(_ context.Context, id int64, includeSecret bool) (*APIKey, error) {
	if key := k.rows[id]; key != nil {
		copy := *key
		if includeSecret {
			k.secretReads++
		} else {
			copy.Key = ""
		}
		return &copy, nil
	}
	return nil, ErrAPIKeyNotFound
}

type detectionFixture struct {
	*ChannelDetection
	cfg       ChannelDetectionConfig
	storage   *detectionMemorySettings
	groupRows detectionTestGroups
	keyRepo   *detectionTestKeys
}

func newDetectionFixture(t *testing.T) *detectionFixture {
	t.Helper()
	settings := &detectionMemorySettings{values: map[string]string{SettingKeyChannelDetectionFeature: "enable-one"}}
	groups := detectionTestGroups{
		1: {ID: 1, Name: "First", Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6.2-sol"}}},
		2: {ID: 2, Name: "Second", Status: StatusActive, ModelAllowlist: GroupModelAllowlist{Models: []string{"claude-sonnet-5", "*"}}},
	}
	group1, group2 := int64(1), int64(2)
	keys := &detectionTestKeys{rows: map[int64]*APIKey{
		11: {ID: 11, Key: "test-only-first", Status: StatusActive, GroupID: &group1, Group: groups[1], Quota: 1, User: &User{Status: StatusActive}},
		22: {ID: 22, Key: "test-only-second", Status: StatusActive, GroupID: &group2, Group: groups[2], RateLimit1d: 1, User: &User{Status: StatusActive}},
	}}
	cfg := ChannelDetectionConfig{Enabled: true, IntervalMinutes: 60, Groups: []ChannelDetectionSelection{
		{GroupID: group1, Model: "gpt-6.2-sol", APIKeyID: 11},
		{GroupID: group2, Model: "claude-sonnet-5", APIKeyID: 22},
	}}
	svc := &ChannelDetection{
		settings: settings, groups: groups, keys: keys, coordinator: settings, runtime: &config.Config{RunMode: config.RunModeStandard},
	}
	return &detectionFixture{ChannelDetection: svc, cfg: cfg, storage: settings, groupRows: groups, keyRepo: keys}
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
		{GroupID: 2, Model: "*", APIKeyID: 22},
		{GroupID: 1, Model: "claude-sonnet-5", APIKeyID: 11},
	} {
		require.ErrorIs(t, s.Save(ctx, ChannelDetectionConfig{Enabled: true, IntervalMinutes: 60, Groups: []ChannelDetectionSelection{invalid}}), errDetectionModel)
	}
	wrongBinding := ChannelDetectionSelection{GroupID: 1, Model: "gpt-6.2-sol", APIKeyID: 22}
	require.ErrorIs(t, s.Save(ctx, ChannelDetectionConfig{Enabled: true, IntervalMinutes: 60, Groups: []ChannelDetectionSelection{wrongBinding}}), errDetectionKey)
	require.NoError(t, s.Save(ctx, config))

	s.groupRows[1].ModelAllowlist.Models = nil
	delete(s.keyRepo.rows, 22)
	config.Enabled = false
	require.NoError(t, s.Save(ctx, config))
	saved, err := s.Config(ctx)
	require.NoError(t, err)
	require.Equal(t, config, saved)
	config.Enabled = true
	require.ErrorIs(t, s.Save(ctx, config), errDetectionModel)
	s.groupRows[1].ModelAllowlist.Models = []string{"gpt-6.2-sol"}
	require.ErrorIs(t, s.Save(ctx, config), errDetectionKey)
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
		require.Equal(t, "Bearer test-only-first", c.GetHeader("Authorization"))
		require.True(t, ChannelDetectionBindingMatches(c.Request.Context(), s.keyRepo.rows[11]))
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
	secretReads := s.keyRepo.secretReads
	groups, err := s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	require.NotNil(t, groups[0].Sample)
	require.Equal(t, ChannelDetectionCounts{Passed: 1, Failed: 1, Errors: 1}, groups[0].Sample.Reasoning)
	require.Equal(t, ChannelDetectionCounts{Passed: 1, Errors: 1, Unattributed: 1}, groups[0].Sample.Fingerprint)
	require.Equal(t, "generated", groups[0].Sample.Pelican)
	require.Contains(t, groups[0].Sample.SVG, "<circle")
	require.Nil(t, groups[1].Sample)
	require.Equal(t, secretReads, s.keyRepo.secretReads)
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
	key := s.keyRepo.rows[11]
	originalBinding := key.GroupID
	for _, binding := range []*int64{nil, &s.groupRows[2].ID} {
		key.GroupID = binding
		groups, err = s.PublicGroups(ctx)
		require.NoError(t, err)
		require.Equal(t, []ChannelDetectionPublicGroup{{Name: "First", Model: "gpt-6.2-sol"}}, groups)
	}
	key.GroupID = originalBinding
	expiredAt := time.Now().Add(-time.Hour)
	key.ExpiresAt = &expiredAt
	groups, err = s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelDetectionPublicGroup{{Name: "First", Model: "gpt-6.2-sol"}}, groups)
	key.ExpiresAt = nil
	require.NoError(t, s.settings.Set(ctx, SettingKeyChannelDetectionFeature, ""))
	_, err = s.PublicGroups(ctx)
	require.ErrorIs(t, err, errDetectionDisabled)
	require.NoError(t, s.settings.Set(ctx, SettingKeyChannelDetectionFeature, "enable-two"))
	groups, err = s.PublicGroups(ctx)
	require.NoError(t, err)
	require.Equal(t, []ChannelDetectionPublicGroup{{Name: "First", Model: "gpt-6.2-sol"}}, groups)
}

func TestChannelDetectionBindingRechecksDatabaseAfterAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*APIKey, *APIKey)
	}{
		{"auth binding changed", func(auth, _ *APIKey) { id := int64(2); auth.GroupID = &id }},
		{"cached auth and database binding changed", func(_, stored *APIKey) { id := int64(2); stored.GroupID = &id }},
		{"cached auth and database key rotated", func(_, stored *APIKey) { stored.Key = "rotated" }},
		{"cached auth uses unlimited metadata", func(auth, _ *APIKey) { auth.Quota = 0 }},
		{"limit changed while authenticating", func(_, stored *APIKey) { stored.Quota = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDetectionFixture(t)
			keys := s.keyRepo.rows
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth := *keys[11]
				stored := *keys[11]
				tc.change(&auth, &stored)
				keys[11] = &stored
				require.False(t, ChannelDetectionBindingMatches(r.Context(), &auth))
				// A public caller's headers never create an internal marker.
				r.Header.Set("X-Expected-Group-ID", "1")
				require.True(t, ChannelDetectionBindingMatches(context.Background(), &auth))
				w.WriteHeader(http.StatusForbidden)
			})
			_, err := s.probe(context.Background(), h, s.cfg.Groups[0], "test", 128, detectionTestCurrent)
			require.ErrorIs(t, err, errDetectionResponse)
		})
	}
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
	_, err := s.probe(context.Background(), h, s.cfg.Groups[0], "capture test", 128, detectionTestCurrent)
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

func TestChannelDetectionUsesOnlyLimitsBilledInCurrentMode(t *testing.T) {
	for _, tc := range []struct {
		name, mode     string
		windowsEnabled bool
		windows        bool
		quota          float64
		quotaUsed      float64
		windowUsed     float64
		want           bool
	}{
		{name: "simple total quota only", mode: config.RunModeSimple, windowsEnabled: true, quota: 1},
		{name: "simple monetary windows disabled", mode: config.RunModeSimple, windows: true, quota: 1},
		{name: "simple billed window", mode: config.RunModeSimple, windowsEnabled: true, windows: true, want: true},
		{name: "simple window ignores lifetime quota", mode: config.RunModeSimple, windowsEnabled: true, windows: true, quota: 1, quotaUsed: 1, want: true},
		{name: "simple window reached", mode: config.RunModeSimple, windowsEnabled: true, windows: true, windowUsed: 1},
		{name: "standard only quota", mode: config.RunModeStandard, quota: 1, want: true},
		{name: "standard only window", mode: config.RunModeStandard, windows: true, want: true},
		{name: "standard quota exhausted", mode: config.RunModeStandard, windows: true, quota: 1, quotaUsed: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newDetectionFixture(t)
			s.runtime = &config.Config{RunMode: tc.mode, SimpleModeKeyRateLimitEnabled: tc.windowsEnabled}
			key := s.keyRepo.rows[11]
			key.Quota, key.QuotaUsed = tc.quota, tc.quotaUsed
			if tc.windows {
				now := time.Now()
				key.RateLimit5h, key.Usage5h, key.Window5hStart = 1, tc.windowUsed, &now
			}
			s.cfg.Groups = s.cfg.Groups[:1]
			err := s.Save(ctx, s.cfg)
			if tc.want {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errDetectionKey)
			}
			_, err = s.probeKey(ctx, s.cfg.Groups[0], true)
			if tc.want {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, errDetectionKey)
			}
		})
	}
}

func TestChannelDetectionRevalidatesBeforeEveryDispatch(t *testing.T) {
	for _, kind := range []string{"manual allowlist", "automatic allowlist", "key", "lease"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			s := newDetectionFixture(t)
			s.groupRows[1].ModelAllowlist.Enabled = kind == "automatic allowlist"
			require.NoError(t, s.Save(ctx, s.cfg))
			attempts := 0
			gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				require.Equal(t, 1, attempts, "a second billable dispatch was made")
				require.True(t, ChannelDetectionBindingMatches(r.Context(), s.keyRepo.rows[11]))
				w.WriteHeader(http.StatusBadGateway)
				switch kind {
				case "manual allowlist", "automatic allowlist":
					s.groupRows[1].ModelAllowlist.Models = nil
				case "key":
					s.keyRepo.rows[11].GroupID = nil
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
			if kind == "lease" {
				require.Empty(t, latest.Samples)
				require.Contains(t, latest.AttemptedAt, int64(1))
				return
			}
			require.NotNil(t, latest.Samples[1])
			require.Equal(t, ChannelDetectionCounts{Errors: 1, Untested: 2}, latest.Samples[1].Reasoning)
		})
	}
}

func TestChannelDetectionRevalidatesAfterGatewayAuth(t *testing.T) {
	ctx := context.Background()
	s := newDetectionFixture(t)
	require.NoError(t, s.Save(ctx, s.cfg))
	s.groupRows[1].ModelAllowlist.Enabled = false
	gateway := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, ChannelDetectionBindingMatches(r.Context(), s.keyRepo.rows[11]))
		s.groupRows[1].ModelAllowlist.Models = nil
		require.False(t, ChannelDetectionDispatchAllowed(r.Context()))
		require.ErrorIs(t, r.Context().Err(), context.Canceled)
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := s.probe(ctx, gateway, s.cfg.Groups[0], "queued", 128, detectionTestCurrent)
	require.ErrorIs(t, err, errDetectionResponse)
	require.True(t, ChannelDetectionDispatchAllowed(ctx), "ordinary requests have no internal marker")
}
