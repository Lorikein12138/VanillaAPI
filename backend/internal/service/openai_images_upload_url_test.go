//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	uploadURLTestAPIKey    = "sk-test-placeholder-not-real"
	uploadURLTestSignature = "SIGNATURE-SHOULD-NOT-LEAK"
	uploadURLTestBaseURL   = "https://images-upstream.example.test"
	uploadURLTestPutTarget = "https://oss.example.test/bucket/object.png"
)

var (
	uploadURLTestPNG  = append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{1}, 64)...)
	uploadURLTestJPEG = append([]byte("\xFF\xD8\xFF\xE0\x00\x10JFIF\x00"), bytes.Repeat([]byte{2}, 64)...)
	uploadURLTestWEBP = append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{3}, 64)...)
	uploadURLTestGIF  = append([]byte("GIF89a"), bytes.Repeat([]byte{4}, 64)...)
)

type uploadURLTestCall struct {
	method      string
	url         string
	contentType string
	auth        string
	body        []byte
}

type uploadURLTestUpstream struct {
	HTTPUpstream

	mu    sync.Mutex
	calls []uploadURLTestCall
	n     int

	uploadURLStatus int
	uploadURLBody   string
	putTarget       string
	putStatus       int
	putErr          error
}

func (u *uploadURLTestUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = append(u.calls, uploadURLTestCall{
		method:      req.Method,
		url:         req.URL.String(),
		contentType: req.Header.Get("Content-Type"),
		auth:        req.Header.Get("Authorization"),
		body:        body,
	})
	jsonResp := func(status int, payload string) *http.Response {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"req-upload-test"}},
			Body:       io.NopCloser(strings.NewReader(payload)),
		}
	}
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/v1/images/upload-url"):
		if u.uploadURLStatus != 0 {
			return jsonResp(u.uploadURLStatus, u.uploadURLBody), nil
		}
		if u.uploadURLBody != "" {
			return jsonResp(http.StatusOK, u.uploadURLBody), nil
		}
		u.n++
		target := u.putTarget
		if target == "" {
			target = uploadURLTestPutTarget
		}
		contentType := gjson.GetBytes(body, "content_type").String()
		return jsonResp(http.StatusOK, fmt.Sprintf(
			`{"content_type":%q,"hint":"PUT the bytes","image_url":"https://cdn.example.test/uploaded-%d.png","max_bytes":33554432,"method":"PUT","upload_url":"%s?n=%d&Signature=%s"}`,
			contentType, u.n, target, u.n, uploadURLTestSignature,
		)), nil
	case req.Method == http.MethodPut:
		if u.putErr != nil {
			return nil, &url.Error{Op: "Put", URL: req.URL.String(), Err: u.putErr}
		}
		status := u.putStatus
		if status == 0 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("<Error>" + req.URL.RawQuery + "</Error>"))}, nil
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/v1/images/edits"):
		return jsonResp(http.StatusOK, `{"created":1,"data":[{"url":"https://cdn.example.test/out.png"}]}`), nil
	default:
		return jsonResp(http.StatusNotFound, `{"error":{"message":"unexpected test route"}}`), nil
	}
}

func (u *uploadURLTestUpstream) snapshot() []uploadURLTestCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]uploadURLTestCall(nil), u.calls...)
}

func (u *uploadURLTestUpstream) callsMatching(method, pathSuffix string) []uploadURLTestCall {
	var out []uploadURLTestCall
	for _, call := range u.snapshot() {
		if call.method != method {
			continue
		}
		if pathSuffix != "" {
			parsed, err := url.Parse(call.url)
			if err != nil || !strings.HasSuffix(parsed.Path, pathSuffix) {
				continue
			}
		}
		out = append(out, call)
	}
	return out
}

type uploadURLTestFile struct {
	field       string
	name        string
	contentType string
	data        []byte
}

