package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type openAISnapshotCacheStub struct {
	SchedulerCache
	snapshotAccounts []*Account
	accountsByID     map[int64]*Account
}

type schedulerTestOpenAIAccountRepo struct {
	AccountRepository
	accounts []Account
}

type openAIAccountRuntimeStatsRetentionRepo struct {
	AccountRepository
	mu       sync.Mutex
	calls    [][]int64
	getByIDs func(context.Context, []int64) ([]*Account, error)
}

func (r *openAIAccountRuntimeStatsRetentionRepo) GetByIDs(ctx context.Context, ids []int64) ([]*Account, error) {
	idsCopy := append([]int64(nil), ids...)
	r.mu.Lock()
	r.calls = append(r.calls, idsCopy)
	getByIDs := r.getByIDs
	r.mu.Unlock()
	if getByIDs == nil {
		return nil, nil
	}
	return getByIDs(ctx, idsCopy)
}

func (r *openAIAccountRuntimeStatsRetentionRepo) callsSnapshot() [][]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	calls := make([][]int64, len(r.calls))
	for index := range r.calls {
		calls[index] = append([]int64(nil), r.calls[index]...)
	}
	return calls
}

type schedulerTestLogRecord struct {
	message string
	attrs   map[string]any
}

type schedulerTestLogHandler struct {
	mu      sync.Mutex
	records []schedulerTestLogRecord
}

type blockingOpenAIProbeTraceLogHandler struct {
	entered     chan struct{}
	unblock     chan struct{}
	enteredOnce sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	records     []schedulerTestLogRecord
}

func (h *schedulerTestLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *schedulerTestLogHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Resolve().Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, schedulerTestLogRecord{message: record.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *schedulerTestLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *schedulerTestLogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *schedulerTestLogHandler) snapshot() []schedulerTestLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]schedulerTestLogRecord, len(h.records))
	copy(result, h.records)
	return result
}

func (h *blockingOpenAIProbeTraceLogHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *blockingOpenAIProbeTraceLogHandler) Handle(_ context.Context, record slog.Record) error {
	attrs := make(map[string]any)
	record.Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.Resolve().Any()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, schedulerTestLogRecord{message: record.Message, attrs: attrs})
	h.mu.Unlock()
	if record.Message == "openai_account_health_probe" {
		h.enteredOnce.Do(func() {
			close(h.entered)
			<-h.unblock
		})
	}
	return nil
}

func (h *blockingOpenAIProbeTraceLogHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *blockingOpenAIProbeTraceLogHandler) WithGroup(string) slog.Handler {
	return h
}

func (h *blockingOpenAIProbeTraceLogHandler) release() {
	h.releaseOnce.Do(func() { close(h.unblock) })
}

func (h *blockingOpenAIProbeTraceLogHandler) snapshot() []schedulerTestLogRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]schedulerTestLogRecord, len(h.records))
	copy(result, h.records)
	return result
}

func (r schedulerTestOpenAIAccountRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	for i := range r.accounts {
		if r.accounts[i].ID == id {
			return &r.accounts[i], nil
		}
	}
	return nil, errors.New("account not found")
}

func (r schedulerTestOpenAIAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r schedulerTestOpenAIAccountRepo) ListSchedulableByPlatform(ctx context.Context, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r schedulerTestOpenAIAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	return r.ListSchedulableByPlatform(ctx, platform)
}

type schedulerGroupAwareOpenAIAccountRepo struct {
	schedulerTestOpenAIAccountRepo
}

func (r schedulerGroupAwareOpenAIAccountRepo) ListSchedulableByGroupIDAndPlatform(ctx context.Context, groupID int64, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform && openAIStickyAccountMatchesGroup(&acc, &groupID) {
			result = append(result, acc)
		}
	}
	return result, nil
}

func (r schedulerGroupAwareOpenAIAccountRepo) ListSchedulableUngroupedByPlatform(ctx context.Context, platform string) ([]Account, error) {
	var result []Account
	for _, acc := range r.accounts {
		if acc.Platform == platform && openAIStickyAccountMatchesGroup(&acc, nil) {
			result = append(result, acc)
		}
	}
	return result, nil
}

type schedulerTestConcurrencyCache struct {
	ConcurrencyCache
	loadBatchErr    error
	loadMap         map[int64]*AccountLoadInfo
	acquireResults  map[int64]bool
	waitCounts      map[int64]int
	skipDefaultLoad bool
	acquiredIDs     *[]int64
	releasedIDs     *[]int64
}

func (c schedulerTestConcurrencyCache) AcquireAccountSlot(ctx context.Context, accountID int64, maxConcurrency int, requestID string) (bool, error) {
	if c.acquiredIDs != nil {
		*c.acquiredIDs = append(*c.acquiredIDs, accountID)
	}
	if c.acquireResults != nil {
		if result, ok := c.acquireResults[accountID]; ok {
			return result, nil
		}
	}
	return true, nil
}

func (c schedulerTestConcurrencyCache) ReleaseAccountSlot(ctx context.Context, accountID int64, requestID string) error {
	if c.releasedIDs != nil {
		*c.releasedIDs = append(*c.releasedIDs, accountID)
	}
	return nil
}

func (c schedulerTestConcurrencyCache) GetAccountsLoadBatch(ctx context.Context, accounts []AccountWithConcurrency) (map[int64]*AccountLoadInfo, error) {
	if c.loadBatchErr != nil {
		return nil, c.loadBatchErr
	}
	out := make(map[int64]*AccountLoadInfo, len(accounts))
	if c.skipDefaultLoad && c.loadMap != nil {
		for _, acc := range accounts {
			if load, ok := c.loadMap[acc.ID]; ok {
				out[acc.ID] = load
			}
		}
		return out, nil
	}
	for _, acc := range accounts {
		if c.loadMap != nil {
			if load, ok := c.loadMap[acc.ID]; ok {
				out[acc.ID] = load
				continue
			}
		}
		out[acc.ID] = &AccountLoadInfo{AccountID: acc.ID, LoadRate: 0}
	}
	return out, nil
}

func (c schedulerTestConcurrencyCache) GetAccountWaitingCount(ctx context.Context, accountID int64) (int, error) {
	if c.waitCounts != nil {
		if count, ok := c.waitCounts[accountID]; ok {
			return count, nil
		}
	}
	return 0, nil
}

type schedulerTestGatewayCache struct {
	sessionBindings   map[string]int64
	deletedSessions   map[string]int
	refreshedSessions map[string]int
}

func (c *schedulerTestGatewayCache) GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error) {
	if id, ok := c.sessionBindings[sessionHash]; ok {
		return id, nil
	}
	return 0, ErrStickySessionNotFound
}

func (c *schedulerTestGatewayCache) SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error {
	if c.sessionBindings == nil {
		c.sessionBindings = make(map[string]int64)
	}
	c.sessionBindings[sessionHash] = accountID
	return nil
}

func (c *schedulerTestGatewayCache) RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error {
	if c.refreshedSessions == nil {
		c.refreshedSessions = make(map[string]int)
	}
	c.refreshedSessions[sessionHash]++
	return nil
}

func (c *schedulerTestGatewayCache) DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error {
	if c.sessionBindings == nil {
		return nil
	}
	if c.deletedSessions == nil {
		c.deletedSessions = make(map[string]int)
	}
	c.deletedSessions[sessionHash]++
	delete(c.sessionBindings, sessionHash)
	return nil
}

func (c *schedulerTestGatewayCache) SetGrokVideoPendingBilling(_ context.Context, _ string, _ []byte, _ time.Duration) error {
	return nil
}
func (c *schedulerTestGatewayCache) GetGrokVideoPendingBilling(_ context.Context, _ string) ([]byte, error) {
	return nil, nil
}
func (c *schedulerTestGatewayCache) ClaimGrokVideoBilled(_ context.Context, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

func (c *schedulerTestGatewayCache) ReleaseGrokVideoBilled(_ context.Context, _ string) error {
	return nil
}

func (c *schedulerTestGatewayCache) SetReasoningContent(_ context.Context, _ string, _ string, _ time.Duration) error {
	return nil
}
func (c *schedulerTestGatewayCache) GetReasoningContent(_ context.Context, _ string) (string, error) {
	return "", ErrReasoningContentNotFound
}

func newSchedulerTestOpenAIWSV2Config() *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	return cfg
}

func newSchedulerTestSubscriptionPriorityConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 0
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 0
	return cfg
}

type openAIAdvancedSchedulerSettingRepoStub struct {
	values map[string]string
}

type schedulerCacheTestSettingsRepo struct {
	mu sync.Mutex

	values           map[string]string
	getMultipleErr   error
	getMultipleCalls int
	blockBatchCall   int
	batchStarted     chan struct{}
	releaseBatch     <-chan struct{}
	batchBlocked     bool
	blockValueKey    string
	valueStarted     chan string
	releaseValue     <-chan struct{}
	valueBlocked     bool
	getAllErr        error
	setMultipleErr   error
	setCalls         int
	firstSetStarted  chan struct{}
	releaseFirstSet  <-chan struct{}
	secondSetStarted chan struct{}
}

func (s *schedulerCacheTestSettingsRepo) Get(ctx context.Context, key string) (*Setting, error) {
	value, err := s.GetValue(ctx, key)
	if err != nil {
		return nil, err
	}
	return &Setting{Key: key, Value: value}, nil
}

func (s *schedulerCacheTestSettingsRepo) GetValue(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	value, ok := s.values[key]
	block := key == s.blockValueKey && !s.valueBlocked && s.valueStarted != nil
	if block {
		s.valueBlocked = true
	}
	started, release := s.valueStarted, s.releaseValue
	s.mu.Unlock()
	if block {
		started <- key
		<-release
	}
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (s *schedulerCacheTestSettingsRepo) Set(context.Context, string, string) error {
	panic("unexpected call to Set")
}

func (s *schedulerCacheTestSettingsRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	s.mu.Lock()
	s.getMultipleCalls++
	call := s.getMultipleCalls
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := s.values[key]; ok {
			values[key] = value
		}
	}
	err := s.getMultipleErr
	block := call == s.blockBatchCall && !s.batchBlocked && s.batchStarted != nil
	if block {
		s.batchBlocked = true
	}
	started, release := s.batchStarted, s.releaseBatch
	s.mu.Unlock()
	if block {
		close(started)
		<-release
	}
	return values, err
}

func (s *schedulerCacheTestSettingsRepo) SetMultiple(_ context.Context, updates map[string]string) error {
	s.mu.Lock()
	s.setCalls++
	call := s.setCalls
	if s.setMultipleErr != nil {
		err := s.setMultipleErr
		s.mu.Unlock()
		return err
	}
	for key, value := range updates {
		s.values[key] = value
	}
	var started chan struct{}
	var release <-chan struct{}
	if call == 1 && s.firstSetStarted != nil {
		started, release = s.firstSetStarted, s.releaseFirstSet
	} else if call == 2 && s.secondSetStarted != nil {
		started = s.secondSetStarted
	}
	s.mu.Unlock()
	if started != nil {
		close(started)
	}
	if release != nil {
		<-release
	}
	return nil
}

func (s *schedulerCacheTestSettingsRepo) GetAll(context.Context) (map[string]string, error) {
	s.mu.Lock()
	values := make(map[string]string, len(s.values))
	for key, value := range s.values {
		values[key] = value
	}
	err := s.getAllErr
	s.mu.Unlock()
	return values, err
}

func (s *schedulerCacheTestSettingsRepo) Delete(context.Context, string) error {
	panic("unexpected call to Delete")
}

func schedulerCacheTestSettingsValues(smartEnabled, advancedEnabled bool, topK, rateMultiplier, priorityWeight string) map[string]string {
	return map[string]string{
		SettingKeyOpenAILowUpstreamRatePriorityEnabled:               "false",
		SettingKeySmartSchedulingEnabled:                             strconv.FormatBool(smartEnabled),
		SettingKeyOpenAIOAuthSchedulingRateMultiplier:                rateMultiplier,
		openAIAdvancedSchedulerSettingKey:                            strconv.FormatBool(advancedEnabled),
		SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled:       "false",
		SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled: "false",
		SettingKeyOpenAIAdvancedSchedulerLBTopK:                      topK,
		SettingKeyOpenAIAdvancedSchedulerWeightPriority:              priorityWeight,
		SettingKeyOpenAIAdvancedSchedulerWeightLoad:                  "0",
		SettingKeyOpenAIAdvancedSchedulerWeightQueue:                 "0.75",
		SettingKeyOpenAIAdvancedSchedulerWeightErrorRate:             "1.25",
		SettingKeyOpenAIAdvancedSchedulerWeightTTFT:                  "0.5",
		SettingKeyOpenAIAdvancedSchedulerWeightReset:                 "",
		SettingKeyOpenAIAdvancedSchedulerWeightQuotaHeadroom:         "0.2",
		SettingKeyOpenAIAdvancedSchedulerWeightUpstreamCost:          "1.5",
		SettingKeyOpenAIAdvancedSchedulerWeightPreviousResponse:      "8",
		SettingKeyOpenAIAdvancedSchedulerWeightSessionSticky:         "4",
	}
}

func schedulerCacheTestSystemSettings(smartEnabled, advancedEnabled, lowRatePriority bool, topK, rateMultiplier, priorityWeight string) *SystemSettings {
	rate, err := strconv.ParseFloat(rateMultiplier, 64)
	if err != nil {
		panic(err)
	}
	return &SystemSettings{
		SmartSchedulingEnabled:                        smartEnabled,
		OpenAIAdvancedSchedulerEnabled:                advancedEnabled,
		OpenAILowUpstreamRatePriorityEnabled:          lowRatePriority,
		OpenAIOAuthSchedulingRateMultiplier:           &rate,
		OpenAIAdvancedSchedulerLBTopK:                 topK,
		OpenAIAdvancedSchedulerWeightPriority:         priorityWeight,
		OpenAIAdvancedSchedulerWeightLoad:             "0",
		OpenAIAdvancedSchedulerWeightQueue:            "0.75",
		OpenAIAdvancedSchedulerWeightErrorRate:        "1.25",
		OpenAIAdvancedSchedulerWeightTTFT:             "0.5",
		OpenAIAdvancedSchedulerWeightReset:            "",
		OpenAIAdvancedSchedulerWeightQuotaHeadroom:    "0.2",
		OpenAIAdvancedSchedulerWeightUpstreamCost:     "1.5",
		OpenAIAdvancedSchedulerWeightPreviousResponse: "8",
		OpenAIAdvancedSchedulerWeightSessionSticky:    "4",
	}
}

func (s *openAIAdvancedSchedulerSettingRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	value, err := s.GetValue(ctx, key)
	if err != nil {
		return nil, err
	}
	return &Setting{Key: key, Value: value}, nil
}

func (s *openAIAdvancedSchedulerSettingRepoStub) GetValue(_ context.Context, key string) (string, error) {
	if s == nil || s.values == nil {
		return "", ErrSettingNotFound
	}
	value, ok := s.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (s *openAIAdvancedSchedulerSettingRepoStub) Set(context.Context, string, string) error {
	panic("unexpected call to Set")
}

func (s *openAIAdvancedSchedulerSettingRepoStub) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, err := s.GetValue(context.Background(), key); err == nil {
			result[key] = value
		}
	}
	return result, nil
}

func (s *openAIAdvancedSchedulerSettingRepoStub) SetMultiple(context.Context, map[string]string) error {
	panic("unexpected call to SetMultiple")
}

func (s *openAIAdvancedSchedulerSettingRepoStub) GetAll(context.Context) (map[string]string, error) {
	panic("unexpected call to GetAll")
}

func (s *openAIAdvancedSchedulerSettingRepoStub) Delete(context.Context, string) error {
	panic("unexpected call to Delete")
}

func newOpenAIAdvancedSchedulerRateLimitService(enabled string, values ...string) *RateLimitService {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	repo := &openAIAdvancedSchedulerSettingRepoStub{
		values: map[string]string{},
	}
	if enabled != "" {
		repo.values[openAIAdvancedSchedulerSettingKey] = enabled
	}
	if len(values) > 0 && values[0] != "" {
		repo.values[SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled] = values[0]
	}
	if len(values) > 1 && values[1] != "" {
		repo.values[SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled] = values[1]
	}
	return &RateLimitService{
		settingService: NewSettingService(repo, &config.Config{}),
	}
}

func newOpenAISchedulerModeRateLimitService(smartEnabled, advancedEnabled bool) *RateLimitService {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	values := map[string]string{
		SettingKeySmartSchedulingEnabled:  strconv.FormatBool(smartEnabled),
		openAIAdvancedSchedulerSettingKey: strconv.FormatBool(advancedEnabled),
	}
	repo := &openAIAdvancedSchedulerSettingRepoStub{values: values}
	return &RateLimitService{settingService: NewSettingService(repo, &config.Config{})}
}

func newSmartSchedulerTestAccount(id int64, groupID int64, priority int) Account {
	return Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    priority,
		GroupIDs:    []int64{groupID},
	}
}

func newSmartSchedulerTestService(accounts []Account, concurrency schedulerTestConcurrencyCache) *OpenAIGatewayService {
	groupID := int64(40101)
	for i := range accounts {
		if len(accounts[i].GroupIDs) == 0 {
			accounts[i].GroupIDs = []int64{groupID}
		}
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 7
	return &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:                cfg,
		rateLimitService:   newOpenAISchedulerModeRateLimitService(true, false),
		concurrencyService: NewConcurrencyService(concurrency),
	}
}

func (s *openAISnapshotCacheStub) GetSnapshot(ctx context.Context, bucket SchedulerBucket) ([]*Account, bool, error) {
	if len(s.snapshotAccounts) == 0 {
		return nil, false, nil
	}
	out := make([]*Account, 0, len(s.snapshotAccounts))
	for _, account := range s.snapshotAccounts {
		if account == nil {
			continue
		}
		cloned := *account
		out = append(out, &cloned)
	}
	return out, true, nil
}

func (s *openAISnapshotCacheStub) GetAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s.accountsByID == nil {
		return nil, nil
	}
	account := s.accountsByID[accountID]
	if account == nil {
		return nil, nil
	}
	cloned := *account
	return &cloned, nil
}

func TestOpenAIGatewayService_OpenAIAdvancedSchedulerRuntimeSettings_DBOverridesConfig(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 11
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights = config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority:         1,
		Load:             2,
		Queue:            3,
		ErrorRate:        4,
		TTFT:             5,
		Reset:            6,
		QuotaHeadroom:    7,
		UpstreamCost:     8,
		PreviousResponse: 9,
		SessionSticky:    10,
	}
	repo := &openAIAdvancedSchedulerSettingRepoStub{
		values: map[string]string{
			openAIAdvancedSchedulerSettingKey:                       "true",
			SettingKeyOpenAIAdvancedSchedulerLBTopK:                 "3",
			SettingKeyOpenAIAdvancedSchedulerWeightPriority:         "2.5",
			SettingKeyOpenAIAdvancedSchedulerWeightReset:            "0.25",
			SettingKeyOpenAIAdvancedSchedulerWeightPreviousResponse: "12",
		},
	}
	svc := &OpenAIGatewayService{
		cfg:              cfg,
		rateLimitService: &RateLimitService{settingService: NewSettingService(repo, cfg)},
	}

	ctx := context.Background()
	require.Equal(t, 3, svc.openAIWSLBTopKForRequest(ctx))
	weights := svc.openAIWSSchedulerWeightsForRequest(ctx)
	require.Equal(t, 2.5, weights.Priority)
	require.Equal(t, 2.0, weights.Load)
	require.Equal(t, 8.0, weights.UpstreamCost)
	require.Equal(t, 0.25, weights.Reset)
	require.Equal(t, 12.0, weights.Previous)
	require.Equal(t, 10.0, weights.SessionSticky)
}

