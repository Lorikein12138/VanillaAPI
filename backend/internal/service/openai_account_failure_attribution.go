package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// openAIAccountFailureAttribution is deliberately internal: it describes the
// source of a completed upstream attempt and is never serialized to clients.
type openAIAccountFailureAttribution uint8

const (
	openAIAccountFailureUnknown openAIAccountFailureAttribution = iota
	openAIAccountFailureAuthentication
	openAIAccountFailureQuota
	openAIAccountFailurePermission
	openAIAccountFailureProxyAuthentication
	openAIAccountFailureFirstOutputTimeout
	openAIAccountFailureRequestScope
	openAIAccountFailureProviderScope
	openAIAccountFailureCapacity
	openAIAccountFailureModelUnavailable
	openAIAccountFailureRequestTooLarge
	openAIAccountFailureRequestInvalid
	openAIAccountFailureSilentRefusal
	openAIAccountFailureTransport
	openAIAccountFailureClientCanceled
	openAIAccountFailureNoObservedError
	openAIAccountFailureProviderResponse
	openAIAccountFailureConflictingEvidence
	// An annotated neutral response must not fall back to public response
	// headers when separate classification headers intentionally excluded them.
	openAIAccountFailureUnclassifiedResponse
)

type openAIAccountFailureClassification struct {
	attributable bool
	reason       openAIAccountFailureAttribution
}

type openAIAccountFailureSourceError struct {
	err         error
	attribution openAIAccountFailureAttribution
}

func (e *openAIAccountFailureSourceError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *openAIAccountFailureSourceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func withOpenAIAccountFailureAttribution(err error, attribution openAIAccountFailureAttribution) error {
	if err == nil || attribution == openAIAccountFailureUnknown {
		return err
	}
	return &openAIAccountFailureSourceError{err: err, attribution: attribution}
}

func (a openAIAccountFailureAttribution) isAccountFailure() bool {
	switch a {
	case openAIAccountFailureAuthentication,
		openAIAccountFailureQuota,
		openAIAccountFailurePermission,
		openAIAccountFailureProxyAuthentication,
		openAIAccountFailureFirstOutputTimeout:
		return true
	default:
		return false
	}
}

func (a openAIAccountFailureAttribution) String() string {
	switch a {
	case openAIAccountFailureAuthentication:
		return "account_authentication"
	case openAIAccountFailureQuota:
		return "account_quota"
	case openAIAccountFailurePermission:
		return "account_permission"
	case openAIAccountFailureProxyAuthentication:
		return "account_proxy_authentication"
	case openAIAccountFailureFirstOutputTimeout:
		return "first_output_timeout"
	case openAIAccountFailureRequestScope:
		return "request_scope"
	case openAIAccountFailureProviderScope:
		return "provider_scope"
	case openAIAccountFailureCapacity:
		return "capacity_or_transient_retry"
	case openAIAccountFailureModelUnavailable:
		return "model_unavailable"
	case openAIAccountFailureRequestTooLarge:
		return "request_body_too_large"
	case openAIAccountFailureRequestInvalid:
		return "request_or_policy_error"
	case openAIAccountFailureSilentRefusal:
		return "silent_refusal"
	case openAIAccountFailureTransport:
		return "environment_transport"
	case openAIAccountFailureClientCanceled:
		return "client_canceled"
	case openAIAccountFailureNoObservedError:
		return "failure_without_error"
	case openAIAccountFailureConflictingEvidence:
		return "conflicting_evidence"
	case openAIAccountFailureProviderResponse:
		return "provider_response"
	default:
		return "unknown_source"
	}
}

// classifyOpenAIAccountFailure is the one gate shared by scheduler health and
// the API-key health breaker. Ambiguous failures are intentionally neutral.
func classifyOpenAIAccountFailure(err error) openAIAccountFailureClassification {
	if err == nil {
		return openAIAccountFailureClassification{reason: openAIAccountFailureNoObservedError}
	}
	if errors.Is(err, context.Canceled) {
		return openAIAccountFailureClassification{reason: openAIAccountFailureClientCanceled}
	}
	var sourceErr *openAIAccountFailureSourceError
	if errors.As(err, &sourceErr) && sourceErr != nil {
		return openAIAccountFailureClassification{
			attributable: sourceErr.attribution.isAccountFailure(),
			reason:       sourceErr.attribution,
		}
	}
	var policyBlocked *OpenAIFastBlockedError
	if errors.As(err, &policyBlocked) {
		return openAIAccountFailureClassification{reason: openAIAccountFailureRequestInvalid}
	}

	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) {
		return classifyOpenAIUpstreamFailoverFailure(failoverErr)
	}

	var imageErr *OpenAIImagesUpstreamError
	if errors.As(err, &imageErr) {
		if imageErr == nil {
			return openAIAccountFailureClassification{reason: openAIAccountFailureUnknown}
		}
		if imageErr.SynthesizedFromModelText {
			return openAIAccountFailureClassification{reason: openAIAccountFailureRequestInvalid}
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]string{
			"code": imageErr.Code, "type": imageErr.ErrorType, "param": imageErr.Param, "message": imageErr.Message,
		}})
		reason := classifyOpenAIUpstreamResponse(imageErr.StatusCode, nil, body, imageErr.Message)
		return openAIAccountFailureClassification{attributable: reason.isAccountFailure(), reason: reason}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return openAIAccountFailureClassification{reason: openAIAccountFailureTransport}
	}
	return openAIAccountFailureClassification{reason: openAIAccountFailureUnknown}
}