func buildUploadURLTestMultipart(t *testing.T, fields [][2]string, files []uploadURLTestFile) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for _, field := range fields {
		require.NoError(t, writer.WriteField(field[0], field[1]))
	}
	for _, file := range files {
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, file.field, file.name))
		header.Set("Content-Type", file.contentType)
		part, err := writer.CreatePart(header)
		require.NoError(t, err)
		_, err = part.Write(file.data)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return buf.Bytes(), writer.FormDataContentType()
}

type uploadURLTestRun struct {
	upstream *uploadURLTestUpstream
	recorder *httptest.ResponseRecorder
	ctx      *gin.Context
	result   *OpenAIForwardResult
	err      error
	logs     *inMemoryLogSink
}

func runUploadURLTestForward(t *testing.T, enabled bool, upstream *uploadURLTestUpstream, body []byte, contentType string) *uploadURLTestRun {
	t.Helper()
	logs, restore := captureStructuredLog(t)
	defer restore()

	svc := &OpenAIGatewayService{httpUpstream: upstream, cfg: &config.Config{}}
	account := &Account{
		ID: 56, Name: "image-upload-test", Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
		Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"api_key": uploadURLTestAPIKey, "base_url": uploadURLTestBaseURL},
		Extra:       map[string]any{},
	}
	if enabled {
		account.Extra[openAIImagesUploadURLEnabledExtraKey] = true
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", contentType)
	parsed, err := svc.ParseOpenAIImagesRequest(c, body)
	require.NoError(t, err)

	result, forwardErr := svc.ForwardImages(context.Background(), c, account, body, parsed, "")
	return &uploadURLTestRun{upstream: upstream, recorder: recorder, ctx: c, result: result, err: forwardErr, logs: logs}
}

// assertNoUploadSecretsLeaked 校验签名地址与上游 Key 不出现在响应、错误、运维错误详情与日志里。
func assertNoUploadSecretsLeaked(t *testing.T, run *uploadURLTestRun) {
	t.Helper()
	var haystack strings.Builder
	haystack.WriteString(run.recorder.Body.String())
	if run.err != nil {
		haystack.WriteString(run.err.Error())
	}
	for _, key := range []string{OpsUpstreamErrorMessageKey, OpsUpstreamErrorDetailKey} {
		if value, ok := run.ctx.Get(key); ok {
			haystack.WriteString(fmt.Sprint(value))
		}
	}
	if value, ok := run.ctx.Get(OpsUpstreamErrorsKey); ok {
		if events, ok := value.([]*OpsUpstreamErrorEvent); ok {
			for _, ev := range events {
				haystack.WriteString(fmt.Sprintf("%+v", *ev))
			}
		}
	}
	run.logs.mu.Lock()
	for _, ev := range run.logs.events {
		haystack.WriteString(fmt.Sprintf("%+v", *ev))
	}
	run.logs.mu.Unlock()
	text := haystack.String()
	require.NotContains(t, text, uploadURLTestSignature)
	require.NotContains(t, text, "Signature=")
	require.NotContains(t, text, uploadURLTestAPIKey)
}

func requireUploadURLClientError(t *testing.T, run *uploadURLTestRun, status int, code string) {
	t.Helper()
	var imageErr *OpenAIImagesUpstreamError
	require.ErrorAs(t, run.err, &imageErr)
	require.Equal(t, status, imageErr.StatusCode)
	require.Equal(t, status, run.recorder.Code)
	require.Equal(t, code, gjson.Get(run.recorder.Body.String(), "error.code").String())
	require.Empty(t, run.upstream.callsMatching(http.MethodPost, "/v1/images/edits"), "edits must not be forwarded")
}