func TestOpenAIGatewayService_OpenAIAdvancedSchedulerRuntimeSettings_InvalidWeightSumsFallBackToConfig(t *testing.T) {
	base := config.GatewayOpenAIWSSchedulerScoreWeights{
		Priority: 1, Load: 2, Queue: 3, ErrorRate: 4, TTFT: 5, Reset: 6,
		QuotaHeadroom: 7, UpstreamCost: 8, PreviousResponse: 9, SessionSticky: 10,
	}
	maxFloat := strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64)
	tests := []struct {
		name   string
		values map[string]string
	}{
		{
			name: "invalid single value",
			values: map[string]string{
				SettingKeyOpenAIAdvancedSchedulerWeightPriority: "NaN",
			},
		},
		{
			name: "base sum overflow",
			values: map[string]string{
				SettingKeyOpenAIAdvancedSchedulerWeightPriority: maxFloat,
				SettingKeyOpenAIAdvancedSchedulerWeightLoad:     maxFloat,
			},
		},
		{
			name: "sticky total sum overflow",
			values: map[string]string{
				SettingKeyOpenAIAdvancedSchedulerWeightPriority:         maxFloat,
				SettingKeyOpenAIAdvancedSchedulerWeightPreviousResponse: maxFloat,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
			tt.values[openAIAdvancedSchedulerSettingKey] = "true"
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.SchedulerScoreWeights = base
			repo := &openAIAdvancedSchedulerSettingRepoStub{values: tt.values}
			svc := &OpenAIGatewayService{cfg: cfg, rateLimitService: &RateLimitService{settingService: NewSettingService(repo, cfg)}}

			require.Equal(t, (&OpenAIGatewayService{cfg: cfg}).openAIWSSchedulerWeights(), svc.openAIWSSchedulerWeightsForRequest(context.Background()))
		})
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_DefaultDisabledUsesLegacyLoadAwareness(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10106)
	accounts := []Account{
		{
			ID:          36001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
		},
		{
			ID:          36002,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	cache := &schedulerTestGatewayCache{}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_disabled_001", 36001, time.Hour))
	require.False(t, svc.isOpenAIAdvancedSchedulerEnabled(ctx))

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"resp_disabled_001",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(36002), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickyPreviousHit)
}

// Regression: the legacy load-batch path had two bare ErrNoAvailableAccounts
// exits that bypassed the diagnostics added for both the advanced scheduler and
// the non-batched legacy selector. This is the default path when load batching
// is enabled, so quota auto-pause could still surface as an opaque 503.
func TestOpenAIGatewayService_SelectAccountWithScheduler_DefaultDisabled_LoadBatchReportsFilterReasons(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 0.9})
	groupID := int64(10107)
	quotaPaused := Account{
		ID:          36003,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra: map[string]any{
			"codex_7d_used_percent":  95.0,
			"codex_7d_reset_at":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"codex_usage_updated_at": time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	mappingMiss := Account{
		ID:          36004,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-4o": "gpt-4o"},
		},
	}
	excluded := Account{
		ID:          36005,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{quotaPaused, mappingMiss, excluded}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	require.False(t, svc.isOpenAIAdvancedSchedulerEnabled(ctx))
	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"",
		"gpt-5.4-mini",
		map[int64]struct{}{excluded.ID: {}},
		OpenAIUpstreamTransportAny,
		false,
	)

	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.EqualError(t, err, "no available OpenAI accounts supporting model: gpt-5.4-mini (pool=3, filtered: excluded=1 model_not_supported=1 quota_auto_pause_7d=1)")
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_DefaultDisabled_RequiredWSV2_SkipsHTTPOnlyAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10108)
	accounts := []Account{
		{
			ID:          36011,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
		{
			ID:          36012,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
	}
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportResponsesWebsocketV2,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(36012), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
}

func openAIAccountRuntimeStatForTest(t *testing.T, stats *openAIAccountRuntimeStats, accountID int64) *openAIAccountRuntimeStat {
	t.Helper()
	stat, release, ok := stats.acquire(accountID, true, true)
	require.True(t, ok)
	t.Cleanup(release)
	return stat
}

func seedSmartDegradedAccount(t *testing.T, stats *openAIAccountRuntimeStats, accountID int64, reason string, now time.Time) {
	t.Helper()
	for index := 0; index < openAIAccountSmartWindowSize; index++ {
		success := true
		if reason == "error_rate" && index < 2 {
			success = false
		}
		ttft := (*int)(nil)
		if reason == "ttft" {
			value := 12000
			ttft = &value
		}
		stats.reportAtForRequest(accountID, success, ttft, now.Add(time.Duration(index)*time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	}
	require.Equal(t, openAIAccountHealthDegraded, stats.health(accountID, now.Add(time.Minute), openAIStickyEscapeConfig{}).level)
	stat, release, ok := stats.acquire(accountID, false, true)
	require.True(t, ok)
	stat.nextProbeAt.Store(now.Add(-time.Second).UnixNano())
	release()
}

func addIdleOpenAIAccountRuntimeStat(t *testing.T, stats *openAIAccountRuntimeStats, accountID int64, lastActivity time.Time) {
	t.Helper()
	stat, release, ok := stats.acquire(accountID, true, true)
	require.True(t, ok)
	stat.lastActivityAt.Store(lastActivity.UnixNano())
	release()
}

func openAIAccountRuntimeStatSampleCount(t *testing.T, stats *openAIAccountRuntimeStats, accountID int64) int64 {
	t.Helper()
	stat, release, ok := stats.acquire(accountID, false, false)
	require.True(t, ok)
	defer release()
	return stat.sampleCount.Load()
}

func acquireAndReportSmartProbe(t *testing.T, stats *openAIAccountRuntimeStats, accountID int64, success bool, ttft *int, now time.Time) uint64 {
	t.Helper()
	probeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(accountID, now, probeID))
	stats.markProbeSlotAcquired(accountID, probeID, now)
	stats.reportAtForRequest(accountID, success, ttft, now, openAIStickyEscapeConfig{}, false, true, probeID, true)
	return probeID
}

func selectSmartAccountForTest(t *testing.T, svc *OpenAIGatewayService, groupID int64, session string) *AccountSelectionResult {
	t.Helper()
	selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", session, "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	return selection
}

func completeSmartProbeSelection(t *testing.T, svc *OpenAIGatewayService, selection *AccountSelectionResult) {
	t.Helper()
	require.NotZero(t, selection.Account.smartHealthProbeID)
	svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, nil)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthAllDegradedRecoveryFairness(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		accounts   []Account
		firstIDs   []int64
		dueOffsets map[int64]time.Duration
	}{
		{
			name: "two_accounts_high_numeric_priority_gets_real_probe_lease",
			accounts: []Account{
				newSmartSchedulerTestAccount(7001, 70001, 10),
				newSmartSchedulerTestAccount(7002, 70001, 90),
			},
			firstIDs: []int64{7001, 7002},
		},
		{
			name: "N_accounts_same_and_non_contiguous_priorities_use_due_time_then_id",
			accounts: []Account{
				newSmartSchedulerTestAccount(7014, 70002, 90),
				newSmartSchedulerTestAccount(7013, 70002, 30),
				newSmartSchedulerTestAccount(7012, 70002, 30),
				newSmartSchedulerTestAccount(7011, 70002, 10),
			},
			firstIDs: []int64{7011, 7012, 7013, 7014},
		},
		{
			name: "new_batch_uses_earliest_due_before_id",
			accounts: []Account{
				newSmartSchedulerTestAccount(7023, 70003, 10),
				newSmartSchedulerTestAccount(7024, 70003, 90),
			},
			firstIDs: []int64{7024, 7023},
			dueOffsets: map[int64]time.Duration{
				7023: -time.Minute,
				7024: -2 * time.Minute,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groupID := tc.accounts[0].GroupIDs[0]
			svc := newSmartSchedulerTestService(tc.accounts, schedulerTestConcurrencyCache{})
			scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
			require.True(t, ok)
			now := time.Now().UTC()
			for _, account := range tc.accounts {
				seedSmartDegradedAccount(t, scheduler.stats, account.ID, "error_rate", now)
				offset, configured := tc.dueOffsets[account.ID]
				if !configured {
					offset = -time.Minute
				}
				openAIAccountRuntimeStatForTest(t, scheduler.stats, account.ID).nextProbeAt.Store(now.Add(offset).UnixNano())
			}
			for _, wantID := range tc.firstIDs {
				for sample := 0; sample < openAIAccountSmartProbeBatchLimit; sample++ {
					selection := selectSmartAccountForTest(t, svc, groupID, "")
					require.Equal(t, wantID, selection.Account.ID)
					require.NotZero(t, selection.Account.smartHealthProbeID, "candidate must acquire a real request lease")
					completeSmartProbeSelection(t, svc, selection)
				}
			}
		})
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthMixedPoolRecoveryOpportunity(t *testing.T) {
	groupID := int64(70011)
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(7021, groupID, 0),
		newSmartSchedulerTestAccount(7022, groupID, 80),
	}, schedulerTestConcurrencyCache{})
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	now := time.Now()
	seedSmartDegradedAccount(t, scheduler.stats, 7021, "error_rate", now)
	for index := 0; index < openAIAccountSmartWindowSize; index++ {
		scheduler.stats.reportAtForRequest(7022, true, intPtrForTest(100), now.Add(time.Duration(index)*time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	}
	selection := selectSmartAccountForTest(t, svc, groupID, "")
	require.Equal(t, int64(7021), selection.Account.ID)
	require.NotZero(t, selection.Account.smartHealthProbeID)
	completeSmartProbeSelection(t, svc, selection)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthEscapedStickyCanProbe(t *testing.T) {
	groupID := int64(70012)
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:escaped-probe": 7031}}
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(7031, groupID, 10),
		newSmartSchedulerTestAccount(7032, groupID, 30),
	}, schedulerTestConcurrencyCache{})
	svc.cache = cache
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, 7031, "error_rate", time.Now())
	selection, decision, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "escaped-probe", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(7031), selection.Account.ID)
	require.NotZero(t, selection.Account.smartHealthProbeID)
	require.True(t, decision.HealthEscape)
	require.Equal(t, int64(7031), cache.sessionBindings["openai:escaped-probe"])
	completeSmartProbeSelection(t, svc, selection)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthPriorityFallbackAfterProbeFailure(t *testing.T) {
	groupID := int64(70013)
	acquired := make([]int64, 0, 3)
	concurrency := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{7041: false, 7042: true}, acquiredIDs: &acquired}
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(7041, groupID, 80),
		newSmartSchedulerTestAccount(7042, groupID, 10),
	}, concurrency)
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, 7041, "error_rate", time.Now())
	selection := selectSmartAccountForTest(t, svc, groupID, "")
	require.Equal(t, int64(7042), selection.Account.ID)
	require.Zero(t, selection.Account.smartHealthProbeID)
	require.Equal(t, []int64{7041, 7042}, acquired[:2])
	stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, 7041)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	require.Empty(t, stat.probeBatchSamples)
	stat.mu.Unlock()
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthProbeResultIsolation(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Now()
	seedSmartDegradedAccount(t, stats, 7051, "error_rate", now)
	probeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(7051, now, probeID))
	stats.markProbeSlotAcquired(7051, probeID, now)
	stats.reportAtForRequest(7051, true, intPtrForTest(100), now.Add(time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	stats.reportAtForRequest(7051, false, nil, now.Add(2*time.Second), openAIStickyEscapeConfig{}, false, true, probeID+1, true)
	stat := openAIAccountRuntimeStatForTest(t, stats, 7051)
	stat.mu.Lock()
	require.Equal(t, probeID, stat.probeID)
	require.Empty(t, stat.probeBatchSamples)
	stat.mu.Unlock()
	stats.reportAtForRequest(7051, true, nil, now.Add(3*time.Second), openAIStickyEscapeConfig{}, false, true, probeID, true)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	require.Len(t, stat.probeBatchSamples, 1)
	require.True(t, stat.probeBatchSamples[0].success)
	stat.mu.Unlock()
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthProbeWaitPlanNotStarted(t *testing.T) {
	groupID := int64(70014)
	concurrency := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{7061: false, 7062: false}}
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(7061, groupID, 10),
		newSmartSchedulerTestAccount(7062, groupID, 20),
	}, concurrency)
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, 7061, "error_rate", time.Now())
	selection := selectSmartAccountForTest(t, svc, groupID, "")
	require.NotNil(t, selection.WaitPlan)
	require.Zero(t, selection.Account.smartHealthProbeID)
	require.Zero(t, svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal)
	stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, 7061)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	stat.mu.Unlock()
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthProbeLongRequestLease(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	start := time.Unix(1770000000, 0)
	seedSmartDegradedAccount(t, stats, 7071, "error_rate", start)
	probeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(7071, start, probeID))
	stats.markProbeSlotAcquired(7071, probeID, start)
	require.False(t, stats.probeEligible(7071, start.Add(31*time.Second)))
	require.False(t, stats.tryAcquireProbeLeaseWithID(7071, start.Add(31*time.Second), probeID+1))
	stats.cancelProbeLeaseForID(7071, probeID, start.Add(31*time.Second))
	require.True(t, stats.probeEligible(7071, start.Add(31*time.Second)))
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthPostWriteFailureEndsProbe(t *testing.T) {
	groupID := int64(70015)
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(7081, groupID, 10),
		newSmartSchedulerTestAccount(7082, groupID, 20),
	}, schedulerTestConcurrencyCache{})
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, 7081, "error_rate", time.Now())
	selection := selectSmartAccountForTest(t, svc, groupID, "")
	require.Equal(t, int64(7081), selection.Account.ID)
	require.NotZero(t, selection.Account.smartHealthProbeID)
	svc.ObserveOpenAIAccountHealthFailure(context.Background(), selection.Account, &UpstreamFailoverError{Scope: GatewayFailureScopeAccount})
	stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, 7081)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	require.Len(t, stat.probeBatchSamples, 1)
	require.False(t, stat.probeBatchSamples[0].success)
	stat.mu.Unlock()
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	selection = selectSmartAccountForTest(t, svc, groupID, "")
	require.NotZero(t, selection.Account.smartHealthProbeID)
	stat.mu.Lock()
	before := len(stat.probeBatchSamples)
	stat.mu.Unlock()
	credentialErr := &UpstreamFailoverError{Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeRequest}
	svc.ObserveOpenAIAccountHealthFailure(context.Background(), selection.Account, credentialErr)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	require.Len(t, stat.probeBatchSamples, before)
	stat.mu.Unlock()
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIAccountRuntimeStats_ProbeRecoveryInvariant(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Now()
	seedSmartDegradedAccount(t, stats, 7091, "error_rate", now)
	probeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(7091, now, probeID))
	stats.markProbeSlotAcquired(7091, probeID, now)
	stats.reportAtForRequest(7091, true, nil, now.Add(time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	require.Equal(t, openAIAccountHealthDegraded, stats.health(7091, now.Add(time.Second), openAIStickyEscapeConfig{}).level)
	stats.cancelProbeLeaseForID(7091, probeID+1, now.Add(2*time.Second))
	stat := openAIAccountRuntimeStatForTest(t, stats, 7091)
	stat.mu.Lock()
	require.Equal(t, probeID, stat.probeID)
	stat.mu.Unlock()
	stats.reportAtForRequest(7091, true, nil, now.Add(3*time.Second), openAIStickyEscapeConfig{}, false, true, probeID, true)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	require.Len(t, stat.probeBatchSamples, 1)
	stat.mu.Unlock()
}

func TestSmartSchedulingStall_ThreeSampleMean(t *testing.T) {
	now := time.Unix(1771000000, 0).UTC()
	report := func(stats *openAIAccountRuntimeStats, accountID int64, success bool, ttft *int, at time.Time) {
		stats.reportAtForRequest(accountID, success, ttft, at, openAIStickyEscapeConfig{}, false, true, 0, true)
	}

	t.Run("two_of_three_errors_degrade_but_one_of_three_does_not", func(t *testing.T) {
		oneOfThree := newOpenAIAccountRuntimeStats()
		report(oneOfThree, 7101, false, nil, now)
		report(oneOfThree, 7101, true, nil, now.Add(time.Second))
		report(oneOfThree, 7101, true, nil, now.Add(2*time.Second))
		assessment := oneOfThree.health(7101, now.Add(2*time.Second), openAIStickyEscapeConfig{})
		require.Equal(t, openAIAccountHealthUnknown, assessment.level)
		require.InDelta(t, 1.0/3.0, assessment.errorRate, 1e-12)
		require.False(t, assessment.hasTTFT, "missing TTFT leaves that signal immature")

		twoOfThree := newOpenAIAccountRuntimeStats()
		report(twoOfThree, 7102, false, nil, now)
		report(twoOfThree, 7102, true, nil, now.Add(time.Second))
		report(twoOfThree, 7102, false, nil, now.Add(2*time.Second))
		assessment = twoOfThree.health(7102, now.Add(2*time.Second), openAIStickyEscapeConfig{})
		require.Equal(t, openAIAccountHealthDegraded, assessment.level)
		require.Equal(t, openAIAccountHealthReasonErrorRate, assessment.reason)
		require.InDelta(t, 2.0/3.0, assessment.errorRate, 1e-12)
	})

	t.Run("positive_ttft_uses_three_sample_arithmetic_mean", func(t *testing.T) {
		meanStats := newOpenAIAccountRuntimeStats()
		meanSamples := []int{12590, 7269, 6853}
		for index, sample := range meanSamples {
			report(meanStats, 7108, true, &sample, now.Add(time.Duration(index)*time.Second))
		}
		assessment := meanStats.health(7108, now.Add(2*time.Second), openAIStickyEscapeConfig{})
		require.True(t, assessment.hasTTFT)
		require.Equal(t, 8904.0, assessment.ttft)
		require.Equal(t, openAIAccountHealthHealthy, assessment.level, "an 8904ms arithmetic mean is healthy")

		boundaryStats := newOpenAIAccountRuntimeStats()
		boundary := 10000
		for index := 0; index < openAIAccountSmartWindowSize; index++ {
			report(boundaryStats, 7103, true, &boundary, now.Add(time.Duration(index)*time.Second))
		}
		assessment = boundaryStats.health(7103, now.Add(2*time.Second), openAIStickyEscapeConfig{})
		require.True(t, assessment.hasTTFT)
		require.InDelta(t, 10000, assessment.ttft, 1e-9)
		require.Equal(t, openAIAccountHealthHealthy, assessment.level, "exactly 10 seconds is not degraded")

		slowStats := newOpenAIAccountRuntimeStats()
		slowValues := []int{10000, 10000, 10001}
		for index, sample := range slowValues {
			report(slowStats, 7104, true, &sample, now.Add(time.Duration(index)*time.Second))
		}
		assessment = slowStats.health(7104, now.Add(2*time.Second), openAIStickyEscapeConfig{})
		require.InDelta(t, 10000+1.0/3.0, assessment.ttft, 1e-9)
		require.Greater(t, assessment.ttft, float64(openAIAccountSmartTTFTThreshold.Milliseconds()))
		require.Equal(t, openAIAccountHealthDegraded, assessment.level)
		require.Equal(t, openAIAccountHealthReasonTTFT, assessment.reason)

		failureWithTTFT := newOpenAIAccountRuntimeStats()
		observedTTFT := 9000
		report(failureWithTTFT, 7105, false, &observedTTFT, now)
		report(failureWithTTFT, 7105, true, &observedTTFT, now.Add(time.Second))
		report(failureWithTTFT, 7105, true, &observedTTFT, now.Add(2*time.Second))
		stat := openAIAccountRuntimeStatForTest(t, failureWithTTFT, 7105)
		require.Len(t, stat.smartTTFTWindow, 3, "a post-first-output failure contributes its positive TTFT")
	})

	t.Run("success_replaces_old_failure_and_expired_window_is_unknown", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		report(stats, 7106, false, nil, now)
		report(stats, 7106, true, nil, now.Add(time.Second))
		report(stats, 7106, true, nil, now.Add(2*time.Second))
		report(stats, 7106, true, nil, now.Add(3*time.Second))
		assessment := stats.health(7106, now.Add(3*time.Second), openAIStickyEscapeConfig{})
		require.Equal(t, openAIAccountHealthUnknown, assessment.level)
		require.Zero(t, assessment.errorRate, "the latest three results replace an older failure")
		require.Equal(t, openAIAccountHealthUnknown,
			stats.health(7106, now.Add(openAIAccountSmartWindowDuration+time.Second), openAIStickyEscapeConfig{}).level,
			"results outside the rolling ten-minute window are pruned")
	})

	t.Run("long_generation_uses_first_token_time_only", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		firstToken := 1300
		for index := 0; index < openAIAccountSmartWindowSize; index++ {
			report(stats, 7107, true, &firstToken, now.Add(time.Duration(index)*time.Second))
		}
		assessment := stats.health(7107, now.Add(166*time.Second), openAIStickyEscapeConfig{})
		require.Equal(t, openAIAccountHealthHealthy, assessment.level)
		require.InDelta(t, 1300, assessment.ttft, 1e-9)
	})
}

func TestSmartSchedulingStall_MeanLogField(t *testing.T) {
	logHandler := captureOpenAIProbeTraceLogs(t)
	stats := newOpenAIAccountRuntimeStats()
	now := time.Unix(1771000200, 0).UTC()
	for index, sample := range []int{12000, 13000, 14000} {
		stats.reportAtForRequest(7109, true, &sample, now.Add(time.Duration(index)*time.Second), openAIStickyEscapeConfig{}, true, true, 0, true)
	}

	var healthChanged *schedulerTestLogRecord
	for _, record := range logHandler.snapshot() {
		if record.message == "openai_account_health_changed" {
			recordCopy := record
			healthChanged = &recordCopy
			break
		}
	}
	require.NotNil(t, healthChanged, "smart degradation retains its health-change log")
	ttft, hasTTFT := healthChanged.attrs["ttft_ms"]
	require.True(t, hasTTFT, "the stable ttft_ms log key remains present")
	require.Equal(t, 13000.0, ttft, "ttft_ms is the three-sample arithmetic mean")
}

func TestOpenAIAccountScheduler_SmartHealthRankingUsesMean(t *testing.T) {
	now := time.Now()
	stats := newOpenAIAccountRuntimeStats()
	for index, sample := range []int{12590, 7269, 6853} {
		stats.reportAtForRequest(7111, true, &sample, now.Add(time.Duration(index-3)*time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	}
	for index, sample := range []int{10000, 10000, 10000} {
		stats.reportAtForRequest(7112, true, &sample, now.Add(time.Duration(index-3)*time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	}

	_, legacyEWMA, hasLegacyTTFT := stats.snapshot(7111)
	require.True(t, hasLegacyTTFT)
	require.InDelta(t, 10591.24, legacyEWMA, 1e-9, "the legacy snapshot remains EWMA")

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 1
	service := &OpenAIGatewayService{cfg: cfg}
	scheduler := &defaultOpenAIAccountScheduler{service: service, stats: stats}
	accounts := []*Account{
		{ID: 7111, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
		{ID: 7112, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true},
	}
	plan := scheduler.buildOpenAIAccountLoadPlan(context.Background(), OpenAIAccountScheduleRequest{SmartHealthEnabled: true}, accounts, map[int64]*AccountLoadInfo{
		7111: {AccountID: 7111},
		7112: {AccountID: 7112},
	})
	require.Len(t, plan.allCandidates, 2)
	byID := make(map[int64]openAIAccountCandidateScore, len(plan.allCandidates))
	for _, candidate := range plan.allCandidates {
		byID[candidate.account.ID] = candidate
	}
	require.Equal(t, 8904.0, byID[7111].ttft)
	require.Equal(t, 10000.0, byID[7112].ttft)
	require.Greater(t, byID[7111].score, byID[7112].score, "smart ranking favors the lower arithmetic mean")
}

func TestSmartSchedulingStall_Legacy(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Unix(1771000100, 0).UTC()
	for index := 0; index < 3; index++ {
		stats.reportAtForRequest(7110, false, nil, now.Add(time.Duration(index)*time.Second), defaultOpenAIAccountHealthThresholds(), false, false, 0, true)
	}
	require.Equal(t, openAIAccountHealthHealthy, stats.health(7110, now.Add(2*time.Second), defaultOpenAIAccountHealthThresholds()).level)
	stats.reportAtForRequest(7110, false, nil, now.Add(3*time.Second), defaultOpenAIAccountHealthThresholds(), false, false, 0, true)
	require.Equal(t, openAIAccountHealthDegraded, stats.health(7110, now.Add(3*time.Second), defaultOpenAIAccountHealthThresholds()).level,
		"S=false retains the legacy EWMA/error threshold")
}

func TestSmartSchedulingStall_RecoveryBackoffLadder(t *testing.T) {
	base := time.Unix(1772000000, 0).UTC()
	stats := newOpenAIAccountRuntimeStats()
	for index := 0; index < openAIAccountSmartWindowSize; index++ {
		success := index >= 2
		stats.reportAtForRequest(7201, success, nil, base.Add(time.Duration(index)*time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	}
	stat := openAIAccountRuntimeStatForTest(t, stats, 7201)
	require.Equal(t, openAIAccountHealthDegraded, stats.health(7201, base.Add(9*time.Second), openAIStickyEscapeConfig{}).level)
	require.Equal(t, 0, stat.backoffIndex)
	require.Equal(t, base.Add(2*time.Second+time.Minute).UnixNano(), stat.nextProbeAt.Load())

	// An unattributable terminal ends the live lease without counting a batch result or advancing backoff.
	probeID := stats.probeSequence.Add(1)
	due := time.Unix(0, stat.nextProbeAt.Load())
	require.True(t, stats.tryAcquireProbeLeaseWithID(7201, due, probeID))
	stats.markProbeSlotAcquired(7201, probeID, due)
	stats.reportAtForRequest(7201, false, nil, due, openAIStickyEscapeConfig{}, false, true, probeID, false)
	stat.mu.Lock()
	require.Zero(t, stat.probeID)
	require.Empty(t, stat.probeBatchSamples)
	stat.mu.Unlock()
	require.Equal(t, 0, stat.backoffIndex)
	require.Equal(t, due.UnixNano(), stat.nextProbeAt.Load())

	for _, wantDelay := range []time.Duration{
		5 * time.Minute,
		10 * time.Minute,
		30 * time.Minute,
		time.Hour,
		time.Hour,
	} {
		due = time.Unix(0, stat.nextProbeAt.Load())
		firstFailureAt := due.Add(time.Second)
		acquireAndReportSmartProbe(t, stats, 7201, false, nil, firstFailureAt)
		stat.mu.Lock()
		require.Len(t, stat.probeBatchSamples, 1, "one failure remains eligible for the same batch")
		stat.mu.Unlock()
		acquireAndReportSmartProbe(t, stats, 7201, false, nil, firstFailureAt.Add(time.Second))
		require.Equal(t, wantDelay, time.Duration(stat.nextProbeAt.Load()-firstFailureAt.Add(time.Second).UnixNano()))
		require.Equal(t, wantDelay, openAIAccountHealthProbeCooldowns[stat.backoffIndex])
	}

	// Recovery clears the ladder; the next independent degradation starts at one minute again.
	recoveryStart := time.Unix(0, stat.nextProbeAt.Load())
	for index := 0; index < openAIAccountSmartProbeBatchLimit; index++ {
		acquireAndReportSmartProbe(t, stats, 7201, true, nil, recoveryStart.Add(time.Duration(index)*time.Second))
	}
	require.Equal(t, 0, stat.backoffIndex)
	for index := 0; index < openAIAccountSmartWindowSize; index++ {
		success := index >= 2
		stats.reportAtForRequest(7201, success, nil, recoveryStart.Add(20*time.Second+time.Duration(index)*time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
	}
	require.Equal(t, openAIAccountHealthDegraded, stats.health(7201, recoveryStart.Add(30*time.Second), openAIStickyEscapeConfig{}).level)
	require.Equal(t, 0, stat.backoffIndex)
	require.Equal(t, recoveryStart.Add(21*time.Second+time.Minute).UnixNano(), stat.nextProbeAt.Load())
}

func TestSmartSchedulingStall_Recovery(t *testing.T) {
	base := time.Unix(1773000000, 0).UTC()
	stats := newOpenAIAccountRuntimeStats()
	seedSmartDegradedAccount(t, stats, 7301, "error_rate", base)
	stat := openAIAccountRuntimeStatForTest(t, stats, 7301)
	firstProbe := time.Unix(0, stat.nextProbeAt.Load())
	stat.nextProbeAt.Store(firstProbe.UnixNano())

	for index := 0; index < openAIAccountSmartProbeBatchLimit; index++ {
		probeAt := firstProbe.Add(time.Duration(index) * time.Second)
		if index > 0 {
			require.True(t, stats.probeEligible(7301, probeAt), "the next real request can continue immediately")
		}
		success := index != 3
		acquireAndReportSmartProbe(t, stats, 7301, success, nil, probeAt)
		if index < openAIAccountSmartProbeBatchLimit-1 {
			stat.mu.Lock()
			require.Equal(t, openAIAccountHealthDegraded, stat.smartLevel, "the first nine results cannot recover")
			require.Len(t, stat.probeBatchSamples, index+1)
			stat.mu.Unlock()
		}
	}
	assessment := stats.health(7301, firstProbe.Add(9*time.Second), openAIStickyEscapeConfig{})
	require.Equal(t, openAIAccountHealthUnknown, assessment.level, "the error-reason batch clears degraded; the independent TTFT signal has no samples")
	require.Zero(t, assessment.errorRate)
	require.False(t, assessment.hasTTFT)
	stat.mu.Lock()
	require.Zero(t, stat.backoffIndex)
	require.Zero(t, stat.nextProbeAt.Load())
	require.Empty(t, stat.probeBatchSamples)
	require.Len(t, stat.smartErrorWindow, openAIAccountSmartWindowSize)
	require.Empty(t, stat.smartTTFTWindow)
	stat.mu.Unlock()

	// With all ten positive TTFT values, exactly 10 seconds is healthy; an old degraded history is replaced.
	allTTFTStats := newOpenAIAccountRuntimeStats()
	seedSmartDegradedAccount(t, allTTFTStats, 7302, "ttft", base)
	allTTFTStat := openAIAccountRuntimeStatForTest(t, allTTFTStats, 7302)
	recoveryTTFTValues := make([]int, openAIAccountSmartProbeBatchLimit)
	for index := range recoveryTTFTValues {
		recoveryTTFTValues[index] = 10000
	}
	recoveryTTFTValues[0] = 10001
	recoveryTTFTValues[1] = 9999
	for index := 0; index < openAIAccountSmartProbeBatchLimit; index++ {
		probeAt := base.Add(time.Minute + time.Duration(index)*time.Second)
		value := recoveryTTFTValues[index]
		acquireAndReportSmartProbe(t, allTTFTStats, 7302, true, &value, probeAt)
	}
	assessment = allTTFTStats.health(7302, base.Add(time.Minute+10*time.Second), openAIStickyEscapeConfig{})
	require.Equal(t, openAIAccountHealthHealthy, assessment.level)
	require.InDelta(t, 10000, assessment.ttft, 1e-9)
	allTTFTStat.mu.Lock()
	require.Zero(t, allTTFTStat.backoffIndex)
	require.Len(t, allTTFTStat.smartTTFTWindow, openAIAccountSmartWindowSize)
	allTTFTStat.mu.Unlock()
}

func TestOpenAIAccountRuntimeStats_InconclusiveAndOrdinaryTraffic(t *testing.T) {
	base := time.Unix(1774000000, 0).UTC()

	t.Run("missing_required_ttft_reschedules_without_backoff", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		seedSmartDegradedAccount(t, stats, 7401, "ttft", base)
		stat := openAIAccountRuntimeStatForTest(t, stats, 7401)
		probeAt := time.Unix(0, stat.nextProbeAt.Load())
		probeID := stats.probeSequence.Add(1)
		require.True(t, stats.tryAcquireProbeLeaseWithID(7401, probeAt, probeID))
		stats.markProbeSlotAcquired(7401, probeID, probeAt)
		stats.reportAtForRequest(7401, true, nil, probeAt, openAIStickyEscapeConfig{}, false, true, probeID, true)
		stat.mu.Lock()
		require.Zero(t, stat.probeID)
		require.Empty(t, stat.probeBatchSamples)
		stat.mu.Unlock()
		require.Equal(t, 0, stat.backoffIndex)
		require.Equal(t, probeAt.Add(time.Minute).UnixNano(), stat.nextProbeAt.Load())
	})

	t.Run("ordinary_results_do_not_become_probe_samples", func(t *testing.T) {
		stats := newOpenAIAccountRuntimeStats()
		seedSmartDegradedAccount(t, stats, 7402, "error_rate", base)
		stat := openAIAccountRuntimeStatForTest(t, stats, 7402)
		probeAt := time.Unix(0, stat.nextProbeAt.Load())
		acquireAndReportSmartProbe(t, stats, 7402, true, nil, probeAt)
		stats.reportAtForRequest(7402, true, nil, probeAt.Add(time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
		stat.mu.Lock()
		require.Len(t, stat.probeBatchSamples, 1)
		stat.mu.Unlock()
		stats.reportAtForRequest(7402, false, nil, probeAt.Add(2*time.Second), openAIStickyEscapeConfig{}, false, true, 0, true)
		stat.mu.Lock()
		require.Empty(t, stat.probeBatchSamples, "ordinary attributable failure invalidates the batch without advancing the ladder")
		stat.mu.Unlock()
		require.Equal(t, 0, stat.backoffIndex)
		require.Equal(t, probeAt.Add(2*time.Second+time.Minute).UnixNano(), stat.nextProbeAt.Load())
	})
}

func TestOpenAIGatewayService_SmartThresholdModeIsolation(t *testing.T) {
	ctx := context.Background()
	for _, advanced := range []bool{false, true} {
		name := "advanced_off"
		if advanced {
			name = "advanced_on"
		}
		t.Run("smart_on_"+name, func(t *testing.T) {
			service := &OpenAIGatewayService{
				cfg:              &config.Config{},
				rateLimitService: newOpenAISchedulerModeRateLimitService(true, advanced),
			}
			service.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
			service.cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
			service.cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
			settings := service.openAIAdvancedSchedulerRuntimeSettings(ctx)
			require.True(t, settings.smartEnabled)
			require.Equal(t, advanced, settings.enabled)
			require.Equal(t, float64(15000), service.openAIStickyEscapeConfig().ttftMs)
			require.Equal(t, 0.5, service.openAIStickyEscapeConfig().errorRate)

			stats := newOpenAIAccountRuntimeStats()
			now := time.Unix(1775000000, 0).UTC()
			for index := 0; index < openAIAccountSmartWindowSize; index++ {
				success := index >= 2
				stats.reportAtForRequest(7501, success, nil, now.Add(time.Duration(index)*time.Second), service.openAIStickyEscapeConfig(), false, settings.smartEnabled, 0, true)
			}
			require.Equal(t, openAIAccountHealthDegraded, stats.health(7501, now.Add(9*time.Second), service.openAIStickyEscapeConfig()).level,
				"S=true uses the dedicated 10% threshold, independent of A and sticky_escape_error_rate=0.5")
		})
	}

	t.Run("smart_off_advanced_on_keeps_configured_ttft_threshold", func(t *testing.T) {
		service := &OpenAIGatewayService{cfg: &config.Config{}, rateLimitService: newOpenAISchedulerModeRateLimitService(false, true)}
		service.cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
		service.cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
		service.cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
		settings := service.openAIAdvancedSchedulerRuntimeSettings(ctx)
		require.False(t, settings.smartEnabled)
		require.True(t, settings.enabled)
		stats := newOpenAIAccountRuntimeStats()
		now := time.Unix(1775001000, 0).UTC()
		legacySamples := []int{12590, 7269, 6853}
		for index, ttft := range legacySamples {
			stats.reportAtForRequest(7502, true, &ttft, now.Add(time.Duration(index)*time.Second), service.openAIStickyEscapeConfig(), false, false, 0, true)
		}
		require.NotEqual(t, openAIAccountHealthDegraded, stats.health(7502, now.Add(3*time.Minute), service.openAIStickyEscapeConfig()).level)
		_, legacyEWMA, hasTTFT := stats.snapshot(7502)
		require.True(t, hasTTFT)
		require.InDelta(t, 10591.24, legacyEWMA, 1e-9, "legacy EWMA remains active for S=false")
		stat := openAIAccountRuntimeStatForTest(t, stats, 7502)
		stat.mu.Lock()
		require.False(t, stat.smartMode, "S=false must not create smart windows")
		require.Empty(t, stat.smartTTFTWindow)
		stat.mu.Unlock()
	})

	t.Run("smart_off_advanced_off_remains_legacy", func(t *testing.T) {
		service := &OpenAIGatewayService{rateLimitService: newOpenAISchedulerModeRateLimitService(false, false)}
		settings := service.openAIAdvancedSchedulerRuntimeSettings(ctx)
		require.False(t, settings.smartEnabled)
		require.False(t, settings.enabled)
	})
}

type failingSmartHealthSettingsRepo struct{ SettingRepository }

func (f failingSmartHealthSettingsRepo) GetMultiple(context.Context, []string) (map[string]string, error) {
	return nil, errors.New("settings unavailable")
}

func (f failingSmartHealthSettingsRepo) GetValue(context.Context, string) (string, error) {
	return "", errors.New("settings unavailable")
}

func TestOpenAIGatewayService_SmartHealthSnapshotReadOnly(t *testing.T) {
	ctx := context.Background()
	accounts := []*Account{
		{ID: 7601, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{ID: 7602, Platform: PlatformAnthropic, Type: AccountTypeAPIKey},
		{ID: 7603, Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
	}

	t.Run("does_not_initialize_unpublished_stats", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		service := &OpenAIGatewayService{rateLimitService: newOpenAISchedulerModeRateLimitService(true, false)}
		require.Nil(t, service.SmartHealthSnapshot(ctx, accounts))
		require.Nil(t, service.openaiAccountStats)
		require.Nil(t, service.openaiAccountStatsPublished.Load())
		require.Nil(t, service.openaiScheduler)
	})

	t.Run("settings_failure_omits_snapshot", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		service := &OpenAIGatewayService{
			rateLimitService: &RateLimitService{settingService: NewSettingService(failingSmartHealthSettingsRepo{}, &config.Config{})},
		}
		require.Nil(t, service.SmartHealthSnapshot(ctx, accounts))
		require.Nil(t, service.openaiScheduler)
	})

	t.Run("smart_off_omits_published_degraded_state", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		service := &OpenAIGatewayService{rateLimitService: newOpenAISchedulerModeRateLimitService(false, true)}
		stats := newOpenAIAccountRuntimeStats()
		seedSmartDegradedAccount(t, stats, 7601, "error_rate", time.Now())
		service.openaiAccountStatsPublished.Store(stats)
		require.Nil(t, service.SmartHealthSnapshot(ctx, accounts))
	})

	t.Run("published_stats_are_read_atomically_for_applicable_accounts", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		service := &OpenAIGatewayService{rateLimitService: newOpenAISchedulerModeRateLimitService(true, false)}
		first := newOpenAIAccountRuntimeStats()
		second := newOpenAIAccountRuntimeStats()
		now := time.Now().UTC()
		seedSmartDegradedAccount(t, first, 7601, "error_rate", now)
		seedSmartDegradedAccount(t, second, 7601, "ttft", now)
		seedSmartDegradedAccount(t, first, 7602, "error_rate", now)
		openAIAccountRuntimeStatForTest(t, first, 7601).nextProbeAt.Store(now.Add(15 * time.Minute).UnixNano())
		openAIAccountRuntimeStatForTest(t, first, 7602).nextProbeAt.Store(now.Add(15 * time.Minute).UnixNano())
		service.openaiAccountStats = first
		service.openaiAccountStatsPublished.Store(first)
		initial := service.SmartHealthSnapshot(ctx, accounts)
		require.Contains(t, initial, int64(7601))
		require.Equal(t, "cooldown", initial[7601].State)
		require.Equal(t, "error_rate", initial[7601].Reason)
		require.Equal(t, 1, initial[7601].BackoffStep)
		require.NotNil(t, initial[7601].NextProbeAt)
		require.Equal(t, time.UTC, initial[7601].NextProbeAt.Location())
		require.NotContains(t, initial, int64(7602), "non-OpenAI-compatible accounts are not exposed")
		require.NotContains(t, initial, int64(7603), "unknown accounts without a runtime stat are omitted")

		var readers sync.WaitGroup
		for reader := 0; reader < 8; reader++ {
			readers.Add(1)
			go func() {
				defer readers.Done()
				for iteration := 0; iteration < 50; iteration++ {
					snapshot := service.SmartHealthSnapshot(ctx, accounts)
					require.Contains(t, snapshot, int64(7601))
					require.Contains(t, []string{"error_rate", "ttft"}, snapshot[7601].Reason)
				}
			}()
		}
		for iteration := 0; iteration < 100; iteration++ {
			if iteration%2 == 0 {
				service.openaiAccountStatsPublished.Store(second)
			} else {
				service.openaiAccountStatsPublished.Store(first)
			}
		}
		readers.Wait()
		require.Same(t, first, service.openaiAccountStats)
		require.Nil(t, service.openaiScheduler, "snapshot reads do not initialize the scheduler")
	})
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthProbeDecisionLog(t *testing.T) {
	previousLogger := slog.Default()
	logHandler := &schedulerTestLogHandler{}
	slog.SetDefault(slog.New(logHandler))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	t.Run("only_slot_acquired_probe_is_counted_and_logs_are_redacted", func(t *testing.T) {
		groupID := int64(7701)
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(77011, groupID, 10),
			newSmartSchedulerTestAccount(77012, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, 77011, "error_rate", time.Now())
		selection := selectSmartAccountForTest(t, svc, groupID, "smart-probe-log")
		require.NotZero(t, selection.Account.smartHealthProbeID)
		require.Equal(t, int64(1), svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal)
		svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, nil)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		var decision *schedulerTestLogRecord
		records := logHandler.snapshot()
		for i := range records {
			if records[i].message == "openai_account_schedule_decision" && records[i].attrs["group_id"] == groupID {
				decision = &records[i]
			}
		}
		require.NotNil(t, decision)
		require.Equal(t, "selected", decision.attrs["reason"])
		for _, record := range records {
			for _, sensitiveKey := range []string{"api_key", "credential", "prompt", "body", "session_hash", "previous_response_id", "response_id"} {
				_, present := record.attrs[sensitiveKey]
				require.False(t, present, "sensitive field %q in %s", sensitiveKey, record.message)
			}
		}
	})

	t.Run("wait_plan_does_not_count_as_probe", func(t *testing.T) {
		groupID := int64(7702)
		concurrency := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{77021: false, 77022: false}}
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(77021, groupID, 10),
			newSmartSchedulerTestAccount(77022, groupID, 20),
		}, concurrency)
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, 77021, "error_rate", time.Now())
		selection := selectSmartAccountForTest(t, svc, groupID, "smart-probe-wait-log")
		require.NotNil(t, selection.WaitPlan)
		require.Zero(t, svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal)
		require.Zero(t, selection.Account.smartHealthProbeID)
	})

	for _, advanced := range []bool{false, true} {
		name := "advanced_off"
		if advanced {
			name = "advanced_on"
		}
		t.Run("smart_off_"+name+"_does_not_probe", func(t *testing.T) {
			groupID := int64(7703)
			svc := newSmartSchedulerTestService([]Account{
				newSmartSchedulerTestAccount(77031, groupID, 10),
				newSmartSchedulerTestAccount(77032, groupID, 20),
			}, schedulerTestConcurrencyCache{})
			svc.rateLimitService = newOpenAISchedulerModeRateLimitService(false, advanced)
			var scheduler *defaultOpenAIAccountScheduler
			if advanced {
				var ok bool
				scheduler, ok = svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
				require.True(t, ok)
				for index := 0; index < 4; index++ {
					scheduler.stats.report(77031, false, nil)
				}
			}
			selection := selectSmartAccountForTest(t, svc, groupID, "smart-off-no-probe")
			require.Zero(t, selection.Account.smartHealthProbeID)
			require.Zero(t, svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal)
			if scheduler != nil {
				stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, 77031)
				stat.mu.Lock()
				require.False(t, stat.smartMode)
				require.Empty(t, stat.smartErrorWindow)
				stat.mu.Unlock()
			} else {
				require.Nil(t, svc.openaiAccountStats, "S=false/A=false remains on the legacy path")
			}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_DefaultDisabled_RequiredWSV2_NoAvailableAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10109)
	accounts := []Account{
		{
			ID:          36021,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
	}
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportResponsesWebsocketV2,
		false,
	)
	require.ErrorContains(t, err, "no available OpenAI accounts")
	require.Nil(t, selection)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_DefaultDisabled_EmbeddingsSkipsChatOnlyAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10110)
	accounts := []Account{
		{
			ID:          36031,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			Credentials: map[string]any{
				"openai_capabilities": []any{"chat_completions"},
			},
		},
		{
			ID:          36032,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			Credentials: map[string]any{
				"openai_capabilities": []any{"chat_completions", "embeddings"},
			},
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"",
		"",
		"text-embedding-3-small",
		nil,
		OpenAIUpstreamTransportHTTPSSE,
		OpenAIEndpointCapabilityEmbeddings,
		false,
		false,
		true,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(36032), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
}

func TestOpenAIGatewayService_SelectAccountForTokenCount_DoesNotAcquireGenerationSlot(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10115)
	acquiredIDs := make([]int64, 0)
	accounts := []Account{
		{
			ID: 36501, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
			Credentials: map[string]any{"openai_capabilities": []any{"chat_completions"}},
		},
		{
			ID: 36502, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5,
			Credentials: map[string]any{"openai_capabilities": []any{"embeddings"}},
		},
		{
			ID: 36503, Platform: PlatformGrok, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10,
			Credentials: map[string]any{"openai_capabilities": []any{"chat_completions"}},
		},
		{
			ID: 36504, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
			Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 15,
			Credentials: map[string]any{
				"openai_capabilities": []any{"chat_completions"},
				"model_mapping":       map[string]any{"gpt-4o": "gpt-4o"},
			},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:       &schedulerTestGatewayCache{},
		cfg:         &config.Config{},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{36501: false},
			acquiredIDs:    &acquiredIDs,
		}),
	}

	account, err := svc.SelectAccountForTokenCount(
		ctx,
		&groupID,
		"",
		"gpt-5.1",
		OpenAIEndpointCapabilityChatCompletions,
		PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(36501), account.ID)
	require.Empty(t, acquiredIDs, "token counting must not acquire a generation slot")
}

// 生图意图的 /v1/responses 请求要求 OpenAIEndpointCapabilityResponses：探测确认
// 不支持 Responses API 的 APIKey 账号必须被排除，避免 forward 阶段降级为无法生图
// 的 Chat Completions 直转（#4417）。
func TestOpenAIGatewayService_SelectAccountWithScheduler_ResponsesCapabilityExcludesUnsupportedAPIKey(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10120)

	newSvc := func(accounts []Account) *OpenAIGatewayService {
		cfg := &config.Config{}
		cfg.Gateway.Scheduling.LoadBatchEnabled = false
		return &OpenAIGatewayService{
			accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
			cache:              &schedulerTestGatewayCache{},
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		}
	}

	supported := Account{
		ID: 37001, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
	}
	// 更高优先级但探测确认不支持 Responses——若门控失效会被优先选中。
	unsupported := Account{
		ID: 37002, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5,
		Extra: map[string]any{"openai_responses_supported": false},
	}

	t.Run("生图意图仅选中支持 responses 的账号", func(t *testing.T) {
		svc := newSvc([]Account{supported, unsupported})
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "gpt-image-2", nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityResponses,
			false, false, false,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(37001), selection.Account.ID)
	})

	t.Run("仅有不支持 responses 的账号时生图意图无可用账号", func(t *testing.T) {
		svc := newSvc([]Account{unsupported})
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "gpt-image-2", nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityResponses,
			false, false, false,
		)
		require.Error(t, err)
		require.Nil(t, selection)
	})

	t.Run("非生图路径仍可选中不支持 responses 的账号", func(t *testing.T) {
		svc := newSvc([]Account{unsupported})
		selection, _, err := svc.SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "gpt-5.1", nil,
			OpenAIUpstreamTransportAny, OpenAIEndpointCapabilityChatCompletions,
			false, false, true,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, int64(37002), selection.Account.ID)
	})
}

