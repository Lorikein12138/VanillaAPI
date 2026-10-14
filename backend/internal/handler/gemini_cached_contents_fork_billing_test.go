//go:build unit

package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCachedContentAliasStatsAndStorageChargedOnce(t *testing.T) {
	channelService := cachedBillingChannelService(42, cachedBillingAccountStatsRules())
	env := newCachedBillingEnvWithChannelService(t, channelService, 500, 900)
	env.group.RateMultiplier = 1.5
	account := env.accounts[900]
	channelUsage := service.ChannelUsageFields{
		OriginalModel:      "gemini-alias",
		ChannelMappedModel: "gemini-2.5-pro",
		BillingModelSource: service.BillingModelSourceRequested,
	}

	err := env.h.gatewayService.RecordUsage(context.Background(), &service.RecordUsageInput{
		Result: &service.ForwardResult{
			RequestID:              "gcache:create:alias-1",
			Model:                  "gemini-alias",
			UpstreamModel:          "gemini-2.5-pro",
			Usage:                  service.ClaudeUsage{InputTokens: 1},
			CacheStorageTokenHours: 2,
		},
		APIKey:             env.apiKey,
		User:               env.apiKey.User,
		Account:            account,
		ChannelUsageFields: channelUsage,
	})
	require.NoError(t, err)
	require.Len(t, env.usage.totals(), 1)
	log := env.usage.first()
	require.Equal(t, "gemini-alias", log.Model)
	require.Equal(t, "gemini-alias", log.RequestedModel)
	require.NotNil(t, log.UpstreamModel)
	require.Equal(t, "gemini-2.5-pro", *log.UpstreamModel)
	require.InDelta(t, cachedBillingInputPrice+2*cachedBillingAliasStoragePrice, log.TotalCost, 1e-12,
		"the requested alias supplies input and storage prices exactly once")
	require.InDelta(t, log.TotalCost*1.5, log.ActualCost, 1e-12, "the non-unit group multiplier applies to the client charge")
	require.NotNil(t, log.AccountStatsCost)
	require.InDelta(t, 0.000002+2*cachedBillingAliasStoragePrice, *log.AccountStatsCost, 1e-12,
		"the group-matched requested alias token rule gets one official storage charge")

	targetErr := env.h.gatewayService.RecordUsage(context.Background(), &service.RecordUsageInput{
		Result: &service.ForwardResult{
			RequestID: "gcache:create:target-account-rule", Model: "gemini-2.5-pro", UpstreamModel: "gemini-2.5-pro",
			Usage: service.ClaudeUsage{InputTokens: 1}, CacheStorageTokenHours: 2,
		},
		APIKey: env.apiKey, User: env.apiKey.User, Account: account,
		ChannelUsageFields: service.ChannelUsageFields{OriginalModel: "gemini-2.5-pro", ChannelMappedModel: "gemini-2.5-pro",
			BillingModelSource: service.BillingModelSourceRequested},
	})
	require.NoError(t, targetErr)
	targetLog := env.usage.all()[1]
	require.NotNil(t, targetLog.AccountStatsCost)
	require.InDelta(t, 0.0001+2*cachedBillingStoragePrice, *targetLog.AccountStatsCost, 1e-12,
		"the account-matched target rule has its own token price and one official storage charge")
	require.NotEqual(t, *targetLog.AccountStatsCost, *log.AccountStatsCost,
		"the requested alias and upstream target use distinct account-stat rules")
}

func TestCachedContentAliasStatsNilFallbackIncludesStorageOnce(t *testing.T) {
	env := newCachedBillingEnv(t, 500, 900)
	err := env.h.gatewayService.RecordUsage(context.Background(), &service.RecordUsageInput{
		Result: &service.ForwardResult{
			RequestID: "gcache:create:nil-fallback", Model: "gemini-alias", UpstreamModel: "gemini-2.5-pro",
			Usage: service.ClaudeUsage{InputTokens: 1}, CacheStorageTokenHours: 2,
		},
		APIKey: env.apiKey, User: env.apiKey.User, Account: env.accounts[900],
		ChannelUsageFields: service.ChannelUsageFields{OriginalModel: "gemini-alias", ChannelMappedModel: "gemini-2.5-pro",
			BillingModelSource: service.BillingModelSourceRequested},
	})
	require.NoError(t, err)
	log := env.usage.first()
	require.Nil(t, log.AccountStatsCost, "without account-stat pricing, total cost already contains storage once")
	require.InDelta(t, cachedBillingInputPrice+2*cachedBillingAliasStoragePrice, log.TotalCost, 1e-12)
}

