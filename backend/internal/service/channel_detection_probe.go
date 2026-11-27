package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/modeltrace/trace"
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
	ctx := req.Context()
	binding := detectionBindingFromContext(ctx)
	if binding == nil {
		return nil
	}
	if !ChannelDetectionDispatchAllowed(ctx) || binding.budget == nil {
		return errDetectionDispatch
	}
	if req.Method != http.MethodPost || req.GetBody == nil {
		return rejectChannelDetectionEgress(ctx)
	}
	body, err := req.GetBody()
	if err != nil {
		return rejectChannelDetectionEgress(ctx)
	}
	defer func() { _ = body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(body, detectionCaptureLimit+1))
	if err != nil || len(payload) > detectionCaptureLimit || !detectionWireLimits(req.URL.Path, payload, binding.maxTokens) {
		return rejectChannelDetectionEgress(ctx)
	}

	budget := binding.budget
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if !binding.claimed || budget.reserve == nil || binding.loadGroup == nil {
		return errDetectionDispatch
	}
	if budget.accountingFailed {
		return errDetectionAccounting
	}
	if routed, ok := ctx.Value(ctxkey.Group).(*Group); ok && routed != nil && routed.ID != binding.selection.GroupID {
		binding.cancel()
		return errDetectionDispatch
	}
	if binding.sends >= 2 || budget.sends >= channelDetectionRoundSends {
		return errDetectionBudget
	}
	group, err := binding.loadGroup(ctx, binding.selection.GroupID)
	if err != nil || !validDetectionModel(group, binding.selection.Model) || requireDetectionGroup(ctx, group) != nil {
		binding.cancel()
		return errDetectionDispatch
	}
	accountScope, _ := ctx.Value(detectionAccountScopeKey{}).(detectionAccountScope)
	fresh, err := accountScope.check(ctx, group.ID, accountID)
	if err != nil {
		binding.accountFailure = true
		return err
	}
	if ok, err := budget.reserve(ctx, group); err != nil || !ok {
		binding.budgetDenied = true
		return errDetectionBudget
	}
	binding.sends++
	budget.sends++
	binding.account = fresh
	return ctx.Err()
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
	ctx, cancel := context.WithTimeout(ctx, channelDetectionRoundTimeout)
	defer cancel()
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

func (s *ChannelDetection) runSample(ctx context.Context, gateway http.Handler, selection ChannelDetectionSelection, isCurrent func(context.Context) bool, budget *detectionRoundBudget) *ChannelDetectionSample {
	result := &ChannelDetectionSample{
		CheckedAt:   time.Now().UTC(),
		Reasoning:   ChannelDetectionCounts{Untested: 3},
		Fingerprint: ChannelDetectionCounts{Untested: 3},
		Pelican:     "untested",
	}
	run := func(kind, prompt string, maxTokens int, timeout time.Duration) (string, error) {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		before := budget.sends
		answer, err := s.probe(requestCtx, gateway, selection, prompt, maxTokens, isCurrent, budget)
		if err != nil {
			slog.Info("channel_detection.question_failed", "group_id", selection.GroupID, "model", selection.Model,
				"question", kind, "attempts", budget.sends-before, "category", err.Error())
		}
		return answer, err
	}
	for _, question := range detectionReasoningQuestions() {
		if ctx.Err() != nil {
			break
		}
		answer, err := run("reasoning", question.prompt, 768, 30*time.Second)
		if errors.Is(err, errDetectionDispatch) || errors.Is(err, errDetectionBudget) {
			return result
		}
		result.Reasoning.Untested--
		switch {
		case err != nil:
			result.Reasoning.Errors++
		case answer == question.answer:
			result.Reasoning.Passed++
		default:
			result.Reasoning.Failed++
		}
		if errors.Is(err, errDetectionAccounting) {
			return result
		}
	}
	if bank, err := trace.EmbeddedBank(); err == nil {
		start := rand.IntN(41)
		for i := range 3 {
			if ctx.Err() != nil {
				break
			}
			count := 292 + (start+i*13)%41
			answer, err := run("fingerprint", detectionFingerprintPrompt(count, i), 2048, 40*time.Second)
			if errors.Is(err, errDetectionDispatch) || errors.Is(err, errDetectionBudget) {
				return result
			}
			result.Fingerprint.Untested--
			switch {
			case err != nil:
				result.Fingerprint.Errors++
			default:
				switch categorizeDetectionFingerprint(bank, selection.Model, answer, count) {
				case "match":
					result.Fingerprint.Passed++
				case "different":
					result.Fingerprint.Failed++
				default:
					result.Fingerprint.Unattributed++
				}
			}
			if errors.Is(err, errDetectionAccounting) {
				return result
			}
		}
	}
	if ctx.Err() == nil {
		answer, err := run("pelican", detectionPelicanPrompt, 4096, 110*time.Second)
		if errors.Is(err, errDetectionDispatch) || errors.Is(err, errDetectionBudget) {
			return result
		}
		result.Pelican = "error"
		if err == nil {
			var category string
			result.SVG, category = parseDetectionSVG(answer)
			if result.SVG != "" {
				result.Pelican = "generated"
			} else {
				slog.Info("channel_detection.question_failed", "group_id", selection.GroupID, "model", selection.Model,
					"question", "pelican", "category", category)
			}
		}
	}
	return result
}