// alpha/search 调度必须同时放行 OAuth 与 APIKey 账号：v0.1.157 曾因 OAuth-only
// 门控把 APIKey 账号从候选池剔除，纯 APIKey 分组的独立搜索请求在选号阶段就
// 报无可用账号，Codex 网页搜索整体失效（转发层其实一直支持 APIKey 路径）。
func TestOpenAIGatewayService_SelectAccountWithScheduler_AlphaSearchAllowsAPIKeyAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10125)
	accounts := []Account{
		{
			ID:          38001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, _, err := svc.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"",
		"",
		"gpt-5.6-sol",
		nil,
		OpenAIUpstreamTransportHTTPSSE,
		OpenAIEndpointCapabilityAlphaSearch,
		false,
		false,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(38001), selection.Account.ID)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_DefaultDisabled_AllowsGrokChatAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10113)
	accounts := []Account{
		{
			ID:          36041,
			Platform:    PlatformGrok,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"",
		"",
		"grok-4.3",
		nil,
		OpenAIUpstreamTransportAny,
		OpenAIEndpointCapabilityChatCompletions,
		false,
		false,
		false,
		PlatformGrok,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(36041), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_GrokMediaCapabilityFiltersIneligibleAccounts(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10114)
	ineligible := Account{
		ID: 36051, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5,
		Extra: map[string]any{GrokMediaEligibleExtraKey: false},
	}
	eligible := Account{
		ID: 36052, Platform: PlatformGrok, Type: AccountTypeOAuth,
		Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0,
		Extra: map[string]any{GrokMediaEligibleExtraKey: true},
	}
	newService := func(accounts []Account) *OpenAIGatewayService {
		cfg := &config.Config{}
		cfg.Gateway.Scheduling.LoadBatchEnabled = false
		return &OpenAIGatewayService{
			accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
			cache:              &schedulerTestGatewayCache{},
			cfg:                cfg,
			concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		}
	}

	t.Run("media generation skips higher priority ineligible account", func(t *testing.T) {
		selection, _, err := newService([]Account{ineligible, eligible}).SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "grok-imagine-video", nil,
			OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityGrokMediaGeneration,
			false, false, false, PlatformGrok,
		)

		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, eligible.ID, selection.Account.ID)
	})

	t.Run("media generation fails closed when all accounts are ineligible", func(t *testing.T) {
		selection, _, err := newService([]Account{ineligible}).SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "grok-imagine-video", nil,
			OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityGrokMediaGeneration,
			false, false, false, PlatformGrok,
		)

		require.Error(t, err)
		require.ErrorIs(t, err, ErrNoAvailableAccounts)
		require.Nil(t, selection)
	})

	t.Run("chat remains routable on media-ineligible account", func(t *testing.T) {
		selection, _, err := newService([]Account{ineligible}).SelectAccountWithSchedulerForCapability(
			ctx, &groupID, "", "", "grok-4.3", nil,
			OpenAIUpstreamTransportHTTPSSE, OpenAIEndpointCapabilityChatCompletions,
			false, false, false, PlatformGrok,
		)

		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, ineligible.ID, selection.Account.ID)
	})
}

// Regression #4599: when the advanced scheduler's load-balance initial filter
// drops every candidate, the resulting "no available OpenAI accounts" error must
// carry per-reason exclusion counts. Previously all filter branches were silent
// (debug logs at best), so a 503 with excluded_account_count=0 could not be
// diagnosed from the error alone.
func TestOpenAIGatewayService_SelectAccountWithScheduler_NoAvailableErrorReportsQuotaAutoPauseExclusion(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 0.9})
	groupID := int64(101201)
	accounts := []Account{
		{
			ID:          38101,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Extra: map[string]any{
				"codex_7d_used_percent":  95.0,
				"codex_7d_reset_at":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
				"codex_usage_updated_at": time.Now().Add(-time.Minute).Format(time.RFC3339),
			},
		},
	}
	cfg := &config.Config{}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "gpt-5.4-mini", nil, OpenAIUpstreamTransportAny, false,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.EqualError(t, err, "no available OpenAI accounts supporting model: gpt-5.4-mini (pool=1, filtered: quota_auto_pause_7d=1)")
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_NoAvailableErrorReportsModelNotSupported(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(101202)
	// OAuth account with empty model_mapping: foreign-family models (grok-*)
	// are not servable by Codex upstream (#3662) and are dropped by the filter.
	accounts := []Account{
		{
			ID:          38111,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "grok-4.5", nil, OpenAIUpstreamTransportAny, false,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	require.EqualError(t, err, "no available OpenAI accounts supporting model: grok-4.5 (pool=1, filtered: model_not_supported=1)")
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_NoAvailableErrorAggregatesReasonsDeterministically(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold7d: 0.9})
	groupID := int64(101203)
	quotaPaused := Account{
		ID:          38121,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra: map[string]any{
			"codex_7d_used_percent":  95.0,
			"codex_7d_reset_at":      time.Now().Add(24 * time.Hour).Format(time.RFC3339),
			"codex_usage_updated_at": time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	mappingMiss := Account{
		ID:          38122,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"gpt-4o": "gpt-4o"},
		},
	}
	excluded := Account{
		ID:          38123,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{quotaPaused, mappingMiss, excluded}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "gpt-5.4-mini", map[int64]struct{}{38123: {}}, OpenAIUpstreamTransportAny, false,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	// Reasons are sorted lexicographically, so the message is deterministic.
	require.EqualError(t, err, "no available OpenAI accounts supporting model: gpt-5.4-mini (pool=3, filtered: excluded=1 model_not_supported=1 quota_auto_pause_7d=1)")
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_NoAvailableErrorReportsEmptyPool(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(101204)
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{},
		cache:              &schedulerTestGatewayCache{},
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(
		ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false,
	)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrNoAvailableAccounts)
	require.Nil(t, selection)
	require.EqualError(t, err, "no available OpenAI accounts supporting model: gpt-5.1 (pool=0)")
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_EnabledUsesAdvancedPreviousResponseRouting(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10107)
	accounts := []Account{
		{
			ID:          37001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
		{
			ID:          37002,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_enabled_001", 37001, time.Hour))
	require.True(t, svc.isOpenAIAdvancedSchedulerEnabled(ctx))

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"resp_enabled_001",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37001), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	require.True(t, decision.StickyPreviousHit)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_DefaultDisabledUsesLegacyPreviousResponseRouting(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10108)
	accounts := []Account{
		{
			ID:          37011,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			GroupIDs:    []int64{groupID},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
		{
			ID:          37012,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAISchedulerModeRateLimitService(false, false),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_legacy_001", 37011, time.Hour))
	require.Nil(t, svc.getOpenAIAccountScheduler(ctx), "both scheduler switches are disabled")

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"resp_legacy_001",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37011), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	require.True(t, decision.StickyPreviousHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_StickyWeightedSessionInTopKUsesStickyFirst(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(101071)
	accounts := []Account{
		{
			ID:          37101,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    100,
			GroupIDs:    []int64{groupID},
		},
		{
			ID:          37102,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 2
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 0.7
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 0.8
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 0.5
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.SessionSticky = 3
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{
		"openai:session_hash_weighted_topk": 37101,
	}}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_hash_weighted_topk",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37101), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.True(t, decision.StickySessionHit)
	require.Equal(t, 2, decision.TopK)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_StickyWeightedPreviousRequiresMovableContext(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(101072)
	accounts := []Account{
		{
			ID:          37111,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    100,
			GroupIDs:    []int64{groupID},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
		{
			ID:          37112,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
	}
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.OpenAIWS.LBTopK = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 0.7
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 0.8
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 0.5
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_weighted_unmovable", 37111, time.Hour))

	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"resp_weighted_unmovable",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		OpenAIEndpointCapabilityChatCompletions,
		false,
		false,
		true,
		PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37111), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	require.True(t, decision.StickyPreviousHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	selection, decision, err = svc.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"resp_weighted_unmovable",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		OpenAIEndpointCapabilityChatCompletions,
		false,
		true,
		true,
		PlatformOpenAI,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37112), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickyPreviousHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_PreviousResponseCompactUnsupportedDeletesBinding(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(101073)
	accounts := []Account{
		{
			ID:          37121,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
				"openai_compact_mode":                           OpenAICompactModeForceOff,
			},
		},
		{
			ID:          37122,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    10,
			GroupIDs:    []int64{groupID},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
				"openai_compact_mode":                           OpenAICompactModeForceOn,
			},
		},
	}
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.OpenAIWS.LBTopK = 2
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 0.7
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 0.8
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 0.5
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_compact_unsupported", 37121, time.Hour))

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"resp_compact_unsupported",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		true,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37122), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickyPreviousHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	accountID, err := store.GetResponseAccount(ctx, groupID, "resp_compact_unsupported")
	require.NoError(t, err)
	require.Zero(t, accountID)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_Enabled_EmbeddingsSkipsChatOnlyAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10111)
	accounts := []Account{
		{
			ID:          37011,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			Credentials: map[string]any{
				"openai_capabilities": []any{"chat_completions"},
			},
		},
		{
			ID:          37012,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			Credentials: map[string]any{
				"openai_capabilities": []any{"chat_completions", "embeddings"},
			},
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"",
		"",
		"text-embedding-3-small",
		nil,
		OpenAIUpstreamTransportHTTPSSE,
		OpenAIEndpointCapabilityEmbeddings,
		false,
		false,
		true,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37012), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, 1, decision.CandidateCount)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_Enabled_EmbeddingsSkipsChatOnlyStickyBindings(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(10112)
	accounts := []Account{
		{
			ID:          37021,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			Credentials: map[string]any{
				"openai_capabilities": []any{"chat_completions"},
			},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
		{
			ID:          37022,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			Credentials: map[string]any{
				"openai_capabilities": []any{"chat_completions", "embeddings"},
			},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
	}
	cfg := newSchedulerTestOpenAIWSV2Config()
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:session_hash_embeddings": 37021,
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_embeddings_chat_only", 37021, time.Hour))

	selection, decision, err := svc.SelectAccountWithSchedulerForCapability(
		ctx,
		&groupID,
		"resp_embeddings_chat_only",
		"session_hash_embeddings",
		"text-embedding-3-small",
		nil,
		OpenAIUpstreamTransportHTTPSSE,
		OpenAIEndpointCapabilityEmbeddings,
		false,
		false,
		true,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37022), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickyPreviousHit)
	require.False(t, decision.StickySessionHit)
	require.Equal(t, int64(37022), cache.sessionBindings["openai:session_hash_embeddings"])
}

func TestOpenAIGatewayService_OpenAIAccountSchedulerMetrics_DisabledNoOp(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	svc := &OpenAIGatewayService{}
	ttft := 120
	svc.ReportOpenAIAccountScheduleResult(&Account{ID: 10}, "", true, &ttft)
	svc.RecordOpenAIAccountSwitch()

	snapshot := svc.SnapshotOpenAIAccountSchedulerMetrics()
	require.Equal(t, OpenAIAccountSchedulerMetricsSnapshot{}, snapshot)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SkipsQuarantinedSharedProxy(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	proxyA := int64(4698)
	proxyB := int64(4699)
	accounts := []Account{
		{ID: 469801, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, ProxyID: &proxyA},
		{ID: 469802, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, ProxyID: &proxyA},
		{ID: 469803, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, ProxyID: &proxyB},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		openaiProxyStreamCircuit: newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
			failureThreshold: 1,
			failureWindow:    time.Minute,
			quarantineTTL:    10 * time.Minute,
			maxEntries:       16,
		}),
	}
	svc.openaiProxyStreamCircuit.recordFailure(proxyA, time.Now())

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), nil, "", "", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(469803), selection.Account.ID)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_FailsOpenWhenAllProxiesQuarantined(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	// Every schedulable account shares the quarantined proxy: the circuit must
	// degrade to a preference instead of zeroing out capacity (#5056).
	proxyA := int64(5056)
	accounts := []Account{
		{ID: 505601, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, ProxyID: &proxyA},
		{ID: 505602, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, ProxyID: &proxyA},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.LoadBatchEnabled = false
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cfg:                cfg,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
		openaiProxyStreamCircuit: newOpenAIProxyStreamCircuit(openAIProxyStreamCircuitSettings{
			failureThreshold: 1,
			failureWindow:    time.Minute,
			quarantineTTL:    10 * time.Minute,
			maxEntries:       16,
		}),
	}
	tripped, _ := svc.openaiProxyStreamCircuit.recordFailure(proxyA, time.Now())
	require.True(t, tripped)

	selection, _, err := svc.SelectAccountWithScheduler(
		context.Background(), nil, "", "", "gpt-5.6-sol", nil, OpenAIUpstreamTransportAny, false,
	)
	require.NoError(t, err, "quarantine must fail open instead of returning no available accounts")
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.NotNil(t, selection.Account.ProxyID)
	require.Equal(t, proxyA, *selection.Account.ProxyID)
	require.True(t, svc.openaiProxyStreamCircuit.isBlocked(proxyA, time.Now()),
		"fail-open must not clear the quarantine; only a completed stream or TTL expiry does")
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionStickyRateLimitedAccountFallsBackToFreshCandidate(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10101)
	rateLimitedUntil := time.Now().Add(30 * time.Minute)
	staleSticky := &Account{ID: 31001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}}
	staleBackup := &Account{ID: 31002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	freshSticky := &Account{ID: 31001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, RateLimitResetAt: &rateLimitedUntil}
	freshBackup := &Account{ID: 31002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_rate_limited": 31001}}
	snapshotCache := &openAISnapshotCacheStub{snapshotAccounts: []*Account{staleSticky, staleBackup}, accountsByID: map[int64]*Account{31001: freshSticky, 31002: freshBackup}}
	snapshotService := &SchedulerSnapshotService{cache: snapshotCache}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{*freshSticky, *freshBackup}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		schedulerSnapshot:  snapshotService,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_rate_limited", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(31002), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_AutoPauseBy5hThreshold(t *testing.T) {
	ctx := context.Background()
	primary := Account{
		ID:          35001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":   95.0,
			"auto_pause_5h_threshold": 0.95,
		},
	}
	secondary := Account{ID: 35002, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35002), account.ID)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_AllowsBelow5hThreshold(t *testing.T) {
	ctx := context.Background()
	primary := Account{
		ID:          35101,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":   80.0,
			"auto_pause_5h_threshold": 0.95,
		},
	}
	secondary := Account{ID: 35102, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35101), account.ID)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_AutoPauseBy7dThreshold(t *testing.T) {
	ctx := context.Background()
	primary := Account{
		ID:          35201,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_7d_used_percent":   95.0,
			"auto_pause_7d_threshold": 0.95,
		},
	}
	secondary := Account{ID: 35202, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35202), account.ID)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_UnconfiguredThresholdKeepsLegacyBehavior(t *testing.T) {
	ctx := context.Background()
	primary := Account{ID: 35301, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, Extra: map[string]any{"codex_5h_used_percent": 99.0, "codex_7d_used_percent": 99.0}}
	secondary := Account{ID: 35302, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35301), account.ID)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_UsesGlobalDefaultThreshold(t *testing.T) {
	ctx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.95})
	primary := Account{
		ID:          35401,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent": 95.0,
		},
	}
	secondary := Account{ID: 35402, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35402), account.ID)
}

// Regression: a per-account explicit-disable flag exempts the account from auto-pause
// even when a global default threshold is set. Without this, "leave threshold blank"
// silently falls back to global default and admins have no way to whitelist a single
// account.
func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_PerAccountDisableOverridesGlobalDefault(t *testing.T) {
	ctx := withOpenAIQuotaAutoPauseSettings(context.Background(), OpsOpenAIAccountQuotaAutoPauseSettings{DefaultThreshold5h: 0.95})
	// Account has high usage AND no per-account threshold (would normally fall back to
	// the global default and get paused), but the explicit disable flag is set.
	primary := Account{
		ID:          35701,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":  99.0,
			"auto_pause_5h_disabled": true,
		},
	}
	secondary := Account{ID: 35702, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35701), account.ID)
}

// Disable is per-window: disabling only 5h must still allow 7d auto-pause to fire.
func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_PerWindowDisableScoped(t *testing.T) {
	ctx := context.Background()
	primary := Account{
		ID:          35801,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":   99.0,
			"codex_7d_used_percent":   99.0,
			"auto_pause_5h_disabled":  true,
			"auto_pause_7d_threshold": 0.95,
		},
	}
	secondary := Account{ID: 35802, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35802), account.ID, "7d auto-pause must still fire even though 5h is disabled")
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_StaleUsageWindowResetSkipsPause(t *testing.T) {
	ctx := context.Background()
	// Usage is over threshold but the window's reset time has already passed, so the
	// cached percentage is stale (the real window rolled over) and the account must NOT
	// stay paused — otherwise it could be skipped forever with no traffic to refresh it.
	primary := Account{
		ID:          35501,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":   99.0,
			"auto_pause_5h_threshold": 0.95,
			"codex_5h_reset_at":       time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	secondary := Account{ID: 35502, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35501), account.ID)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_FreshUsageWindowStillPauses(t *testing.T) {
	ctx := context.Background()
	// Same as above but the window has not reset yet, so the account stays paused.
	primary := Account{
		ID:          35601,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":   99.0,
			"auto_pause_5h_threshold": 0.95,
			"codex_5h_reset_at":       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
	}
	secondary := Account{ID: 35602, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35602), account.ID)
}

