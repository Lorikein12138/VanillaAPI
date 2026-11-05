package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 部分 OpenAI 兼容上游的 /v1/images/edits 只接受 JSON 且拒绝 data: URL，要求先调用
// POST /v1/images/upload-url 取得预签名地址、PUT 原始字节，再把返回的 image_url 填进
// images[].image_url / mask.image_url。账号 Extra 开关开启后，网关在拿到账号槽位之后
// 代客户端完成这一步；开关默认关闭，关闭时转发行为与原先逐字节一致。
const (
	openAIImagesUploadURLEnabledExtraKey = "openai_images_upload_url_enabled"

	openAIImagesUploadURLEndpoint   = "/v1/images/upload-url"
	openAIImagesUploadURLDefaultURL = "https://api.openai.com/v1/images/upload-url"

	// openAIImagesUploadURLMaxImageBytes 是本地预检的单图上限，上游 max_bytes 更小时以上游为准。
	openAIImagesUploadURLMaxImageBytes = 32 << 20
	// openAIImagesUploadURLResponseMaxBytes 限制 upload-url 成功响应体的读取量。
	openAIImagesUploadURLResponseMaxBytes = 64 << 10
	// openAIImagesUploadPutTimeout 是单张图片 PUT 到预签名地址的超时。
	openAIImagesUploadPutTimeout = 120 * time.Second

	openAIImagesUploadFailedCode    = "image_upload_failed"
	openAIImagesUploadFailedMessage = "上游图片上传失败，请稍后重试（upstream image upload failed, please retry later）"
	openAIImagesInvalidImageCode    = "invalid_image"
)

// openAIImagesUploadURLContentTypes 是允许上传的图片格式，以字节嗅探结果为准。
var openAIImagesUploadURLContentTypes = map[string]struct{}{
	"image/png":  {},
	"image/jpeg": {},
	"image/webp": {},
}

// IsOpenAIImagesUploadURLEnabled 报告 API Key 账号是否开启「图生图经上游 upload-url 上传」。
// 只认 Extra 中显式为 true 的布尔值，缺省或其他类型一律视为关闭。
func (a *Account) IsOpenAIImagesUploadURLEnabled() bool {
	return a != nil && a.Type == AccountTypeAPIKey && a.getExtraBool(openAIImagesUploadURLEnabledExtraKey)
}

// openAIImagesUploadSource 是一张需要先上传的输入图。
type openAIImagesUploadSource struct {
	label       string // 面向客户端的中英文描述
	param       string // 错误响应中的 param
	jsonPath    string // JSON 请求体中需要改写的路径（multipart 来源为空）
	isMask      bool
	data        []byte
	contentType string
}

type openAIImagesUploadURLResponse struct {
	UploadURL   string          `json:"upload_url"`
	ImageURL    string          `json:"image_url"`
	Method      string          `json:"method"`
	ContentType string          `json:"content_type"`
	MaxBytes    json.RawMessage `json:"max_bytes"`
}

