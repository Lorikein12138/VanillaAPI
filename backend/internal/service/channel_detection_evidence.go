package service

import (
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
)

// PrepareChannelDetectionSend reserves the egress budget and observes the
// specific ensuing HTTP call. The caller must settle it even on errors.
func PrepareChannelDetectionSend(req *http.Request, accountID int64) (*http.Request, func(*http.Response), error) {
	account, err := reserveChannelDetectionSend(req, accountID)
	if err != nil {
		return nil, nil, err
	}
	if account == nil {
		return req, nil, nil
	}
	observed, settle := observeDetectionAttempt(req, detectionBindingFromContext(req.Context()).budget, account)
	return observed, settle, nil
}

func observeDetectionAttempt(req *http.Request, budget *detectionRoundBudget, account *Account) (*http.Request, func(*http.Response)) {
	fingerprint, err := detectionAccountRouteFingerprint(account)
	eligible := err == nil && !account.IsShadow() && (account.ProxyID == nil || account.Proxy != nil) &&
		detectionWireCredentialMatches(req, account)
	budget.mu.Lock()
	budget.attempts++
	budget.mu.Unlock()

	var mu sync.Mutex
	var settled, wrote bool
	ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			if !settled && info.Err == nil {
				wrote = true
			}
		},
	})
	return req.WithContext(ctx), func(resp *http.Response) {
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return
		}
		settled = true
		budget.mu.Lock()
		defer budget.mu.Unlock()
		if !wrote && resp == nil {
			budget.ambiguous = true
			return
		}
		budget.provenSends++
		route := hex.EncodeToString(fingerprint[:])
		if budget.provenSends == 1 {
			budget.accountID, budget.routeFingerprint = account.ID, route
		}
		if !eligible || account.ID != budget.accountID || route != budget.routeFingerprint {
			budget.ambiguous = true
		}
	}
}

// Check the finished outbound request against the DB snapshot used to reserve
// the send. OAuth providers can return a different token from their cache.
// Unknown or conflicting auth schemes still yield public probe results.
func detectionWireCredentialMatches(req *http.Request, account *Account) bool {
	if req.URL.User != nil {
		return false
	}
	var bearer, apiKey, workspace []string
	for name, values := range req.Header {
		switch strings.ToLower(name) {
		case "authorization":
			bearer = append(bearer, values...)
		case "x-api-key":
			apiKey = append(apiKey, values...)
		case "chatgpt-account-id":
			workspace = append(workspace, values...)
		case "x-goog-api-key", "cookie":
			if len(values) != 0 {
				return false
			}
		}
	}
	if len(bearer)+len(apiKey) != 1 {
		return false
	}
	var expected string
	switch account.Type {
	case AccountTypeAPIKey:
		expected = strings.TrimSpace(account.GetCredential("api_key"))
		if len(workspace) != 0 {
			return false
		}
	case AccountTypeOAuth:
		if len(bearer) != 1 || (account.Platform != PlatformAnthropic && account.Platform != PlatformOpenAI) {
			return false
		}
		expected = strings.TrimSpace(account.GetCredential("access_token"))
		if account.Platform == PlatformOpenAI && account.GetChatGPTAccountID() != "" {
			if len(workspace) != 1 || workspace[0] != account.GetChatGPTAccountID() {
				return false
			}
		} else if len(workspace) != 0 {
			return false
		}
	default:
		return false
	}
	if expected == "" {
		return false
	}
	if len(apiKey) == 1 {
		return subtle.ConstantTimeCompare([]byte(apiKey[0]), []byte(expected)) == 1
	}
	scheme, token, ok := strings.Cut(bearer[0], " ")
	return ok && strings.EqualFold(scheme, "Bearer") &&
		subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

func DetectionAccountMatchesProof(account *Account, proof ChannelDetectionProof) bool {
	if proof.Version != channelDetectionProofVersion || account == nil || account.ID != proof.AccountID || account.IsShadow() ||
		(account.ProxyID != nil && account.Proxy == nil) {
		return false
	}
	fingerprint, err := detectionAccountRouteFingerprint(account)
	return err == nil && hex.EncodeToString(fingerprint[:]) == proof.RouteFingerprint
}

func (b *detectionRoundBudget) proof(run channelDetectionRun) (ChannelDetectionProof, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	proof := ChannelDetectionProof{Version: channelDetectionProofVersion, SampleID: run.SampleID, Source: run.Source}
	if !b.ambiguous && b.provenSends > 0 && b.provenSends == b.attempts {
		proof.AccountID, proof.RouteFingerprint, proof.Sends = b.accountID, b.routeFingerprint, b.provenSends
	}
	return proof, b.attempts > 0
}