func TestOpenAIImagesUploadURL_MultipartSingleImage(t *testing.T) {
	upstream := &uploadURLTestUpstream{}
	body, contentType := buildUploadURLTestMultipart(t,
		[][2]string{{"model", "gpt-image-2"}, {"prompt", "make it blue"}, {"n", "1"}, {"size", "1024x1024"}},
		[]uploadURLTestFile{{field: "image", name: "a.png", contentType: "application/octet-stream", data: uploadURLTestPNG}},
	)
	run := runUploadURLTestForward(t, true, upstream, body, contentType)
	require.NoError(t, run.err)
	require.NotNil(t, run.result)
	require.Equal(t, 1, run.result.ImageCount)

	tickets := upstream.callsMatching(http.MethodPost, "/v1/images/upload-url")
	require.Len(t, tickets, 1)
	require.Equal(t, "Bearer "+uploadURLTestAPIKey, tickets[0].auth)
	require.Equal(t, "image/png", gjson.GetBytes(tickets[0].body, "content_type").String())
	require.True(t, strings.HasPrefix(tickets[0].url, uploadURLTestBaseURL+"/v1/images/upload-url"))

	puts := upstream.callsMatching(http.MethodPut, "")
	require.Len(t, puts, 1)
	require.Equal(t, uploadURLTestPNG, puts[0].body)
	require.Equal(t, "image/png", puts[0].contentType)
	require.Empty(t, puts[0].auth, "PUT to the signed url must not carry account credentials")

	edits := upstream.callsMatching(http.MethodPost, "/v1/images/edits")
	require.Len(t, edits, 1)
	require.Equal(t, "application/json", edits[0].contentType)
	forwarded := edits[0].body
	require.Equal(t, "gpt-image-2", gjson.GetBytes(forwarded, "model").String())
	require.Equal(t, "make it blue", gjson.GetBytes(forwarded, "prompt").String())
	require.Equal(t, gjson.Number, gjson.GetBytes(forwarded, "n").Type)
	require.Equal(t, int64(1), gjson.GetBytes(forwarded, "n").Int())
	require.Equal(t, "1024x1024", gjson.GetBytes(forwarded, "size").String())
	require.Equal(t, []string{"https://cdn.example.test/uploaded-1.png"}, uploadURLTestImageURLs(forwarded))
	require.False(t, gjson.GetBytes(forwarded, "mask").Exists())

	// 上传在主请求之前、按顺序发生。
	calls := upstream.snapshot()
	require.Len(t, calls, 3)
	require.Equal(t, http.MethodPost, calls[0].method)
	require.Equal(t, http.MethodPut, calls[1].method)
	require.Equal(t, http.MethodPost, calls[2].method)
	assertNoUploadSecretsLeaked(t, run)
}

func TestOpenAIImagesUploadURL_MultipartMultipleImagesAndMask(t *testing.T) {
	upstream := &uploadURLTestUpstream{}
	body, contentType := buildUploadURLTestMultipart(t,
		[][2]string{{"model", "gpt-image-2"}, {"prompt", "merge"}, {"stream", "false"}, {"output_compression", "80"}, {"quality", "high"}},
		[]uploadURLTestFile{
			{field: "image[]", name: "a.png", contentType: "image/png", data: uploadURLTestPNG},
			{field: "image[]", name: "b.jpg", contentType: "image/jpeg", data: uploadURLTestJPEG},
			{field: "mask", name: "m.webp", contentType: "image/webp", data: uploadURLTestWEBP},
		},
	)
	run := runUploadURLTestForward(t, true, upstream, body, contentType)
	require.NoError(t, run.err)
	require.Equal(t, 1, run.result.ImageCount)

	tickets := upstream.callsMatching(http.MethodPost, "/v1/images/upload-url")
	require.Len(t, tickets, 3)
	require.Equal(t, "image/png", gjson.GetBytes(tickets[0].body, "content_type").String())
	require.Equal(t, "image/jpeg", gjson.GetBytes(tickets[1].body, "content_type").String())
	require.Equal(t, "image/webp", gjson.GetBytes(tickets[2].body, "content_type").String())
	puts := upstream.callsMatching(http.MethodPut, "")
	require.Len(t, puts, 3)
	require.Equal(t, uploadURLTestPNG, puts[0].body)
	require.Equal(t, uploadURLTestJPEG, puts[1].body)
	require.Equal(t, uploadURLTestWEBP, puts[2].body)

	forwarded := upstream.callsMatching(http.MethodPost, "/v1/images/edits")[0].body
	require.Equal(t, []string{"https://cdn.example.test/uploaded-1.png", "https://cdn.example.test/uploaded-2.png"}, uploadURLTestImageURLs(forwarded))
	require.Equal(t, "https://cdn.example.test/uploaded-3.png", gjson.GetBytes(forwarded, "mask.image_url").String())
	require.Equal(t, gjson.False, gjson.GetBytes(forwarded, "stream").Type)
	require.Equal(t, int64(80), gjson.GetBytes(forwarded, "output_compression").Int())
	require.Equal(t, "high", gjson.GetBytes(forwarded, "quality").String())
	assertNoUploadSecretsLeaked(t, run)
}