// prepareOpenAIImagesUploadURLBody 把图生图请求中的文件 / data URL / 裸 base64 逐张上传到上游，
// 返回改写后的 JSON 请求体；http(s) 公网 URL 原样保留。任一张失败即整单失败，错误已写给客户端。
func (s *OpenAIGatewayService) prepareOpenAIImagesUploadURLBody(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	parsed *OpenAIImagesRequest,
	body []byte,
	bodyContentType string,
	upstreamModel string,
) ([]byte, error) {
	var (
		out     []byte
		sources []*openAIImagesUploadSource
		err     error
	)
	if parsed.Multipart {
		// body 是改写 model 后的 multipart，边界已重新生成，必须用与之配套的 Content-Type 解析。
		out, err = openAIImagesMultipartTextFieldsJSON(body, bodyContentType)
		if err != nil {
			return nil, err
		}
		for i := range parsed.Uploads {
			sources = append(sources, &openAIImagesUploadSource{
				label: fmt.Sprintf("第 %d 张参考图（image %d）", i+1, i+1),
				param: "image",
				data:  parsed.Uploads[i].Data,
			})
		}
		if parsed.MaskUpload != nil {
			sources = append(sources, &openAIImagesUploadSource{
				label:  "遮罩（mask）",
				param:  "mask",
				isMask: true,
				data:   parsed.MaskUpload.Data,
			})
		}
	} else {
		out = body
		sources, err = collectOpenAIImagesJSONUploadSources(body)
		if err != nil {
			return nil, s.writeOpenAIImagesUploadClientError(c, err)
		}
	}

	// 先在本地校验全部图片，任何一张不合格都不触达上游。
	for _, src := range sources {
		if err := validateOpenAIImagesUploadSource(src, openAIImagesUploadURLMaxImageBytes); err != nil {
			return nil, s.writeOpenAIImagesUploadClientError(c, err)
		}
	}

	var imageURLs []string
	maskURL := ""
	for _, src := range sources {
		imageURL, err := s.uploadOpenAIImagesSource(ctx, c, account, token, src, upstreamModel)
		if err != nil {
			return nil, err
		}
		switch {
		case src.jsonPath != "":
			out, err = sjson.SetBytes(out, src.jsonPath, imageURL)
			if err != nil {
				return nil, fmt.Errorf("rewrite uploaded image url: %w", err)
			}
		case src.isMask:
			maskURL = imageURL
		default:
			imageURLs = append(imageURLs, imageURL)
		}
	}
	if parsed.Multipart {
		images := make([]map[string]string, 0, len(imageURLs))
		for _, imageURL := range imageURLs {
			images = append(images, map[string]string{"image_url": imageURL})
		}
		if out, err = sjson.SetBytes(out, "images", images); err != nil {
			return nil, fmt.Errorf("build images field: %w", err)
		}
		if maskURL != "" {
			if out, err = sjson.SetBytes(out, "mask", map[string]string{"image_url": maskURL}); err != nil {
				return nil, fmt.Errorf("build mask field: %w", err)
			}
		}
	}
	return out, nil
}

// openAIImagesMultipartTextFieldsJSON 把 multipart 请求的文本字段转成 JSON 对象。
// n / output_compression / partial_images 转为整数，stream 转为布尔，其余保持字符串；
// 文件字段与空值跳过，同名字段以最后一次为准。
func openAIImagesMultipartTextFieldsJSON(body []byte, contentType string) ([]byte, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("parse multipart content-type: %w", err)
	}
	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, fmt.Errorf("multipart boundary is required")
	}
	var (
		order  []string
		values = map[string]json.RawMessage{}
	)
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart body: %w", err)
		}
		name := strings.TrimSpace(part.FormName())
		if name == "" || part.FileName() != "" || name == "mask" || name == "image" || strings.HasPrefix(name, "image[") {
			_ = part.Close()
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, openAIImageMaxUploadPartSize+1))
		_ = part.Close()
		if err != nil {
			return nil, fmt.Errorf("read multipart field %s: %w", name, err)
		}
		value := strings.TrimSpace(string(data))
		if value == "" {
			continue
		}
		var encoded []byte
		switch name {
		case "n", "output_compression", "partial_images":
			n, convErr := strconv.Atoi(value)
			if convErr != nil {
				return nil, fmt.Errorf("invalid %s field value", name)
			}
			encoded, err = json.Marshal(n)
		case "stream":
			b, convErr := strconv.ParseBool(value)
			if convErr != nil {
				return nil, fmt.Errorf("invalid stream field value")
			}
			encoded, err = json.Marshal(b)
		default:
			encoded, err = json.Marshal(value)
		}
		if err != nil {
			return nil, fmt.Errorf("encode multipart field %s: %w", name, err)
		}
		if _, seen := values[name]; !seen {
			order = append(order, name)
		}
		values[name] = encoded
	}
	out := []byte{'{'}
	for i, name := range order {
		if i > 0 {
			out = append(out, ',')
		}
		key, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("encode multipart field name: %w", err)
		}
		out = append(out, key...)
		out = append(out, ':')
		out = append(out, values[name]...)
	}
	return append(out, '}'), nil
}

// openAIImagesUploadClientError 是本地校验失败（客户端输入问题），按 400 返回且不计入账号失败。
type openAIImagesUploadClientError struct {
	param   string
	message string
}

func (e *openAIImagesUploadClientError) Error() string { return e.message }