// Issue #2994: an account poisoned with an inflated used% (e.g. from the reverted #2918
// inversion) gets excluded from scheduling, and a paused account never receives traffic to
// refresh its snapshot. When the snapshot is stale (codex_usage_updated_at older than the
// staleness bound) the account must be allowed a request so it can self-heal from the real
// response headers when no valid future window reset is known.
func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_StaleUsageSnapshotSkipsPause_Issue2994(t *testing.T) {
	ctx := context.Background()
	primary := Account{
		ID:          35701,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":   99.0,
			"auto_pause_5h_threshold": 0.95,
			// No reset time is known, so stale snapshot self-heal applies.
			// Snapshot is stale: older than openAICodexAutoPauseStaleAfter (2h).
			"codex_usage_updated_at": time.Now().Add(-3 * time.Hour).Format(time.RFC3339),
		},
	}
	secondary := Account{ID: 35702, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35701), account.ID)
}

// Issue #2994 guardrail: a genuinely-exhausted account whose snapshot was refreshed recently
// (codex_usage_updated_at fresh) must STILL be auto-paused. The stale self-heal must not let a
// real 99%-used account escape pause.
func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_FreshExhaustedSnapshotStillPauses_Issue2994(t *testing.T) {
	ctx := context.Background()
	primary := Account{
		ID:          35801,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			"codex_5h_used_percent":   99.0,
			"auto_pause_5h_threshold": 0.95,
			"codex_5h_reset_at":       time.Now().Add(time.Hour).Format(time.RFC3339),
			// Snapshot refreshed 1 minute ago: not stale, so the account stays paused.
			"codex_usage_updated_at": time.Now().Add(-time.Minute).Format(time.RFC3339),
		},
	}
	secondary := Account{ID: 35802, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5}
	svc := &OpenAIGatewayService{accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}}, cfg: &config.Config{}}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(35802), account.ID)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_SkipsFreshlyRateLimitedSnapshotCandidate(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10102)
	rateLimitedUntil := time.Now().Add(30 * time.Minute)
	stalePrimary := &Account{ID: 32001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}}
	staleSecondary := &Account{ID: 32002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	freshPrimary := &Account{ID: 32001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, RateLimitResetAt: &rateLimitedUntil}
	freshSecondary := &Account{ID: 32002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	snapshotCache := &openAISnapshotCacheStub{snapshotAccounts: []*Account{stalePrimary, staleSecondary}, accountsByID: map[int64]*Account{32001: freshPrimary, 32002: freshSecondary}}
	snapshotService := &SchedulerSnapshotService{cache: snapshotCache}
	svc := &OpenAIGatewayService{
		accountRepo:       schedulerTestOpenAIAccountRepo{accounts: []Account{*freshPrimary, *freshSecondary}},
		cfg:               &config.Config{},
		rateLimitService:  newOpenAIAdvancedSchedulerRateLimitService("true"),
		schedulerSnapshot: snapshotService,
	}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(32002), account.ID)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_ModelRateLimitOnlySkipsThatModel(t *testing.T) {
	ctx := context.Background()
	resetAt := time.Now().Add(30 * time.Minute).Format(time.RFC3339)
	primary := Account{
		ID:          32101,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    0,
		Extra: map[string]any{
			modelRateLimitsKey: map[string]any{
				"gpt-5.4": map[string]any{
					"rate_limit_reset_at": resetAt,
				},
			},
		},
	}
	secondary := Account{
		ID:          32102,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Priority:    5,
	}
	svc := &OpenAIGatewayService{
		accountRepo: schedulerTestOpenAIAccountRepo{accounts: []Account{primary, secondary}},
		cfg:         &config.Config{},
	}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.4", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(32102), account.ID)

	account, err = svc.SelectAccountForModelWithExclusions(ctx, nil, "", "gpt-5.3", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(32101), account.ID)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionStickyDBRuntimeRecheckSkipsStaleCachedAccount(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10103)
	rateLimitedUntil := time.Now().Add(30 * time.Minute)
	staleSticky := &Account{ID: 33001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}}
	staleBackup := &Account{ID: 33002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	dbSticky := Account{ID: 33001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, RateLimitResetAt: &rateLimitedUntil}
	dbBackup := Account{ID: 33002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_db_runtime_recheck": 33001}}
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{staleSticky, staleBackup},
		accountsByID:     map[int64]*Account{33001: staleSticky, 33002: staleBackup},
	}
	snapshotService := &SchedulerSnapshotService{cache: snapshotCache}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{dbSticky, dbBackup}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		schedulerSnapshot:  snapshotService,
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_db_runtime_recheck", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(33002), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
}

func TestOpenAIGatewayService_SelectAccountForModelWithExclusions_DBRuntimeRecheckSkipsStaleCachedCandidate(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10104)
	rateLimitedUntil := time.Now().Add(30 * time.Minute)
	stalePrimary := &Account{ID: 34001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}}
	staleSecondary := &Account{ID: 34002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	dbPrimary := Account{ID: 34001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}, RateLimitResetAt: &rateLimitedUntil}
	dbSecondary := Account{ID: 34002, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 5, GroupIDs: []int64{groupID}}
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{stalePrimary, staleSecondary},
		accountsByID:     map[int64]*Account{34001: stalePrimary, 34002: staleSecondary},
	}
	snapshotService := &SchedulerSnapshotService{cache: snapshotCache}
	svc := &OpenAIGatewayService{
		accountRepo:       schedulerTestOpenAIAccountRepo{accounts: []Account{dbPrimary, dbSecondary}},
		cfg:               &config.Config{},
		rateLimitService:  newOpenAIAdvancedSchedulerRateLimitService("true"),
		schedulerSnapshot: snapshotService,
	}

	account, err := svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, account)
	require.Equal(t, int64(34002), account.ID)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_DBFreshGroupRecheckReleasesMovedAccount(t *testing.T) {
	ctx := context.Background()
	groupID, otherGroupID := int64(10105), int64(10106)
	stalePrimary := &Account{ID: 34101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}}
	staleBackup := &Account{ID: 34102, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10, GroupIDs: []int64{groupID}}
	dbPrimary := *stalePrimary
	dbPrimary.GroupIDs = []int64{otherGroupID}
	dbBackup := *staleBackup
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{stalePrimary, staleBackup},
		accountsByID:     map[int64]*Account{stalePrimary.ID: stalePrimary, staleBackup.ID: staleBackup},
	}
	acquiredIDs, releasedIDs := []int64{}, []int64{}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	svc := &OpenAIGatewayService{
		accountRepo:       schedulerTestOpenAIAccountRepo{accounts: []Account{dbPrimary, dbBackup}},
		cfg:               cfg,
		schedulerSnapshot: &SchedulerSnapshotService{cache: snapshotCache},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquiredIDs: &acquiredIDs,
			releasedIDs: &releasedIDs,
		}),
	}
	scheduler := &defaultOpenAIAccountScheduler{service: svc}

	selection, _, err := scheduler.tryAcquireOpenAISelectionOrder(ctx, OpenAIAccountScheduleRequest{
		GroupID: &groupID, Platform: PlatformOpenAI, RequestedModel: "gpt-5.1",
	}, []openAIAccountCandidateScore{
		{account: stalePrimary, loadInfo: &AccountLoadInfo{AccountID: stalePrimary.ID}},
		{account: staleBackup, loadInfo: &AccountLoadInfo{AccountID: staleBackup.ID}},
	})
	require.NoError(t, err)
	require.Equal(t, staleBackup.ID, selection.Account.ID)
	require.Equal(t, []int64{stalePrimary.ID, staleBackup.ID}, acquiredIDs)
	require.Equal(t, []int64{stalePrimary.ID}, releasedIDs)
	selection.ReleaseFunc()
}

func TestOpenAIGatewayService_SelectAccountWithLoadAwareness_DBFreshGroupRecheckWaitsOnValidAccount(t *testing.T) {
	ctx := context.Background()
	groupID, otherGroupID := int64(10107), int64(10108)
	stalePrimary := &Account{ID: 34201, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}}
	staleBackup := &Account{ID: 34202, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10, GroupIDs: []int64{groupID}}
	dbPrimary := *stalePrimary
	dbPrimary.GroupIDs = []int64{otherGroupID}
	dbBackup := *staleBackup
	snapshotCache := &openAISnapshotCacheStub{
		snapshotAccounts: []*Account{stalePrimary, staleBackup},
		accountsByID:     map[int64]*Account{stalePrimary.ID: stalePrimary, staleBackup.ID: staleBackup},
	}
	cfg := &config.Config{RunMode: config.RunModeStandard}
	cfg.Gateway.Scheduling.LoadBatchEnabled = true
	svc := &OpenAIGatewayService{
		accountRepo:       schedulerTestOpenAIAccountRepo{accounts: []Account{dbPrimary, dbBackup}},
		cfg:               cfg,
		schedulerSnapshot: &SchedulerSnapshotService{cache: snapshotCache},
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{staleBackup.ID: false},
		}),
	}

	selection, err := svc.SelectAccountWithLoadAwareness(ctx, &groupID, "", "gpt-5.1", nil)
	require.NoError(t, err)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, staleBackup.ID, selection.Account.ID)
	require.Equal(t, staleBackup.ID, selection.WaitPlan.AccountID)
}

func TestOpenAIGatewayService_RecheckSelectedOpenAIAccountFromDB_SimpleModeUsesFullPool(t *testing.T) {
	grouped := Account{ID: 34301, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, GroupIDs: []int64{99}}
	svc := &OpenAIGatewayService{
		accountRepo:       schedulerTestOpenAIAccountRepo{accounts: []Account{grouped}},
		cfg:               &config.Config{RunMode: config.RunModeSimple},
		schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{}},
	}
	requestedGroupID := int64(100)

	for _, groupID := range []*int64{nil, &requestedGroupID} {
		fresh := svc.recheckSelectedOpenAIAccountFromDB(context.Background(), &grouped, groupID, PlatformOpenAI, "gpt-5.1", false, "")
		require.NotNil(t, fresh)
		require.Equal(t, grouped.ID, fresh.ID)
	}

	ungrouped := grouped
	ungrouped.ID++
	ungrouped.GroupIDs = nil
	standardSvc := &OpenAIGatewayService{
		accountRepo:       schedulerTestOpenAIAccountRepo{accounts: []Account{grouped, ungrouped}},
		cfg:               &config.Config{RunMode: config.RunModeStandard},
		schedulerSnapshot: &SchedulerSnapshotService{cache: &openAISnapshotCacheStub{}},
	}
	require.Nil(t, standardSvc.recheckSelectedOpenAIAccountFromDB(context.Background(), &grouped, nil, PlatformOpenAI, "gpt-5.1", false, ""))
	require.NotNil(t, standardSvc.recheckSelectedOpenAIAccountFromDB(context.Background(), &ungrouped, nil, PlatformOpenAI, "gpt-5.1", false, ""))
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_PreviousResponseSticky(t *testing.T) {
	ctx := context.Background()
	groupID := int64(9)
	account := Account{
		ID:          1001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 2,
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
		},
	}
	cache := &schedulerTestGatewayCache{}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.StickySessionTTLSeconds = 1800
	cfg.Gateway.OpenAIWS.StickyResponseIDTTLSeconds = 3600

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	store := svc.getOpenAIWSStateStore()
	require.NoError(t, store.BindResponseAccount(ctx, groupID, "resp_prev_001", account.ID, time.Hour))

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"resp_prev_001",
		"session_hash_001",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, account.ID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerPreviousResponse, decision.Layer)
	require.True(t, decision.StickyPreviousHit)
	require.Equal(t, account.ID, cache.sessionBindings["openai:session_hash_001"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionSticky(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10)
	account := Account{
		ID:          2001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		GroupIDs:    []int64{groupID},
	}
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:session_hash_abc": account.ID,
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_hash_abc",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, account.ID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionStickyBusyKeepsSticky(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10100)
	accounts := []Account{
		{
			ID:          21001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		},
		{
			ID:          21002,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    9,
			GroupIDs:    []int64{groupID},
		},
	}
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:session_hash_sticky_busy": 21001,
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 45 * time.Second
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = false
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true

	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{
			21001: false, // sticky 账号已满
			21002: true,  // 若回退负载均衡会命中该账号（本测试要求不能切换）
		},
		waitCounts: map[int64]int{
			21001: 999,
		},
		loadMap: map[int64]*AccountLoadInfo{
			21001: {AccountID: 21001, LoadRate: 90, WaitingCount: 9},
			21002: {AccountID: 21002, LoadRate: 1, WaitingCount: 0},
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_hash_sticky_busy",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21001), selection.Account.ID, "busy sticky account should remain selected")
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, int64(21001), selection.WaitPlan.AccountID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionStickyEscapeByTTFT(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10101)
	accounts := []Account{
		{
			ID:          21101,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		},
		{
			ID:          21102,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    1,
			GroupIDs:    []int64{groupID},
		},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_sticky_ttft": 21101}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	concurrencyCache := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{21102: true}}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	fastTTFT := 14999
	svc.openaiAccountStats.report(21101, true, &fastTTFT)
	stableTTFT := 14999
	svc.openaiAccountStats.report(21101, true, &stableTTFT)

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_ttft", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21101), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	slowTTFT := 20000
	for i := 0; i < 3; i++ {
		svc.openaiAccountStats.report(21101, true, &slowTTFT)
	}

	selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_ttft", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21102), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	require.Equal(t, int64(21101), cache.sessionBindings["openai:session_hash_sticky_ttft"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionStickyEscapeByErrorRate(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10102)
	accounts := []Account{
		{ID: 21201, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
		{ID: 21202, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID}},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_sticky_error_rate": 21201}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{21202: true}}),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	for i := 0; i < 3; i++ {
		svc.openaiAccountStats.report(21201, false, nil)
	}
	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_error_rate", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21201), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	for i := 0; i < 2; i++ {
		svc.openaiAccountStats.report(21201, false, nil)
	}

	selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_error_rate", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21202), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	require.Equal(t, int64(21201), cache.sessionBindings["openai:session_hash_sticky_error_rate"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionStickyBusyEscapes(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10103)
	accounts := []Account{
		{ID: 21301, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
		{ID: 21302, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID}},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_sticky_busy_escape": 21301}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 45 * time.Second
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{21301: false, 21302: true},
		waitCounts:     map[int64]int{21301: 999},
		loadMap: map[int64]*AccountLoadInfo{
			21301: {AccountID: 21301, LoadRate: 95, WaitingCount: 9},
			21302: {AccountID: 21302, LoadRate: 1, WaitingCount: 0},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_busy_escape", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21302), selection.Account.ID)
	require.Nil(t, selection.WaitPlan)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionStickyEscapeDisabledKeepsLegacyBehavior(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10104)
	accounts := []Account{
		{ID: 21401, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
		{ID: 21402, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 1, GroupIDs: []int64{groupID}},
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:session_hash_sticky_disabled": 21401}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = false
	cfg.Gateway.OpenAIScheduler.StickyEscapeTTFTMs = 15000
	cfg.Gateway.OpenAIScheduler.StickyEscapeErrorRate = 0.5
	cfg.Gateway.Scheduling.StickySessionMaxWaiting = 2
	cfg.Gateway.Scheduling.StickySessionWaitTimeout = 45 * time.Second
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{21401: false, 21402: true},
		waitCounts:     map[int64]int{21401: 999},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	slowTTFT := 20000
	svc.openaiAccountStats.report(21401, true, &slowTTFT)
	for i := 0; i < 5; i++ {
		svc.openaiAccountStats.report(21401, false, nil)
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_sticky_disabled", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21401), selection.Account.ID)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, int64(21401), selection.WaitPlan.AccountID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SubscriptionPriorityChoosesSubscriptionPoolFirst(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10120)
	cheapAPIKey := upstreamCostTestAccount(21602, UpstreamBillingProbeStatusOK, 0.01, time.Now().Add(-time.Minute), 30*time.Minute)
	cheapAPIKey.Status = StatusActive
	cheapAPIKey.Schedulable = true
	cheapAPIKey.Concurrency = 1
	cheapAPIKey.Priority = 0
	cheapAPIKey.GroupIDs = []int64{groupID}
	accounts := []Account{
		{
			ID:          21601,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    10,
			GroupIDs:    []int64{groupID},
			Credentials: map[string]any{"plan_type": "plus"},
		},
		*cheapAPIKey,
	}
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{21601: true, 21602: true},
		loadMap: map[int64]*AccountLoadInfo{
			21601: {AccountID: 21601, LoadRate: 90, WaitingCount: 1},
			21602: {AccountID: 21602, LoadRate: 0, WaitingCount: 0},
		},
	}
	cfg := newSchedulerTestSubscriptionPriorityConfig()
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.UpstreamCost = 100
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "", "true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_subscription_first", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21601), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, 1, decision.TopK)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SubscriptionPriorityFallsBackWhenSubscriptionFull(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10121)
	accounts := []Account{
		{
			ID:          21611,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
			Credentials: map[string]any{"plan_type": "team"},
		},
		{
			ID:          21612,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    9,
			GroupIDs:    []int64{groupID},
		},
	}
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{21611: false, 21612: true},
		loadMap: map[int64]*AccountLoadInfo{
			21611: {AccountID: 21611, LoadRate: 0, WaitingCount: 0},
			21612: {AccountID: 21612, LoadRate: 90, WaitingCount: 1},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                newSchedulerTestSubscriptionPriorityConfig(),
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "", "true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_subscription_fallback", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21612), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.True(t, selection.Acquired)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SubscriptionPriorityDisabledUsesScore(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10122)
	accounts := []Account{
		{
			ID:          21621,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    10,
			GroupIDs:    []int64{groupID},
			Credentials: map[string]any{"plan_type": "pro"},
		},
		{
			ID:          21622,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		},
	}
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{21621: true, 21622: true},
		loadMap: map[int64]*AccountLoadInfo{
			21621: {AccountID: 21621, LoadRate: 90, WaitingCount: 1},
			21622: {AccountID: 21622, LoadRate: 0, WaitingCount: 0},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                newSchedulerTestSubscriptionPriorityConfig(),
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "", "false"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_subscription_disabled", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21622), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_UsesAccountPriorityWithinGroupPool(t *testing.T) {
	ctx := context.Background()
	groupID := int64(10123)
	accounts := []Account{
		{
			ID:          21631,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    1,
			AccountGroups: []AccountGroup{
				{AccountID: 21631, GroupID: groupID, Priority: 100},
			},
			GroupIDs: []int64{groupID},
		},
		{
			ID:          21632,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    100000,
			AccountGroups: []AccountGroup{
				{AccountID: 21632, GroupID: groupID, Priority: 1},
			},
			GroupIDs: []int64{groupID},
		},
	}
	cfg := newSchedulerTestSubscriptionPriorityConfig()
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 0
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 0
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_group_priority", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(21631), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIAccountScheduler_SkipsAccountBlockedForRequestedModel(t *testing.T) {
	now := time.Now()
	account := &Account{ID: 21633, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	svc := &OpenAIGatewayService{openaiModelTransient: newOpenAIAccountModelTransientState(128)}
	svc.openaiModelTransient.recordFailure(account.ID, "gpt-5.5", now)
	svc.openaiModelTransient.recordFailure(account.ID, "gpt-5.5", now.Add(time.Millisecond))
	scheduler := &defaultOpenAIAccountScheduler{service: svc}

	require.False(t, scheduler.isAccountRequestCompatible(context.Background(), account, OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.5"}))
	require.True(t, scheduler.isAccountRequestCompatible(context.Background(), account, OpenAIAccountScheduleRequest{RequestedModel: "gpt-5.6-sol"}))
}

func TestReportOpenAIAccountScheduleResult_SuccessClearsModelTransientState(t *testing.T) {
	svc := &OpenAIGatewayService{openaiModelTransient: newOpenAIAccountModelTransientState(128)}
	now := time.Now()
	svc.openaiModelTransient.recordFailure(21636, "gpt-5.5", now)
	svc.openaiModelTransient.recordFailure(21636, "gpt-5.5", now.Add(time.Millisecond))
	require.True(t, svc.openaiModelTransient.isBlocked(21636, "gpt-5.5", now.Add(2*time.Millisecond)))

	svc.ReportOpenAIAccountScheduleResult(&Account{ID: 21636}, "gpt-5.5", true, nil)

	require.False(t, svc.openaiModelTransient.isBlocked(21636, "gpt-5.5", now.Add(2*time.Millisecond)))
}

func TestDefaultOpenAIAccountScheduler_ShouldEscapeStickyAccount_ThresholdBoundary(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	accountID := int64(21501)
	ttft := 15000
	stats.report(accountID, true, &ttft)
	stats.report(accountID, false, nil)
	stats.report(accountID, true, nil)
	scheduler := &defaultOpenAIAccountScheduler{stats: stats}

	reason, errorRate, observedTTFT, shouldEscape := scheduler.shouldEscapeStickyAccount(accountID, openAIStickyEscapeConfig{
		enabled:   true,
		ttftMs:    15000,
		errorRate: 0.5,
	})
	require.False(t, shouldEscape)
	require.Empty(t, reason)
	require.InDelta(t, 0.16, errorRate, 1e-9)
	require.InDelta(t, 15000, observedTTFT, 1e-9)

	for i := 0; i < 4; i++ {
		stats.report(accountID, false, nil)
	}
	reason, errorRate, _, shouldEscape = scheduler.shouldEscapeStickyAccount(accountID, openAIStickyEscapeConfig{
		enabled:   true,
		ttftMs:    15000,
		errorRate: 1,
	})
	require.False(t, shouldEscape)
	require.Empty(t, reason)
	reason, errorRate, observedTTFT, shouldEscape = scheduler.shouldEscapeStickyAccount(accountID, openAIStickyEscapeConfig{
		enabled:   true,
		ttftMs:    15000,
		errorRate: errorRate,
	})
	require.False(t, shouldEscape)
	require.Empty(t, reason)
	require.InDelta(t, 0.655936, errorRate, 1e-9)
	require.InDelta(t, 15000, observedTTFT, 1e-9)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SessionSticky_ForceHTTP(t *testing.T) {
	ctx := context.Background()
	groupID := int64(1010)
	account := Account{
		ID:          2101,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		GroupIDs:    []int64{groupID},
		Extra: map[string]any{
			"openai_ws_force_http": true,
		},
	}
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:session_hash_force_http": account.ID,
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_hash_force_http",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, account.ID, selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_RequiredWSV2_SkipsStickyHTTPAccount(t *testing.T) {
	ctx := context.Background()
	groupID := int64(1011)
	accounts := []Account{
		{
			ID:          2201,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
		},
		{
			ID:          2202,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			GroupIDs:    []int64{groupID},
			Extra: map[string]any{
				"openai_apikey_responses_websockets_v2_enabled": true,
			},
		},
	}
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:session_hash_ws_only": 2201,
		},
	}
	cfg := newSchedulerTestOpenAIWSV2Config()

	// 构造“HTTP-only 账号负载更低”的场景，验证 required transport 会强制过滤。
	concurrencyCache := schedulerTestConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			2201: {AccountID: 2201, LoadRate: 0, WaitingCount: 0},
			2202: {AccountID: 2202, LoadRate: 90, WaitingCount: 5},
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_hash_ws_only",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportResponsesWebsocketV2,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(2202), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	require.Equal(t, 1, decision.CandidateCount)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_ClearsStickyAccountOutsideGroup(t *testing.T) {
	ctx := context.Background()
	groupID := int64(1013)
	accounts := []Account{
		{
			ID:          2401,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
		{
			ID:          2402,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
			AccountGroups: []AccountGroup{
				{AccountID: 2402, GroupID: groupID},
			},
		},
	}
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:session_hash_removed_group": 2401,
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_hash_removed_group",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(2402), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.False(t, decision.StickySessionHit)
	require.Equal(t, 1, cache.deletedSessions["openai:session_hash_removed_group"])
	require.Equal(t, int64(2402), cache.sessionBindings["openai:session_hash_removed_group"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_RequiredWSV2_NoAvailableAccount(t *testing.T) {
	ctx := context.Background()
	groupID := int64(1012)
	accounts := []Account{
		{
			ID:          2301,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                newSchedulerTestOpenAIWSV2Config(),
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportResponsesWebsocketV2,
		false,
	)
	require.Error(t, err)
	require.Nil(t, selection)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, 0, decision.CandidateCount)
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_LoadBalanceTopKFallback(t *testing.T) {
	ctx := context.Background()
	groupID := int64(11)
	accounts := []Account{
		{
			ID:          3001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
		{
			ID:          3002,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
		{
			ID:          3003,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
		},
	}

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 2
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 0.4
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1.0
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 1.0
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 0.2
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 0.1

	concurrencyCache := schedulerTestConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			3001: {AccountID: 3001, LoadRate: 95, WaitingCount: 8},
			3002: {AccountID: 3002, LoadRate: 20, WaitingCount: 1},
			3003: {AccountID: 3003, LoadRate: 10, WaitingCount: 0},
		},
		acquireResults: map[int64]bool{
			3003: false, // top1 失败，必须回退到 top-K 的下一候选
			3002: true,
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(3002), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.Equal(t, 3, decision.CandidateCount)
	require.Equal(t, 2, decision.TopK)
	require.Greater(t, decision.LoadSkew, 0.0)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

// Regression: TopK initial filter must drop quota-auto-paused accounts. Otherwise
// the candidate pool is filled with paused accounts, healthy accounts fall outside
// TopK, and the scheduler returns "no available accounts" even though healthy ones
// exist.
func TestOpenAIGatewayService_SelectAccountWithScheduler_LoadBalanceTopKExcludesQuotaPaused(t *testing.T) {
	ctx := context.Background()
	groupID := int64(110)
	accounts := []Account{
		{
			ID:          37001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			Extra: map[string]any{
				"codex_5h_used_percent":   96.0,
				"auto_pause_5h_threshold": 0.95,
			},
		},
		{
			ID:          37002,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    5,
		},
	}

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 1 // TopK=1 makes the bug fatal: paused account would crowd out the healthy one entirely
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 0.4
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1.0
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 1.0

	concurrencyCache := schedulerTestConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			37001: {AccountID: 37001, LoadRate: 5, WaitingCount: 0},
			37002: {AccountID: 37002, LoadRate: 5, WaitingCount: 0},
		},
		acquireResults: map[int64]bool{
			37002: true,
		},
	}

	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(37002), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	// Only the healthy account should ever enter the candidate pool; the paused one
	// must be filtered out at the initial-filter stage.
	require.Equal(t, 1, decision.CandidateCount)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_OpenAIAccountSchedulerMetrics(t *testing.T) {
	ctx := context.Background()
	groupID := int64(12)
	account := Account{
		ID:          4001,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		GroupIDs:    []int64{groupID},
	}
	cache := &schedulerTestGatewayCache{
		sessionBindings: map[string]int64{
			"openai:session_hash_metrics": account.ID,
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: []Account{account}},
		cache:              cache,
		cfg:                &config.Config{},
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}

	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "session_hash_metrics", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	svc.ReportOpenAIAccountScheduleResult(&account, "", true, intPtrForTest(120))
	svc.RecordOpenAIAccountSwitch()

	snapshot := svc.SnapshotOpenAIAccountSchedulerMetrics()
	require.GreaterOrEqual(t, snapshot.SelectTotal, int64(1))
	require.GreaterOrEqual(t, snapshot.StickySessionHitTotal, int64(1))
	require.GreaterOrEqual(t, snapshot.AccountSwitchTotal, int64(1))
	require.GreaterOrEqual(t, snapshot.SchedulerLatencyMsAvg, float64(0))
	require.GreaterOrEqual(t, snapshot.StickyHitRatio, 0.0)
	require.GreaterOrEqual(t, snapshot.RuntimeStatsAccountCount, 1)
}

func intPtrForTest(v int) *int {
	return &v
}

func TestOpenAIAccountRuntimeStats_ReportAndSnapshot(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	stats.report(1001, true, nil)
	firstTTFT := 100
	stats.report(1001, false, &firstTTFT)
	secondTTFT := 200
	stats.report(1001, false, &secondTTFT)

	errorRate, ttft, hasTTFT := stats.snapshot(1001)
	require.True(t, hasTTFT)
	require.InDelta(t, 0.36, errorRate, 1e-9)
	require.InDelta(t, 120.0, ttft, 1e-9)
	require.Equal(t, 1, stats.size())
}

func TestOpenAIAccountRuntimeStats_ReportConcurrent(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()

	const (
		accountCount = 4
		workers      = 16
		iterations   = 800
	)
	var wg sync.WaitGroup
	wg.Add(workers)
	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				accountID := int64(i%accountCount + 1)
				success := (i+worker)%3 != 0
				ttft := 80 + (i+worker)%40
				stats.report(accountID, success, &ttft)
			}
		}()
	}
	wg.Wait()

	require.Equal(t, accountCount, stats.size())
	for accountID := int64(1); accountID <= accountCount; accountID++ {
		errorRate, ttft, hasTTFT := stats.snapshot(accountID)
		require.GreaterOrEqual(t, errorRate, 0.0)
		require.LessOrEqual(t, errorRate, 1.0)
		require.True(t, hasTTFT)
		require.Greater(t, ttft, 0.0)
	}
}

func TestOpenAIAccountRuntimeStats_ColdStartMaturityAndStaleness(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Unix(1700000000, 0)
	thresholds := defaultOpenAIAccountHealthThresholds()
	ttft := 100

	stats.reportAt(5001, true, &ttft, now, thresholds, false)
	require.Equal(t, openAIAccountHealthUnknown, stats.health(5001, now, thresholds).level)
	stats.reportAt(5001, true, &ttft, now, thresholds, false)
	require.Equal(t, openAIAccountHealthUnknown, stats.health(5001, now, thresholds).level)
	stats.reportAt(5001, true, &ttft, now, thresholds, false)
	require.Equal(t, openAIAccountHealthHealthy, stats.health(5001, now, thresholds).level)

	stat := openAIAccountRuntimeStatForTest(t, stats, 5001)
	stat.lastObservedAt.Store(now.Add(-openAIAccountHealthStaleAfter).UnixNano())
	require.Equal(t, openAIAccountHealthHealthy, stats.health(5001, now, thresholds).level)
	stat.lastObservedAt.Store(now.Add(-openAIAccountHealthStaleAfter - time.Nanosecond).UnixNano())
	require.Equal(t, openAIAccountHealthUnknown, stats.health(5001, now, thresholds).level)
}

func TestOpenAIAccountHealthClassification_ThresholdBoundaries(t *testing.T) {
	thresholds := openAIStickyEscapeConfig{ttftMs: 15000, errorRate: 0.5}

	level, _ := classifyOpenAIAccountHealth(0.5, 15000, true, openAIAccountLegacyHealthMinSamples, thresholds)
	require.Equal(t, openAIAccountHealthHealthy, level)
	level, reason := classifyOpenAIAccountHealth(0.500001, 100, true, openAIAccountLegacyHealthMinSamples, thresholds)
	require.Equal(t, openAIAccountHealthDegraded, level)
	require.Equal(t, "error_rate", openAIAccountHealthReasonName(reason))
	level, reason = classifyOpenAIAccountHealth(0.1, 15000.001, true, openAIAccountLegacyHealthMinSamples, thresholds)
	require.Equal(t, openAIAccountHealthDegraded, level)
	require.Equal(t, "ttft", openAIAccountHealthReasonName(reason))
	level, _ = classifyOpenAIAccountHealth(0.9, 100, true, openAIAccountLegacyHealthMinSamples-1, thresholds)
	require.Equal(t, openAIAccountHealthUnknown, level)
}

func TestOpenAIAccountRuntimeStats_ConcurrentReportAndProbeLease(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Now()
	thresholds := defaultOpenAIAccountHealthThresholds()
	for i := 0; i < 5; i++ {
		stats.reportAt(5002, false, nil, now, thresholds, false)
	}
	stat := openAIAccountRuntimeStatForTest(t, stats, 5002)
	stat.nextProbeAt.Store(now.Add(-time.Second).UnixNano())

	const workers = 16
	results := make(chan bool, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			results <- stats.tryAcquireProbeLease(5002, now)
		}()
	}
	wg.Wait()
	close(results)
	acquired := 0
	for result := range results {
		if result {
			acquired++
		}
	}
	require.Equal(t, 1, acquired)
	stats.cancelProbeLease(5002, now)

	var reportWG sync.WaitGroup
	reportWG.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer reportWG.Done()
			stats.report(int64(5100+i%4), i%2 == 0, intPtrForTest(100+i))
		}()
	}
	reportWG.Wait()
	require.Equal(t, 5, stats.size())
}

func TestOpenAIGatewayService_SchedulerModeMatrix(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name     string
		smart    bool
		advanced bool
	}{
		{name: "legacy", smart: false, advanced: false},
		{name: "experimental", smart: false, advanced: true},
		{name: "smart", smart: true, advanced: false},
		{name: "smart_and_experimental", smart: true, advanced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			svc := &OpenAIGatewayService{
				cfg:              &config.Config{},
				rateLimitService: newOpenAISchedulerModeRateLimitService(tt.smart, tt.advanced),
			}
			scheduler := svc.getOpenAIAccountScheduler(ctx)
			if !tt.smart && !tt.advanced {
				require.Nil(t, scheduler)
				require.Nil(t, svc.openaiAccountStats)
				return
			}
			require.NotNil(t, scheduler)
			require.NotNil(t, svc.openaiAccountStats)
			require.Equal(t, tt.smart, svc.isSmartSchedulingEnabled(ctx))
			require.Equal(t, tt.advanced, svc.isOpenAIAdvancedSchedulerEnabled(ctx))
		})
	}
}

func TestOpenAIAdvancedSchedulerSettingsCache_StaleFlightCannotOverrideSave(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

	releaseBatch := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseBatch) })
	repo := &schedulerCacheTestSettingsRepo{
		values:         schedulerCacheTestSettingsValues(false, true, "2", "0.4", "0.25"),
		blockBatchCall: 1,
		batchStarted:   make(chan struct{}),
		releaseBatch:   releaseBatch,
	}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: NewSettingService(repo, &config.Config{})}}
	readResult := make(chan openAIAdvancedSchedulerRuntimeSettings, 1)
	go func() { readResult <- svc.openAIAdvancedSchedulerRuntimeSettings(context.Background()) }()
	<-repo.batchStarted

	updated := schedulerCacheTestSystemSettings(true, false, true, "9", "1.75", "2.5")
	require.NoError(t, svc.rateLimitService.settingService.UpdateSettings(context.Background(), updated))
	releaseOnce.Do(func() { close(releaseBatch) })
	settings := <-readResult
	require.True(t, settings.smartEnabled)
	require.True(t, settings.smartEnabledKnown)
	require.False(t, settings.enabled)
	require.True(t, settings.lowUpstreamRatePriorityEnabled)
	require.Equal(t, 9, settings.lbTopKOverride)
	require.NotNil(t, settings.oauthSchedulingRateMultiplier)
	require.InDelta(t, 1.75, *settings.oauthSchedulingRateMultiplier, 1e-9)
	require.InDelta(t, 2.5, settings.weightOverrides["priority"], 1e-9)

	cached, ok := openAIAdvancedSchedulerSettingCache.Load().(*cachedOpenAIAdvancedSchedulerSetting)
	require.True(t, ok)
	require.Equal(t, openAIAdvancedSchedulerSettingGeneration.Load(), cached.generation)
	require.True(t, cached.smartEnabled)
	require.False(t, cached.enabled)
	repo.mu.Lock()
	require.Equal(t, 1, repo.getMultipleCalls, "the blocked old read is discarded instead of starting a second DB read after the save snapshot is published")
	repo.mu.Unlock()
}

func TestSmartSchedulingStall_CacheGeneration(t *testing.T) {
	t.Run("missing setting defaults to thirty", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

		repo := &schedulerCacheTestSettingsRepo{
			values: schedulerCacheTestSettingsValues(true, false, "2", "1", "1"),
		}
		gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: NewSettingService(repo, &config.Config{})}}
		settings := gateway.openAIAdvancedSchedulerRuntimeSettings(context.Background())
		require.True(t, settings.smartEnabled)
		require.Equal(t, 30, settings.firstOutputTimeoutSeconds)
	})

	t.Run("late old read cannot mix smart switch and timeout generations", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

		releaseBatch := make(chan struct{})
		var releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(releaseBatch) })
		values := schedulerCacheTestSettingsValues(false, false, "2", "1", "1")
		values[SettingKeyOpenAIFirstOutputTimeoutSeconds] = "30"
		repo := &schedulerCacheTestSettingsRepo{
			values:         values,
			blockBatchCall: 1,
			batchStarted:   make(chan struct{}),
			releaseBatch:   releaseBatch,
		}
		settingService := NewSettingService(repo, &config.Config{})
		gateway := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: settingService}}
		readResult := make(chan openAIAdvancedSchedulerRuntimeSettings, 1)
		go func() { readResult <- gateway.openAIAdvancedSchedulerRuntimeSettings(context.Background()) }()
		<-repo.batchStarted

		updated := schedulerCacheTestSystemSettings(true, false, false, "2", "1", "1")
		updated.OpenAIFirstOutputTimeoutSeconds = 0
		require.NoError(t, settingService.UpdateSettings(context.Background(), updated))
		releaseOnce.Do(func() { close(releaseBatch) })

		settings := <-readResult
		require.True(t, settings.smartEnabled)
		require.Zero(t, settings.firstOutputTimeoutSeconds)
		cached, ok := openAIAdvancedSchedulerSettingCache.Load().(*cachedOpenAIAdvancedSchedulerSetting)
		require.True(t, ok)
		require.Equal(t, openAIAdvancedSchedulerSettingGeneration.Load(), cached.generation)
		require.True(t, cached.smartEnabled)
		require.Zero(t, cached.firstOutputTimeoutSeconds)
		repo.mu.Lock()
		require.Equal(t, 1, repo.getMultipleCalls, "the late pre-write read is discarded in favor of the published snapshot")
		repo.mu.Unlock()
	})
}

