package service

import (
	"context"
	"io"
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

type adaptiveStreamDisconnectedWriter struct{ gin.ResponseWriter }

func (adaptiveStreamDisconnectedWriter) WriteString(string) (int, error) { return 0, io.ErrClosedPipe }

type adaptiveStreamTestFrame struct {
	step   time.Duration
	line   string
	cancel bool
}

type adaptiveStreamTestReader struct {
	now    *time.Time
	cancel context.CancelFunc
	frames []adaptiveStreamTestFrame
}

func (r *adaptiveStreamTestReader) Read(p []byte) (int, error) {
	if len(r.frames) == 0 {
		return 0, io.EOF
	}
	frame := r.frames[0]
	r.frames = r.frames[1:]
	*r.now = r.now.Add(frame.step)
	if frame.cancel {
		r.cancel()
	}
	return copy(p, frame.line), nil
}

type adaptiveStreamTestUpstream struct {
	httpUpstreamRecorder
	secondHTTP bool
}

func (u *adaptiveStreamTestUpstream) Do(req *http.Request, proxyURL string, id int64, concurrency int) (*http.Response, error) {
	if u.secondHTTP {
		return adaptiveHTTP(req.Context(), func() (*http.Response, error) {
			return u.httpUpstreamRecorder.Do(req, proxyURL, id, concurrency)
		})
	}
	return u.httpUpstreamRecorder.Do(req, proxyURL, id, concurrency)
}

func TestGatewayAdaptiveStreamHealthReadsNormalizedEffort(t *testing.T) {
	for _, tc := range []struct {
		name, platform, want string
		group                *Group
	}{
		{name: "native-glm-xhigh-to-max", platform: PlatformZhipu, want: "max"},
		{name: "openai-group-policy-before-glm-normalization", platform: PlatformOpenAI, want: "high", group: &Group{
			Platform:                PlatformOpenAI,
			ReasoningEffortMappings: []ReasoningEffortMapping{{From: "xhigh", To: "medium"}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const model = "glm-5.2"
			const body = "{\"model\":\"glm-5.2\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"reasoning_effort\":\"xhigh\",\"stream\":true}"
			account := adaptiveTestAccount(801, tc.platform, 1)
			account.Credentials = map[string]any{"api_protocol": APIProtocolChatCompletions, "api_key": "test-key", "base_url": "https://upstream.example"}
			account.Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
			now := time.Now()
			telemetry := &adaptiveTelemetry{}
			telemetry.streamHealth.clock = func() time.Time { return now }

			newRequest := func() (context.Context, []byte) {
				ctx := streamHealthContext(context.Background(), telemetry, 72, body)
				forward := []byte(body)
				if tc.group != nil {
					var changed bool
					var err error
					forward, changed, err = ApplyOpenAIReasoningEffortPolicy(forward, tc.group.MaxReasoningEffort,
						tc.group.ReasoningEffortMappings, tc.group.MaxReasoningEffortOverLimit)
					require.NoError(t, err)
					require.True(t, changed)
					ctx = SetGatewayAdaptiveChatPolicyInput(ctx, forward)
				}
				route, ok := gatewayAdaptiveRoute(ctx)
				require.True(t, ok)
				require.True(t, strings.HasPrefix(route.effort, "xhigh/"), "legacy effort remains ingress effort")
				return ctx, forward
			}

			ctx, forward := newRequest()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
			upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(&adaptiveStreamTestReader{now: &now, frames: []adaptiveStreamTestFrame{
					{step: 12 * time.Second, line: "data: {\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"},
				}})}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
			result, err := svc.ForwardAsChatCompletions(ctx, c, &account, forward, "", "")
			require.NoError(t, err)
			require.Equal(t, "glm-5.2", result.UpstreamModel)
			require.Equal(t, tc.want, gjson.GetBytes(upstream.lastBody, "reasoning_effort").String())
			require.EqualValues(t, 1, telemetry.streamHealth.counters[streamSampled].Load())

			next, _ := newRequest()
			route, _ := gatewayAdaptiveRoute(next)
			key, ok := adaptiveStreamCandidateKey(next, route, &account, model)
			require.True(t, ok)
			require.True(t, strings.HasPrefix(key.sample.route.effort, tc.want+"/"))
			require.Len(t, telemetry.streamHealth.series[key], 1)
			require.Equal(t, 12*time.Second, telemetry.streamHealth.series[key][0].latency)
		})
	}

	t.Run("uncertain-outbound-token-clamp", func(t *testing.T) {
		body := "{\"max_tokens\":100000," + streamHealthBody[1:]
		account := adaptiveTestAccount(802, PlatformOpenAI, 1)
		account.Credentials = map[string]any{"api_key": "test-key", "base_url": "https://ollama.com"}
		account.Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
		ctx := streamHealthContext(context.Background(), &adaptiveTelemetry{}, 72, body)
		route, _ := gatewayAdaptiveRoute(ctx)
		_, ok := adaptiveStreamCandidateKey(ctx, route, &account, streamHealthPublicModel)
		require.False(t, ok)
	})
}

func TestGatewayAdaptiveStreamHealthRawChatObservation(t *testing.T) {
	for _, name := range []string{"protected-success", "canceled-drain", "client-disconnect", "truncated", "role-only", "double-http", "http-redirect", "model-conflict",
		"tier-conflict", "tool-first", "unexpected-late-tool", "escaped-late-tool", "reasoning-first", "unsupported-media", "normalized-effort", "switch-disabled",
		"failed-finish-eof", "error-event-then-done", "error-payload-then-done", "interim-usage-truncated", "usage-before-more-output", "empty-usage-eof", "success-finish-eof", "final-usage-eof", "multi-partial", "multi-complete", "multi-duplicate", "multi-out-of-order", "multi-invalid-index", "multi-missing-index", "multi-unknown-count", "multi-over-limit", "multi-unknown-done", "multi-unknown-usage"} {
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			group := int64(72)
			account, _, _ := streamHealthFixture(group)
			now := time.Now().Truncate(time.Microsecond)
			telemetry := &adaptiveTelemetry{}
			telemetry.streamHealth.clock = func() time.Time { return now }
			body := streamHealthBody
			if name == "unsupported-media" {
				body = strings.Replace(body, "\"hello\"", "[{\"type\":\"image_url\",\"image_url\":{\"url\":\"https://example.org/image.png\"}}]", 1)
			}
			if name == "normalized-effort" {
				body = "{\"reasoning_effort\":\"high\"," + body[1:]
			}
			if strings.HasPrefix(name, "multi-") {
				count := "2"
				switch name {
				case "multi-unknown-count", "multi-unknown-done", "multi-unknown-usage":
					count = "null"
				case "multi-over-limit":
					count = "65"
				}
				body = "{\"n\":" + count + "," + body[1:]
			}
			ctx := protectAdaptiveSelection(streamHealthContext(parent, telemetry, group, body))
			if name == "switch-disabled" {
				ctx = parent
			}

			prefix := "data: {\"model\":\"" + streamHealthEconomyModel + "\",\"choices\":[{\"index\":0,\"delta\":"
			frames := []adaptiveStreamTestFrame{
				{step: time.Millisecond, line: prefix + "{\"role\":\"assistant\"}}]}\n\n"},
				{step: time.Millisecond, line: ": upstream heartbeat\n\n"},
				{step: 39800*time.Millisecond - 2*time.Millisecond, line: prefix + "{\"content\":\"hello\"}}]}\n\n"},
				{step: time.Second, line: "data: [DONE]\n\ndata: [DONE]\n\n"},
			}
			want := streamSampled
			switch name {
			case "multi-partial", "multi-complete", "multi-duplicate", "multi-out-of-order", "multi-invalid-index", "multi-missing-index", "multi-unknown-count", "multi-over-limit", "multi-unknown-done", "multi-unknown-usage":
				stop := func(index string) string {
					return "data: {\"choices\":[{\"index\":" + index + ",\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
				}
				frames[2].line = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"}},{\"index\":1,\"delta\":{\"content\":\"second\"}}]}\n\n"
				more := "data: {\"choices\":[{\"index\":1,\"delta\":{\"content\":\"more\"},\"finish_reason\":null}]}\n\n"
				frames[3].line = stop("0") + more
				switch name {
				case "multi-complete":
					frames[3].line += stop("1")
				case "multi-out-of-order":
					frames[3].line = stop("1") + stop("0") + stop("1")
				case "multi-duplicate":
					frames[3].line += stop("0")
					want = streamNoTerminal
				case "multi-invalid-index":
					frames[3].line += stop("2") + stop("1")
					want = streamNoTerminal
				case "multi-missing-index":
					frames[3].line += strings.Replace(stop("1"), "\"index\":1,", "", 1)
					want = streamNoTerminal
				case "multi-unknown-count", "multi-over-limit":
					frames[3].line += stop("1")
					want = streamNoTerminal
				case "multi-unknown-done":
					frames[3].line += "data: [DONE]\n\n"
				case "multi-unknown-usage":
					frames[3].line += "data: {\"choices\":[],\"usage\":{\"completion_tokens\":2}}\n\n"
				default:
					want = streamNoTerminal
				}
			case "failed-finish-eof":
				frames[3].line = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"error\"}]}\n\n"
				want = streamNoTerminal
			case "error-event-then-done":
				frames[3].line = "event: error\ndata: {\"message\":\"failed\"}\n\n" + frames[3].line
				want = streamNoTerminal
			case "error-payload-then-done":
				frames[3].line = "data: {\"error\":{\"message\":\"failed\"}}\n\n" + frames[3].line
				want = streamNoTerminal
			case "interim-usage-truncated":
				frames[2].line = strings.TrimSuffix(frames[2].line, "}\n\n") + ",\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n"
				frames = frames[:3]
				want = streamNoTerminal
			case "usage-before-more-output":
				frames[3].line = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" + frames[2].line
				want = streamNoTerminal
			case "empty-usage-eof":
				frames[3].line = "data: {\"choices\":[],\"usage\":{}}\n\n"
				want = streamNoTerminal
			case "success-finish-eof":
				frames[3].line = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
			case "final-usage-eof":
				frames[3].line = "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n"
			case "canceled-drain":
				frames[2].cancel, want = true, streamCanceled
			case "client-disconnect":
				want = streamCanceled
			case "truncated":
				frames, want = frames[:3], streamNoTerminal
			case "role-only":
				frames, want = append(frames[:2], frames[3]), streamNoToken
			case "double-http", "http-redirect":
				want = streamMultiAttempt
			case "model-conflict":
				frames[2].line = strings.Replace(frames[2].line, streamHealthEconomyModel, streamHealthPublicModel, 1)
				want = streamMismatch
			case "tier-conflict":
				frames[2].line = strings.Replace(frames[2].line, "{\"model\":", "{\"service_tier\":\"priority\",\"model\":", 1)
				want = streamMismatch
			case "tool-first":
				frames[1].line = prefix + "{\"tool_calls\":[{\"id\":\"call_1\",\"function\":{\"name\":\"do_work\"}}]}}]}\n\n"
				frames[2].line = prefix + "{\"tool_calls\":[{\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n"
			case "unexpected-late-tool", "escaped-late-tool":
				frames[3].line = prefix + "{\"tool_calls\":[{\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" + frames[3].line
				if name == "escaped-late-tool" {
					frames[3].line = strings.Replace(frames[3].line, "arguments", "argum\\u0065nts", 1)
				}
			case "reasoning-first":
				frames[2].line = prefix + "{\"reasoning\":\"thinking\"}}]}\n\n"
			case "unsupported-media":
				want = streamUnsupported
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			if name == "client-disconnect" {
				c.Writer = adaptiveStreamDisconnectedWriter{c.Writer}
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
			upstream := &adaptiveStreamTestUpstream{secondHTTP: name == "double-http"}
			upstream.resp = &http.Response{StatusCode: http.StatusOK,
				Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body:   io.NopCloser(&adaptiveStreamTestReader{now: &now, cancel: cancel, frames: frames})}
			if name == "http-redirect" {
				upstream.resp.Request = &http.Request{Response: &http.Response{StatusCode: http.StatusTemporaryRedirect}}
			}
			svc := &OpenAIGatewayService{cfg: &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}}, httpUpstream: upstream}
			result, err := svc.ForwardAsChatCompletions(ctx, c, &account, []byte(body), "", "")
			if name == "truncated" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, telemetry.series, "protected requests retain the old sampling guard")
			if name == "switch-disabled" {
				for i := range streamCounterCount {
					require.Zero(t, telemetry.streamHealth.counters[i].Load())
				}
				return
			}
			switch name {
			case "failed-finish-eof":
				require.Contains(t, rec.Body.String(), "\"finish_reason\":\"error\"")
			case "error-event-then-done", "error-payload-then-done":
				require.Contains(t, rec.Body.String(), "data: [DONE]")
			case "interim-usage-truncated", "final-usage-eof":
				require.Equal(t, 9, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
			}
			if want == streamNoTerminal && name != "truncated" {
				require.NotNil(t, result.FirstTokenMs)
				require.Less(t, *result.FirstTokenMs, 39800)
			}
			require.EqualValues(t, 1, telemetry.streamHealth.counters[want].Load())
			if want != streamSampled {
				require.Empty(t, telemetry.streamHealth.series)
				return
			}
			require.NotNil(t, result.FirstTokenMs)
			require.Less(t, *result.FirstTokenMs, 39800)
			require.Len(t, telemetry.streamHealth.series, 1)
			key := streamHealthKey(t, ctx, &account)
			if name == "tool-first" || name == "unexpected-late-tool" || name == "escaped-late-tool" {
				require.Empty(t, telemetry.streamHealth.series[key])
				key.work |= adaptiveWorkUnexpectedTools
			}
			samples := telemetry.streamHealth.series[key]
			require.Len(t, samples, 1)
			require.Equal(t, 39800*time.Millisecond, samples[0].latency)
		})
	}
}