func TestCachedContentExtensionKeepsChannelUsageAndPricingAt(t *testing.T) {
	env := newCachedBillingEnvWithChannelService(t, cachedBillingChannelService(42, cachedBillingAccountStatsRules()), 1000, 900)
	env.group.RateMultiplier = 1.5
	env.seed(time.Now().Add(time.Hour))
	cache := env.repo.records["abc"]
	cache.Model = "gemini-alias"
	cache.UpstreamModel = "models/gemini-2.5-pro"
	cache.ChannelUsage = service.ChannelUsageFields{
		ChannelID:          5,
		OriginalModel:      "gemini-alias",
		ChannelMappedModel: "gemini-2.5-pro",
		BillingModelSource: service.BillingModelSourceRequested,
	}

	first := env.patchTTL("abc", "7200s")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	firstUntil := env.repo.records["abc"].ExpireTime
	second := env.patchTTL("abc", "10800s")
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())
	logs := env.waitForUsage(t, 2)
	require.Len(t, logs, 2)

	usageLogs := env.usage.all()
	requestIDs := map[string]struct{}{}
	for _, usageLog := range usageLogs {
		require.True(t, strings.HasPrefix(usageLog.RequestID, "gcache:patch:abc:"), usageLog.RequestID)
		requestIDs[usageLog.RequestID] = struct{}{}
		require.NotNil(t, usageLog.ChannelID)
		require.Equal(t, int64(5), *usageLog.ChannelID, "every extension retains the creation ChannelUsage")
		require.Equal(t, "gemini-alias", usageLog.RequestedModel)
		require.Zero(t, usageLog.InputTokens)
		require.Zero(t, usageLog.OutputTokens)
		require.Zero(t, usageLog.TotalTokens(), "storage-only extensions are not model-token requests")
		require.InDelta(t, usageLog.TotalCost*1.5, usageLog.ActualCost, 1e-10)
	}
	require.Len(t, requestIDs, 2, "each extension has its own idempotent usage record")
	require.True(t, env.repo.records["abc"].ExpireTime.After(firstUntil), "the second request extends from the first committed expiry")
}

func TestCachedContentCreateAndExtensionsCarryDistinctRequestPricingTimes(t *testing.T) {
	env := newCachedBillingEnv(t, 1000, 900)
	env.upstream.createBody = cachedBillingCreateBody(time.Now().Add(100 * time.Hour))

	created := env.create()
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	require.Len(t, env.repo.records, 1)
	cacheID := ""
	for id := range env.repo.records {
		cacheID = id
	}
	time.Sleep(5 * time.Millisecond)
	first := env.patchTTL(cacheID, "7200s")
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	time.Sleep(5 * time.Millisecond)
	second := env.patchTTL(cacheID, "10800s")
	require.Equal(t, http.StatusOK, second.Code, second.Body.String())

	pricingTimes := env.upstream.pricingTimes()
	require.Len(t, pricingTimes, 3, "one create and two upstream extensions each carry the request context")
	for i, pricingAt := range pricingTimes {
		require.False(t, pricingAt.IsZero(), "upstream request %d must carry a frozen token-pricing time", i)
		if i > 0 {
			require.NotEqual(t, pricingTimes[i-1], pricingAt, "each HTTP request captures its own pricing instant")
		}
	}
}

