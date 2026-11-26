package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	AdaptiveOpenAIResponsesFirstOutputTimeout = "first_output_timeout"
	adaptiveOpenAIResponsesDefaultSeconds     = 30
)

var (
	ErrAdaptiveOpenAIResponsesReplayUnavailable  = errors.New("native Responses replay unavailable")
	ErrAdaptiveOpenAIResponsesClientDisconnected = errors.New("client disconnected during native Responses streaming")
)

type adaptiveOpenAIResponsesFirstOutputKey struct{}

type adaptiveOpenAIResponsesFirstOutput struct {
	enabled    bool
	replayable bool
	replaying  bool
}

// WithAdaptiveOpenAIResponsesFirstOutput captures the original POST before any
// gateway or account rewrites. Callers restrict it to bare Responses HTTP SSE.
func WithAdaptiveOpenAIResponsesFirstOutput(ctx context.Context, body []byte, headers http.Header) context.Context {
	if ingress, _ := ctx.Value(adaptiveIngressKey{}).(*adaptiveIngress); ingress == nil ||
		gjson.GetBytes(body, "stream").Type != gjson.True || !adaptiveResponsesText(body) {
		return ctx
	}
	return context.WithValue(ctx, adaptiveOpenAIResponsesFirstOutputKey{}, adaptiveOpenAIResponsesFirstOutput{
		enabled: true, replayable: adaptiveResponsesReplayBody(body) && adaptiveResponsesReplayIngressHeaders(headers),
	})
}

func WithAdaptiveOpenAIResponsesReplay(ctx context.Context) context.Context {
	policy := adaptiveResponsesFirstOutputPolicy(ctx)
	policy.replaying = true
	return context.WithValue(ctx, adaptiveOpenAIResponsesFirstOutputKey{}, policy)
}

func adaptiveResponsesFirstOutputPolicy(ctx context.Context) adaptiveOpenAIResponsesFirstOutput {
	policy, _ := ctx.Value(adaptiveOpenAIResponsesFirstOutputKey{}).(adaptiveOpenAIResponsesFirstOutput)
	return policy
}

func IsAdaptiveOpenAIResponsesFirstOutput(ctx context.Context) bool {
	return adaptiveResponsesFirstOutputPolicy(ctx).enabled
}

func (s *OpenAIGatewayService) CanReplayAdaptiveOpenAIResponses(ctx context.Context, account *Account, body []byte) bool {
	return account != nil && account.IsOpenAIApiKey() && !account.IsOpenAIPassthroughEnabled() &&
		!adaptiveMayAddNativeSession(account) && len(account.GetHeaderOverrides()) == 0 &&
		resolveOpenAIWSDecisionByClientTransport(s.getOpenAIWSProtocolResolver().Resolve(account), OpenAIClientTransportHTTP).Transport == OpenAIUpstreamTransportHTTPSSE &&
		s.resolveUpstreamProtocolFor(ctx, account, APIProtocolResponses, upstreamRoutingModel(account, body, "")) == APIProtocolResponses
}

func adaptiveResponsesText(body []byte) bool {
	payload := gjson.ParseBytes(body)
	for _, field := range []string{"audio", "image", "file", "input_audio", "input_image", "input_file"} {
		if payload.Get(field).Exists() && payload.Get(field).Type != gjson.Null {
			return false
		}
	}
	if modalities := payload.Get("modalities"); modalities.Exists() {
		if !modalities.IsArray() {
			return false
		}
		for _, modality := range modalities.Array() {
			if modality.String() != "text" {
				return false
			}
		}
	}
	for _, field := range []string{"input", "messages", "tools"} {
		items := payload.Get(field)
		if adaptiveResponsesMediaItem(items) {
			return false
		}
		for _, item := range items.Array() {
			if adaptiveResponsesMediaItem(item) {
				return false
			}
			for _, part := range item.Get("content").Array() {
				if adaptiveResponsesMediaItem(part) {
					return false
				}
			}
		}
		for _, part := range items.Get("content").Array() {
			if adaptiveResponsesMediaItem(part) {
				return false
			}
		}
	}
	return true
}

