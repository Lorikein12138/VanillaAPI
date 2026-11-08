//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Seed3D 与 Seedance 共用方舟 /contents/generations/tasks 协议（火山方舟「3D 生成」文档）：
// 创建体是 model + content[text(--fileformat/--subdivisionlevel), image_url]，
// 轮询成功时 3D 文件在 content.file_url（zip），usage 带 completion_tokens / total_tokens。
const seed3DSucceededStatusBody = `{"id":"cgt-3d-1","model":"doubao-seed3d-2-0-260328","status":"succeeded","created_at":1762560000,"updated_at":1762560600,"content":{"file_url":"https://ark-content.example/3d/cgt-3d-1.zip"},"error":null,"fileformat":"glb","subdivisionlevel":"medium","usage":{"completion_tokens":30000,"total_tokens":30000}}`

func TestSeed3DCreateForwardsArkContentUnchanged(t *testing.T) {
	body := []byte(`{"model":"doubao-seed3d-2.0","content":[{"type":"text","text":"--subdivisionlevel medium --fileformat glb"},{"type":"image_url","image_url":{"url":"https://example.com/object.png"}}]}`)
	account := seedanceTestAccount()
	account.Credentials["base_url"] = "https://ark.cn-beijing.volces.com/api/coding/v3"
	account.Credentials["model_mapping"] = map[string]any{"doubao-seed3d-2.0": "doubao-seed3d-2-0-260328"}
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"cgt-3d-1"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	c, w := grokMediaContentTestContext(http.MethodPost, "/api/v3/contents/generations/tasks", nil)

	result, err := svc.ForwardSeedance(context.Background(), c, account, SeedanceEndpointCreate, "", body)

	require.NoError(t, err)
	require.JSONEq(t, `{"id":"cgt-3d-1"}`, w.Body.String())
	require.Equal(t, "seedance:cgt-3d-1", result.ResponseID)
	require.Equal(t, "doubao-seed3d-2.0", result.BillingModel)
	require.Equal(t, "https://ark.cn-beijing.volces.com/api/coding/v3/contents/generations/tasks", upstream.request.URL.String())
	forwarded, err := io.ReadAll(upstream.request.Body)
	require.NoError(t, err)
	require.Equal(t, "doubao-seed3d-2-0-260328", gjson.GetBytes(forwarded, "model").String())
	require.Equal(t, gjson.GetBytes(body, "content").Raw, gjson.GetBytes(forwarded, "content").Raw)
}

func TestSeed3DStatusKeepsFileURLAndBillsCompletionTokensOnlyOnSuccess(t *testing.T) {
	for _, status := range []string{"queued", "running", "failed", "succeeded"} {
		t.Run(status, func(t *testing.T) {
			body := seed3DSucceededStatusBody
			if status != "succeeded" {
				body = `{"id":"cgt-3d-1","model":"doubao-seed3d-2-0-260328","status":"` + status + `","content":null,"usage":{"completion_tokens":30000,"total_tokens":30000}}`
			}
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(body)}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			c, w := grokMediaContentTestContext(http.MethodGet, "/api/v3/contents/generations/tasks/cgt-3d-1", nil)

			result, err := svc.ForwardSeedance(context.Background(), c, seedanceTestAccount(), SeedanceEndpointStatus, "seedance:cgt-3d-1", nil)

			require.NoError(t, err)
			require.JSONEq(t, body, w.Body.String(), "轮询响应必须原样透传，3D 文件地址不得丢")
			if status == "succeeded" {
				require.Equal(t, "https://ark-content.example/3d/cgt-3d-1.zip", gjson.Get(w.Body.String(), "content.file_url").String())
				require.Equal(t, 30000, result.Usage.OutputTokens)
			} else {
				require.Zero(t, result.Usage.OutputTokens, "未成功的任务不计费")
			}
			require.Zero(t, result.VideoCount)
		})
	}
}

// 渠道 16：doubao-seed3d-2.0 按次 2.40 元。方舟官方口径是「3 万 token/次 × 0.80 元/万 token」，
// 两种渠道配置对同一次成功任务都必须结算 2.40。
func TestSeed3DCompletionCostMatchesChannelPerRequestAndTokenPricing(t *testing.T) {
	const model = "doubao-seed3d-2.0"
	cases := []struct {
		name    string
		pricing *ChannelModelPricing
		mode    BillingMode
	}{
		{name: "per_request", mode: BillingModePerRequest, pricing: &ChannelModelPricing{BillingMode: BillingModePerRequest, PerRequestPrice: f64p(2.40)}},
		{name: "token", mode: BillingModeToken, pricing: &ChannelModelPricing{BillingMode: BillingModeToken, InputPrice: f64p(0), OutputPrice: f64p(80e-6)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groupID := int64(916)
			resolver := newOpenAITokenImageChannelPricingResolverForTest(t, groupID, model)
			cached, ok := resolver.channelService.cache.Load().(*channelCache)
			require.True(t, ok)
			cached.pricingByGroupModel[channelModelKey{groupID: groupID, model: model}] = tc.pricing
			apiKey := &APIKey{ID: 16, GroupID: &groupID, Group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}}
			svc := &OpenAIGatewayService{billingService: resolver.billingService, resolver: resolver}

			cost, err := svc.calculateOpenAIRecordUsageCost(context.Background(), &OpenAIForwardResult{
				Model: model, BillingModel: model, ResponseID: "seedance:cgt-3d-1",
				Usage: OpenAIUsage{OutputTokens: 30000},
			}, apiKey, []string{model}, 1, 1, 1, 1, UsageTokens{OutputTokens: 30000}, "", nil, false, time.Time{})

			require.NoError(t, err)
			require.NotNil(t, cost)
			require.Equal(t, string(tc.mode), cost.BillingMode)
			require.InDelta(t, 2.40, cost.TotalCost, 1e-9)
			require.InDelta(t, 2.40, cost.ActualCost, 1e-9)
		})
	}
}
