package service

import (
	"bytes"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// PrepareChannelDetectionSend reserves the egress budget and observes the
// specific ensuing HTTP call. The caller must settle it even on errors.
func PrepareChannelDetectionSend(req *http.Request, accountID int64) (*http.Request, func(*http.Response), error) {
	account, err := reserveChannelDetectionSend(req, accountID)
	if err != nil {
		return nil, nil, err
	}
	if account == nil {
		return req, nil, nil
	}
	observed, settle := observeDetectionAttempt(req, detectionBindingFromContext(req.Context()).budget, account)
	return observed, settle, nil
}

func observeDetectionAttempt(req *http.Request, budget *detectionRoundBudget, account *Account) (*http.Request, func(*http.Response)) {
	fingerprint, err := detectionAccountRouteFingerprint(account)
	eligible := err == nil && !account.IsShadow() && (account.ProxyID == nil || account.Proxy != nil) &&
		detectionWireCredentialMatches(req, account)
	budget.mu.Lock()
	budget.attempts++
	budget.mu.Unlock()

	var mu sync.Mutex
	var settled, wrote bool
	ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			if !settled && info.Err == nil {
				wrote = true
			}
		},
	})
	return req.WithContext(ctx), func(resp *http.Response) {
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return
		}
		settled = true
		budget.mu.Lock()
		defer budget.mu.Unlock()
		if !wrote && resp == nil {
			budget.ambiguous = true
			return
		}
		budget.provenSends++
		route := hex.EncodeToString(fingerprint[:])
		if budget.provenSends == 1 {
			budget.accountID, budget.routeFingerprint = account.ID, route
		}
		if !eligible || account.ID != budget.accountID || route != budget.routeFingerprint {
			budget.ambiguous = true
		}
	}
}

// Check the finished outbound request against the DB snapshot used to reserve
// the send. OAuth providers can return a different token from their cache.
// Unknown or conflicting auth schemes still yield public probe results.
func detectionWireCredentialMatches(req *http.Request, account *Account) bool {
	if req.URL.User != nil {
		return false
	}
	var bearer, apiKey, workspace []string
	for name, values := range req.Header {
		switch strings.ToLower(name) {
		case "authorization":
			bearer = append(bearer, values...)
		case "x-api-key":
			apiKey = append(apiKey, values...)
		case "chatgpt-account-id":
			workspace = append(workspace, values...)
		case "x-goog-api-key", "cookie":
			if len(values) != 0 {
				return false
			}
		}
	}
	if len(bearer)+len(apiKey) != 1 {
		return false
	}
	var expected string
	switch account.Type {
	case AccountTypeAPIKey:
		expected = strings.TrimSpace(account.GetCredential("api_key"))
		if len(workspace) != 0 {
			return false
		}
	case AccountTypeOAuth:
		if len(bearer) != 1 || (account.Platform != PlatformAnthropic && account.Platform != PlatformOpenAI) {
			return false
		}
		expected = strings.TrimSpace(account.GetCredential("access_token"))
		if account.Platform == PlatformOpenAI && account.GetChatGPTAccountID() != "" {
			if len(workspace) != 1 || workspace[0] != account.GetChatGPTAccountID() {
				return false
			}
		} else if len(workspace) != 0 {
			return false
		}
	default:
		return false
	}
	if expected == "" {
		return false
	}
	if len(apiKey) == 1 {
		return subtle.ConstantTimeCompare([]byte(apiKey[0]), []byte(expected)) == 1
	}
	scheme, token, ok := strings.Cut(bearer[0], " ")
	return ok && strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func DetectionAccountMatchesProof(account *Account, proof ChannelDetectionProof) bool {
	if proof.Version != channelDetectionProofVersion || account == nil || account.ID != proof.AccountID || account.IsShadow() ||
		(account.ProxyID != nil && account.Proxy == nil) {
		return false
	}
	fingerprint, err := detectionAccountRouteFingerprint(account)
	return err == nil && hex.EncodeToString(fingerprint[:]) == proof.RouteFingerprint
}

func (b *detectionRoundBudget) proof(run channelDetectionRun) (ChannelDetectionProof, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	proof := ChannelDetectionProof{Version: channelDetectionProofVersion, SampleID: run.SampleID, Source: run.Source}
	if !b.ambiguous && b.provenSends > 0 && b.provenSends == b.attempts {
		proof.AccountID, proof.RouteFingerprint, proof.Sends = b.accountID, b.routeFingerprint, b.provenSends
	}
	return proof, b.attempts > 0
}

func (b *detectionRoundBudget) autoConfirmable() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.trackCompletion && !b.ambiguous && !b.accountingFailed &&
		b.attempts > 0 && b.attempts == b.sends && b.attempts == b.provenSends &&
		b.attempts == b.completedResponses && b.attempts == b.usageSettled
}

// ObserveChannelDetectionResponse verifies the original protocol's completion
// signal. A gateway bridge may otherwise synthesize "completed" after EOF.
func ObserveChannelDetectionResponse(req *http.Request, resp *http.Response) {
	binding := detectionBindingFromContext(req.Context())
	if binding == nil || !binding.budget.trackCompletion || resp.StatusCode != http.StatusOK {
		return
	}
	resp.Body = &detectionResponseBody{ReadCloser: resp.Body, budget: binding.budget, path: req.URL.Path}
}

