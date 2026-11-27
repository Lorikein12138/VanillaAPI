package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const detectionCaptureLimit = 256 << 10

var (
	errDetectionResponse     = errors.New("invalid detection response")
	errDetectionDispatch     = errors.New("detection dispatch no longer eligible")
	errDetectionAccount      = errors.New("account_ineligible")
	errDetectionAccounting   = errors.New("account_usage_unsettled")
	errDetectionBudget       = errors.New("send_budget")
	errDetectionOutputLimit  = errors.New("unbounded_upstream")
	errDetectionTimeout      = errors.New("question_timeout")
	errDetectionHTTP         = errors.New("transport_or_http")
	errDetectionNonCompleted = errors.New("non_completed")
	errDetectionNoText       = errors.New("no_output_text")
)

type detectionBindingKey struct{}

type detectionRoundBudget struct {
	mu               sync.Mutex
	reserve          func(context.Context, *Group) (bool, error)
	sends            int
	accountingFailed bool
	attempts         int
	provenSends      int
	accountID        int64
	routeFingerprint string
	ambiguous        bool
}

type detectionBinding struct {
	selection      ChannelDetectionSelection
	group          *Group
	payload        []byte
	sessionID      string
	maxTokens      int
	stillValid     func(context.Context) bool
	loadGroup      func(context.Context, int64) (*Group, error)
	account        *Account
	accountFailure bool
	cancel         context.CancelFunc
	budget         *detectionRoundBudget
	sends          int // protected by budget.mu
	budgetDenied   bool
	unbounded      bool
	claimed        bool
}

func detectionBindingFromContext(ctx context.Context) *detectionBinding {
	binding, _ := ctx.Value(detectionBindingKey{}).(*detectionBinding)
	return binding
}

// IsChannelDetectionProbe only recognizes a capability constructed in this package.
func IsChannelDetectionProbe(ctx context.Context) bool {
	return detectionBindingFromContext(ctx) != nil
}

func ChannelDetectionSessionID(ctx context.Context) string {
	if binding := detectionBindingFromContext(ctx); binding != nil {
		return binding.sessionID
	}
	return ""
}

// ChannelDetectionIngress is the only projection from a worker capability into
// the gateway's request-local routing fields. Zero IDs carry no privileges.
func ChannelDetectionIngress(r *http.Request) (*APIKey, bool) {
	binding := detectionBindingFromContext(r.Context())
	if binding == nil || binding.group == nil || binding.group.ID != binding.selection.GroupID ||
		binding.budget == nil || binding.budget.reserve == nil || !validDetectionModel(binding.group, binding.selection.Model) ||
		r.Method != http.MethodPost || r.URL.EscapedPath() != "/v1/responses" || r.URL.RawQuery != "" ||
		r.Body == nil || !ChannelDetectionDispatchAllowed(r.Context()) {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(len(binding.payload)+1)))
	_ = r.Body.Close()
	if err != nil || !bytes.Equal(body, binding.payload) {
		return nil, false
	}
	binding.budget.mu.Lock()
	defer binding.budget.mu.Unlock()
	if binding.claimed {
		return nil, false
	}
	binding.claimed = true
	r.Body = io.NopCloser(bytes.NewReader(body))
	group := *binding.group
	groupID := group.ID
	return &APIKey{GroupID: &groupID, Group: &group, User: &User{}}, true
}

// ChannelDetectionDispatchAllowed rechecks eligibility without consuming a send.
func ChannelDetectionDispatchAllowed(ctx context.Context) bool {
	binding := detectionBindingFromContext(ctx)
	if binding == nil {
		return true
	}
	if _, bounded := ctx.Deadline(); !bounded || ctx.Err() != nil {
		return false
	}
	if binding.stillValid == nil || !binding.stillValid(ctx) {
		binding.cancel()
		return false
	}
	return true
}

// ReserveChannelDetectionSend validates the final wire payload and atomically
// reserves one daily send under the active lease. HTTP transport is the only
// supported egress because it can make each physical attempt accountable.
func ReserveChannelDetectionSend(req *http.Request, accountID int64) error {
	_, err := reserveChannelDetectionSend(req, accountID)
	return err
}

