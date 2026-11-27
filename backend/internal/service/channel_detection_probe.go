package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/modeltrace/trace"
	"github.com/google/uuid"
)

const detectionCaptureLimit = 256 << 10

var (
	errDetectionResponse = errors.New("invalid detection response")
	errDetectionDispatch = errors.New("detection dispatch no longer eligible")
)

type detectionBindingKey struct{}
type detectionBinding struct {
	keyID, groupID int64
	sessionID      string
	stillValid     func(context.Context, *APIKey) bool
}

func isChannelDetectionProbe(ctx context.Context) bool {
	_, ok := ctx.Value(detectionBindingKey{}).(detectionBinding)
	return ok
}

// ChannelDetectionSessionID supplies the same independent session across the
// native OpenAI and Anthropic-compatibility Responses routes.
func ChannelDetectionSessionID(ctx context.Context) string {
	binding, _ := ctx.Value(detectionBindingKey{}).(detectionBinding)
	return binding.sessionID
}

// ChannelDetectionBindingMatches allows ordinary requests and checks internal
// probes against the key and binding observed by gateway authentication.
func ChannelDetectionBindingMatches(ctx context.Context, key *APIKey) bool {
	expected, isProbe := ctx.Value(detectionBindingKey{}).(detectionBinding)
	return !isProbe || (key != nil && key.ID == expected.keyID && key.GroupID != nil &&
		*key.GroupID == expected.groupID && expected.stillValid(ctx, key))
}

// ChannelDetectionDispatchAllowed rechecks internal probes just before a
// forward, including attempts following a scheduling wait or upstream retry.
func ChannelDetectionDispatchAllowed(ctx context.Context) bool {
	binding, isProbe := ctx.Value(detectionBindingKey{}).(detectionBinding)
	return !isProbe || binding.stillValid(ctx, nil)
}

type detectionCapture struct {
	header http.Header
	body   bytes.Buffer
	status int
	cancel context.CancelFunc
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

func (s *ChannelDetection) probe(ctx context.Context, gateway http.Handler, selection ChannelDetectionSelection, prompt string, maxTokens int, isCurrent func(context.Context) bool) (string, error) {
	if _, err := s.probeGroup(ctx, selection); err != nil {
		return "", errDetectionDispatch
	}
	key, err := s.probeKey(ctx, selection, true)
	if err != nil {
		return "", errDetectionDispatch
	}
	if !isCurrent(ctx) {
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ctx = context.WithValue(ctx, detectionBindingKey{}, detectionBinding{
		keyID: key.ID, groupID: selection.GroupID, sessionID: session,
		stillValid: func(ctx context.Context, authenticated *APIKey) bool {
			if _, err := s.probeGroup(ctx, selection); err == nil {
				current, err := s.probeKey(ctx, selection, true)
				if err == nil && current.Key == key.Key && s.sameProbeKeyLimits(key, current) &&
					(authenticated == nil || s.sameProbeKeyLimits(authenticated, current)) && isCurrent(ctx) {
					return true
				}
			}
			cancel()
			return false
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Host = "localhost"
	req.RemoteAddr = "127.0.0.1:0"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("User-Agent", "VanillaAPI-ChannelDetection/1")
	req.Header.Set("Session_Id", session)
	req.Header.Set("X-Client-Request-Id", session)
	writer := &detectionCapture{header: make(http.Header), cancel: cancel}
	gateway.ServeHTTP(writer, req)
	if ctx.Err() != nil || writer.status != http.StatusOK {
		return "", errDetectionResponse
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
	if err := json.Unmarshal(body, &output); err != nil || output.Status != "completed" {
		return "", errDetectionResponse
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
		return "", errDetectionResponse
	}
	return reply, nil
}

func (s *ChannelDetection) runSample(ctx context.Context, gateway http.Handler, selection ChannelDetectionSelection, isCurrent func(context.Context) bool) *ChannelDetectionSample {
	result := &ChannelDetectionSample{
		CheckedAt:   time.Now().UTC(),
		Reasoning:   ChannelDetectionCounts{Untested: 3},
		Fingerprint: ChannelDetectionCounts{Untested: 3},
		Pelican:     "untested",
	}
	run := func(prompt string, maxTokens int, timeout time.Duration) (string, error) {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return s.probe(requestCtx, gateway, selection, prompt, maxTokens, isCurrent)
	}
	for _, question := range detectionReasoningQuestions() {
		if ctx.Err() != nil {
			break
		}
		answer, err := run(question.prompt, 768, 30*time.Second)
		if errors.Is(err, errDetectionDispatch) {
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
	}
	if bank, err := trace.EmbeddedBank(); err == nil {
		start := rand.IntN(41)
		for i := range 3 {
			if ctx.Err() != nil {
				break
			}
			count := 292 + (start+i*13)%41
			answer, err := run(detectionFingerprintPrompt(count, i), 2048, 40*time.Second)
			if errors.Is(err, errDetectionDispatch) {
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
		}
	}
	if ctx.Err() == nil {
		answer, err := run("Draw an original, recognizable illustration of a pelican riding a bicycle, as a single SVG with viewBox='0 0 480 360'. Return only the SVG. Use basic SVG shapes, paths and presentation attributes. No script, CSS, external references, embedded images, fonts or animation.", 4096, 75*time.Second)
		if errors.Is(err, errDetectionDispatch) {
			return result
		}
		result.Pelican = "error"
		if err == nil {
			if result.SVG = sanitizeDetectionSVG(answer); result.SVG != "" {
				result.Pelican = "generated"
			}
		}
	}
	return result
}

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