func openAIAccountFailureStatusCode(err error) int {
	var failoverErr *UpstreamFailoverError
	if errors.As(err, &failoverErr) && failoverErr != nil {
		return failoverErr.StatusCode
	}
	var imageErr *OpenAIImagesUpstreamError
	if errors.As(err, &imageErr) && imageErr != nil {
		return imageErr.StatusCode
	}
	return 0
}

func classifyOpenAIUpstreamFailoverFailure(err *UpstreamFailoverError) openAIAccountFailureClassification {
	if err == nil {
		return openAIAccountFailureClassification{reason: openAIAccountFailureUnknown}
	}
	// Explicit request/provider scope and request-size semantics take priority
	// over legacy ScopeAccount fields used to control failover behavior.
	if err.Scope == GatewayFailureScopeRequest {
		return openAIAccountFailureClassification{reason: openAIAccountFailureRequestScope}
	}
	if err.Scope == GatewayFailureScopeProvider {
		return openAIAccountFailureClassification{reason: openAIAccountFailureProviderScope}
	}
	if err.Reason == openAIRequestBodyTooLargeReason || err.StatusCode == http.StatusRequestEntityTooLarge {
		return openAIAccountFailureClassification{reason: openAIAccountFailureRequestTooLarge}
	}
	if err.accountFailureAttribution == openAIAccountFailureFirstOutputTimeout || isOpenAIFirstOutputTimeoutError(err) {
		return openAIAccountFailureClassification{attributable: true, reason: openAIAccountFailureFirstOutputTimeout}
	}
	if err.accountFailureAttribution == openAIAccountFailureUnclassifiedResponse {
		if err.RequestScopedTransient || err.RetryableOnSameAccount {
			return openAIAccountFailureClassification{reason: openAIAccountFailureCapacity}
		}
		return openAIAccountFailureClassification{reason: openAIAccountFailureUnknown}
	}
	if err.accountFailureAttribution != openAIAccountFailureUnknown {
		return openAIAccountFailureClassification{
			attributable: err.accountFailureAttribution.isAccountFailure(),
			reason:       err.accountFailureAttribution,
		}
	}
	if err.RequestScopedTransient || err.RetryableOnSameAccount {
		return openAIAccountFailureClassification{reason: openAIAccountFailureCapacity}
	}
	reason := classifyOpenAIUpstreamResponse(err.StatusCode, err.ResponseHeaders, err.ResponseBody, "")
	if reason != openAIAccountFailureUnknown {
		return openAIAccountFailureClassification{attributable: reason.isAccountFailure(), reason: reason}
	}
	if err.Stage == GatewayFailureStageAccountAuth && err.Scope == GatewayFailureScopeAccount {
		return openAIAccountFailureClassification{attributable: true, reason: openAIAccountFailureAuthentication}
	}
	if err.Reason == OpenAIUpstreamAccessStateReason {
		return openAIAccountFailureClassification{attributable: true, reason: openAIAccountFailurePermission}
	}
	return openAIAccountFailureClassification{reason: openAIAccountFailureUnknown}
}

func isOpenAIFirstOutputTimeoutError(err *UpstreamFailoverError) bool {
	if err == nil || err.StatusCode != http.StatusGatewayTimeout {
		return false
	}
	for _, path := range []string{"error.type", "error.code", "type", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(err.ResponseBody, path).String()), "first_output_timeout") {
			return true
		}
	}
	return false
}

func annotateOpenAIUpstreamFailoverError(err *UpstreamFailoverError, headers http.Header, body []byte, upstreamMsg string) {
	if err == nil {
		return
	}
	err.accountFailureAttribution = classifyOpenAIUpstreamResponse(err.StatusCode, headers, body, upstreamMsg)
	if err.accountFailureAttribution == openAIAccountFailureUnknown {
		err.accountFailureAttribution = openAIAccountFailureUnclassifiedResponse
	}
}