func TestOpenAIImagesUploadURL_JSONDataURLAndBareBase64(t *testing.T) {
	upstream := &uploadURLTestUpstream{}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(uploadURLTestPNG)
	bare := base64.StdEncoding.EncodeToString(uploadURLTestJPEG)
	maskURL := "data:image/webp;base64," + base64.StdEncoding.EncodeToString(uploadURLTestWEBP)
	body := []byte(fmt.Sprintf(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":%q},{"image_url":"https://public.example.test/ref.png"},{"image_url":%q}],"mask":{"image_url":%q},"size":"2048x2048"}`,
		dataURL, bare, maskURL))
	run := runUploadURLTestForward(t, true, upstream, body, "application/json")
	require.NoError(t, run.err)
	require.Equal(t, 1, run.result.ImageCount)

	require.Len(t, upstream.callsMatching(http.MethodPost, "/v1/images/upload-url"), 3)
	puts := upstream.callsMatching(http.MethodPut, "")
	require.Len(t, puts, 3)
	require.Equal(t, uploadURLTestPNG, puts[0].body)
	require.Equal(t, uploadURLTestJPEG, puts[1].body)
	require.Equal(t, uploadURLTestWEBP, puts[2].body)

	forwarded := upstream.callsMatching(http.MethodPost, "/v1/images/edits")[0].body
	require.Equal(t, []string{
		"https://cdn.example.test/uploaded-1.png",
		"https://public.example.test/ref.png",
		"https://cdn.example.test/uploaded-2.png",
	}, uploadURLTestImageURLs(forwarded))
	require.Equal(t, "https://cdn.example.test/uploaded-3.png", gjson.GetBytes(forwarded, "mask.image_url").String())
	require.Equal(t, "2048x2048", gjson.GetBytes(forwarded, "size").String())
	require.NotContains(t, string(forwarded), "data:")
	assertNoUploadSecretsLeaked(t, run)
}