func reserveChannelDetectionSend(req *http.Request, accountID int64) (*Account, error) {
	ctx := req.Context()
	binding := detectionBindingFromContext(ctx)
	if binding == nil {
		return nil, nil
	}
	if !ChannelDetectionDispatchAllowed(ctx) || binding.budget == nil {
		return nil, errDetectionDispatch
	}
	if req.Method != http.MethodPost || req.GetBody == nil {
		return nil, rejectChannelDetectionEgress(ctx)
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, rejectChannelDetectionEgress(ctx)
	}
	defer func() { _ = body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(body, detectionCaptureLimit+1))
	if err != nil || len(payload) > detectionCaptureLimit || !detectionWireLimits(req.URL.Path, payload, binding.maxTokens) {
		return nil, rejectChannelDetectionEgress(ctx)
	}

	budget := binding.budget
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if !binding.claimed || budget.reserve == nil || binding.loadGroup == nil {
		return nil, errDetectionDispatch
	}
	if budget.accountingFailed {
		return nil, errDetectionAccounting
	}
	if routed, ok := ctx.Value(ctxkey.Group).(*Group); ok && routed != nil && routed.ID != binding.selection.GroupID {
		binding.cancel()
		return nil, errDetectionDispatch
	}
	if budget.sends >= channelDetectionRoundSends {
		return nil, errDetectionBudget
	}
	group, err := binding.loadGroup(ctx, binding.selection.GroupID)
	if err != nil || !validDetectionModel(group, binding.selection.Model) || requireDetectionGroup(ctx, group) != nil {
		binding.cancel()
		return nil, errDetectionDispatch
	}
	accountScope, _ := ctx.Value(detectionAccountScopeKey{}).(detectionAccountScope)
	fresh, err := accountScope.check(ctx, group.ID, accountID)
	if err != nil {
		binding.accountFailure = true
		return nil, err
	}
	if ok, err := budget.reserve(ctx, group); err != nil || !ok {
		binding.budgetDenied = true
		return nil, errDetectionBudget
	}
	binding.sends++
	budget.sends++
	binding.account = fresh
	return fresh, ctx.Err()
}

// requireDetectionGroup rejects a routing result outside the selected group.
func requireDetectionGroup(ctx context.Context, group *Group) error {
	binding := detectionBindingFromContext(ctx)
	if binding == nil {
		return nil
	}
	if !detectionGroupEligible(group) || group.ID != binding.selection.GroupID {
		return errDetectionDispatch
	}
	return nil
}

func IsChannelDetectionGuardError(err error) bool {
	return errors.Is(err, errDetectionDispatch) || errors.Is(err, errDetectionBudget) || errors.Is(err, errDetectionOutputLimit) || errors.Is(err, errDetectionAccount) || errors.Is(err, errDetectionAccounting)
}

func rejectChannelDetectionEgress(ctx context.Context) error {
	if binding := detectionBindingFromContext(ctx); binding != nil && binding.budget != nil {
		binding.budget.mu.Lock()
		binding.unbounded = true
		binding.budget.mu.Unlock()
	}
	return errDetectionOutputLimit
}

func detectionWireLimits(path string, body []byte, limit int) bool {
	if limit <= 0 || !gjson.ValidBytes(body) {
		return false
	}
	payload := gjson.ParseBytes(body)
	if store := payload.Get("store"); store.Exists() && store.Type != gjson.False {
		return false
	}
	fields := []string{"max_output_tokens", "max_tokens", "max_completion_tokens"}
	for _, field := range fields {
		value := payload.Get(field)
		if value.Exists() && (value.Type != gjson.Number || value.Int() <= 0 ||
			value.Num != float64(value.Int()) || value.Int() > int64(limit)) {
			return false
		}
	}
	switch {
	case strings.HasSuffix(path, "/responses"):
		return payload.Get("max_output_tokens").Exists()
	case strings.HasSuffix(path, "/messages"):
		return payload.Get("max_tokens").Exists()
	case strings.HasSuffix(path, "/chat/completions"):
		return payload.Get("max_tokens").Exists() || payload.Get("max_completion_tokens").Exists()
	default:
		return false
	}
}

type detectionCapture struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	overflow bool
	cancel   context.CancelFunc
}

func (w *detectionCapture) Header() http.Header { return w.header }

func (w *detectionCapture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *detectionCapture) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if len(body) > detectionCaptureLimit-w.body.Len() {
		w.overflow = true
		w.cancel()
		return 0, errDetectionResponse
	}
	return w.body.Write(body)
}

func (w *detectionCapture) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
}