type detectionResponseBody struct {
	io.ReadCloser
	budget              *detectionRoundBudget
	path                string
	mu                  sync.Mutex
	body                bytes.Buffer
	reading             int
	eof, failed, closed bool
}

func (b *detectionResponseBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.reading++
	b.mu.Unlock()
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reading--
	b.eof = b.eof || errors.Is(err, io.EOF)
	if (err != nil && !errors.Is(err, io.EOF)) || n > detectionCaptureLimit-b.body.Len() {
		b.failed = true
	}
	if !b.closed && !b.failed {
		_, _ = b.body.Write(p[:n])
	}
	return n, err
}

func (b *detectionResponseBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed && !b.failed && b.reading == 0 && err == nil && detectionUpstreamCompleted(b.path, b.body.Bytes(), b.eof) {
		b.budget.mu.Lock()
		b.budget.completedResponses++
		b.budget.mu.Unlock()
	}
	b.closed = true
	return err
}

func detectionUpstreamCompleted(path string, body []byte, eof bool) bool {
	isResponses := strings.HasSuffix(path, "/responses")
	isMessages := strings.HasSuffix(path, "/messages")
	isChat := strings.HasSuffix(path, "/chat/completions")
	if eof && gjson.ValidBytes(body) {
		value := gjson.ParseBytes(body)
		switch {
		case isResponses:
			return completedNativeDetection(value)
		case isMessages:
			return value.Get("error").Type == gjson.Null &&
				value.Get("type").String() == "message" && normalDetectionStop(value.Get("stop_reason").String())
		case isChat:
			return value.Get("error").Type == gjson.Null && completedDetectionChatChoice(value)
		}
	}
	// Inspect all received frames, including bytes the gateway buffered after
	// a terminal event. The observer never drains the body on its own.
	var parser openAICompatSSEFrameParser
	var stopReason string
	completed := false
	stream := string(body)
	for {
		line, rest, ok := strings.Cut(stream, "\n")
		if !ok {
			break
		}
		stream = rest
		line = strings.TrimSuffix(line, "\r")
		if header, ok := extractOpenAISSEEventLine(line); ok && parser.eventType != "" && parser.eventType != header {
			return false
		}
		frame, _ := parser.AddLine(line)
		if line != "" {
			continue
		}
		data := strings.TrimSpace(frame.Data)
		if data == "" {
			if frame.EventType != "" {
				return false
			}
			continue
		}
		if data == "[DONE]" {
			if frame.EventType != "" && frame.EventType != "message" {
				return false
			}
			if isChat && stopReason != "" {
				completed = true
			}
			if !completed {
				return false
			}
			continue
		}
		if !gjson.Valid(data) {
			return false
		}
		value := gjson.Parse(data)
		eventType := effectiveOpenAISSEEventType([]byte(data), frame.EventType)
		if !value.IsObject() || value.Get("error").Type != gjson.Null || eventType == "error" ||
			(frame.EventType != "" && frame.EventType != "message" && frame.EventType != eventType) {
			return false
		}
		switch {
		case isResponses && (eventType == "response.completed" || eventType == "response.done"):
			if !completedNativeDetection(value.Get("response")) {
				return false
			}
			completed = true
		case isResponses && isOpenAICompatResponsesTerminalEvent(eventType):
			return false
		case isMessages && eventType == "message_delta":
			if reason := value.Get("delta.stop_reason"); reason.Type != gjson.Null {
				if !normalDetectionStop(reason.String()) || (stopReason != "" && stopReason != reason.String()) {
					return false
				}
				stopReason = reason.String()
			}
		case isMessages && eventType == "message_stop":
			if stopReason == "" {
				return false
			}
			completed = true
		case isChat:
			if len(value.Get("choices").Array()) > 1 {
				return false
			}
			if reason := value.Get("choices.0.finish_reason"); reason.Type != gjson.Null {
				if !completedDetectionChatChoice(value) || (stopReason != "" && stopReason != reason.String()) {
					return false
				}
				stopReason = reason.String()
			}
		}
	}
	// A parsed terminal must be blank-line framed. An unfinished field may
	// contain a contradictory event, even if the transport delivered EOF.
	return completed && parser.eventType == "" && len(parser.dataLines) == 0 &&
		(strings.TrimSpace(stream) == "" || strings.HasPrefix(stream, ":"))
}

func completedNativeDetection(value gjson.Result) bool {
	if value.Get("status").String() != "completed" || value.Get("incomplete_details").Type != gjson.Null || value.Get("error").Type != gjson.Null {
		return false
	}
	for _, item := range value.Get("output").Array() {
		if status := item.Get("status").String(); status != "" && status != "completed" {
			return false
		}
	}
	return true
}

func normalDetectionStop(reason string) bool {
	return reason == "end_turn" || reason == "stop_sequence" || reason == "tool_use"
}

func completedDetectionChatChoice(value gjson.Result) bool {
	choices := value.Get("choices").Array()
	return len(choices) == 1 && (choices[0].Get("finish_reason").String() == "stop" ||
		choices[0].Get("finish_reason").String() == "tool_calls")
}