func TestOpenAIAdvancedSchedulerSettingsCache_StaleWaiterCannotUseOldResult(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

	releaseBatch := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseBatch) })
	repo := &schedulerCacheTestSettingsRepo{
		values:         schedulerCacheTestSettingsValues(true, false, "3", "0.6", "0.5"),
		blockBatchCall: 1,
		batchStarted:   make(chan struct{}),
		releaseBatch:   releaseBatch,
	}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: NewSettingService(repo, &config.Config{})}}
	leaderResult := make(chan openAIAdvancedSchedulerRuntimeSettings, 1)
	go func() { leaderResult <- svc.openAIAdvancedSchedulerRuntimeSettings(context.Background()) }()
	<-repo.batchStarted

	waiterJoined := make(chan struct{})
	var joinedOnce sync.Once
	waiterResult := make(chan openAIAdvancedSchedulerRuntimeSettings, 1)
	go func() {
		waiterResult <- svc.openAIAdvancedSchedulerRuntimeSettingsWithFlightRegistered(context.Background(), func() {
			joinedOnce.Do(func() { close(waiterJoined) })
		})
	}()
	<-waiterJoined

	updated := schedulerCacheTestSystemSettings(false, true, false, "11", "0.8", "3.5")
	require.NoError(t, svc.rateLimitService.settingService.UpdateSettings(context.Background(), updated))
	releaseOnce.Do(func() { close(releaseBatch) })
	for _, result := range []<-chan openAIAdvancedSchedulerRuntimeSettings{leaderResult, waiterResult} {
		settings := <-result
		require.False(t, settings.smartEnabled, "the old true result must not reach the reader after the save")
		require.True(t, settings.smartEnabledKnown)
		require.True(t, settings.enabled, "the newly saved independent A switch is preserved")
		require.Equal(t, 11, settings.lbTopKOverride)
		require.InDelta(t, 3.5, settings.weightOverrides["priority"], 1e-9)
	}
	repo.mu.Lock()
	require.Equal(t, 1, repo.getMultipleCalls, "both old-flight consumers reuse the published current snapshot")
	repo.mu.Unlock()
}

func TestOpenAIAdvancedSchedulerSettingsCache_FallbackHonorsGeneration(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

	releaseValue := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseValue) })
	repo := &schedulerCacheTestSettingsRepo{
		values:         schedulerCacheTestSettingsValues(false, true, "4", "0.7", "0.75"),
		getMultipleErr: errors.New("batch unavailable"),
		blockValueKey:  SettingKeySmartSchedulingEnabled,
		valueStarted:   make(chan string, 1),
		releaseValue:   releaseValue,
	}
	svc := &OpenAIGatewayService{rateLimitService: &RateLimitService{settingService: NewSettingService(repo, &config.Config{})}}
	readResult := make(chan openAIAdvancedSchedulerRuntimeSettings, 1)
	go func() { readResult <- svc.openAIAdvancedSchedulerRuntimeSettings(context.Background()) }()
	require.Equal(t, SettingKeySmartSchedulingEnabled, <-repo.valueStarted)

	updated := schedulerCacheTestSystemSettings(true, false, true, "13", "1.9", "4.5")
	require.NoError(t, svc.rateLimitService.settingService.UpdateSettings(context.Background(), updated))
	releaseOnce.Do(func() { close(releaseValue) })
	settings := <-readResult
	require.True(t, settings.smartEnabled)
	require.True(t, settings.smartEnabledKnown)
	require.False(t, settings.enabled)
	require.True(t, settings.lowUpstreamRatePriorityEnabled)
	require.Equal(t, 13, settings.lbTopKOverride)
	require.NotNil(t, settings.oauthSchedulingRateMultiplier)
	require.InDelta(t, 1.9, *settings.oauthSchedulingRateMultiplier, 1e-9)
	require.InDelta(t, 4.5, settings.weightOverrides["priority"], 1e-9)
	repo.mu.Lock()
	require.Equal(t, 1, repo.getMultipleCalls)
	repo.mu.Unlock()
}

func TestOpenAIGatewayService_SchedulerModeAfterSettingsSave(t *testing.T) {
	ctx := context.Background()
	groupID := int64(81201)
	for _, mode := range []struct {
		name     string
		smart    bool
		advanced bool
		wantID   int64
	}{
		{name: "legacy", smart: false, advanced: false, wantID: 81211},
		{name: "experimental", smart: false, advanced: true},
		{name: "smart", smart: true, advanced: false, wantID: 81212},
		{name: "smart_and_experimental", smart: true, advanced: true, wantID: 81212},
	} {
		t.Run(mode.name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			defer resetOpenAIAdvancedSchedulerSettingCacheForTest()

			accounts := []Account{
				newSmartSchedulerTestAccount(81211, groupID, 0),
				newSmartSchedulerTestAccount(81212, groupID, 10),
			}
			svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
			repo := &schedulerCacheTestSettingsRepo{values: schedulerCacheTestSettingsValues(false, false, "7", "1", "1")}
			settingService := NewSettingService(repo, &config.Config{})
			svc.rateLimitService = &RateLimitService{settingService: settingService}
			settings := schedulerCacheTestSystemSettings(mode.smart, mode.advanced, false, "7", "1", "1")
			require.NoError(t, settingService.UpdateSettings(ctx, settings))
			if mode.smart {
				scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
				require.True(t, ok)
				seedSmartDegradedAccount(t, scheduler.stats, 81211, "error_rate", time.Now().Add(-time.Minute))
				openAIAccountRuntimeStatForTest(t, scheduler.stats, 81211).nextProbeAt.Store(time.Now().Add(time.Hour).UnixNano())
			}

			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.NotNil(t, selection.Account)
			if mode.wantID != 0 {
				require.Equal(t, mode.wantID, selection.Account.ID, "the request is routed by the just-published S/A mode")
			} else {
				require.Contains(t, []int64{81211, 81212}, selection.Account.ID, "the A-only route selects from the live candidate set")
			}
			require.Equal(t, mode.smart, svc.isSmartSchedulingEnabled(ctx))
			require.Equal(t, mode.advanced, svc.isOpenAIAdvancedSchedulerEnabled(ctx))
			require.Equal(t, mode.smart || mode.advanced, svc.openaiScheduler != nil)
			require.Zero(t, selection.Account.smartHealthProbeID)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthBeatsPriority(t *testing.T) {
	groupID := int64(40101)
	accounts := []Account{
		newSmartSchedulerTestAccount(5201, groupID, 0),
		newSmartSchedulerTestAccount(5202, groupID, 10),
	}
	svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
	ctx := context.Background()
	scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	require.NotNil(t, scheduler)
	for i := 0; i < 4; i++ {
		scheduler.stats.report(5201, false, nil)
	}

	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(5202), selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthWeightedStickyDoesNotBypassPrimary(t *testing.T) {
	groupID := int64(40101)
	accounts := []Account{
		newSmartSchedulerTestAccount(5251, groupID, 0),
		newSmartSchedulerTestAccount(5252, groupID, 10),
	}
	concurrency := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{5252: false}}
	svc := newSmartSchedulerTestService(accounts, concurrency)
	cache := &schedulerTestGatewayCache{}
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	defer resetOpenAIAdvancedSchedulerSettingCacheForTest()
	svc.rateLimitService = &RateLimitService{
		settingService: NewSettingService(&openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
			SettingKeySmartSchedulingEnabled:                             "true",
			openAIAdvancedSchedulerSettingKey:                            "true",
			SettingKeyOpenAIAdvancedSchedulerStickyWeightedEnabled:       "true",
			SettingKeyOpenAIAdvancedSchedulerSubscriptionPriorityEnabled: "false",
		}}, &config.Config{}),
	}
	svc.cache = cache
	ctx := context.Background()
	scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	require.NotNil(t, scheduler)
	for i := 0; i < 4; i++ {
		scheduler.stats.report(5251, false, nil)
	}
	require.NoError(t, svc.cache.SetSessionAccountID(ctx, groupID, "smart-weighted-sticky", 5251, time.Hour))

	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-weighted-sticky", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(5252), selection.Account.ID)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, int64(5251), cache.sessionBindings["smart-weighted-sticky"])
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthKeepsHealthySticky(t *testing.T) {
	groupID := int64(40101)
	accounts := []Account{
		newSmartSchedulerTestAccount(5301, groupID, 10),
		newSmartSchedulerTestAccount(5302, groupID, 0),
	}
	cache := &schedulerTestGatewayCache{}
	svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
	svc.cache = cache
	ctx := context.Background()
	scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	require.NotNil(t, scheduler)
	for i := 0; i < 3; i++ {
		scheduler.stats.report(5301, true, intPtrForTest(100))
	}
	require.NoError(t, cache.SetSessionAccountID(ctx, groupID, "openai:smart-sticky", 5301, time.Hour))

	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-sticky", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, int64(5301), selection.Account.ID)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthEscapesDegradedSticky(t *testing.T) {
	groupID := int64(40101)
	accounts := []Account{
		newSmartSchedulerTestAccount(5401, groupID, 0),
		newSmartSchedulerTestAccount(5402, groupID, 10),
	}
	cache := &schedulerTestGatewayCache{}
	svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
	svc.cache = cache
	ctx := context.Background()
	scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	require.NotNil(t, scheduler)
	for i := 0; i < 4; i++ {
		scheduler.stats.report(5401, false, nil)
	}
	require.NoError(t, cache.SetSessionAccountID(ctx, groupID, "smart-escape", 5401, time.Hour))

	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-escape", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.Equal(t, int64(5402), selection.Account.ID)
	require.Equal(t, int64(5401), cache.sessionBindings["smart-escape"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestSmartSchedulingStall_Sticky(t *testing.T) {
	t.Run("degraded_sticky_yields_to_primary_without_refresh", func(t *testing.T) {
		groupID := int64(54101)
		const stickyID int64 = 5411
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"smart-stall-sticky": stickyID}}
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(stickyID, groupID, 0),
			newSmartSchedulerTestAccount(5412, groupID, 10),
		}, schedulerTestConcurrencyCache{})
		svc.cache = cache
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		now := time.Now()
		seedSmartDegradedAccount(t, scheduler.stats, stickyID, "error_rate", now)
		openAIAccountRuntimeStatForTest(t, scheduler.stats, stickyID).nextProbeAt.Store(now.Add(time.Hour).UnixNano())

		selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "smart-stall-sticky", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.Equal(t, int64(5412), selection.Account.ID)
		require.Equal(t, stickyID, cache.sessionBindings["smart-stall-sticky"], "escape does not rewrite the existing sticky binding")
		require.Zero(t, cache.refreshedSessions["smart-stall-sticky"], "the degraded sticky lease must not be renewed")
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	})

	t.Run("all_degraded_uses_priority_first_without_creating_sticky", func(t *testing.T) {
		groupID := int64(54102)
		const stickyID int64 = 5422
		cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"smart-stall-all-degraded": stickyID}}
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(5421, groupID, 10),
			newSmartSchedulerTestAccount(stickyID, groupID, 90),
		}, schedulerTestConcurrencyCache{})
		svc.cache = cache
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		now := time.Now()
		for _, accountID := range []int64{5421, stickyID} {
			seedSmartDegradedAccount(t, scheduler.stats, accountID, "error_rate", now)
			openAIAccountRuntimeStatForTest(t, scheduler.stats, accountID).nextProbeAt.Store(now.Add(time.Hour).UnixNano())
		}

		selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", "smart-stall-all-degraded", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.Equal(t, int64(5421), selection.Account.ID, "the non-probe fail-open pool remains priority-first")
		require.Equal(t, stickyID, cache.sessionBindings["smart-stall-all-degraded"], "fail-open does not rewrite a degraded sticky binding")
		require.Zero(t, cache.refreshedSessions["smart-stall-all-degraded"])
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	})
}