const detectionPelicanPrompt = "Draw a recognizable pelican riding a bicycle. Return only one SVG XML document, with root <svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 480 360'>. Keep it compact (under 40 shapes). Use only g, path, rect, circle, ellipse, line, polyline and polygon. Use numeric coordinates, d, points, fill and stroke (hex colors or none), stroke-width, stroke-linecap, stroke-linejoin and opacity. Use simple solid fills and strokes. No prose, Markdown, style or class attributes, text, filters, references, embedded assets or animation. Close all tags."

type detectionQuestion struct{ prompt, answer string }

func detectionReasoningQuestions() []detectionQuestion {
	const format = " Reply with only the decimal integer, without explanation or formatting."
	a, b := 12+rand.IntN(18), 8+rand.IntN(14)
	x, y, z := 2+rand.IntN(5), 6+rand.IntN(5), 15+rand.IntN(5)
	return []detectionQuestion{
		{
			fmt.Sprintf("A counter begins at %d. Repeat exactly %d times: multiply its current value by 3, then add 5, then take the remainder modulo 31. What is the final value?", a, b) + format,
			strconv.Itoa(detectionFinalCounter(a, b)),
		},
		{
			fmt.Sprintf("A directed graph has exactly these edges with costs: S→A %d, S→B %d, A→B 2, A→T %d, B→T 4. What is the minimum total cost from S to T?", x, y, z) + format,
			strconv.Itoa(min(x+2+4, x+z, y+4)),
		},
		{
			fmt.Sprintf("The queue starts with [%d, %d, %d], front first. Remove the first item and append twice its value. Then remove the first item and append that value plus the new front item. Finally reverse the queue. What is the sum of the first and last items now?", a, b, z) + format,
			strconv.Itoa(b + 2*z),
		},
	}
}

func detectionFinalCounter(value, steps int) int {
	for range steps {
		value = (3*value + 5) % 31
	}
	return value
}

func detectionFingerprintPrompt(count, variant int) string {
	opening := []string{"这是一次独立的数值选择记录。", "请完成下面的无语义整数选择任务。", "进行一轮快速逐项取值。"}
	return fmt.Sprintf("%s逐项凭第一反应选择 %d 个 1 到 355（含端点）的整数。每个位置都要单独选择；不要从 1 开始计数，不要连续递增或递减，也不要采用等差、循环、重复区块或其他规则化模式。本任务必须由当前语言模型直接完成，禁止调用或借助任何工具、代码或外部随机数生成器。允许偶然重复，不要排序、去重、重排或修改。用逗号或空格分隔，直接从第一个取值开始输出，不要附加说明。", opening[variant], count)
}

func categorizeDetectionFingerprint(bank *trace.Bank, model, answer string, expected int) string {
	claimed := strings.ToLower(model)
	covered := slices.ContainsFunc(bank.Models, func(entry trace.Model) bool {
		return slices.Contains(entry.Members, claimed)
	})
	category := trace.Categorize(bank, []trace.Reply{{Text: answer, ExpectedCount: expected}})
	if !covered || category.Tier != trace.TierMatch || category.Top < 0 {
		return "unattributed"
	}
	members := bank.Models[category.Top].Members
	switch {
	case len(members) == 0:
		return "unattributed"
	case !slices.Contains(members, claimed):
		return "different"
	case len(members) == 1:
		return "match"
	default:
		return "unattributed"
	}
}