func TestOpenAIImagesUploadURL_PublicURLIsNotUploaded(t *testing.T) {
	upstream := &uploadURLTestUpstream{}
	body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"https://public.example.test/ref.png"}]}`)
	run := runUploadURLTestForward(t, true, upstream, body, "application/json")
	require.NoError(t, run.err)
	require.Equal(t, 1, run.result.ImageCount)

	require.Empty(t, upstream.callsMatching(http.MethodPost, "/v1/images/upload-url"))
	require.Empty(t, upstream.callsMatching(http.MethodPut, ""))
	edits := upstream.callsMatching(http.MethodPost, "/v1/images/edits")
	require.Len(t, edits, 1)
	expected, _, err := rewriteOpenAIImagesModel(body, "application/json", "gpt-image-2")
	require.NoError(t, err)
	require.Equal(t, expected, edits[0].body)
	require.Equal(t, "application/json", edits[0].contentType)
}

func TestOpenAIImagesUploadURL_DisabledForwardsByteIdentical(t *testing.T) {
	t.Run("multipart", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{}
		body, contentType := buildUploadURLTestMultipart(t,
			[][2]string{{"model", "gpt-image-2"}, {"prompt", "edit"}},
			[]uploadURLTestFile{{field: "image", name: "a.png", contentType: "image/png", data: uploadURLTestPNG}},
		)
		run := runUploadURLTestForward(t, false, upstream, body, contentType)
		require.NoError(t, run.err)

		calls := upstream.snapshot()
		require.Len(t, calls, 1, "switch off must not call upload-url or PUT")
		expectedBody, expectedType, err := rewriteOpenAIImagesModel(body, contentType, "gpt-image-2")
		require.NoError(t, err)
		// 改写 model 时 multipart 边界随机生成：把实际边界换成期望边界后逐字节比较。
		actualBoundary := uploadURLTestBoundary(t, calls[0].contentType)
		expectedBoundary := uploadURLTestBoundary(t, expectedType)
		require.Equal(t, expectedBody, bytes.ReplaceAll(calls[0].body, []byte(actualBoundary), []byte(expectedBoundary)))
		require.Equal(t, expectedType, strings.ReplaceAll(calls[0].contentType, actualBoundary, expectedBoundary))
	})
	t.Run("json data url", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{}
		body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(uploadURLTestPNG) + `"}]}`)
		run := runUploadURLTestForward(t, false, upstream, body, "application/json")
		require.NoError(t, run.err)

		calls := upstream.snapshot()
		require.Len(t, calls, 1)
		expectedBody, _, err := rewriteOpenAIImagesModel(body, "application/json", "gpt-image-2")
		require.NoError(t, err)
		require.Equal(t, expectedBody, calls[0].body)
	})
	t.Run("non-boolean flag stays off", func(t *testing.T) {
		account := &Account{Type: AccountTypeAPIKey, Extra: map[string]any{openAIImagesUploadURLEnabledExtraKey: "true"}}
		require.False(t, account.IsOpenAIImagesUploadURLEnabled())
		require.False(t, (&Account{Type: AccountTypeOAuth, Extra: map[string]any{openAIImagesUploadURLEnabledExtraKey: true}}).IsOpenAIImagesUploadURLEnabled())
		require.True(t, (&Account{Type: AccountTypeAPIKey, Extra: map[string]any{openAIImagesUploadURLEnabledExtraKey: true}}).IsOpenAIImagesUploadURLEnabled())
	})
}

func TestOpenAIImagesUploadURL_LocalValidationRejectsWithoutUpstream(t *testing.T) {
	t.Run("oversize multipart", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{}
		big := append(append([]byte{}, uploadURLTestPNG...), make([]byte, openAIImagesUploadURLMaxImageBytes)...)
		body, contentType := buildUploadURLTestMultipart(t,
			[][2]string{{"model", "gpt-image-2"}, {"prompt", "edit"}},
			[]uploadURLTestFile{{field: "image", name: "big.png", contentType: "image/png", data: big}},
		)
		run := runUploadURLTestForward(t, true, upstream, body, contentType)
		requireUploadURLClientError(t, run, http.StatusBadRequest, openAIImagesInvalidImageCode)
		require.Empty(t, upstream.snapshot(), "no upstream call on local validation failure")
		require.Contains(t, run.recorder.Body.String(), "32 MB")
		assertNoUploadSecretsLeaked(t, run)
	})
	t.Run("unsupported type json", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{}
		body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/gif;base64,` + base64.StdEncoding.EncodeToString(uploadURLTestGIF) + `"}]}`)
		run := runUploadURLTestForward(t, true, upstream, body, "application/json")
		requireUploadURLClientError(t, run, http.StatusBadRequest, openAIImagesInvalidImageCode)
		require.Empty(t, upstream.snapshot())
		require.Equal(t, "images[0].image_url", gjson.Get(run.recorder.Body.String(), "error.param").String())
	})
	t.Run("second image invalid fails whole request", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{}
		body, contentType := buildUploadURLTestMultipart(t,
			[][2]string{{"model", "gpt-image-2"}, {"prompt", "edit"}},
			[]uploadURLTestFile{
				{field: "image[]", name: "a.png", contentType: "image/png", data: uploadURLTestPNG},
				{field: "image[]", name: "b.gif", contentType: "image/gif", data: uploadURLTestGIF},
			},
		)
		run := runUploadURLTestForward(t, true, upstream, body, contentType)
		requireUploadURLClientError(t, run, http.StatusBadRequest, openAIImagesInvalidImageCode)
		require.Empty(t, upstream.snapshot(), "validation runs before any upload")
	})
	t.Run("malformed data url", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{}
		body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,@@@not-base64@@@"}]}`)
		run := runUploadURLTestForward(t, true, upstream, body, "application/json")
		requireUploadURLClientError(t, run, http.StatusBadRequest, openAIImagesInvalidImageCode)
		require.Empty(t, upstream.snapshot())
	})
	t.Run("client errors do not count against account health", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{}
		body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/gif;base64,` + base64.StdEncoding.EncodeToString(uploadURLTestGIF) + `"}]}`)
		run := runUploadURLTestForward(t, true, upstream, body, "application/json")
		_, _, eligible := classifyOpenAIAPIKeyHealthFailure(run.err)
		require.False(t, eligible)
		var imageErr *OpenAIImagesUpstreamError
		require.ErrorAs(t, run.err, &imageErr)
		require.False(t, IsOpenAIImagesRetryableUpstreamError(imageErr))
	})
}