func TestCachedContentUsageQuotesMatchDelayedUsageHandoffAcrossPeakValley(t *testing.T) {
	env := newCachedBillingEnvWithChannelService(t, cachedBillingChannelService(42, cachedBillingAccountStatsRules()), 5000, 900)
	env.group.RateMultiplier = 1.5
	env.group.SubscriptionType = service.SubscriptionTypeSubscription
	env.group.PeakRateEnabled = true
	env.group.PeakStart = "08:00"
	env.group.PeakEnd = "20:00"
	env.group.PeakRateMultiplier = 2

	loc := timezone.Location()
	createAt := time.Date(2026, 10, 14, 10, 0, 0, 0, loc)
	extendValleyAt := time.Date(2026, 10, 14, 22, 0, 0, 0, loc)
	extendPeakAt := time.Date(2026, 10, 15, 10, 0, 0, 0, loc)
	require.True(t, createAt.Before(time.Date(2026, 10, 14, 20, 0, 0, 0, loc)))
	require.True(t, extendValleyAt.After(time.Date(2026, 10, 14, 20, 0, 0, 0, loc)))
	require.True(t, extendPeakAt.Before(time.Date(2026, 10, 15, 20, 0, 0, 0, loc)))

	usageFields := service.ChannelUsageFields{
		ChannelID: 5, OriginalModel: "gemini-alias", ChannelMappedModel: "gemini-2.5-pro",
		BillingModelSource: service.BillingModelSourceRequested,
	}
	type usageCase struct {
		pricingAt         time.Time
		requestID         string
		inputTokens       int
		storageTokenHours float64
		quotedActualCost  float64
	}
	cases := []usageCase{
		{pricingAt: createAt, requestID: "gcache:create:fixed-time", inputTokens: cachedBillingTokens,
			storageTokenHours: cachedBillingTokens * 24},
		{pricingAt: extendValleyAt, requestID: "gcache:patch:fixed-valley", storageTokenHours: cachedBillingTokens * 2},
		{pricingAt: extendPeakAt, requestID: "gcache:patch:fixed-peak", storageTokenHours: cachedBillingTokens * 2},
	}
	inputs := make(map[string]*service.RecordUsageInput, len(cases))
	for i := range cases {
		result := &service.ForwardResult{
			RequestID: cases[i].requestID, Model: "gemini-alias", UpstreamModel: "gemini-2.5-pro",
			Usage: service.ClaudeUsage{InputTokens: cases[i].inputTokens}, CacheStorageTokenHours: cases[i].storageTokenHours,
		}
		input := &service.RecordUsageInput{
			Result: result, APIKey: env.apiKey, User: env.apiKey.User, Account: env.accounts[900],
			PricingAt: cases[i].pricingAt, ChannelUsageFields: usageFields,
		}
		inputs[cases[i].requestID] = input
		var quote float64
		env.serve(http.MethodPost, "/", "", nil, func(c *gin.Context) {
			quote = env.h.gatewayService.QuoteUsageCost(c.Request.Context(), input)
		})
		require.Greater(t, quote, 0.0)
		cases[i].quotedActualCost = quote
	}
	require.NotEqual(t, cases[1].quotedActualCost, cases[2].quotedActualCost,
		"the two otherwise identical storage-only extensions use valley and peak multipliers")
	require.InDelta(t, 2, cases[2].quotedActualCost/cases[1].quotedActualCost, 1e-12,
		"the peak multiplier doubles the valley extension charge")

	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{
		WorkerCount: 1, QueueSize: 8, TaskTimeout: 5 * time.Second, OverflowPolicy: "drop", AutoScaleEnabled: false,
	})
	release := make(chan struct{})
	started := make(chan struct{})
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() { unlock(); pool.Stop() })
	require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) {
		close(started)
		<-release
	}))
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("usage worker did not start the blocking task")
	}
	env.h.usageRecordWorkerPool = pool
	for _, usageCase := range cases {
		usageCase := usageCase
		env.serve(http.MethodPost, "/", "", nil, func(c *gin.Context) {
			env.h.submitGeminiCachedContentUsage(c, env.apiKey, env.accounts[900], nil, usageCase.pricingAt,
				usageCase.requestID, "gemini-alias", "gemini-2.5-pro", usageCase.inputTokens,
				usageCase.storageTokenHours, usageFields, nil)
		})
	}
	require.Empty(t, env.usage.all(), "the real usage handoff remains queued while the worker is blocked")
	unlock()
	env.waitForUsage(t, len(cases))
	require.Eventually(t, func() bool { return pool.Stats().CompletedTasks >= uint64(len(cases)+1) }, time.Second, 10*time.Millisecond)

	logs := env.usage.all()
	require.Len(t, logs, len(cases))
	byRequestID := make(map[string]*service.UsageLog, len(logs))
	for _, log := range logs {
		byRequestID[log.RequestID] = log
	}
	require.Len(t, byRequestID, len(cases), "create and extension handoffs use distinct idempotency keys")
	for _, usageCase := range cases {
		log := byRequestID[usageCase.requestID]
		require.NotNil(t, log, usageCase.requestID)
		require.InDelta(t, usageCase.quotedActualCost, log.ActualCost, 1e-9,
			"the asynchronous usage record uses the exact quoted PricingAt")
		require.NotNil(t, log.ChannelID)
		require.Equal(t, int64(5), *log.ChannelID, "all records retain the creation ChannelUsage identity")
		require.Equal(t, "gemini-alias", log.RequestedModel)
		require.NotNil(t, log.UpstreamModel)
		require.Equal(t, "gemini-2.5-pro", *log.UpstreamModel)
	}
	for _, requestID := range []string{cases[1].requestID, cases[2].requestID} {
		log := byRequestID[requestID]
		require.Zero(t, log.InputTokens)
		require.Zero(t, log.OutputTokens)
		require.Zero(t, log.TotalTokens(), "extension usage is storage-only, with no model request charge")
		require.InDelta(t, cachedBillingTokens*2*cachedBillingAliasStoragePrice, log.TotalCost, 1e-9,
			"extension cost contains only official storage, without a per-request fee")
	}
}
