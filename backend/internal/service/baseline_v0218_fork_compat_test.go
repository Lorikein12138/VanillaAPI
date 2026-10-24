//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBaselineV0218SharedAccountSnapshotConcurrentReaders(t *testing.T) {
	master := &Account{
		Platform: PlatformAnthropic,
		Credentials: map[string]any{
			"model_mapping": map[string]any{"claude-sonnet": "sonnet-upstream", "claude-opus": "opus-upstream"},
		},
	}
	master.PrepareSharedSnapshot()

	const readers = 24
	var wg sync.WaitGroup
	errCh := make(chan string, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			replica := *master
			mapping := replica.GetModelMapping()
			if mapping["claude-sonnet"] != "sonnet-upstream" || mapping["claude-opus"] != "opus-upstream" {
				errCh <- "shared snapshot returned an incomplete mapping"
			}
			if replica.modelMappingCache.Load() != nil {
				errCh <- "shared fast path populated the ordinary atomic cache"
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.Empty(t, err)
	}
	require.Nil(t, master.modelMappingCache.Load(), "the shared master must keep its ordinary atomic cache zero")
}

func TestBaselineV0218PreparedMasterResetsPreviouslyUsedAtomicCache(t *testing.T) {
	master := &Account{
		Platform:    PlatformAnthropic,
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet": "sonnet-a"}},
	}
	require.Equal(t, "sonnet-a", master.GetModelMapping()["claude-sonnet"])
	require.NotNil(t, master.modelMappingCache.Load(), "precondition: ordinary cache was used before preparation")

	master.PrepareSharedSnapshot()
	require.NotNil(t, master.modelMappingSharedSnapshot)
	require.Nil(t, master.modelMappingCache.Load(), "preparation must reset the already-used atomic.Value")

	replica := *master
	require.Equal(t, "sonnet-a", replica.GetModelMapping()["claude-sonnet"])
	require.Nil(t, replica.modelMappingCache.Load(), "the shared fast path must leave the copied atomic.Value zero")

	replica.Credentials = map[string]any{"model_mapping": map[string]any{"claude-sonnet": "sonnet-b"}}
	require.Equal(t, "sonnet-b", replica.GetModelMapping()["claude-sonnet"], "changed credentials must use the ordinary signature path")
	require.NotNil(t, replica.modelMappingCache.Load(), "the invalidated request copy may own a normal cache")
	require.Nil(t, master.modelMappingCache.Load(), "the request copy must not mutate its shared master")
	require.Equal(t, "sonnet-a", master.modelMappingSharedSnapshot.mapping["claude-sonnet"])
}

func TestBaselineV0218OrdinaryAccountMappingUpdatesRemainVisible(t *testing.T) {
	raw := map[string]any{"claude-sonnet": "sonnet-a"}
	account := &Account{Platform: PlatformAnthropic, Credentials: map[string]any{"model_mapping": raw}}
	previous := account.GetModelMapping()
	require.Equal(t, "sonnet-a", previous["claude-sonnet"])

	raw["claude-sonnet"] = "sonnet-b"
	current := account.GetModelMapping()
	require.Equal(t, "sonnet-b", current["claude-sonnet"], "ordinary account cache must detect same-length in-place changes")
	require.Equal(t, "sonnet-a", previous["claude-sonnet"], "previous immutable readers must keep their snapshot")
}

func TestBaselineV0218SharedReplicaInvalidatesOnRuntimeMappingVersion(t *testing.T) {
	original := xai.RuntimeModelMappingOptions()
	t.Cleanup(func() { xai.SetRuntimeModelMappingOptions(original) })
	xai.SetRuntimeModelMappingOptions(xai.ModelMappingOptions{})

	master := &Account{Platform: PlatformGrok, Credentials: map[string]any{}}
	master.PrepareSharedSnapshot()
	replica := *master
	require.Equal(t, "claude-sonnet-4-5", replica.GetMappedModel("claude-sonnet-4-5"))
	require.Nil(t, replica.modelMappingCache.Load(), "initial shared read must use the prepared version")

	xai.SetRuntimeModelMappingOptions(xai.ModelMappingOptions{DefaultText: "grok-build-0.1", EnableCrossClientMap: true})
	require.Equal(t, "grok-build-0.1", replica.GetMappedModel("claude-sonnet-4-5"), "runtime version change must invalidate the shared snapshot")
	require.NotNil(t, replica.modelMappingCache.Load(), "the invalidated request copy must use the regular cache")
	require.Nil(t, master.modelMappingCache.Load(), "the request-local fallback must not populate the master")
}

func TestBaselineV0218GeminiThoughtTextAndPartialUsageAreSingleObservedResult(t *testing.T) {
	const partialSSE = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"plan","thought":true}]}}]}` + "\n\n" +
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}` + "\n\n"

	requestCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	body := &geminiAbortingStreamBody{
		data:     bytes.NewReader([]byte(partialSSE)),
		abortErr: context.Canceled,
		onAbort:  cancel,
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestCtx)

	result, err := (&GeminiMessagesCompatService{}).handleChatCompletionsStreamingResponseFromGemini(
		c,
		&http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body},
		time.Now(), "gemini-3-pro", false, false,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.clientDisconnect)
	require.False(t, result.finished, "partial stream must not be treated as a completed upstream response")
	require.Equal(t, 7, result.usage.InputTokens)
	require.Equal(t, 3, result.usage.OutputTokens)

	var reasoning, content strings.Builder
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		payload = strings.TrimSpace(payload)
		if !ok || payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          *string `json:"content"`
					ReasoningContent *string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		require.NoError(t, json.Unmarshal([]byte(payload), &chunk), payload)
		for _, choice := range chunk.Choices {
			if choice.Delta.ReasoningContent != nil {
				reasoning.WriteString(*choice.Delta.ReasoningContent)
			}
			if choice.Delta.Content != nil {
				content.WriteString(*choice.Delta.Content)
			}
		}
	}
	require.Equal(t, "plan", reasoning.String(), "thought must be emitted once in reasoning_content")
	require.Equal(t, "answer", content.String(), "visible text must be emitted once as content")
}