// collectOpenAIImagesJSONUploadSources 找出 JSON 请求中需要上传的 images[].image_url 与 mask.image_url：
// data: URL 必须能解码；不带协议头的字符串能解码为图片时按裸 base64 上传；http(s) URL 不处理。
func collectOpenAIImagesJSONUploadSources(body []byte) ([]*openAIImagesUploadSource, error) {
	var sources []*openAIImagesUploadSource
	var firstErr error
	gjson.GetBytes(body, "images").ForEach(func(key, item gjson.Result) bool {
		i := int(key.Int())
		raw := item.Get("image_url")
		if raw.Type != gjson.String {
			return true
		}
		src, err := openAIImagesJSONUploadSource(raw.String(),
			fmt.Sprintf("第 %d 张参考图（image %d）", i+1, i+1),
			fmt.Sprintf("images[%d].image_url", i),
			fmt.Sprintf("images.%d.image_url", i),
			false,
		)
		if err != nil {
			firstErr = err
			return false
		}
		if src != nil {
			sources = append(sources, src)
		}
		return true
	})
	if firstErr != nil {
		return nil, firstErr
	}
	if raw := gjson.GetBytes(body, "mask.image_url"); raw.Type == gjson.String {
		src, err := openAIImagesJSONUploadSource(raw.String(), "遮罩（mask）", "mask.image_url", "mask.image_url", true)
		if err != nil {
			return nil, err
		}
		if src != nil {
			sources = append(sources, src)
		}
	}
	return sources, nil
}

func openAIImagesJSONUploadSource(raw, label, param, jsonPath string, isMask bool) (*openAIImagesUploadSource, error) {
	value := strings.TrimSpace(raw)
	lower := strings.ToLower(value)
	switch {
	case value == "", strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		return nil, nil
	case strings.HasPrefix(lower, "data:"):
		data, err := decodeOpenAIImagesDataURL(value)
		if err != nil {
			return nil, &openAIImagesUploadClientError{
				param:   param,
				message: fmt.Sprintf("%s的 data URL 无法解析，须为 base64 编码（%s data URL must be base64 encoded）", label, param),
			}
		}
		return &openAIImagesUploadSource{label: label, param: param, jsonPath: jsonPath, isMask: isMask, data: data}, nil
	default:
		data, err := decodeOpenAIImagesBase64Payload(value)
		if err != nil || len(data) == 0 {
			// 既不是 URL 也不是可解码的 base64：交给上游按原样判定。
			return nil, nil
		}
		if _, ok := openAIImagesUploadURLContentTypes[detectedImageContentType(data)]; !ok {
			return nil, nil
		}
		return &openAIImagesUploadSource{label: label, param: param, jsonPath: jsonPath, isMask: isMask, data: data}, nil
	}
}

// decodeOpenAIImagesDataURL 解码 data:<mime>;base64,<payload>。
func decodeOpenAIImagesDataURL(raw string) ([]byte, error) {
	comma := strings.IndexByte(raw, ',')
	if comma < len("data:") {
		return nil, errors.New("data url has no payload")
	}
	meta := strings.ToLower(strings.TrimSpace(raw[len("data:"):comma]))
	if !strings.HasSuffix(meta, ";base64") {
		return nil, errors.New("data url is not base64 encoded")
	}
	return decodeOpenAIImagesBase64Payload(raw[comma+1:])
}

func decodeOpenAIImagesBase64Payload(payload string) ([]byte, error) {
	payload = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n':
			return -1
		}
		return r
	}, payload)
	if payload == "" {
		return nil, errors.New("empty base64 payload")
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err := enc.DecodeString(payload); err == nil {
			return data, nil
		}
	}
	return nil, errors.New("invalid base64 payload")
}

// validateOpenAIImagesUploadSource 校验大小与格式，并以嗅探结果作为上传的 Content-Type。
func validateOpenAIImagesUploadSource(src *openAIImagesUploadSource, maxBytes int64) error {
	if len(src.data) == 0 {
		return &openAIImagesUploadClientError{
			param:   src.param,
			message: fmt.Sprintf("%s为空（%s is empty）", src.label, src.param),
		}
	}
	if int64(len(src.data)) > maxBytes {
		return &openAIImagesUploadClientError{
			param: src.param,
			message: fmt.Sprintf("%s超过 %s 上限（%s exceeds the %s limit）",
				src.label, formatOpenAIImagesUploadLimit(maxBytes), src.param, formatOpenAIImagesUploadLimit(maxBytes)),
		}
	}
	contentType := detectedImageContentType(src.data)
	if _, ok := openAIImagesUploadURLContentTypes[contentType]; !ok {
		return &openAIImagesUploadClientError{
			param:   src.param,
			message: fmt.Sprintf("%s格式不受支持，仅支持 png / jpeg / webp（%s must be png, jpeg or webp）", src.label, src.param),
		}
	}
	src.contentType = contentType
	return nil
}