func TestOpenAIImagesUploadURL_UpstreamMaxBytesRejects(t *testing.T) {
	upstream := &uploadURLTestUpstream{
		uploadURLBody: `{"image_url":"https://cdn.example.test/x.png","max_bytes":16,"method":"PUT","upload_url":"` + uploadURLTestPutTarget + `?Signature=` + uploadURLTestSignature + `"}`,
	}
	body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(uploadURLTestPNG) + `"}]}`)
	run := runUploadURLTestForward(t, true, upstream, body, "application/json")
	requireUploadURLClientError(t, run, http.StatusBadRequest, openAIImagesInvalidImageCode)
	require.Empty(t, upstream.callsMatching(http.MethodPut, ""))
	assertNoUploadSecretsLeaked(t, run)
}

func TestOpenAIImagesUploadURL_UploadURLFailure(t *testing.T) {
	t.Run("client error passes through", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{
			uploadURLStatus: http.StatusBadRequest,
			uploadURLBody:   `{"error":{"type":"invalid_request_error","message":"unsupported content_type"}}`,
		}
		body, contentType := buildUploadURLTestMultipart(t,
			[][2]string{{"model", "gpt-image-2"}, {"prompt", "edit"}},
			[]uploadURLTestFile{{field: "image", name: "a.png", contentType: "image/png", data: uploadURLTestPNG}},
		)
		run := runUploadURLTestForward(t, true, upstream, body, contentType)
		var imageErr *OpenAIImagesUpstreamError
		require.ErrorAs(t, run.err, &imageErr)
		require.Equal(t, http.StatusBadRequest, imageErr.StatusCode)
		require.Contains(t, run.recorder.Body.String(), "unsupported content_type")
		require.Empty(t, upstream.callsMatching(http.MethodPut, ""))
		require.Empty(t, upstream.callsMatching(http.MethodPost, "/v1/images/edits"))
		assertNoUploadSecretsLeaked(t, run)
	})
	t.Run("server error fails over", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{
			uploadURLStatus: http.StatusServiceUnavailable,
			uploadURLBody:   `{"error":{"message":"temporarily unavailable"}}`,
		}
		body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(uploadURLTestPNG) + `"}]}`)
		run := runUploadURLTestForward(t, true, upstream, body, "application/json")
		var failoverErr *UpstreamFailoverError
		require.ErrorAs(t, run.err, &failoverErr)
		require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
		require.Empty(t, upstream.callsMatching(http.MethodPut, ""))
		require.Empty(t, upstream.callsMatching(http.MethodPost, "/v1/images/edits"))
		assertNoUploadSecretsLeaked(t, run)
	})
	t.Run("malformed ticket", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{uploadURLBody: `{"image_url":"https://cdn.example.test/x.png"}`}
		body := []byte(`{"model":"gpt-image-2","prompt":"edit","images":[{"image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString(uploadURLTestPNG) + `"}]}`)
		run := runUploadURLTestForward(t, true, upstream, body, "application/json")
		requireUploadURLClientError(t, run, http.StatusBadGateway, openAIImagesUploadFailedCode)
		require.Empty(t, upstream.callsMatching(http.MethodPut, ""))
	})
}