func (s *ChannelDetection) probe(ctx context.Context, gateway http.Handler, selection ChannelDetectionSelection, prompt string, maxTokens int, isCurrent func(context.Context) bool, budget *detectionRoundBudget) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, detectionPelicanTimeout)
	defer cancel()
	if !isCurrent(ctx) {
		return "", errDetectionDispatch
	}
	group, err := s.probeGroup(ctx, selection)
	if err != nil {
		return "", errDetectionDispatch
	}
	session := uuid.NewString()
	payload, err := json.Marshal(struct {
		Model           string `json:"model"`
		Input           string `json:"input"`
		MaxOutputTokens int    `json:"max_output_tokens"`
		Stream          bool   `json:"stream"`
		Store           bool   `json:"store"`
	}{Model: selection.Model, Input: prompt, MaxOutputTokens: maxTokens})
	if err != nil {
		return "", err
	}
	binding := &detectionBinding{
		selection: selection, group: group, payload: payload, sessionID: session,
		maxTokens: maxTokens, stillValid: isCurrent, cancel: cancel, budget: budget, loadGroup: s.groups.GetGroup,
	}
	ctx = logger.IntoContext(context.WithValue(ctx, detectionBindingKey{}, binding), zap.NewNop())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:0"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "VanillaAPI-ChannelDetection/1")
	req.Header.Set("Session_Id", session)
	req.Header.Set("X-Client-Request-Id", session)
	writer := &detectionCapture{header: make(http.Header), cancel: cancel}
	gateway.ServeHTTP(writer, req)
	if budget.accountingFailed {
		return "", errDetectionAccounting
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", errDetectionTimeout
	}
	if ctx.Err() != nil && !writer.overflow {
		return "", errDetectionDispatch
	}
	if binding.budgetDenied && binding.sends == 0 {
		return "", errDetectionBudget
	}
	if writer.overflow {
		return "", errDetectionResponse
	}
	if binding.accountFailure && writer.status != http.StatusOK {
		return "", errDetectionAccount
	}
	if binding.unbounded && writer.status != http.StatusOK {
		return "", errDetectionOutputLimit
	}
	if writer.status != http.StatusOK {
		return "", errDetectionHTTP
	}
	return detectionResponseText(writer.body.Bytes())
}

func detectionResponseText(body []byte) (string, error) {
	var output struct {
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &output); err != nil {
		return "", errDetectionResponse
	}
	if output.Status != "completed" {
		return "", errDetectionNonCompleted
	}
	var text strings.Builder
	for _, item := range output.Output {
		if item.Type == "message" && item.Role == "assistant" {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					_, _ = text.WriteString(part.Text)
				}
			}
		}
	}
	reply := strings.TrimSpace(text.String())
	if reply == "" {
		return "", errDetectionNoText
	}
	return reply, nil
}

const (
	detectionPelicanOutputLimit = 4096
	detectionPelicanTimeout     = 110 * time.Second
	detectionPelicanPrompt      = "Draw a recognizable pelican riding a bicycle. Return only one complete SVG with root <svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 480 360'>. Use under 40 shapes, only g/path/rect/circle/ellipse/line/polyline/polygon. Use numeric coordinates, d, points, solid hex/none fill and stroke, stroke-width, stroke-linecap, stroke-linejoin, opacity. Close every tag. No prose, Markdown, style/class, text, filters, references, embedded assets or animation."
)

func (s *ChannelDetection) runSample(ctx context.Context, gateway http.Handler, selection ChannelDetectionSelection, isCurrent func(context.Context) bool, budget *detectionRoundBudget) *ChannelDetectionSample {
	result := &ChannelDetectionSample{CheckedAt: time.Now().UTC(), Pelican: "untested"}
	if ctx.Err() != nil {
		return result
	}
	answer, err := s.probe(ctx, gateway, selection, detectionPelicanPrompt, detectionPelicanOutputLimit, isCurrent, budget)
	if err != nil {
		slog.Info("channel_detection.question_failed", "group_id", selection.GroupID, "model", selection.Model,
			"question", "pelican", "attempts", budget.sends, "category", err.Error())
		if !errors.Is(err, errDetectionDispatch) && !errors.Is(err, errDetectionBudget) {
			result.Pelican = "error"
		}
		return result
	}
	var category string
	result.SVG, category = parseDetectionSVG(answer)
	if result.SVG != "" {
		result.Pelican = "generated"
	} else {
		result.Pelican = "error"
		slog.Info("channel_detection.question_failed", "group_id", selection.GroupID, "model", selection.Model,
			"question", "pelican", "category", category)
	}
	return result
}