func adaptiveResponsesMediaItem(item gjson.Result) bool {
	switch item.Get("type").String() {
	case "input_image", "image_url", "image", "input_audio", "audio", "input_file", "file",
		"input_video", "video", "image_generation", "output_image", "output_audio":
		return true
	}
	return false
}

func adaptiveResponsesReplayBody(body []byte) bool {
	payload := gjson.ParseBytes(body)
	if payload.Get("store").Type != gjson.False || payload.Get("stream").Type != gjson.True {
		return false
	}
	if !adaptiveResponsesReplayObject(payload, func(key string, value gjson.Result) bool {
		switch key {
		case "model", "instructions", "service_tier":
			return value.Type == gjson.String
		case "input", "store", "stream":
			return true
		case "max_output_tokens", "temperature", "top_p":
			return value.Type == gjson.Number
		case "reasoning":
			return adaptiveResponsesReplayObject(value, func(key string, value gjson.Result) bool {
				return key == "effort" && value.Type == gjson.String
			})
		default:
			return false
		}
	}) {
		return false
	}
	input := payload.Get("input")
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String()) != ""
	}
	items := input.Array()
	if !input.IsArray() || len(items) == 0 {
		return false
	}
	hasUser := false
	for _, item := range items {
		role := item.Get("role").String()
		switch role {
		case "user":
			hasUser = true
		case "assistant", "developer", "system":
		default:
			return false
		}
		if !adaptiveResponsesReplayObject(item, func(key string, value gjson.Result) bool {
			switch key {
			case "type":
				return value.String() == "message"
			case "role", "content":
				return true
			}
			return false
		}) {
			return false
		}
		content := item.Get("content")
		if content.Type == gjson.String && strings.TrimSpace(content.String()) != "" {
			continue
		}
		parts := content.Array()
		if !content.IsArray() || len(parts) == 0 {
			return false
		}
		for _, part := range parts {
			wantType := "input_text"
			if role == "assistant" {
				wantType = "output_text"
			}
			if part.Get("type").String() != wantType || part.Get("text").Type != gjson.String ||
				strings.TrimSpace(part.Get("text").String()) == "" ||
				!adaptiveResponsesReplayObject(part, func(key string, _ gjson.Result) bool { return key == "type" || key == "text" }) {
				return false
			}
		}
	}
	return hasUser
}

func adaptiveResponsesReplayObject(object gjson.Result, allowed func(string, gjson.Result) bool) bool {
	if !object.IsObject() {
		return false
	}
	seen := make(map[string]bool)
	valid := true
	object.ForEach(func(key, value gjson.Result) bool {
		name := key.String()
		valid = !seen[name] && allowed(name, value)
		seen[name] = true
		return valid
	})
	return valid
}

func adaptiveResponsesReplayHeaders(headers http.Header) bool {
	for name := range headers {
		if !adaptiveResponsesReplayHeader(strings.ToLower(name)) {
			return false
		}
	}
	return true
}

func adaptiveResponsesReplayHeader(name string) bool {
	switch name {
	case "authorization", "x-api-key", "content-type", "content-length", "accept", "accept-language",
		"accept-encoding", "user-agent", "connection", "host", "x-request-id",
		"x-forwarded-for", "x-forwarded-proto", "x-forwarded-host", "x-real-ip":
		return true
	}
	return false
}

func adaptiveResponsesReplayIngressHeaders(headers http.Header) bool {
	for name := range headers {
		lower := strings.ToLower(name)
		if adaptiveNativeSessionHeader(lower) || strings.Contains(lower, "session") || strings.Contains(lower, "conversation") ||
			strings.Contains(lower, "turn-state") || strings.Contains(lower, "continuation") ||
			lower == "cookie" || lower == "idempotency-key" {
			return false
		}
		if openaiAllowedHeaders[lower] && !adaptiveResponsesReplayHeader(lower) {
			return false
		}
	}
	return true
}