func TestOpenAIImagesUploadURL_PutFailure(t *testing.T) {
	newBody := func(t *testing.T) ([]byte, string) {
		return buildUploadURLTestMultipart(t,
			[][2]string{{"model", "gpt-image-2"}, {"prompt", "edit"}},
			[]uploadURLTestFile{
				{field: "image[]", name: "a.png", contentType: "image/png", data: uploadURLTestPNG},
				{field: "image[]", name: "b.png", contentType: "image/png", data: uploadURLTestPNG},
			},
		)
	}
	t.Run("http status", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{putStatus: http.StatusForbidden}
		body, contentType := newBody(t)
		run := runUploadURLTestForward(t, true, upstream, body, contentType)
		requireUploadURLClientError(t, run, http.StatusBadGateway, openAIImagesUploadFailedCode)
		require.Contains(t, run.recorder.Body.String(), "upstream image upload failed")
		require.Len(t, upstream.callsMatching(http.MethodPut, ""), 1, "stop at the first failed image")
		events, _ := run.ctx.Get(OpsUpstreamErrorsKey)
		list, _ := events.([]*OpsUpstreamErrorEvent)
		require.NotEmpty(t, list)
		require.Equal(t, "https://oss.example.test", list[len(list)-1].UpstreamURL)
		assertNoUploadSecretsLeaked(t, run)
	})
	t.Run("transport error carrying signed url", func(t *testing.T) {
		upstream := &uploadURLTestUpstream{putErr: errors.New("connection reset by peer")}
		body, contentType := newBody(t)
		run := runUploadURLTestForward(t, true, upstream, body, contentType)
		requireUploadURLClientError(t, run, http.StatusBadGateway, openAIImagesUploadFailedCode)
		assertNoUploadSecretsLeaked(t, run)
	})
	t.Run("private target rejected before PUT", func(t *testing.T) {
		for _, target := range []string{"https://127.0.0.1/put", "https://10.0.0.8/put", "https://localhost/put", "http://oss.example.test/put", "ftp://oss.example.test/put"} {
			upstream := &uploadURLTestUpstream{putTarget: target}
			body, contentType := newBody(t)
			run := runUploadURLTestForward(t, true, upstream, body, contentType)
			requireUploadURLClientError(t, run, http.StatusBadGateway, openAIImagesUploadFailedCode)
			require.Empty(t, upstream.callsMatching(http.MethodPut, ""), target)
			assertNoUploadSecretsLeaked(t, run)
		}
	})
}

func TestOpenAIImagesMultipartTextFieldsJSON_SkipsFilesAndTypesNumbers(t *testing.T) {
	body, contentType := buildUploadURLTestMultipart(t,
		[][2]string{{"model", "m"}, {"prompt", "p \"quoted\""}, {"n", "2"}, {"partial_images", "1"}, {"stream", "true"}, {"background", ""}, {"user", "u1"}},
		[]uploadURLTestFile{{field: "image", name: "a.png", contentType: "image/png", data: uploadURLTestPNG}},
	)
	out, err := openAIImagesMultipartTextFieldsJSON(body, contentType)
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"m","prompt":"p \"quoted\"","n":2,"partial_images":1,"stream":true,"user":"u1"}`, string(out))
}

func uploadURLTestBoundary(t *testing.T, contentType string) string {
	t.Helper()
	_, params, err := mime.ParseMediaType(contentType)
	require.NoError(t, err)
	require.NotEmpty(t, params["boundary"])
	return params["boundary"]
}

func uploadURLTestImageURLs(body []byte) []string {
	var urls []string
	for _, item := range gjson.GetBytes(body, "images").Array() {
		urls = append(urls, item.Get("image_url").String())
	}
	return urls
}