func TestSmartSchedulingStall_Attribution(t *testing.T) {
	schedulerService := &OpenAIGatewayService{
		rateLimitService:   newOpenAISchedulerModeRateLimitService(true, false),
		openaiAccountStats: newOpenAIAccountRuntimeStats(),
	}
	scheduler, ok := schedulerService.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	timeoutErr := &UpstreamFailoverError{StatusCode: http.StatusGatewayTimeout, SafeToFailoverAfterWrite: true}
	account := &Account{ID: 7801, Platform: PlatformOpenAI, Type: AccountTypeOAuth}

	schedulerService.ReportOpenAIAccountScheduleResult(account, "model", false, nil, timeoutErr)
	stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, account.ID)
	stat.mu.Lock()
	require.Len(t, stat.smartErrorWindow, 1, "an attributable first-output timeout adds one failed attempt")
	require.Equal(t, 1.0, stat.smartErrorWindow[0].value)
	stat.mu.Unlock()

	schedulerService.ObserveOpenAIAccountHealthFailure(context.Background(), account, timeoutErr)
	stat.mu.Lock()
	require.Len(t, stat.smartErrorWindow, 1, "the same failover error cannot be counted a second time")
	stat.mu.Unlock()

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	schedulerService.ObserveOpenAIAccountHealthFailure(cancelCtx, account, errors.New("upstream ended after client cancellation"))
	stat.mu.Lock()
	require.Len(t, stat.smartErrorWindow, 1, "client cancellation takes precedence over an account failure")
	stat.mu.Unlock()

	canceledAccount := &Account{ID: 7802, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	schedulerService.ReportOpenAIAccountScheduleResult(canceledAccount, "model", false, nil, context.Canceled)
	canceledStat := openAIAccountRuntimeStatForTest(t, scheduler.stats, canceledAccount.ID)
	canceledStat.mu.Lock()
	require.Empty(t, canceledStat.smartErrorWindow, "a canceled first-output attempt does not enter the sample window")
	canceledStat.mu.Unlock()

	postOutputAccount := &Account{ID: 7803, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	schedulerService.ObserveOpenAIAccountHealthFailure(context.Background(), postOutputAccount, errors.New("attributable stream failure"))
	postOutputStat := openAIAccountRuntimeStatForTest(t, scheduler.stats, postOutputAccount.ID)
	postOutputStat.mu.Lock()
	require.Len(t, postOutputStat.smartErrorWindow, 1, "an attributable failure after semantic output is counted once")
	postOutputStat.mu.Unlock()
}

func TestSmartSchedulingStall_RecoveryLease(t *testing.T) {
	groupID := int64(40101)
	accounts := []Account{
		newSmartSchedulerTestAccount(5501, groupID, 0),
		newSmartSchedulerTestAccount(5502, groupID, 10),
	}
	concurrency := schedulerTestConcurrencyCache{acquireResults: map[int64]bool{5502: false}}
	svc := newSmartSchedulerTestService(accounts, concurrency)
	ctx := context.Background()
	scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	require.NotNil(t, scheduler)
	seedSmartDegradedAccount(t, scheduler.stats, 5501, "error_rate", time.Now().Add(-time.Minute))

	selection := selectSmartAccountForTest(t, svc, groupID, "single-half-open-probe")
	require.Equal(t, int64(5501), selection.Account.ID)
	require.NotZero(t, selection.Account.smartHealthProbeID)
	require.Equal(t, int64(1), svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal)
	probeID := selection.Account.smartHealthProbeID
	require.False(t, scheduler.stats.tryAcquireProbeLeaseWithID(5501, time.Now(), probeID+1), "an account has only one concurrent half-open lease")
	stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, 5501)
	stat.mu.Lock()
	require.Equal(t, probeID, stat.probeID)
	require.True(t, stat.probeActive.Load())
	stat.mu.Unlock()
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	require.Equal(t, openAIAccountHealthDegraded, scheduler.stats.health(5501, time.Now(), svc.openAIStickyEscapeConfig()).level, "releasing without a result does not recover an account")

	require.Zero(t, accounts[0].smartHealthProbeID)
	svc.ReportOpenAIAccountScheduleResult(&accounts[0], "gpt-5.1", true, intPtrForTest(100))
	require.Equal(t, openAIAccountHealthDegraded, scheduler.stats.health(5501, time.Now(), svc.openAIStickyEscapeConfig()).level, "ordinary success without probe identity does not recover an account")
	stat.mu.Lock()
	require.Empty(t, stat.probeBatchSamples)
	stat.mu.Unlock()

	stat.nextProbeAt.Store(time.Now().Add(-time.Second).UnixNano())
	for index := 0; index < openAIAccountSmartProbeBatchLimit; index++ {
		selection = selectSmartAccountForTest(t, svc, groupID, "single-half-open-probe")
		require.Equal(t, int64(5501), selection.Account.ID)
		require.NotZero(t, selection.Account.smartHealthProbeID)
		ttftMs := 100
		svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, &ttftMs)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}

		assessment := scheduler.stats.health(5501, time.Now(), svc.openAIStickyEscapeConfig())
		if index < openAIAccountSmartProbeBatchLimit-1 {
			require.Equal(t, openAIAccountHealthDegraded, assessment.level, "the first nine attributable probe results do not recover")
			stat.mu.Lock()
			require.Len(t, stat.probeBatchSamples, index+1)
			stat.mu.Unlock()
			continue
		}
		require.Equal(t, openAIAccountHealthHealthy, assessment.level, "the tenth identity-matched result clears degraded and its positive TTFT samples are healthy")
		stat.mu.Lock()
		require.Empty(t, stat.probeBatchSamples)
		require.Zero(t, stat.probeID)
		stat.mu.Unlock()
	}
	require.Equal(t, int64(11), svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal, "only selections that actually acquired a slot count as probes")
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartHealthAllDegradedFailsOpen(t *testing.T) {
	groupID := int64(40101)
	accounts := []Account{
		newSmartSchedulerTestAccount(5601, groupID, 0),
		newSmartSchedulerTestAccount(5602, groupID, 10),
	}
	svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
	ctx := context.Background()
	scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	require.NotNil(t, scheduler)
	for _, id := range []int64{5601, 5602} {
		for i := 0; i < 4; i++ {
			scheduler.stats.report(id, false, nil)
		}
	}

	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Contains(t, []int64{5601, 5602}, selection.Account.ID)
	require.Equal(t, int64(1), svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthAllDegradedTotal)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartOnlyUsesPriorityOrder(t *testing.T) {
	ctx := context.Background()

	t.Run("N=1", func(t *testing.T) {
		groupID := int64(60101)
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(6011, groupID, 90),
		}, schedulerTestConcurrencyCache{})

		selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-n1", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, int64(6011), selection.Account.ID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	})

	t.Run("all_degraded_priority_first", func(t *testing.T) {
		groupID := int64(60104)
		accounts := []Account{
			newSmartSchedulerTestAccount(6041, groupID, 90),
			newSmartSchedulerTestAccount(6042, groupID, 10),
		}
		svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
		svc.cfg.Gateway.OpenAIWS.LBTopK = 1
		scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		require.NotNil(t, scheduler)
		for _, accountID := range []int64{6041, 6042} {
			for i := 0; i < 4; i++ {
				scheduler.stats.report(accountID, false, nil)
			}
		}

		selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-all-degraded", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, int64(6042), selection.Account.ID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	})

	t.Run("non_contiguous_priority_N_gt_3", func(t *testing.T) {
		groupID := int64(60102)
		accounts := []Account{
			newSmartSchedulerTestAccount(6021, groupID, 10),
			newSmartSchedulerTestAccount(6022, groupID, 30),
			newSmartSchedulerTestAccount(6023, groupID, 30),
			newSmartSchedulerTestAccount(6024, groupID, 90),
		}
		svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})

		selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-priority-first", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, int64(6021), selection.Account.ID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}

		acquired := make([]int64, 0, len(accounts))
		concurrency := schedulerTestConcurrencyCache{
			acquireResults: map[int64]bool{6021: false, 6022: false, 6023: true, 6024: true},
			acquiredIDs:    &acquired,
		}
		svc = newSmartSchedulerTestService(accounts, concurrency)
		selection, _, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-priority-fallback", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, 30, selection.Account.Priority)
		require.NotEmpty(t, acquired)
		require.Equal(t, int64(6021), acquired[0])
		for _, accountID := range acquired {
			require.NotEqual(t, int64(6024), accountID, "priority=90 must not be attempted before priority=30 succeeds")
			if accountID == selection.Account.ID {
				break
			}
		}
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	})

	t.Run("same_priority_uses_existing_spread", func(t *testing.T) {
		groupID := int64(60103)
		accounts := []Account{
			newSmartSchedulerTestAccount(6031, groupID, 30),
			newSmartSchedulerTestAccount(6032, groupID, 30),
			newSmartSchedulerTestAccount(6033, groupID, 30),
			newSmartSchedulerTestAccount(6034, groupID, 30),
		}
		svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
		selected := make(map[int64]struct{})
		for i := 0; i < 24; i++ {
			selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", fmt.Sprintf("smart-same-priority-%d", i), "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			selected[selection.Account.ID] = struct{}{}
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		}
		require.GreaterOrEqual(t, len(selected), 2)
	})
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartPrimaryBeatsDegradedRegardlessOfPriority(t *testing.T) {
	ctx := context.Background()
	for _, degradedPriority := range []int{1, 3, 90} {
		t.Run(fmt.Sprintf("degraded_priority_%d", degradedPriority), func(t *testing.T) {
			groupID := int64(60200 + degradedPriority)
			accounts := []Account{
				newSmartSchedulerTestAccount(6101, groupID, 3),
				newSmartSchedulerTestAccount(6102, groupID, degradedPriority),
			}
			svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
			svc.cfg.Gateway.OpenAIWS.LBTopK = 1
			svc.rateLimitService = newOpenAISchedulerModeRateLimitService(true, true)
			scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
			require.True(t, ok)
			require.NotNil(t, scheduler)
			for i := 0; i < 4; i++ {
				scheduler.stats.report(6102, false, nil)
			}

			selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-primary-degraded", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.Equal(t, int64(6101), selection.Account.ID)
			require.Equal(t, 1, decision.PrimaryCount)
			require.Equal(t, 1, decision.DegradedCount)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}
		})
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SmartStickyAndNewSession(t *testing.T) {
	ctx := context.Background()
	groupID := int64(60301)
	accounts := []Account{
		newSmartSchedulerTestAccount(6201, groupID, 10),
		newSmartSchedulerTestAccount(6202, groupID, 30),
	}
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{
		"openai:smart-healthy-sticky": 6202,
	}}
	svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
	svc.cache = cache

	selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-healthy-sticky", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(6202), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	for _, sessionHash := range []string{"smart-new-session-a", "smart-new-session-b"} {
		selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", sessionHash, "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.Equal(t, int64(6201), selection.Account.ID)
		require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}

	selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-same-hash", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(6201), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-same-hash", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(6201), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerSessionSticky, decision.Layer)
	require.True(t, decision.StickySessionHit)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}

	scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	require.NotNil(t, scheduler)
	for i := 0; i < 4; i++ {
		scheduler.stats.report(6201, false, nil)
	}
	require.Equal(t, openAIAccountHealthDegraded, scheduler.stats.health(6201, time.Now(), svc.openAIStickyEscapeConfig()).level)
	cache.sessionBindings["openai:smart-degraded-sticky"] = 6201
	selection, decision, err = svc.SelectAccountWithScheduler(ctx, &groupID, "", "smart-degraded-sticky", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.Equal(t, int64(6202), selection.Account.ID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	require.True(t, decision.HealthEscape)
	require.Equal(t, int64(6201), cache.sessionBindings["openai:smart-degraded-sticky"])
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_LogsDecisionWithoutSensitiveFields(t *testing.T) {
	previousLogger := slog.Default()
	logHandler := &schedulerTestLogHandler{}
	slog.SetDefault(slog.New(logHandler))
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
	})

	ctx := context.Background()
	for index, tt := range []struct {
		name                string
		healthReason        string
		concurrencyEscape   bool
		wantHealthEscape    bool
		wantDecisionReason  string
		wantFallbackAccount bool
	}{
		{name: "error_rate_escape", healthReason: "error_rate", wantHealthEscape: true, wantDecisionReason: "error_rate", wantFallbackAccount: true},
		{name: "ttft_escape", healthReason: "ttft", wantHealthEscape: true, wantDecisionReason: "ttft", wantFallbackAccount: true},
		{name: "ordinary_success", wantDecisionReason: "selected"},
		{name: "concurrency_escape", concurrencyEscape: true, wantDecisionReason: "selected", wantFallbackAccount: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(60410 + index)
			stickyID := int64(6301 + index*2)
			fallbackID := stickyID + 1
			sessionHash := "log-" + tt.name
			cache := &schedulerTestGatewayCache{}
			if tt.healthReason != "" || tt.concurrencyEscape {
				cache.sessionBindings = map[string]int64{"openai:" + sessionHash: stickyID}
			}

			concurrency := schedulerTestConcurrencyCache{}
			if tt.concurrencyEscape {
				concurrency.acquireResults = map[int64]bool{stickyID: false, fallbackID: true}
			}
			svc := newSmartSchedulerTestService([]Account{
				newSmartSchedulerTestAccount(stickyID, groupID, 10),
				newSmartSchedulerTestAccount(fallbackID, groupID, 30),
			}, concurrency)
			svc.cache = cache
			scheduler, ok := svc.getOpenAIAccountScheduler(ctx).(*defaultOpenAIAccountScheduler)
			require.True(t, ok)
			require.NotNil(t, scheduler)
			switch tt.healthReason {
			case "error_rate":
				for i := 0; i < 4; i++ {
					scheduler.stats.report(stickyID, false, nil)
				}
			case "ttft":
				ttftMs := 20000
				for i := 0; i < 3; i++ {
					scheduler.stats.report(stickyID, true, &ttftMs)
				}
			}

			selection, decision, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", sessionHash, "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
			require.NoError(t, err)
			require.NotNil(t, selection)
			wantSelectedID := stickyID
			if tt.wantFallbackAccount {
				wantSelectedID = fallbackID
			}
			require.Equal(t, wantSelectedID, selection.Account.ID)
			require.Equal(t, tt.wantHealthEscape, decision.HealthEscape)
			if selection.ReleaseFunc != nil {
				selection.ReleaseFunc()
			}

			records := logHandler.snapshot()
			var decisionRecord *schedulerTestLogRecord
			for i := range records {
				if records[i].message == "openai_account_schedule_decision" && records[i].attrs["group_id"] == groupID {
					decisionRecord = &records[i]
				}
			}
			require.NotNil(t, decisionRecord)
			require.Equal(t, tt.wantHealthEscape, decisionRecord.attrs["health_escape"])
			require.Equal(t, tt.wantDecisionReason, decisionRecord.attrs["reason"])
			if tt.wantHealthEscape {
				require.Equal(t, int64(2), decisionRecord.attrs["candidate_count"])
				require.Equal(t, int64(1), decisionRecord.attrs["primary_count"])
				require.Equal(t, int64(1), decisionRecord.attrs["degraded_count"])
				require.Equal(t, fallbackID, decisionRecord.attrs["selected_account_id"])
				require.Equal(t, int64(30), decisionRecord.attrs["selected_priority"])
				require.Equal(t, "unknown", decisionRecord.attrs["selected_health"])
				require.Equal(t, "load_balance", decisionRecord.attrs["selection_layer"])
				require.Equal(t, PlatformOpenAI, decisionRecord.attrs["platform"])
				require.Equal(t, "gpt-5.1", decisionRecord.attrs["model"])
			}
		})
	}

	groupID := int64(60499)
	accounts := []Account{
		newSmartSchedulerTestAccount(6399, groupID, 10),
		newSmartSchedulerTestAccount(6400, groupID, 30),
	}
	svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
	svc.cache = &schedulerTestGatewayCache{}
	excluded := map[int64]struct{}{6399: {}, 6400: {}}
	_, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "log-filtered", "gpt-5.1", excluded, OpenAIUpstreamTransportAny, false)
	require.Error(t, err)
	records := logHandler.snapshot()
	var filteredRecord *schedulerTestLogRecord
	for i := range records {
		if records[i].message == "openai_account_schedule_decision" && records[i].attrs["group_id"] == groupID {
			filteredRecord = &records[i]
			break
		}
	}
	require.NotNil(t, filteredRecord)
	require.Equal(t, "no_available_account", filteredRecord.attrs["reason"])
	require.Contains(t, filteredRecord.attrs["filter_reasons"], "excluded=2")

	for _, record := range records {
		for _, sensitiveKey := range []string{"api_key", "credential", "prompt", "body", "session_hash", "previous_response_id", "response_id"} {
			_, present := record.attrs[sensitiveKey]
			require.False(t, present, "sensitive field %q in %s", sensitiveKey, record.message)
		}
	}
}

func TestSelectTopKOpenAICandidates(t *testing.T) {
	candidates := []openAIAccountCandidateScore{
		{
			account:  &Account{ID: 11, Priority: 2},
			loadInfo: &AccountLoadInfo{LoadRate: 10, WaitingCount: 1},
			score:    10.0,
		},
		{
			account:  &Account{ID: 12, Priority: 1},
			loadInfo: &AccountLoadInfo{LoadRate: 20, WaitingCount: 1},
			score:    9.5,
		},
		{
			account:  &Account{ID: 13, Priority: 1},
			loadInfo: &AccountLoadInfo{LoadRate: 30, WaitingCount: 0},
			score:    10.0,
		},
		{
			account:  &Account{ID: 14, Priority: 0},
			loadInfo: &AccountLoadInfo{LoadRate: 40, WaitingCount: 0},
			score:    8.0,
		},
	}

	top2 := selectTopKOpenAICandidates(candidates, 2)
	require.Len(t, top2, 2)
	require.Equal(t, int64(13), top2[0].account.ID)
	require.Equal(t, int64(11), top2[1].account.ID)

	topAll := selectTopKOpenAICandidates(candidates, 8)
	require.Len(t, topAll, len(candidates))
	require.Equal(t, int64(13), topAll[0].account.ID)
	require.Equal(t, int64(11), topAll[1].account.ID)
	require.Equal(t, int64(12), topAll[2].account.ID)
	require.Equal(t, int64(14), topAll[3].account.ID)
}

func TestBuildOpenAIWeightedSelectionOrder_DeterministicBySessionSeed(t *testing.T) {
	candidates := []openAIAccountCandidateScore{
		{
			account:  &Account{ID: 101},
			loadInfo: &AccountLoadInfo{LoadRate: 10, WaitingCount: 0},
			score:    4.2,
		},
		{
			account:  &Account{ID: 102},
			loadInfo: &AccountLoadInfo{LoadRate: 30, WaitingCount: 1},
			score:    3.5,
		},
		{
			account:  &Account{ID: 103},
			loadInfo: &AccountLoadInfo{LoadRate: 50, WaitingCount: 2},
			score:    2.1,
		},
	}
	req := OpenAIAccountScheduleRequest{
		GroupID:        int64PtrForTest(99),
		SessionHash:    "session_seed_fixed",
		RequestedModel: "gpt-5.1",
	}

	first := buildOpenAIWeightedSelectionOrder(candidates, req)
	second := buildOpenAIWeightedSelectionOrder(candidates, req)
	require.Len(t, first, len(candidates))
	require.Len(t, second, len(candidates))
	for i := range first {
		require.Equal(t, first[i].account.ID, second[i].account.ID)
	}
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_LoadBalanceDistributesAcrossSessions(t *testing.T) {
	ctx := context.Background()
	groupID := int64(15)
	accounts := []Account{
		{
			ID:          5101,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 3,
			Priority:    0,
		},
		{
			ID:          5102,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 3,
			Priority:    0,
		},
		{
			ID:          5103,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 3,
			Priority:    0,
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 3
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 1

	concurrencyCache := schedulerTestConcurrencyCache{
		loadMap: map[int64]*AccountLoadInfo{
			5101: {AccountID: 5101, LoadRate: 20, WaitingCount: 1},
			5102: {AccountID: 5102, LoadRate: 20, WaitingCount: 1},
			5103: {AccountID: 5103, LoadRate: 20, WaitingCount: 1},
		},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{sessionBindings: map[string]int64{}},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selected := make(map[int64]int, len(accounts))
	for i := 0; i < 60; i++ {
		sessionHash := fmt.Sprintf("session_hash_lb_%d", i)
		selection, decision, err := svc.SelectAccountWithScheduler(
			ctx,
			&groupID,
			"",
			sessionHash,
			"gpt-5.1",
			nil,
			OpenAIUpstreamTransportAny,
			false,
		)
		require.NoError(t, err)
		require.NotNil(t, selection)
		require.NotNil(t, selection.Account)
		require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
		selected[selection.Account.ID]++
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	}

	// 多 session 应该能打散到多个账号，避免“恒定单账号命中”。
	require.GreaterOrEqual(t, len(selected), 2)
}

func TestDeriveOpenAISelectionSeed_NoAffinityAddsEntropy(t *testing.T) {
	req := OpenAIAccountScheduleRequest{
		RequestedModel: "gpt-5.1",
	}
	seed1 := deriveOpenAISelectionSeed(req)
	time.Sleep(1 * time.Millisecond)
	seed2 := deriveOpenAISelectionSeed(req)
	require.NotZero(t, seed1)
	require.NotZero(t, seed2)
	require.NotEqual(t, seed1, seed2)
}

func TestBuildOpenAIWeightedSelectionOrder_HandlesInvalidScores(t *testing.T) {
	candidates := []openAIAccountCandidateScore{
		{
			account:  &Account{ID: 901},
			loadInfo: &AccountLoadInfo{LoadRate: 5, WaitingCount: 0},
			score:    math.NaN(),
		},
		{
			account:  &Account{ID: 902},
			loadInfo: &AccountLoadInfo{LoadRate: 5, WaitingCount: 0},
			score:    math.Inf(1),
		},
		{
			account:  &Account{ID: 903},
			loadInfo: &AccountLoadInfo{LoadRate: 5, WaitingCount: 0},
			score:    -1,
		},
	}
	req := OpenAIAccountScheduleRequest{
		SessionHash: "seed_invalid_scores",
	}

	order := buildOpenAIWeightedSelectionOrder(candidates, req)
	require.Len(t, order, len(candidates))
	seen := map[int64]struct{}{}
	for _, item := range order {
		seen[item.account.ID] = struct{}{}
	}
	require.Len(t, seen, len(candidates))
}

func TestOpenAISelectionRNG_SeedZeroStillWorks(t *testing.T) {
	rng := newOpenAISelectionRNG(0)
	v1 := rng.nextUint64()
	v2 := rng.nextUint64()
	require.NotEqual(t, v1, v2)
	require.GreaterOrEqual(t, rng.nextFloat64(), 0.0)
	require.Less(t, rng.nextFloat64(), 1.0)
}

func TestOpenAIAccountCandidateHeap_PushPopAndInvalidType(t *testing.T) {
	h := openAIAccountCandidateHeap{}
	h.Push(openAIAccountCandidateScore{
		account:  &Account{ID: 7001},
		loadInfo: &AccountLoadInfo{LoadRate: 0, WaitingCount: 0},
		score:    1.0,
	})
	require.Equal(t, 1, h.Len())
	popped, ok := h.Pop().(openAIAccountCandidateScore)
	require.True(t, ok)
	require.Equal(t, int64(7001), popped.account.ID)
	require.Equal(t, 0, h.Len())

	require.Panics(t, func() {
		h.Push("bad_element_type")
	})
}

func TestClamp01_AllBranches(t *testing.T) {
	require.Equal(t, 0.0, clamp01(-0.2))
	require.Equal(t, 1.0, clamp01(1.3))
	require.Equal(t, 0.5, clamp01(0.5))
}

func TestCalcLoadSkewByMoments_Branches(t *testing.T) {
	require.Equal(t, 0.0, calcLoadSkewByMoments(1, 1, 1))
	// variance < 0 分支：sumSquares/count - mean^2 为负值时应钳制为 0。
	require.Equal(t, 0.0, calcLoadSkewByMoments(1, 0, 2))
	require.GreaterOrEqual(t, calcLoadSkewByMoments(6, 20, 3), 0.0)
}

func TestDefaultOpenAIAccountScheduler_ReportSwitchAndSnapshot(t *testing.T) {
	schedulerAny := newDefaultOpenAIAccountScheduler(&OpenAIGatewayService{}, nil)
	scheduler, ok := schedulerAny.(*defaultOpenAIAccountScheduler)
	require.True(t, ok)

	ttft := 100
	scheduler.ReportResult(1001, true, &ttft)
	scheduler.ReportSwitch()
	scheduler.metrics.recordSelect(OpenAIAccountScheduleDecision{
		Layer:             openAIAccountScheduleLayerLoadBalance,
		LatencyMs:         8,
		LoadSkew:          0.5,
		StickyPreviousHit: true,
	})
	scheduler.metrics.recordSelect(OpenAIAccountScheduleDecision{
		Layer:            openAIAccountScheduleLayerSessionSticky,
		LatencyMs:        6,
		LoadSkew:         0.2,
		StickySessionHit: true,
	})

	snapshot := scheduler.SnapshotMetrics()
	require.Equal(t, int64(2), snapshot.SelectTotal)
	require.Equal(t, int64(1), snapshot.StickyPreviousHitTotal)
	require.Equal(t, int64(1), snapshot.StickySessionHitTotal)
	require.Equal(t, int64(1), snapshot.LoadBalanceSelectTotal)
	require.Equal(t, int64(1), snapshot.AccountSwitchTotal)
	require.Greater(t, snapshot.SchedulerLatencyMsAvg, 0.0)
	require.Greater(t, snapshot.StickyHitRatio, 0.0)
	require.Greater(t, snapshot.LoadSkewAvg, 0.0)
}

func TestOpenAIGatewayService_SchedulerWrappersAndDefaults(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	svc := &OpenAIGatewayService{}
	ttft := 120
	svc.ReportOpenAIAccountScheduleResult(&Account{ID: 10}, "", true, &ttft)
	svc.RecordOpenAIAccountSwitch()
	snapshot := svc.SnapshotOpenAIAccountSchedulerMetrics()
	require.Equal(t, OpenAIAccountSchedulerMetricsSnapshot{}, snapshot)
	require.Equal(t, 7, svc.openAIWSLBTopK())
	require.Equal(t, openaiStickySessionTTL, svc.openAIWSSessionStickyTTL())

	defaultWeights := svc.openAIWSSchedulerWeights()
	require.Equal(t, 1.0, defaultWeights.Priority)
	require.Equal(t, 1.0, defaultWeights.Load)
	require.Equal(t, 0.7, defaultWeights.Queue)
	require.Equal(t, 0.8, defaultWeights.ErrorRate)
	require.Equal(t, 0.5, defaultWeights.TTFT)

	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 9
	cfg.Gateway.OpenAIWS.StickySessionTTLSeconds = 180
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 0.2
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 0.3
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 0.4
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 0.5
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 0.6
	svcWithCfg := &OpenAIGatewayService{cfg: cfg}

	require.Equal(t, 9, svcWithCfg.openAIWSLBTopK())
	require.Equal(t, 180*time.Second, svcWithCfg.openAIWSSessionStickyTTL())
	customWeights := svcWithCfg.openAIWSSchedulerWeights()
	require.Equal(t, 0.2, customWeights.Priority)
	require.Equal(t, 0.3, customWeights.Load)
	require.Equal(t, 0.4, customWeights.Queue)
	require.Equal(t, 0.5, customWeights.ErrorRate)
	require.Equal(t, 0.6, customWeights.TTFT)
}

func TestDefaultOpenAIAccountScheduler_IsAccountTransportCompatible_Branches(t *testing.T) {
	scheduler := &defaultOpenAIAccountScheduler{}
	require.True(t, scheduler.isAccountTransportCompatible(nil, OpenAIUpstreamTransportAny))
	require.True(t, scheduler.isAccountTransportCompatible(nil, OpenAIUpstreamTransportHTTPSSE))
	require.False(t, scheduler.isAccountTransportCompatible(nil, OpenAIUpstreamTransportResponsesWebsocketV2))
	require.False(t, scheduler.isAccountTransportCompatible(nil, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))

	cfg := newSchedulerTestOpenAIWSV2Config()
	scheduler.service = &OpenAIGatewayService{cfg: cfg}
	account := &Account{
		ID:          8801,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
		Concurrency: 1,
		Extra: map[string]any{
			"openai_apikey_responses_websockets_v2_enabled": true,
		},
	}
	require.True(t, scheduler.isAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2))
	require.True(t, scheduler.isAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))

	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeHTTPBridge
	require.False(t, scheduler.isAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2))
	require.True(t, scheduler.isAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))

	account.Extra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
	require.False(t, scheduler.isAccountTransportCompatible(account, OpenAIUpstreamTransportResponsesWebsocketV2Ingress))
}

func int64PtrForTest(v int64) *int64 {
	return &v
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_StickyWeightedFallbackSkipsOutOfGroupStickyAccount(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(101081)
	otherGroupID := int64(101082)
	accounts := []Account{
		{
			ID:          38001,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    10,
			GroupIDs:    []int64{groupID},
		},
		{
			// 会话粘连绑定指向的账号已被移出请求分组（绑定 TTL 内账号改组的场景）。
			ID:          38002,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{otherGroupID},
		},
	}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.LBTopK = 2
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Priority = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Load = 1
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.Queue = 0.7
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.ErrorRate = 0.8
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.TTFT = 0.5
	cfg.Gateway.OpenAIWS.SchedulerScoreWeights.SessionSticky = 3
	cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{
		"openai:session_weighted_out_of_group": 38002,
	}}
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{38001: false, 38002: true},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerGroupAwareOpenAIAccountRepo{schedulerTestOpenAIAccountRepo{accounts: accounts}},
		cache:              cache,
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_weighted_out_of_group",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		false,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	// 组内唯一候选 38001 满并发：必须返回其等待计划，绝不能把请求泄漏到组外的粘连账号 38002。
	require.Equal(t, int64(38001), selection.Account.ID)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, int64(38001), selection.WaitPlan.AccountID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
	// 失效的粘连绑定应被清理，避免后续请求反复走同一条泄漏路径。
	require.Positive(t, cache.deletedSessions["openai:session_weighted_out_of_group"])
}

func TestOpenAIGatewayService_SelectAccountWithScheduler_SubscriptionPriorityWaitsOnBusySubscriptionWhenRegularUnusable(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()

	ctx := context.Background()
	groupID := int64(101091)
	accounts := []Account{
		{
			// 订阅账号：支持 compact，但并发已满（busy-but-waitable）。
			ID:          38011,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeOAuth,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    0,
			GroupIDs:    []int64{groupID},
			Credentials: map[string]any{"plan_type": "team"},
			Extra:       map[string]any{"openai_compact_supported": true},
		},
		{
			// 常规账号：明确不支持 compact，无法服务本次请求。
			ID:          38012,
			Platform:    PlatformOpenAI,
			Type:        AccountTypeAPIKey,
			Status:      StatusActive,
			Schedulable: true,
			Concurrency: 1,
			Priority:    9,
			GroupIDs:    []int64{groupID},
			Extra:       map[string]any{"openai_compact_supported": false},
		},
	}
	concurrencyCache := schedulerTestConcurrencyCache{
		acquireResults: map[int64]bool{38011: false, 38012: true},
	}
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                newSchedulerTestSubscriptionPriorityConfig(),
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService("true", "", "true"),
		concurrencyService: NewConcurrencyService(concurrencyCache),
	}

	selection, decision, err := svc.SelectAccountWithScheduler(
		ctx,
		&groupID,
		"",
		"session_subscription_wait",
		"gpt-5.1",
		nil,
		OpenAIUpstreamTransportAny,
		true,
	)
	// 常规池无可用候选时，忙碌的订阅账号应产生等待计划，而不是直接返回 no available accounts。
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	require.Equal(t, int64(38011), selection.Account.ID)
	require.False(t, selection.Acquired)
	require.NotNil(t, selection.WaitPlan)
	require.Equal(t, int64(38011), selection.WaitPlan.AccountID)
	require.Equal(t, openAIAccountScheduleLayerLoadBalance, decision.Layer)
}

func TestOpenAIAccountRuntimeStats_RetentionReclaimsIdleNonDegraded(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	accountID := int64(91001)
	now := time.Now()
	lastActivity := now.Add(-openAIAccountRuntimeStatsReclaimIdle)
	addIdleOpenAIAccountRuntimeStat(t, stats, accountID, lastActivity)

	stats.smartHealthSnapshots([]int64{accountID}, now)
	stat, release, ok := stats.acquire(accountID, false, false)
	require.True(t, ok)
	require.Equal(t, lastActivity.UnixNano(), stat.lastActivityAt.Load(), "admin snapshots must not refresh scheduling activity")
	release()

	repo := &openAIAccountRuntimeStatsRetentionRepo{
		getByIDs: func(context.Context, []int64) ([]*Account, error) {
			return []*Account{{ID: accountID}}, nil
		},
	}
	require.Equal(t, 1, stats.reclaimIdleBatch(context.Background(), repo, now))
	require.Zero(t, stats.size())
	require.Equal(t, 0, newDefaultOpenAIAccountScheduler(nil, stats).SnapshotMetrics().RuntimeStatsAccountCount)

	stat, release, ok = stats.acquire(accountID, true, true)
	require.True(t, ok)
	require.Zero(t, stat.sampleCount.Load(), "a reclaimed entry must reappear with unknown history")
	stat.mu.Lock()
	require.False(t, stat.smartMode)
	require.Equal(t, openAIAccountHealthUnknown, stat.smartLevel)
	stat.mu.Unlock()
	release()
	require.Equal(t, 1, stats.size())
}

func TestOpenAIAccountRuntimeStats_ReclaimDeletedIDsInBoundedBatches(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Now()
	const accountCount = 205
	for index := 0; index < accountCount; index++ {
		addIdleOpenAIAccountRuntimeStat(t, stats, int64(92000+index), now.Add(-openAIAccountRuntimeStatsReclaimIdle))
	}
	repo := &openAIAccountRuntimeStatsRetentionRepo{}

	for _, wantSize := range []int{105, 5, 0} {
		require.Equal(t, min(openAIAccountRuntimeStatsReclaimBatchSize, stats.size()), stats.reclaimIdleBatch(context.Background(), repo, now))
		require.Equal(t, wantSize, stats.size())
		require.Equal(t, wantSize, newDefaultOpenAIAccountScheduler(nil, stats).SnapshotMetrics().RuntimeStatsAccountCount)
	}

	calls := repo.callsSnapshot()
	require.Len(t, calls, 3)
	require.Equal(t, []int{100, 100, 5}, []int{len(calls[0]), len(calls[1]), len(calls[2])})
	for _, ids := range calls {
		require.LessOrEqual(t, len(ids), openAIAccountRuntimeStatsReclaimBatchSize)
	}
	stat, release, ok := stats.acquire(92000, true, true)
	require.True(t, ok)
	require.Zero(t, stat.sampleCount.Load(), "a deleted account ID must not inherit its old runtime state")
	release()
	require.Equal(t, 1, stats.size())
}