func adaptiveResponsesReplayResponseHeaders(headers http.Header) bool {
	for name := range headers {
		name = strings.ToLower(name)
		if name == "set-cookie" || strings.Contains(name, "session") || strings.Contains(name, "conversation") ||
			strings.Contains(name, "turn-state") || strings.Contains(name, "response-id") ||
			strings.Contains(name, "usage") || strings.Contains(name, "billed") || adaptiveNativeSessionHeader(name) {
			return false
		}
	}
	return true
}

func adaptiveResponsesPreambleAllowsReplay(data, eventType string) bool {
	switch eventType {
	case "response.created", "response.queued", "response.in_progress":
	default:
		return false
	}
	if !gjson.Valid(data) {
		return false
	}
	event := gjson.Parse(data)
	if event.Get("type").String() != eventType {
		return false
	}
	return adaptiveResponsesReplayObject(event, func(key string, value gjson.Result) bool {
		switch key {
		case "type", "sequence_number":
			return true
		case "response":
			return adaptiveResponsesReplayObject(value, func(key string, value gjson.Result) bool {
				switch key {
				case "id", "object", "created_at", "status", "model", "instructions", "service_tier",
					"parallel_tool_calls", "max_output_tokens", "temperature", "top_p", "text", "truncation",
					"max_tool_calls", "top_logprobs":
					return true
				case "tool_choice":
					return value.Type == gjson.Null || value.String() == "auto" || value.String() == "none"
				case "reasoning":
					return value.Type == gjson.Null || adaptiveResponsesReplayObject(value, func(key string, value gjson.Result) bool {
						return (key == "effort" || key == "summary") && (value.Type == gjson.String || value.Type == gjson.Null)
					})
				case "output", "tools":
					return value.IsArray() && len(value.Array()) == 0
				case "usage", "tool_usage", "error", "incomplete_details", "previous_response_id", "conversation",
					"prompt", "prompt_cache_key", "prompt_cache_retention", "safety_identifier", "user":
					return value.Type == gjson.Null
				case "metadata":
					return value.Type == gjson.Null || (value.IsObject() && len(value.Map()) == 0)
				case "store", "background":
					return value.Type == gjson.False
				}
				return false
			})
		}
		return false
	})
}

func (s *OpenAIGatewayService) adaptiveResponsesTimeoutError(
	ctx context.Context, c *gin.Context, account *Account, start time.Time, model, effort string,
	timeout time.Duration, phase string, headers http.Header, replayable bool,
) error {
	if ctx.Err() != nil {
		return ErrAdaptiveOpenAIResponsesClientDisconnected
	}
	err := s.newOpenAIFirstOutputTimeoutError(ctx, c, account, opsUpstreamProxyID(account), opsUpstreamProxyName(account),
		start, model, effort, timeout, phase, headers)
	err.Reason = AdaptiveOpenAIResponsesFirstOutputTimeout
	err.ResponseHeaders = nil // Keep attempt-local headers private even on an exhausted replay.
	err.SafeToFailoverAfterWrite = false
	err.NextAccountAction = NextAccountStop
	if replayable && adaptiveResponsesReplayResponseHeaders(headers) {
		err.NextAccountAction = NextAccountRetry
	}
	return err
}

// The scanner can finish tokens already in its buffer after cutoff, without
// performing additional reads from the old upstream.
type adaptiveResponsesStreamReader struct {
	io.Reader
	stopped atomic.Bool
}

func (r *adaptiveResponsesStreamReader) Read(p []byte) (int, error) {
	if r.stopped.Load() {
		return 0, context.Canceled
	}
	return r.Reader.Read(p)
}