func formatOpenAIImagesUploadLimit(maxBytes int64) string {
	if maxBytes >= 1<<20 && maxBytes%(1<<20) == 0 {
		return fmt.Sprintf("%d MB", maxBytes>>20)
	}
	return fmt.Sprintf("%d bytes", maxBytes)
}

// writeOpenAIImagesUploadClientError 把本地校验失败写成 400；以 OpenAIImagesUpstreamError 返回，
// handler 按客户端错误处理（调度记成功、不触发冷却或健康熔断）。
func (s *OpenAIGatewayService) writeOpenAIImagesUploadClientError(c *gin.Context, err error) error {
	var clientErr *openAIImagesUploadClientError
	if !errors.As(err, &clientErr) {
		return err
	}
	upErr := &OpenAIImagesUpstreamError{
		StatusCode: http.StatusBadRequest,
		ErrorType:  "invalid_request_error",
		Code:       openAIImagesInvalidImageCode,
		Param:      clientErr.param,
		Message:    clientErr.message,
	}
	writeOpenAIImagesUpstreamErrorResponse(c, upErr)
	return upErr
}

// uploadOpenAIImagesSource 申请预签名地址并 PUT 图片字节，返回上游可用的 image_url。
// upload-url 请求的失败与主请求走同一套 failover / 透传逻辑；PUT 失败按上游 502 结束本次请求。
func (s *OpenAIGatewayService) uploadOpenAIImagesSource(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	src *openAIImagesUploadSource,
	upstreamModel string,
) (string, error) {
	reqBody, err := json.Marshal(map[string]string{"content_type": src.contentType})
	if err != nil {
		return "", fmt.Errorf("encode upload-url request: %w", err)
	}
	uploadReq, err := s.buildOpenAIImagesRequest(ctx, c, account, reqBody, "application/json", token, openAIImagesUploadURLEndpoint)
	if err != nil {
		return "", err
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.doOpenAIUpstream(uploadReq, proxyURL, account)
	if err != nil {
		return "", s.openAIImagesAPIKeyTransportError(c, account, uploadReq.URL.String(), err)
	}
	if resp.StatusCode >= 400 {
		_, statusErr := s.handleOpenAIImagesAPIKeyErrorStatus(ctx, c, account, resp, uploadReq.URL.String(), upstreamModel)
		return "", statusErr
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, openAIImagesUploadURLResponseMaxBytes+1))
	_ = resp.Body.Close()
	uploadEndpoint := safeUpstreamURL(uploadReq.URL.String())
	if readErr != nil {
		return "", s.openAIImagesUploadFailure(c, account, 0, uploadEndpoint, "read upload-url response failed")
	}
	if len(respBody) > openAIImagesUploadURLResponseMaxBytes {
		return "", s.openAIImagesUploadFailure(c, account, resp.StatusCode, uploadEndpoint, "upload-url response too large")
	}
	var ticket openAIImagesUploadURLResponse
	if err := json.Unmarshal(respBody, &ticket); err != nil {
		return "", s.openAIImagesUploadFailure(c, account, resp.StatusCode, uploadEndpoint, "upload-url response is not valid JSON")
	}
	ticket.UploadURL = strings.TrimSpace(ticket.UploadURL)
	ticket.ImageURL = strings.TrimSpace(ticket.ImageURL)
	if ticket.UploadURL == "" || ticket.ImageURL == "" {
		return "", s.openAIImagesUploadFailure(c, account, resp.StatusCode, uploadEndpoint, "upload-url response is missing upload_url or image_url")
	}
	if method := strings.TrimSpace(ticket.Method); method != "" && !strings.EqualFold(method, http.MethodPut) {
		return "", s.openAIImagesUploadFailure(c, account, resp.StatusCode, uploadEndpoint, "upload-url response requires unsupported method")
	}
	if lower := strings.ToLower(ticket.ImageURL); !strings.HasPrefix(lower, "https://") && !strings.HasPrefix(lower, "http://") {
		return "", s.openAIImagesUploadFailure(c, account, resp.StatusCode, uploadEndpoint, "upload-url response image_url is not an http(s) url")
	}
	if maxBytes := parseOpenAIImagesUploadMaxBytes(ticket.MaxBytes); maxBytes > 0 && int64(len(src.data)) > maxBytes {
		return "", s.writeOpenAIImagesUploadClientError(c, &openAIImagesUploadClientError{
			param: src.param,
			message: fmt.Sprintf("%s超过上游 %s 上限（%s exceeds the upstream %s limit）",
				src.label, formatOpenAIImagesUploadLimit(maxBytes), src.param, formatOpenAIImagesUploadLimit(maxBytes)),
		})
	}
	putContentType := src.contentType
	if declared := strings.TrimSpace(ticket.ContentType); declared != "" {
		putContentType = declared
	}
	if err := s.putOpenAIImagesUpload(ctx, c, account, proxyURL, ticket.UploadURL, putContentType, src.data); err != nil {
		return "", err
	}
	return ticket.ImageURL, nil
}