func TestOpenAIAccountRuntimeStats_ReclaimPreservesDegradedAndProbeState(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	now := time.Now()
	staleAt := now.Add(-openAIAccountRuntimeStatsReclaimIdle)
	const smartID, legacyID, leaseID, batchID int64 = 93001, 93002, 93003, 93004
	var stat *openAIAccountRuntimeStat

	seedSmartDegradedAccount(t, stats, smartID, "error_rate", now)
	addIdleOpenAIAccountRuntimeStat(t, stats, smartID, staleAt)
	smartStat, release, ok := stats.acquire(smartID, false, false)
	require.True(t, ok)
	smartStat.mu.Lock()
	backoffBefore := smartStat.backoffIndex
	smartStat.mu.Unlock()
	release()

	legacyStat, release, ok := stats.acquire(legacyID, true, true)
	require.True(t, ok)
	legacyStat.mu.Lock()
	legacyStat.sampleCount.Store(openAIAccountLegacyHealthMinSamples)
	legacyStat.healthLevel.Store(uint32(openAIAccountHealthDegraded))
	legacyStat.healthReason.Store(openAIAccountHealthReasonErrorRate)
	legacyStat.nextProbeAt.Store(now.Add(time.Hour).UnixNano())
	legacyStat.mu.Unlock()
	legacyStat.lastActivityAt.Store(staleAt.UnixNano())
	release()

	seedSmartDegradedAccount(t, stats, leaseID, "ttft", now)
	require.True(t, stats.tryAcquireProbeLeaseWithID(leaseID, now, 93003))
	stats.markProbeSlotAcquired(leaseID, 93003, now)
	addIdleOpenAIAccountRuntimeStat(t, stats, leaseID, staleAt)

	batchStat, release, ok := stats.acquire(batchID, true, true)
	require.True(t, ok)
	batchStat.mu.Lock()
	batchStat.probeBatchStarted = now.Add(-time.Minute)
	batchStat.probeBatchSamples = []openAISmartProbeSample{{success: true, observed: now}}
	batchStat.mu.Unlock()
	batchStat.lastActivityAt.Store(staleAt.UnixNano())
	release()

	repo := &openAIAccountRuntimeStatsRetentionRepo{
		getByIDs: func(context.Context, []int64) ([]*Account, error) {
			return []*Account{{ID: smartID}, {ID: legacyID}}, nil
		},
	}
	require.Zero(t, stats.reclaimIdleBatch(context.Background(), repo, now))
	require.Equal(t, 4, stats.size())

	stat, release, ok = stats.acquire(smartID, false, false)
	require.True(t, ok)
	stat.mu.Lock()
	require.Equal(t, openAIAccountHealthDegraded, stat.smartLevel)
	require.Equal(t, backoffBefore, stat.backoffIndex)
	stat.mu.Unlock()
	release()
	stat, release, ok = stats.acquire(legacyID, false, false)
	require.True(t, ok)
	require.Equal(t, uint32(openAIAccountHealthDegraded), stat.healthLevel.Load())
	release()
	stat, release, ok = stats.acquire(leaseID, false, false)
	require.True(t, ok)
	stat.mu.Lock()
	require.Equal(t, uint64(93003), stat.probeID)
	require.False(t, stat.probeBatchStarted.IsZero())
	stat.mu.Unlock()
	release()
	stat, release, ok = stats.acquire(batchID, false, false)
	require.True(t, ok)
	stat.mu.Lock()
	require.Len(t, stat.probeBatchSamples, 1)
	stat.mu.Unlock()
	release()
}

func TestOpenAIAccountRuntimeStats_ReclaimConcurrentReportCannotBecomeOrphaned(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	accountID := int64(94001)
	now := time.Now()
	addIdleOpenAIAccountRuntimeStat(t, stats, accountID, now.Add(-openAIAccountRuntimeStatsReclaimIdle))
	queryStarted := make(chan struct{})
	continueQuery := make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(continueQuery) }) })
	repo := &openAIAccountRuntimeStatsRetentionRepo{
		getByIDs: func(ctx context.Context, _ []int64) ([]*Account, error) {
			close(queryStarted)
			select {
			case <-continueQuery:
				return []*Account{{ID: accountID}}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	type reclaimResult struct {
		deleted int
	}
	result := make(chan reclaimResult, 1)
	go func() {
		result <- reclaimResult{deleted: stats.reclaimIdleBatch(context.Background(), repo, now)}
	}()
	select {
	case <-queryStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("reclaimer did not reach repository lookup")
	}

	stats.reportAtForRequest(accountID, true, nil, now, openAIStickyEscapeConfig{}, false, false, 0, true)
	unblock.Do(func() { close(continueQuery) })
	select {
	case got := <-result:
		require.Zero(t, got.deleted)
	case <-time.After(3 * time.Second):
		t.Fatal("reclaimer did not finish after repository lookup")
	}
	require.Equal(t, 1, stats.size())
	require.Equal(t, int64(1), openAIAccountRuntimeStatSampleCount(t, stats, accountID))
}

func TestOpenAIAccountRuntimeStats_ReclaimRepositoryErrorRetriesSameBatch(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	accountID := int64(95001)
	now := time.Now()
	addIdleOpenAIAccountRuntimeStat(t, stats, accountID, now.Add(-openAIAccountRuntimeStatsReclaimIdle))
	var callCount int
	repo := &openAIAccountRuntimeStatsRetentionRepo{
		getByIDs: func(ctx context.Context, _ []int64) ([]*Account, error) {
			callCount++
			deadline, ok := ctx.Deadline()
			require.True(t, ok, "repository lookup must have its own timeout")
			require.LessOrEqual(t, time.Until(deadline), openAIAccountRuntimeStatsReclaimTimeout)
			if callCount == 1 {
				return nil, errors.New("temporary repository failure")
			}
			return nil, nil
		},
	}
	require.Zero(t, stats.reclaimIdleBatch(context.Background(), repo, now))
	require.Equal(t, 1, stats.size(), "a repository error must retain the complete batch")
	require.Equal(t, 1, stats.reclaimIdleBatch(context.Background(), repo, now))
	require.Zero(t, stats.size())
	requestedBatches := repo.callsSnapshot()
	require.Len(t, requestedBatches, 2)
	require.Equal(t, requestedBatches[0], requestedBatches[1], "a failed batch must be retried before advancing the cursor")
}

func TestOpenAIAccountRuntimeStats_RetentionMaintenanceLifecycle(t *testing.T) {
	stats := newOpenAIAccountRuntimeStats()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stats.startReclamation(ctx, nil)
	stats.maintenanceMu.Lock()
	require.False(t, stats.maintenanceStarted, "maintenance must not start without a repository")
	stats.maintenanceMu.Unlock()

	stats.startReclamation(ctx, &openAIAccountRuntimeStatsRetentionRepo{})
	stats.maintenanceMu.Lock()
	done := stats.maintenanceDone
	require.True(t, stats.maintenanceStarted)
	stats.maintenanceMu.Unlock()
	require.NotNil(t, done)
	stats.startReclamation(ctx, &openAIAccountRuntimeStatsRetentionRepo{})
	stats.maintenanceMu.Lock()
	require.Equal(t, done, stats.maintenanceDone, "only one maintenance loop may be started")
	stats.maintenanceMu.Unlock()

	stats.stopReclamation()
	select {
	case <-done:
	default:
		t.Fatal("maintenance loop did not stop")
	}
	stats.startReclamation(ctx, &openAIAccountRuntimeStatsRetentionRepo{})
	stats.maintenanceMu.Lock()
	require.True(t, stats.maintenanceStopped)
	require.Equal(t, done, stats.maintenanceDone, "a stopped maintenance loop must not restart")
	stats.maintenanceMu.Unlock()
}

func TestOpenAIAccountRuntimeStats_ReclaimWithSchedulerModesDisabled(t *testing.T) {
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
	settingsRepo := &openAIAdvancedSchedulerSettingRepoStub{values: map[string]string{
		SettingKeySmartSchedulingEnabled:  "true",
		openAIAdvancedSchedulerSettingKey: "false",
	}}
	repo := &openAIAccountRuntimeStatsRetentionRepo{}
	const normalID, degradedID int64 = 96001, 96002
	repo.getByIDs = func(_ context.Context, ids []int64) ([]*Account, error) {
		accounts := make([]*Account, 0, len(ids))
		for _, id := range ids {
			if id == normalID || id == degradedID {
				accounts = append(accounts, &Account{ID: id})
			}
		}
		return accounts, nil
	}
	maintenanceCtx, cancel := context.WithCancel(context.Background())
	svc := &OpenAIGatewayService{
		accountRepo:                         repo,
		rateLimitService:                    &RateLimitService{settingService: NewSettingService(settingsRepo, &config.Config{})},
		openaiAccountStatsMaintenanceCtx:    maintenanceCtx,
		openaiAccountStatsMaintenanceCancel: cancel,
	}
	t.Cleanup(svc.CloseOpenAIWSPool)

	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	stats := scheduler.stats
	now := time.Now()
	staleAt := now.Add(-openAIAccountRuntimeStatsReclaimIdle)
	addIdleOpenAIAccountRuntimeStat(t, stats, normalID, staleAt)
	degradedStat, release, ok := stats.acquire(degradedID, true, true)
	require.True(t, ok)
	degradedStat.sampleCount.Store(openAIAccountLegacyHealthMinSamples)
	degradedStat.healthLevel.Store(uint32(openAIAccountHealthDegraded))
	degradedStat.healthReason.Store(openAIAccountHealthReasonErrorRate)
	degradedStat.nextProbeAt.Store(now.Add(time.Hour).UnixNano())
	degradedStat.lastActivityAt.Store(staleAt.UnixNano())
	release()
	require.Equal(t, 2, scheduler.SnapshotMetrics().RuntimeStatsAccountCount)

	settingsRepo.values[SettingKeySmartSchedulingEnabled] = "false"
	settingsRepo.values[openAIAdvancedSchedulerSettingKey] = "false"
	resetOpenAIAdvancedSchedulerSettingCacheForTest()
	require.Nil(t, svc.getOpenAIAccountScheduler(context.Background()), "both disabled switches must preserve the legacy gateway path")
	require.Nil(t, svc.SmartHealthSnapshot(context.Background(), []*Account{{ID: degradedID}}))
	require.Equal(t, 1, stats.reclaimIdleBatch(context.Background(), repo, now))
	require.Equal(t, 1, scheduler.SnapshotMetrics().RuntimeStatsAccountCount)
	require.Equal(t, int64(openAIAccountLegacyHealthMinSamples), openAIAccountRuntimeStatSampleCount(t, stats, degradedID))
	remaining, release, ok := stats.acquire(degradedID, false, false)
	require.True(t, ok)
	require.Equal(t, uint32(openAIAccountHealthDegraded), remaining.healthLevel.Load(), "closing both modes must not erase legacy degraded state")
	release()
}

func captureOpenAIProbeTraceLogs(t *testing.T) *schedulerTestLogHandler {
	t.Helper()
	previousLogger := slog.Default()
	logHandler := &schedulerTestLogHandler{}
	slog.SetDefault(slog.New(logHandler))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	return logHandler
}

func openAIProbeTraceLogRecords(records []schedulerTestLogRecord) []schedulerTestLogRecord {
	var result []schedulerTestLogRecord
	for _, record := range records {
		if record.message == "openai_account_health_probe" {
			result = append(result, record)
		}
	}
	return result
}

func requireOpenAIProbeTraceFieldWhitelist(t *testing.T, record schedulerTestLogRecord) {
	t.Helper()
	allowed := map[string]struct{}{
		"account_id": {}, "group_id": {}, "probe_trace_id": {}, "correlation": {},
		"phase": {}, "outcome": {}, "reason": {}, "batch_ordinal": {},
		"batch_sample_count": {}, "backoff_step": {}, "next_probe_at": {}, "health_level": {},
	}
	for key := range record.attrs {
		_, ok := allowed[key]
		require.True(t, ok, "unexpected probe trace log field %q", key)
	}
	if value, exists := record.attrs["correlation"]; exists {
		correlation, ok := value.(string)
		require.True(t, ok, "correlation must be a string")
		require.Contains(t, []string{"available", "unavailable"}, correlation)
	}
	if value, exists := record.attrs["batch_sample_count"]; exists {
		batchSampleCount, ok := value.(int64)
		require.True(t, ok, "batch_sample_count must be an int64")
		require.GreaterOrEqual(t, batchSampleCount, int64(0))
		require.LessOrEqual(t, batchSampleCount, int64(openAIAccountSmartProbeBatchLimit))
	}
	if value, exists := record.attrs["batch_ordinal"]; exists {
		batchOrdinal, ok := value.(int64)
		require.True(t, ok, "batch_ordinal must be an int64")
		require.GreaterOrEqual(t, batchOrdinal, int64(1))
		require.LessOrEqual(t, batchOrdinal, int64(openAIAccountSmartProbeBatchLimit))
	}
}

func requireOpenAIProbeOpportunityFieldContract(t *testing.T, record schedulerTestLogRecord) {
	t.Helper()
	require.Equal(t, "openai_account_health_all_degraded", record.message)
	require.ElementsMatch(t, []string{"candidate_count", "probe_opportunity"}, openAIProbeOpportunityLogAttrKeys(record.attrs))
	opportunity, ok := record.attrs["probe_opportunity"].(string)
	require.True(t, ok, "probe_opportunity must be a string")
	require.Contains(t, []string{"eligible", "unavailable"}, opportunity)
}

func openAIProbeOpportunityLogAttrKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func TestOpenAIAccountProbeTraceBatchRecoveryAndRedaction(t *testing.T) {
	logHandler := captureOpenAIProbeTraceLogs(t)
	const groupID, degradedID, primaryID int64 = 81101, 81111, 81112
	accounts := []Account{
		newSmartSchedulerTestAccount(degradedID, groupID, 10),
		newSmartSchedulerTestAccount(primaryID, groupID, 20),
	}
	accounts[0].Credentials = map[string]any{
		"api_key":              "probe-api-key-sentinel",
		"request_body":         "probe-body-sentinel",
		"prompt":               "probe-prompt-sentinel",
		"session_id":           "probe-session-id-sentinel",
		"session_hash":         "probe-session-hash-sentinel",
		"response_id":          "probe-response-id-sentinel",
		"previous_response_id": "probe-previous-response-id-sentinel",
	}
	svc := newSmartSchedulerTestService(accounts, schedulerTestConcurrencyCache{})
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))

	traceIDs := make(map[string]struct{}, openAIAccountSmartProbeBatchLimit)
	for ordinal := 1; ordinal <= openAIAccountSmartProbeBatchLimit; ordinal++ {
		before := len(logHandler.snapshot())
		selection := selectSmartAccountForTest(t, svc, groupID, "probe-session-sentinel")
		require.Equal(t, degradedID, selection.Account.ID)
		require.NotZero(t, selection.Account.smartHealthProbeID)

		requestRecords := logHandler.snapshot()[before:]
		traceRecords := openAIProbeTraceLogRecords(requestRecords)
		var start *schedulerTestLogRecord
		for i := range traceRecords {
			if traceRecords[i].attrs["phase"] == "start" {
				start = &traceRecords[i]
				break
			}
		}
		require.NotNil(t, start)
		require.Equal(t, int64(degradedID), start.attrs["account_id"])
		require.Equal(t, int64(groupID), start.attrs["group_id"])
		require.Equal(t, int64(ordinal), start.attrs["batch_ordinal"])
		require.Equal(t, "available", start.attrs["correlation"])
		requireOpenAIProbeTraceFieldWhitelist(t, *start)
		traceID, ok := start.attrs["probe_trace_id"].(string)
		require.True(t, ok)
		require.NotEmpty(t, traceID)
		traceIDs[traceID] = struct{}{}

		var decision *schedulerTestLogRecord
		for i := range requestRecords {
			if requestRecords[i].message == "openai_account_schedule_decision" {
				decision = &requestRecords[i]
			}
		}
		require.NotNil(t, decision)
		require.Equal(t, traceID, decision.attrs["probe_trace_id"])
		require.Equal(t, "available", decision.attrs["correlation"])

		svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, nil)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		allTraceRecords := openAIProbeTraceLogRecords(logHandler.snapshot()[before:])
		var finishes []schedulerTestLogRecord
		for _, record := range allTraceRecords {
			if record.attrs["phase"] == "finish" {
				finishes = append(finishes, record)
			}
		}
		require.Len(t, finishes, 1)
		require.Equal(t, traceID, finishes[0].attrs["probe_trace_id"])
		require.Equal(t, int64(ordinal), finishes[0].attrs["batch_ordinal"])
		require.Equal(t, "qualified", finishes[0].attrs["outcome"])
		require.Equal(t, int64(ordinal), finishes[0].attrs["batch_sample_count"])
		requireOpenAIProbeTraceFieldWhitelist(t, finishes[0])
		for _, record := range logHandler.snapshot() {
			if record.message != "openai_account_health_probe" &&
				record.message != "openai_account_schedule_decision" &&
				record.message != "openai_account_health_all_degraded" {
				continue
			}
			for _, sentinel := range []string{
				"probe-api-key-sentinel", "probe-body-sentinel", "probe-prompt-sentinel",
				"probe-session-id-sentinel", "probe-session-hash-sentinel",
				"probe-response-id-sentinel", "probe-previous-response-id-sentinel",
			} {
				require.NotContains(t, fmt.Sprint(record.attrs), sentinel)
			}
		}
	}
	require.Len(t, traceIDs, openAIAccountSmartProbeBatchLimit)
	require.Equal(t, int64(openAIAccountSmartProbeBatchLimit), svc.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal)
	require.NotEqual(t, openAIAccountHealthDegraded, scheduler.stats.health(degradedID, time.Now(), svc.openAIStickyEscapeConfig()).level)
}

func TestOpenAIAccountProbeTraceConcurrentTerminalAndStaleID(t *testing.T) {
	logHandler := captureOpenAIProbeTraceLogs(t)
	const groupID, degradedID, primaryID int64 = 81201, 81211, 81212
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(degradedID, groupID, 10),
		newSmartSchedulerTestAccount(primaryID, groupID, 20),
	}, schedulerTestConcurrencyCache{})
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
	first := selectSmartAccountForTest(t, svc, groupID, "")
	firstProbeID := first.Account.smartHealthProbeID
	require.NotZero(t, firstProbeID)

	var terminal sync.WaitGroup
	terminal.Add(2)
	go func() {
		defer terminal.Done()
		svc.ReportOpenAIAccountScheduleResult(first.Account, "gpt-5.1", true, nil)
	}()
	go func() {
		defer terminal.Done()
		if first.ReleaseFunc != nil {
			first.ReleaseFunc()
		}
	}()
	terminal.Wait()
	for range 3 {
		if first.ReleaseFunc != nil {
			first.ReleaseFunc()
		}
	}

	firstTraceID := ""
	firstTerminals := 0
	for _, record := range openAIProbeTraceLogRecords(logHandler.snapshot()) {
		if record.attrs["phase"] == "start" && record.attrs["account_id"] == degradedID {
			firstTraceID, _ = record.attrs["probe_trace_id"].(string)
		}
		if record.attrs["probe_trace_id"] == firstTraceID && record.attrs["phase"] != "start" {
			firstTerminals++
		}
	}
	require.NotEmpty(t, firstTraceID)
	require.Equal(t, 1, firstTerminals)

	second := selectSmartAccountForTest(t, svc, groupID, "")
	secondProbeID := second.Account.smartHealthProbeID
	require.NotZero(t, secondProbeID)
	require.NotEqual(t, firstProbeID, secondProbeID)
	svc.ReportOpenAIAccountScheduleResult(first.Account, "gpt-5.1", true, nil)
	stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, degradedID)
	stat.mu.Lock()
	require.Equal(t, secondProbeID, stat.probeID, "a late result for the old probe must not clear the new lease")
	stat.mu.Unlock()
	if second.ReleaseFunc != nil {
		second.ReleaseFunc()
	}

	secondTraceID := ""
	secondTerminals := 0
	for _, record := range openAIProbeTraceLogRecords(logHandler.snapshot()) {
		if record.attrs["phase"] == "start" && record.attrs["account_id"] == degradedID {
			traceID, _ := record.attrs["probe_trace_id"].(string)
			if traceID != firstTraceID {
				secondTraceID = traceID
			}
		}
		if secondTraceID != "" && record.attrs["probe_trace_id"] == secondTraceID && record.attrs["phase"] != "start" {
			secondTerminals++
		}
	}
	require.NotEmpty(t, secondTraceID)
	require.Equal(t, 1, secondTerminals)
}

func TestOpenAIAccountProbeTraceBlockedHandlerDoesNotBlockTerminal(t *testing.T) {
	previousLogger := slog.Default()
	handler := &blockingOpenAIProbeTraceLogHandler{
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() {
		handler.release()
		slog.SetDefault(previousLogger)
	})

	stats := newOpenAIAccountRuntimeStats()
	const accountID int64 = 81221
	now := time.Now()
	seedSmartDegradedAccount(t, stats, accountID, "error_rate", now.Add(-2*time.Minute))
	probeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(accountID, now, probeID))

	startDone := make(chan struct{})
	go func() {
		stats.markProbeSlotAcquired(accountID, probeID, now)
		close(startDone)
	}()
	select {
	case <-handler.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe start log handler was not entered")
	}

	cancelDone := make(chan struct{})
	go func() {
		stats.cancelProbeLeaseForID(accountID, probeID, now.Add(time.Second))
		close(cancelDone)
	}()
	select {
	case <-cancelDone:
	case <-time.After(5 * time.Second):
		t.Fatal("same-account cancellation waited for the blocked log handler")
	}
	select {
	case <-startDone:
		t.Fatal("start call returned before its synchronous log handler was released")
	default:
	}

	handler.release()
	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("probe trace drainer did not finish after releasing the handler")
	}

	traceRecords := openAIProbeTraceLogRecords(handler.snapshot())
	require.Len(t, traceRecords, 2)
	require.Equal(t, "start", traceRecords[0].attrs["phase"])
	require.Equal(t, "cancel", traceRecords[1].attrs["phase"])
	startTraceID, ok := traceRecords[0].attrs["probe_trace_id"].(string)
	require.True(t, ok)
	terminalTraceID, ok := traceRecords[1].attrs["probe_trace_id"].(string)
	require.True(t, ok)
	require.NotEmpty(t, startTraceID)
	require.Equal(t, startTraceID, terminalTraceID)
}

func TestOpenAIAccountProbeTraceAttributionAndBatchTimeout(t *testing.T) {
	logHandler := captureOpenAIProbeTraceLogs(t)
	stats := newOpenAIAccountRuntimeStats()
	now := time.Now()
	const accountID int64 = 81801
	seedSmartDegradedAccount(t, stats, accountID, "error_rate", now.Add(-2*time.Minute))

	probeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(accountID, now, probeID))
	stats.markProbeSlotAcquired(accountID, probeID, now)
	stats.reportAtForRequest(accountID, false, nil, now.Add(time.Second), openAIStickyEscapeConfig{}, false, true, probeID, true)
	traceRecords := openAIProbeTraceLogRecords(logHandler.snapshot())
	require.Len(t, traceRecords, 2)
	require.Equal(t, "start", traceRecords[0].attrs["phase"])
	require.Equal(t, "finish", traceRecords[1].attrs["phase"])
	require.Equal(t, "unqualified", traceRecords[1].attrs["outcome"])
	require.Equal(t, "account_error", traceRecords[1].attrs["reason"])
	require.Len(t, openAIAccountRuntimeStatForTest(t, stats, accountID).probeBatchSamples, 1)

	const unattributableAccountID int64 = 81803
	unattributableStart := now.Add(time.Minute)
	seedSmartDegradedAccount(t, stats, unattributableAccountID, "error_rate", unattributableStart.Add(-2*time.Minute))
	unattributableProbeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(unattributableAccountID, unattributableStart, unattributableProbeID))
	stats.markProbeSlotAcquired(unattributableAccountID, unattributableProbeID, unattributableStart)
	stats.reportAtForRequest(unattributableAccountID, false, nil, unattributableStart.Add(time.Second), openAIStickyEscapeConfig{}, false, true, unattributableProbeID, false)
	traceRecords = openAIProbeTraceLogRecords(logHandler.snapshot())
	var unattributableTerminal *schedulerTestLogRecord
	for i := range traceRecords {
		if traceRecords[i].attrs["account_id"] == unattributableAccountID && traceRecords[i].attrs["phase"] != "start" {
			unattributableTerminal = &traceRecords[i]
		}
	}
	require.NotNil(t, unattributableTerminal)
	require.Equal(t, "cancel", unattributableTerminal.attrs["phase"])
	require.Equal(t, "inconclusive", unattributableTerminal.attrs["outcome"])
	require.Equal(t, "not_attributable", unattributableTerminal.attrs["reason"])
	unattributableStat := openAIAccountRuntimeStatForTest(t, stats, unattributableAccountID)
	unattributableStat.mu.Lock()
	require.Empty(t, unattributableStat.probeBatchSamples)
	unattributableStat.mu.Unlock()

	const timeoutAccountID int64 = 81802
	probeStartedAt := now.Add(-openAIAccountSmartProbeBatchTimeout - time.Minute)
	seedSmartDegradedAccount(t, stats, timeoutAccountID, "error_rate", probeStartedAt.Add(-2*time.Minute))
	timeoutProbeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(timeoutAccountID, probeStartedAt, timeoutProbeID))
	stats.markProbeSlotAcquired(timeoutAccountID, timeoutProbeID, probeStartedAt)
	stats.reportAtForRequest(
		timeoutAccountID,
		true,
		nil,
		probeStartedAt.Add(openAIAccountSmartProbeBatchTimeout+time.Second),
		openAIStickyEscapeConfig{},
		false,
		true,
		timeoutProbeID,
		true,
	)
	traceRecords = openAIProbeTraceLogRecords(logHandler.snapshot())
	var timeoutFinish *schedulerTestLogRecord
	for i := range traceRecords {
		if traceRecords[i].attrs["account_id"] == timeoutAccountID && traceRecords[i].attrs["phase"] == "finish" {
			timeoutFinish = &traceRecords[i]
		}
	}
	require.NotNil(t, timeoutFinish)
	require.Equal(t, "inconclusive", timeoutFinish.attrs["outcome"])
	require.Equal(t, "batch_timeout", timeoutFinish.attrs["reason"])
	require.Zero(t, timeoutFinish.attrs["batch_sample_count"])
	requireOpenAIProbeTraceFieldWhitelist(t, traceRecords[len(traceRecords)-1])

	const retryAccountID int64 = 81804
	retryStart := now.Add(2 * time.Minute)
	seedSmartDegradedAccount(t, stats, retryAccountID, "error_rate", retryStart.Add(-2*time.Minute))
	for ordinal := 1; ordinal <= 2; ordinal++ {
		probeAt := retryStart.Add(time.Duration(ordinal) * time.Second)
		retryProbeID := stats.probeSequence.Add(1)
		require.True(t, stats.tryAcquireProbeLeaseWithID(retryAccountID, probeAt, retryProbeID))
		stats.markProbeSlotAcquired(retryAccountID, retryProbeID, probeAt)
		stats.reportAtForRequest(retryAccountID, false, nil, probeAt.Add(time.Second), openAIStickyEscapeConfig{}, false, true, retryProbeID, true)
	}
	retryStat := openAIAccountRuntimeStatForTest(t, stats, retryAccountID)
	retryAt := time.Unix(0, retryStat.nextProbeAt.Load()).Add(time.Second)
	retryProbeID := stats.probeSequence.Add(1)
	require.True(t, stats.tryAcquireProbeLeaseWithID(retryAccountID, retryAt, retryProbeID))
	stats.markProbeSlotAcquired(retryAccountID, retryProbeID, retryAt)
	stats.cancelProbeLeaseForID(retryAccountID, retryProbeID, retryAt.Add(time.Second))
	var retryOrdinals []int64
	for _, record := range openAIProbeTraceLogRecords(logHandler.snapshot()) {
		if record.attrs["account_id"] == retryAccountID && record.attrs["phase"] == "start" {
			ordinal, ok := record.attrs["batch_ordinal"].(int64)
			require.True(t, ok, "batch_ordinal must be an int64")
			retryOrdinals = append(retryOrdinals, ordinal)
		}
	}
	require.Equal(t, []int64{1, 2, 1}, retryOrdinals)
}