func classifyOpenAIUpstreamResponse(statusCode int, headers http.Header, body []byte, upstreamMsg string) openAIAccountFailureAttribution {
	if isOpenAIRequestBodyTooLargeError(statusCode, upstreamMsg, body) {
		return openAIAccountFailureRequestTooLarge
	}
	if reason := openAISpecificFailureEvidence(body); reason != openAIAccountFailureUnknown {
		return reason
	}
	if isOpenAIRequestScopedCapacityShed(upstreamMsg, body) {
		return openAIAccountFailureCapacity
	}
	if isOpenAIModelUnavailableBody(body) {
		return openAIAccountFailureModelUnavailable
	}
	cyberPolicy, _, _ := detectOpenAICyberPolicy(body)
	if isOpenAIContextWindowError(upstreamMsg, body) || isOpenAIInvalidParameterBody(body) || cyberPolicy {
		return openAIAccountFailureRequestInvalid
	}
	if IsOpenAISilentRefusalErrorBody(body) {
		return openAIAccountFailureSilentRefusal
	}
	if isOpenAIHTTPUpstreamAccessStateError(statusCode, upstreamMsg, body) || isOpenAIAccountPermissionError(statusCode, body) {
		return openAIAccountFailurePermission
	}
	if isOpenAIAccountQuotaResponse(statusCode, headers, body) {
		return openAIAccountFailureQuota
	}
	if statusCode == http.StatusUnauthorized {
		return openAIAccountFailureAuthentication
	}
	if strings.EqualFold(gjson.GetBytes(body, "error.type").String(), "invalid_request_error") ||
		strings.EqualFold(gjson.GetBytes(body, "response.error.type").String(), "invalid_request_error") {
		return openAIAccountFailureRequestInvalid
	}
	if statusCode >= http.StatusInternalServerError {
		return openAIAccountFailureProviderResponse
	}
	return openAIAccountFailureUnknown
}

func isOpenAIModelUnavailableBody(body []byte) bool {
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type", "detail.code", "code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if code == "model_not_found" || code == "model_not_available" || code == "unsupported_model" {
			return true
		}
	}
	return false
}

func isOpenAIInvalidParameterBody(body []byte) bool {
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type", "detail.code", "type"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if code == "invalid_parameter" || code == "invalid_value" {
			return true
		}
	}
	return strings.TrimSpace(gjson.GetBytes(body, "error.param").String()) != "" ||
		strings.TrimSpace(gjson.GetBytes(body, "response.error.param").String()) != ""
}

func isOpenAIAccountPermissionError(statusCode int, body []byte) bool {
	if statusCode != http.StatusForbidden && statusCode != http.StatusBadRequest && statusCode != http.StatusPaymentRequired {
		return false
	}
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type", "detail.code", "code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		switch code {
		case "permission_denied", "insufficient_permissions", "account_deactivated", "workspace_deactivated", "deactivated_workspace":
			return true
		}
	}
	return false
}

func isOpenAIAccountQuotaResponse(statusCode int, headers http.Header, body []byte) bool {
	if statusCode == http.StatusPaymentRequired {
		return true
	}
	if statusCode != http.StatusTooManyRequests && statusCode != http.StatusPaymentRequired {
		return false
	}
	if disposition, _ := classifyOpenAIOAuth429(headers, body); disposition != openAIOAuth429Transient {
		return true
	}
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type", "detail.code", "code"} {
		code := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		switch code {
		case "insufficient_quota", "quota_exceeded", "usage_limit_reached", "billing_hard_limit_reached", "go_usage_limit_error":
			return true
		}
	}
	return false
}

// openAISpecificFailureEvidence resolves concrete sources before broad types or
// status fallbacks. Inconsistent concrete sources do not identify an account.
func openAISpecificFailureEvidence(body []byte) openAIAccountFailureAttribution {
	reason := openAIAccountFailureUnknown
	for _, path := range []string{"error.code", "error.type", "response.error.code", "response.error.type", "detail.code", "code", "type"} {
		var candidate openAIAccountFailureAttribution
		switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String())) {
		case "invalid_api_key", "token_invalidated", "token_revoked", "unauthorized":
			candidate = openAIAccountFailureAuthentication
		case "permission_denied", "insufficient_permissions", "account_deactivated", "workspace_deactivated", "deactivated_workspace":
			candidate = openAIAccountFailurePermission
		case "insufficient_quota", "quota_exceeded", "usage_limit_reached", "billing_hard_limit_reached", "go_usage_limit_error":
			candidate = openAIAccountFailureQuota
		case "model_not_found", "model_not_available", "unsupported_model":
			candidate = openAIAccountFailureModelUnavailable
		case "server_is_overloaded", "slow_down":
			candidate = openAIAccountFailureCapacity
		case "invalid_parameter", "invalid_value", "context_length_exceeded", "cyber_policy":
			candidate = openAIAccountFailureRequestInvalid
		default:
			continue
		}
		if reason != openAIAccountFailureUnknown && reason != candidate {
			return openAIAccountFailureConflictingEvidence
		}
		reason = candidate
	}
	return reason
}