func parseOpenAIImagesUploadMaxBytes(raw json.RawMessage) int64 {
	trimmed := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if trimmed == "" || trimmed == "null" {
		return 0
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// putOpenAIImagesUpload 把图片字节 PUT 到上游返回的预签名地址。
// 目的地先按出站 URL 策略校验，再无条件拒绝回环 / 私网 / 链路本地地址（含 DNS 解析结果），
// 并禁用重定向；不带任何账号凭据。签名地址不进入日志与错误详情。
func (s *OpenAIGatewayService) putOpenAIImagesUpload(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	proxyURL string,
	uploadURL string,
	contentType string,
	data []byte,
) error {
	target, err := s.validateOutboundURL(uploadURL)
	if err == nil {
		err = rejectPrivateImageHost(target)
	}
	if err != nil {
		return s.openAIImagesUploadFailure(c, account, 0, "", "upload target rejected by outbound url policy")
	}
	host := openAIImagesUploadTargetHost(target)
	if s.httpUpstream == nil {
		return s.openAIImagesUploadFailure(c, account, 0, host, "http upstream is not configured")
	}
	putCtx, cancel := context.WithTimeout(WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamPublicHostsOnly(ctx)), openAIImagesUploadPutTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(putCtx, http.MethodPut, target, bytes.NewReader(data))
	if err != nil {
		return s.openAIImagesUploadFailure(c, account, 0, host, "build upload request failed")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return s.openAIImagesUploadFailure(c, account, 0, host, "upload request failed: "+openAIImagesUploadSafeErrorText(err))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return s.openAIImagesUploadFailure(c, account, resp.StatusCode, host, fmt.Sprintf("upload returned status %d", resp.StatusCode))
	}
	return nil
}

// openAIImagesUploadTargetHost 只保留 scheme://host，签名参数与对象路径都不外露。
func openAIImagesUploadTargetHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// openAIImagesUploadSafeErrorText 去掉 *url.Error 携带的完整地址，只留底层原因。
func openAIImagesUploadSafeErrorText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return sanitizeUpstreamErrorMessage(err.Error())
}

// openAIImagesUploadFailure 记录上传失败（运维日志只含主机与原因）并向客户端写 502。
func (s *OpenAIGatewayService) openAIImagesUploadFailure(c *gin.Context, account *Account, upstreamStatus int, upstreamHost string, detail string) error {
	kind := "request_error"
	if upstreamStatus > 0 {
		kind = "http_error"
	}
	setOpsUpstreamError(c, upstreamStatus, detail, "")
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		ProxyID:            opsUpstreamProxyID(account),
		ProxyName:          opsUpstreamProxyName(account),
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: upstreamStatus,
		UpstreamURL:        upstreamHost,
		Kind:               kind,
		Message:            "image upload: " + detail,
	})
	upErr := &OpenAIImagesUpstreamError{
		StatusCode: http.StatusBadGateway,
		ErrorType:  "upstream_error",
		Code:       openAIImagesUploadFailedCode,
		Message:    openAIImagesUploadFailedMessage,
	}
	writeOpenAIImagesUpstreamErrorResponse(c, upErr)
	return upErr
}