func TestOpenAIAccountProbeTraceWaitPlanAndAllDegraded(t *testing.T) {
	logHandler := captureOpenAIProbeTraceLogs(t)
	const waitGroup, waitDegraded, waitPrimary int64 = 81301, 81311, 81312
	waitService := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(waitDegraded, waitGroup, 10),
		newSmartSchedulerTestAccount(waitPrimary, waitGroup, 20),
	}, schedulerTestConcurrencyCache{acquireResults: map[int64]bool{waitDegraded: false, waitPrimary: false}})
	waitScheduler, ok := waitService.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, waitScheduler.stats, waitDegraded, "error_rate", time.Now().Add(-2*time.Minute))
	waitSelection := selectSmartAccountForTest(t, waitService, waitGroup, "")
	require.NotNil(t, waitSelection.WaitPlan)
	require.Zero(t, waitService.SnapshotOpenAIAccountSchedulerMetrics().SmartHealthProbeTotal)
	require.Empty(t, openAIProbeTraceLogRecords(logHandler.snapshot()))

	const unavailableGroup, unavailableID int64 = 81401, 81411
	unavailableService := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(unavailableID, unavailableGroup, 10),
	}, schedulerTestConcurrencyCache{})
	unavailableScheduler, ok := unavailableService.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, unavailableScheduler.stats, unavailableID, "error_rate", time.Now().Add(-2*time.Minute))
	unavailableStat := openAIAccountRuntimeStatForTest(t, unavailableScheduler.stats, unavailableID)
	unavailableStat.nextProbeAt.Store(time.Now().Add(time.Hour).UnixNano())
	before := len(logHandler.snapshot())
	unavailableSelection := selectSmartAccountForTest(t, unavailableService, unavailableGroup, "")
	require.Equal(t, unavailableID, unavailableSelection.Account.ID)
	var unavailableRecord *schedulerTestLogRecord
	for _, record := range logHandler.snapshot()[before:] {
		if record.message == "openai_account_health_all_degraded" {
			unavailableRecord = &record
		}
	}
	require.NotNil(t, unavailableRecord)
	requireOpenAIProbeOpportunityFieldContract(t, *unavailableRecord)
	require.Equal(t, "unavailable", unavailableRecord.attrs["probe_opportunity"])
	require.Len(t, unavailableRecord.attrs, 2)
	require.Empty(t, openAIProbeTraceLogRecords(logHandler.snapshot()[before:]))

	const eligibleGroup, eligibleID int64 = 81501, 81511
	eligibleService := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(eligibleID, eligibleGroup, 10),
	}, schedulerTestConcurrencyCache{})
	eligibleScheduler, ok := eligibleService.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, eligibleScheduler.stats, eligibleID, "error_rate", time.Now().Add(-2*time.Minute))
	before = len(logHandler.snapshot())
	eligibleSelection := selectSmartAccountForTest(t, eligibleService, eligibleGroup, "")
	require.NotZero(t, eligibleSelection.Account.smartHealthProbeID)
	var eligibleRecord *schedulerTestLogRecord
	for _, record := range logHandler.snapshot()[before:] {
		if record.message == "openai_account_health_all_degraded" {
			eligibleRecord = &record
		}
	}
	require.NotNil(t, eligibleRecord)
	requireOpenAIProbeOpportunityFieldContract(t, *eligibleRecord)
	require.Equal(t, "eligible", eligibleRecord.attrs["probe_opportunity"])
	if eligibleSelection.ReleaseFunc != nil {
		eligibleSelection.ReleaseFunc()
	}
	require.Len(t, openAIProbeTraceLogRecords(logHandler.snapshot()[before:]), 2)
}

func TestOpenAIAccountProbeTraceCorrelationFallbackAndSchedulerModes(t *testing.T) {
	if namespace, available := openAIProbeTraceNamespaceFrom(strings.NewReader("")); available || namespace != "" {
		t.Fatalf("empty random source returned namespace=%q available=%v", namespace, available)
	}
	t.Run("instance_scope_disambiguates_local_probe_ids", func(t *testing.T) {
		logHandler := captureOpenAIProbeTraceLogs(t)
		first := newOpenAIAccountRuntimeStats()
		second := newOpenAIAccountRuntimeStats()
		for index, stats := range []*openAIAccountRuntimeStats{first, second} {
			stats.probeTraceNamespace = "test-namespace"
			stats.probeTraceNamespaceAvailable = true
			stats.probeTraceInstanceID = uint64(index + 1)
			seedSmartDegradedAccount(t, stats, 81600, "error_rate", time.Now().Add(-2*time.Minute))
			require.True(t, stats.tryAcquireProbeLeaseWithID(81600, time.Now(), 1))
			stats.markProbeSlotAcquired(81600, 1, time.Now())
		}
		var traceIDs []string
		for _, record := range openAIProbeTraceLogRecords(logHandler.snapshot()) {
			if record.attrs["phase"] == "start" {
				traceID, ok := record.attrs["probe_trace_id"].(string)
				require.True(t, ok, "probe_trace_id must be a string")
				traceIDs = append(traceIDs, traceID)
			}
		}
		require.Len(t, traceIDs, 2)
		require.NotEqual(t, traceIDs[0], traceIDs[1], "same local probeID in separate stats instances must not collide")
		first.cancelProbeLeaseForID(81600, 1, time.Now())
		second.cancelProbeLeaseForID(81600, 1, time.Now())
	})

	t.Run("random_source_failure_keeps_unavailable_events", func(t *testing.T) {
		logHandler := captureOpenAIProbeTraceLogs(t)
		const groupID, degradedID, primaryID int64 = 81601, 81611, 81612
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(degradedID, groupID, 10),
			newSmartSchedulerTestAccount(primaryID, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		scheduler.stats.probeTraceNamespaceAvailable = false
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		selection := selectSmartAccountForTest(t, svc, groupID, "")
		require.NotZero(t, selection.Account.smartHealthProbeID)
		records := logHandler.snapshot()
		var decision *schedulerTestLogRecord
		for _, record := range records {
			if record.message == "openai_account_schedule_decision" {
				decision = &record
			}
		}
		require.NotNil(t, decision)
		require.Equal(t, "unavailable", decision.attrs["correlation"])
		_, hasDecisionTrace := decision.attrs["probe_trace_id"]
		require.False(t, hasDecisionTrace)
		traces := openAIProbeTraceLogRecords(records)
		require.Len(t, traces, 1)
		require.Equal(t, "start", traces[0].attrs["phase"])
		require.Equal(t, "unavailable", traces[0].attrs["correlation"])
		_, hasStartTrace := traces[0].attrs["probe_trace_id"]
		require.False(t, hasStartTrace)
		svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, nil)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		traces = openAIProbeTraceLogRecords(logHandler.snapshot())
		require.Len(t, traces, 2)
		require.Equal(t, "finish", traces[1].attrs["phase"])
		require.Equal(t, "unavailable", traces[1].attrs["correlation"])
	})

	for _, tc := range []struct {
		name            string
		smartEnabled    bool
		advancedEnabled bool
	}{
		{name: "smart_off_advanced_off"},
		{name: "smart_off_advanced_on", advancedEnabled: true},
		{name: "smart_on_advanced_off", smartEnabled: true},
		{name: "smart_on_advanced_on", smartEnabled: true, advancedEnabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetOpenAIAdvancedSchedulerSettingCacheForTest()
			t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
			logHandler := captureOpenAIProbeTraceLogs(t)
			const groupID, degradedID, primaryID int64 = 81701, 81711, 81712
			svc := newSmartSchedulerTestService([]Account{
				newSmartSchedulerTestAccount(degradedID, groupID, 10),
				newSmartSchedulerTestAccount(primaryID, groupID, 20),
			}, schedulerTestConcurrencyCache{})
			svc.rateLimitService = newOpenAISchedulerModeRateLimitService(tc.smartEnabled, tc.advancedEnabled)
			if tc.smartEnabled {
				scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
				require.True(t, ok)
				seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
			}
			selection := selectSmartAccountForTest(t, svc, groupID, "")
			if tc.smartEnabled {
				require.NotZero(t, selection.Account.smartHealthProbeID)
				svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, nil)
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				require.Len(t, openAIProbeTraceLogRecords(logHandler.snapshot()), 2)
			} else {
				if selection.ReleaseFunc != nil {
					selection.ReleaseFunc()
				}
				require.Empty(t, openAIProbeTraceLogRecords(logHandler.snapshot()), "S=false must not emit smart probe events")
			}
		})
	}
	t.Run("smart_on_ordinary_selection_has_no_probe_trace", func(t *testing.T) {
		resetOpenAIAdvancedSchedulerSettingCacheForTest()
		t.Cleanup(resetOpenAIAdvancedSchedulerSettingCacheForTest)
		logHandler := captureOpenAIProbeTraceLogs(t)
		const groupID int64 = 81799
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(81791, groupID, 10),
			newSmartSchedulerTestAccount(81792, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		svc.rateLimitService = newOpenAISchedulerModeRateLimitService(true, false)
		selection := selectSmartAccountForTest(t, svc, groupID, "")
		require.Zero(t, selection.Account.smartHealthProbeID)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
		var decision *schedulerTestLogRecord
		for _, record := range logHandler.snapshot() {
			if record.message == "openai_account_schedule_decision" {
				decision = &record
			}
		}
		require.NotNil(t, decision)
		_, hasTrace := decision.attrs["probe_trace_id"]
		require.False(t, hasTrace)
		_, hasCorrelation := decision.attrs["correlation"]
		require.False(t, hasCorrelation)
		require.Empty(t, openAIProbeTraceLogRecords(logHandler.snapshot()))
	})
}

type schedulerProbeAfterFuncEntry struct {
	callback func()
	finished chan struct{}
}

type schedulerProbeAfterFuncContext struct {
	context.Context
	done            chan struct{}
	callbackGate    <-chan struct{}
	callbackStarted chan struct{}
	mu              sync.Mutex
	canceled        bool
	callbacks       []schedulerProbeAfterFuncEntry
}

func newSchedulerProbeAfterFuncContext(gate <-chan struct{}) *schedulerProbeAfterFuncContext {
	return &schedulerProbeAfterFuncContext{
		Context:         context.Background(),
		done:            make(chan struct{}),
		callbackGate:    gate,
		callbackStarted: make(chan struct{}, 1),
	}
}

func (c *schedulerProbeAfterFuncContext) Done() <-chan struct{} {
	return c.done
}

func (c *schedulerProbeAfterFuncContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *schedulerProbeAfterFuncContext) AfterFunc(callback func()) func() bool {
	entry := schedulerProbeAfterFuncEntry{callback: callback, finished: make(chan struct{})}
	c.mu.Lock()
	c.callbacks = append(c.callbacks, entry)
	canceled := c.canceled
	c.mu.Unlock()
	if canceled {
		c.startCallback(entry)
	}
	return func() bool { return false }
}

func (c *schedulerProbeAfterFuncContext) startCallback(entry schedulerProbeAfterFuncEntry) {
	go func() {
		select {
		case c.callbackStarted <- struct{}{}:
		default:
		}
		if c.callbackGate != nil {
			<-c.callbackGate
		}
		entry.callback()
		close(entry.finished)
	}()
}

func (c *schedulerProbeAfterFuncContext) cancel() {
	c.mu.Lock()
	if c.canceled {
		c.mu.Unlock()
		return
	}
	c.canceled = true
	close(c.done)
	callbacks := append([]schedulerProbeAfterFuncEntry(nil), c.callbacks...)
	c.mu.Unlock()
	for _, entry := range callbacks {
		c.startCallback(entry)
	}
}

// waitCallbacks waits only for the fake-context propagation callbacks; context.AfterFunc
// may run its registered cleanup callback in a separate goroutine.
func (c *schedulerProbeAfterFuncContext) waitCallbacks(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	callbacks := append([]schedulerProbeAfterFuncEntry(nil), c.callbacks...)
	c.mu.Unlock()
	for _, entry := range callbacks {
		select {
		case <-entry.finished:
		case <-time.After(time.Second):
			t.Fatal("request-context cleanup callback did not finish")
		}
	}
}

func selectSmartAccountForTestWithContext(t *testing.T, svc *OpenAIGatewayService, ctx context.Context, groupID int64) *AccountSelectionResult {
	t.Helper()
	selection, _, err := svc.SelectAccountWithScheduler(ctx, &groupID, "", "", "gpt-5.1", nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	return selection
}

func selectSmartProbeDirectForTest(t *testing.T, svc *OpenAIGatewayService, ctx context.Context, account Account, groupID int64) *AccountSelectionResult {
	t.Helper()
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	budget := newOpenAISelectionProbeBudget()
	budget.enableLimit()
	selection, _, err := scheduler.tryAcquireOpenAISelectionOrderWithBudget(
		ctx,
		OpenAIAccountScheduleRequest{
			GroupID:            &groupID,
			Platform:           PlatformOpenAI,
			SmartHealthEnabled: true,
			RequestedModel:     "gpt-5.1",
			RequiredTransport:  OpenAIUpstreamTransportAny,
		},
		[]openAIAccountCandidateScore{{account: &account, healthProbe: true}},
		budget,
	)
	require.NoError(t, err)
	require.NotNil(t, selection)
	require.NotNil(t, selection.Account)
	return selection
}

func openAIProbeLeaseStateForTest(t *testing.T, stats *openAIAccountRuntimeStats, accountID int64) (uint64, int, int, int) {
	t.Helper()
	stat, release, ok := stats.acquire(accountID, false, false)
	require.True(t, ok)
	defer release()
	stat.mu.Lock()
	defer stat.mu.Unlock()
	return stat.probeID, len(stat.probeBatchSamples), int(stat.sampleCount.Load()), stat.backoffIndex
}

func waitForOpenAIProbeCancellationCleanupForTest(t *testing.T, stats *openAIAccountRuntimeStats, accountID int64) {
	t.Helper()
	stat, release, ok := stats.acquire(accountID, false, false)
	require.True(t, ok)
	defer release()

	require.Eventually(t, func() bool {
		activeID, _, _, _ := openAIProbeLeaseStateForTest(t, stats, accountID)
		stat.mu.Lock()
		traceStarted := stat.probeTraceStarted
		stat.mu.Unlock()
		stat.probeTraceQueueMu.Lock()
		traceDrained := len(stat.probeTraceQueue) == 0 && !stat.probeTraceDraining
		stat.probeTraceQueueMu.Unlock()
		return activeID == 0 && !traceStarted && traceDrained
	}, time.Second, time.Millisecond, "matching probe cancellation must clear its lease and drain its trace")
}

func TestOpenAIAccountProbeReleaseBeforeSuccessReport(t *testing.T) {
	logHandler := captureOpenAIProbeTraceLogs(t)
	const groupID, degradedID, primaryID int64 = 82101, 82111, 82112
	var acquiredIDs, releasedIDs []int64
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(degradedID, groupID, 10),
		newSmartSchedulerTestAccount(primaryID, groupID, 20),
	}, schedulerTestConcurrencyCache{acquiredIDs: &acquiredIDs, releasedIDs: &releasedIDs})
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selection := selectSmartAccountForTestWithContext(t, svc, ctx, groupID)
	require.Equal(t, degradedID, selection.Account.ID)
	probeID := selection.Account.smartHealthProbeID
	require.NotZero(t, probeID)

	selection.ReleaseFunc()
	selection.ReleaseFunc()
	require.Equal(t, []int64{degradedID}, releasedIDs, "physical slot must be released once")
	activeID, batchCount, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
	require.Equal(t, probeID, activeID, "releasing the physical slot must preserve the pending probe")
	require.Zero(t, batchCount)
	traceRecords := openAIProbeTraceLogRecords(logHandler.snapshot())
	require.Len(t, traceRecords, 1)
	require.Equal(t, "start", traceRecords[0].attrs["phase"])

	ttft := 250
	svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, &ttft)
	activeID, batchCount, _, _ = openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
	require.Zero(t, activeID)
	require.Equal(t, 1, batchCount)
	traceRecords = openAIProbeTraceLogRecords(logHandler.snapshot())
	require.Len(t, traceRecords, 2)
	require.Equal(t, "finish", traceRecords[1].attrs["phase"])
	require.Equal(t, "qualified", traceRecords[1].attrs["outcome"])
}

func TestOpenAIAccountProbeReleaseBeforeBatchRecovery(t *testing.T) {
	const groupID, degradedID, primaryID int64 = 82201, 82211, 82212
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(degradedID, groupID, 10),
		newSmartSchedulerTestAccount(primaryID, groupID, 20),
	}, schedulerTestConcurrencyCache{})
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))

	for ordinal := 1; ordinal <= openAIAccountSmartProbeBatchLimit; ordinal++ {
		ctx, cancel := context.WithCancel(context.Background())
		selection := selectSmartAccountForTestWithContext(t, svc, ctx, groupID)
		require.Equal(t, degradedID, selection.Account.ID)
		require.NotZero(t, selection.Account.smartHealthProbeID)
		selection.ReleaseFunc()
		activeID, batchCount, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Equal(t, selection.Account.smartHealthProbeID, activeID)
		require.Equal(t, ordinal-1, batchCount)

		ttft := 1000
		svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, &ttft)
		activeID, batchCount, _, _ = openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Zero(t, activeID)
		if ordinal < openAIAccountSmartProbeBatchLimit {
			require.Equal(t, ordinal, batchCount)
			require.Equal(t, openAIAccountHealthDegraded, scheduler.stats.health(degradedID, time.Now(), svc.openAIStickyEscapeConfig()).level)
		}
		if ordinal == openAIAccountSmartProbeBatchLimit {
			require.Zero(t, batchCount, "the completed batch is reset on recovery")
			require.NotEqual(t, openAIAccountHealthDegraded, scheduler.stats.health(degradedID, time.Now(), svc.openAIStickyEscapeConfig()).level)
		}
		cancel()
	}
}

func TestOpenAIAccountProbeAbortAndAttribution(t *testing.T) {
	t.Run("non_cancellable_context_release_cleans_lease", func(t *testing.T) {
		logHandler := captureOpenAIProbeTraceLogs(t)
		const groupID, degradedID, primaryID int64 = 82301, 82311, 82312
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(degradedID, groupID, 10),
			newSmartSchedulerTestAccount(primaryID, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		selection := selectSmartAccountForTest(t, svc, groupID, "")
		_, beforeBatchCount, _, beforeBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		selection.ReleaseFunc()
		activeID, batchCount, _, afterBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Zero(t, activeID)
		require.Equal(t, beforeBatchCount, batchCount)
		require.Equal(t, beforeBackoff, afterBackoff)
		traceRecords := openAIProbeTraceLogRecords(logHandler.snapshot())
		require.Len(t, traceRecords, 2)
		require.Equal(t, "cancel", traceRecords[1].attrs["phase"])
	})

	t.Run("reported_client_cancellation", func(t *testing.T) {
		const groupID, degradedID, primaryID int64 = 82321, 82331, 82332
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(degradedID, groupID, 10),
			newSmartSchedulerTestAccount(primaryID, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		selection := selectSmartAccountForTestWithContext(t, svc, ctx, groupID)
		_, beforeBatchCount, _, beforeBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", false, nil, context.Canceled)
		activeID, batchCount, _, afterBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Zero(t, activeID)
		require.Equal(t, beforeBatchCount, batchCount)
		require.Equal(t, beforeBackoff, afterBackoff)
		selection.ReleaseFunc()
	})

	t.Run("non_account_error", func(t *testing.T) {
		const groupID, degradedID, primaryID int64 = 82401, 82411, 82412
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(degradedID, groupID, 10),
			newSmartSchedulerTestAccount(primaryID, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		selection := selectSmartAccountForTest(t, svc, groupID, "")
		_, beforeBatchCount, _, beforeBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		requestErr := &UpstreamFailoverError{Stage: GatewayFailureStageAccountAuth, Scope: GatewayFailureScopeRequest}
		svc.ObserveOpenAIAccountHealthFailure(context.Background(), selection.Account, requestErr)
		activeID, batchCount, _, afterBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Zero(t, activeID)
		require.Equal(t, beforeBatchCount, batchCount)
		require.Equal(t, beforeBackoff, afterBackoff)
		selection.ReleaseFunc()
	})

	t.Run("request_context_ends_without_terminal", func(t *testing.T) {
		const groupID, degradedID, primaryID int64 = 82501, 82511, 82512
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(degradedID, groupID, 10),
			newSmartSchedulerTestAccount(primaryID, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		ctx, cancel := context.WithCancel(context.Background())
		selection := selectSmartAccountForTestWithContext(t, svc, ctx, groupID)
		_, beforeBatchCount, _, beforeBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		cancel()
		require.Eventually(t, func() bool {
			activeID, _, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
			return activeID == 0
		}, time.Second, time.Millisecond)
		activeID, batchCount, _, afterBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Zero(t, activeID)
		require.Equal(t, beforeBatchCount, batchCount)
		require.Equal(t, beforeBackoff, afterBackoff)
		if selection.ReleaseFunc != nil {
			selection.ReleaseFunc()
		}
	})

	t.Run("profit_terminal_veto_cancels_immediately", func(t *testing.T) {
		const groupID, degradedID, primaryID int64 = 82601, 82611, 82612
		degraded := newSmartSchedulerTestAccount(degradedID, groupID, 10)
		degraded.RateMultiplier = float64PtrForTest(1)
		svc := newSmartSchedulerTestService([]Account{
			degraded,
			newSmartSchedulerTestAccount(primaryID, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		selection := selectSmartAccountForTest(t, svc, groupID, "")
		_, beforeBatchCount, _, beforeBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		gateCtx := context.WithValue(context.Background(), openAIProfitControlGateCtxKey{}, &openAIProfitControlGate{threshold: 0.1, pricingAt: time.Now()})
		_, vetoed, _ := svc.ProfitControlVetoLatest(gateCtx, selection.Account)
		require.True(t, vetoed)
		activeID, batchCount, _, afterBackoff := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Zero(t, activeID)
		require.Equal(t, beforeBatchCount, batchCount)
		require.Equal(t, beforeBackoff, afterBackoff)
		selection.ReleaseFunc()
	})
}

func TestOpenAIAccountProbeLongRequestAndStaleResult(t *testing.T) {
	t.Run("request_over_thirty_seconds_stays_single_probe", func(t *testing.T) {
		const groupID, degradedID, primaryID int64 = 82701, 82711, 82712
		svc := newSmartSchedulerTestService([]Account{
			newSmartSchedulerTestAccount(degradedID, groupID, 10),
			newSmartSchedulerTestAccount(primaryID, groupID, 20),
		}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		ctx, cancel := context.WithCancel(context.Background())
		first := selectSmartAccountForTestWithContext(t, svc, ctx, groupID)
		firstProbeID := first.Account.smartHealthProbeID
		require.NotZero(t, firstProbeID)
		first.ReleaseFunc()
		stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, degradedID)
		stat.mu.Lock()
		stat.probeStartedAt = time.Now().Add(-31 * time.Second)
		stat.mu.Unlock()

		second := selectSmartAccountForTest(t, svc, groupID, "")
		require.NotEqual(t, firstProbeID, second.Account.smartHealthProbeID)
		activeID, _, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Equal(t, firstProbeID, activeID)
		if second.ReleaseFunc != nil {
			second.ReleaseFunc()
		}
		cancel()
		require.Eventually(t, func() bool {
			activeID, _, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
			return activeID == 0
		}, time.Second, time.Millisecond)
	})

	t.Run("report_and_cancel_have_one_terminal", func(t *testing.T) {
		for _, scenario := range []struct {
			name             string
			wantBatchCount   int
			assertBatchCount bool
		}{
			{name: "report_first", wantBatchCount: 1, assertBatchCount: true},
			{name: "cancel_first", wantBatchCount: 0, assertBatchCount: true},
			{name: "unplanned_race"},
		} {
			t.Run(scenario.name, func(t *testing.T) {
				const groupID, degradedID int64 = 82801, 82811
				account := newSmartSchedulerTestAccount(degradedID, groupID, 10)
				svc := newSmartSchedulerTestService([]Account{account}, schedulerTestConcurrencyCache{})
				scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
				require.True(t, ok)
				seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
				ctx := newSchedulerProbeAfterFuncContext(nil)
				selection := selectSmartProbeDirectForTest(t, svc, ctx, account, groupID)
				probeID := selection.Account.smartHealthProbeID
				require.NotZero(t, probeID)
				selection.ReleaseFunc()
				waitForPropagationCallbacks := func() {
					t.Helper()
					select {
					case <-ctx.callbackStarted:
					case <-time.After(time.Second):
						t.Fatal("fake-context propagation callback did not start")
					}
					ctx.waitCallbacks(t)
				}
				ttft := 1000
				reportDone := make(chan struct{})
				report := func() {
					svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, &ttft)
					close(reportDone)
				}
				waitForReport := func() {
					t.Helper()
					select {
					case <-reportDone:
					case <-time.After(time.Second):
						t.Fatal("probe result report did not finish")
					}
				}
				switch scenario.name {
				case "report_first":
					go report()
					waitForReport()
					ctx.cancel()
					waitForPropagationCallbacks()
				case "cancel_first":
					ctx.cancel()
					waitForPropagationCallbacks()
					waitForOpenAIProbeCancellationCleanupForTest(t, scheduler.stats, degradedID)
					activeID, batchCount, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
					require.Zero(t, activeID, "probe lease must be cleared before the late report")
					require.Zero(t, batchCount, "cancellation must not add a batch sample")
					go report()
					waitForReport()
				default:
					start := make(chan struct{})
					cancelDone := make(chan struct{})
					go func() {
						<-start
						report()
					}()
					go func() {
						<-start
						ctx.cancel()
						close(cancelDone)
					}()
					close(start)
					waitForReport()
					select {
					case <-cancelDone:
					case <-time.After(time.Second):
						t.Fatal("request cancellation did not finish")
					}
					waitForPropagationCallbacks()
					waitForOpenAIProbeCancellationCleanupForTest(t, scheduler.stats, degradedID)
				}

				activeID, batchCount, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
				require.Zero(t, activeID)
				stat := openAIAccountRuntimeStatForTest(t, scheduler.stats, degradedID)
				stat.mu.Lock()
				traceStarted := stat.probeTraceStarted
				stat.mu.Unlock()
				require.False(t, traceStarted)
				if scenario.assertBatchCount {
					require.Equal(t, scenario.wantBatchCount, batchCount)
				} else {
					require.GreaterOrEqual(t, batchCount, 0)
					require.LessOrEqual(t, batchCount, 1)
				}
			})
		}
	})

	t.Run("old_cancellation_callback_cannot_clear_new_identity", func(t *testing.T) {
		const groupID, degradedID int64 = 82901, 82911
		account := newSmartSchedulerTestAccount(degradedID, groupID, 10)
		svc := newSmartSchedulerTestService([]Account{account}, schedulerTestConcurrencyCache{})
		scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
		require.True(t, ok)
		seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
		gate := make(chan struct{})
		oldCtx := newSchedulerProbeAfterFuncContext(gate)
		first := selectSmartProbeDirectForTest(t, svc, oldCtx, account, groupID)
		firstProbeID := first.Account.smartHealthProbeID
		first.ReleaseFunc()
		ttft := 1000
		svc.ReportOpenAIAccountScheduleResult(first.Account, "gpt-5.1", true, &ttft)
		second := selectSmartProbeDirectForTest(t, svc, context.Background(), account, groupID)
		secondProbeID := second.Account.smartHealthProbeID
		require.NotZero(t, secondProbeID)
		require.NotEqual(t, firstProbeID, secondProbeID)

		oldCtx.cancel()
		select {
		case <-oldCtx.callbackStarted:
		case <-time.After(time.Second):
			t.Fatal("old request cleanup callback did not start")
		}
		close(gate)
		oldCtx.waitCallbacks(t)
		activeID, batchCount, _, _ := openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Equal(t, secondProbeID, activeID)
		require.Equal(t, 1, batchCount)
		svc.ReportOpenAIAccountScheduleResult(first.Account, "gpt-5.1", true, &ttft)
		activeID, batchCount, _, _ = openAIProbeLeaseStateForTest(t, scheduler.stats, degradedID)
		require.Equal(t, secondProbeID, activeID)
		require.Equal(t, 1, batchCount)
		second.ReleaseFunc()
	})
}

func TestOpenAIAccountProbeTraceRedactionAfterRelease(t *testing.T) {
	logHandler := captureOpenAIProbeTraceLogs(t)
	const groupID, degradedID, primaryID int64 = 83001, 83011, 83012
	svc := newSmartSchedulerTestService([]Account{
		newSmartSchedulerTestAccount(degradedID, groupID, 10),
		newSmartSchedulerTestAccount(primaryID, groupID, 20),
	}, schedulerTestConcurrencyCache{})
	scheduler, ok := svc.getOpenAIAccountScheduler(context.Background()).(*defaultOpenAIAccountScheduler)
	require.True(t, ok)
	seedSmartDegradedAccount(t, scheduler.stats, degradedID, "error_rate", time.Now().Add(-2*time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selection := selectSmartAccountForTestWithContext(t, svc, ctx, groupID)
	require.Equal(t, degradedID, selection.Account.ID)
	require.NotZero(t, selection.Account.smartHealthProbeID)
	selection.ReleaseFunc()
	ttft := 750
	svc.ReportOpenAIAccountScheduleResult(selection.Account, "gpt-5.1", true, &ttft)

	traceRecords := openAIProbeTraceLogRecords(logHandler.snapshot())
	require.Len(t, traceRecords, 2)
	require.Equal(t, "start", traceRecords[0].attrs["phase"])
	require.Equal(t, "finish", traceRecords[1].attrs["phase"])
	require.Equal(t, "qualified", traceRecords[1].attrs["outcome"])
	for _, record := range traceRecords {
		requireOpenAIProbeTraceFieldWhitelist(t, record)
	}
}
