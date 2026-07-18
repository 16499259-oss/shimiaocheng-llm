// Package proxy — wildcard passthrough handler (/v1/passthrough/).
//
// This handler forwards ANY HTTP method + arbitrary sub-path + query + body to the
// upstream provider resolved by the Router, WITHOUT rewriting the path, model, or
// normalizing the JSON (unlike the chat Handler). It exists to proxy MCP servers
// and other arbitrary upstream endpoints behind the gateway's auth / quota / key-hiding.
//
// Shared invariants (see docs/design-mcp-passthrough.md §8):
//   - Double switch: requests only forward when BOTH the global
//     cfg.Proxy.PassthroughEnabled AND the target provider's allow_passthrough
//     are true; otherwise 403 passthrough_disabled.
//   - Key hiding: the client sub-key is stripped and the provider's real key is
//     injected per its auth scheme. The real key is NEVER logged.
//   - It reuses the SAME governance as the chat path: auth (SubKeyAuth
//     middleware), per-user concurrency (tryAcquireConcurrency),
//     quota (quota.Checker.CheckAndDeduct), routing (router.Router), and
//     call logging (models.InsertCallLog).
package proxy

import (
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
)

// PassthroughHandler serves the wildcard /v1/passthrough/ subtree
// (matches any method and any sub-path under that prefix).
type PassthroughHandler struct {
	QuotaChecker  *quota.Checker
	MultiplierEng *quota.MultiplierEngine
	Router        *router.Router
	// PassthroughEnabled is the GLOBAL master switch getter. It is a
	// func (closing over cfg.Proxy.PassthroughEnabled) so the live config
	// value is read on every request without a lock. When it returns false
	// the endpoint is fully closed (403 passthrough_disabled).
	PassthroughEnabled func() bool
	// SyncTimeout caps a buffered (non-streaming) upstream response.
	SyncTimeout time.Duration // default 300s
	// StreamTimeout caps a streaming (SSE / chunked) upstream response.
	// MCP GET /sse long-connections can run long; 10min is the P0 ceiling.
	StreamTimeout time.Duration // default 10min
}

// ServeHTTP orchestrates the passthrough flow:
// global switch → concurrency → body budget → provider resolve →
// per-provider gate → multiplier+quota → upstream forward → response copy.
func (h *PassthroughHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	// ── Auth (already applied by SubKeyAuth middleware) ──
	userID := auth.GetUserID(r)
	if userID == 0 {
		writeProxyError(w, http.StatusUnauthorized, "Not authenticated", "not_authenticated")
		return
	}

	// ── Global master switch (defence in depth) ──
	if h.PassthroughEnabled == nil || !h.PassthroughEnabled() {
		writeProxyError(w, http.StatusForbidden, "Passthrough is disabled", "passthrough_disabled")
		return
	}

	// ── Per-user concurrent request cap (reuse proxy.tryAcquireConcurrency) ──
	// Released on every subsequent return path via the deferred call below.
	maxConc := auth.GetMaxConcurrency(r)
	if !tryAcquireConcurrency(userID, int(maxConc)) {
		log.Printf("WARN: passthrough concurrency limit exceeded user=%d max=%d", userID, maxConc)
		// Observability only: no call_log row (mirrors chat handler).
		w.Header().Set("Retry-After", "1")
		writeProxyError(w, http.StatusTooManyRequests,
			"并发请求数超过上限", "concurrency_limit_exceeded")
		return
	}
	defer releaseConcurrency(userID)

	// ── Sub-path + raw query (everything after /v1/passthrough) ──
	// r.URL.Path is e.g. "/v1/passthrough/mcp/sse"; subPath keeps the
	// leading slash so it concatenates cleanly with the provider base URL.
	subPath := strings.TrimPrefix(r.URL.Path, "/v1/passthrough")
	rawQuery := r.URL.RawQuery

	// ── Request body budget (per-user cap, clamped to [1, ceiling]) ──
	// Methods without a body (GET/HEAD/DELETE/OPTIONS/TRACE) read nothing.
	budget := auth.GetMaxBodySize(r)
	if budget <= 0 {
		budget = models.DefaultMaxBodySize
	}
	if budget > models.MaxBodySizeCeiling {
		budget = models.MaxBodySizeCeiling
	}
	readLimit := budget
	if readLimit < 1 {
		readLimit = 1
	}

	hasBody := r.ContentLength != 0 &&
		r.Method != http.MethodGet && r.Method != http.MethodHead &&
		r.Method != http.MethodDelete && r.Method != http.MethodOptions &&
		r.Method != http.MethodTrace

	var bodyBytes []byte
	if hasBody {
		r.Body = http.MaxBytesReader(w, r.Body, readLimit)
		var readErr error
		bodyBytes, readErr = io.ReadAll(r.Body)
		if readErr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(readErr, &maxErr) {
				writeProxyError(w, http.StatusRequestEntityTooLarge,
					"请求操作已超过模型最大请求限制", "request_entity_too_large")
				return
			}
			writeProxyError(w, http.StatusBadRequest, "Failed to read request body", "bad_request")
			return
		}
	}

	// ── Resolve upstream provider (fixed route mode or auto) ──
	routeMode := auth.GetRouteMode(r)
	fixedProvider := auth.GetFixedProvider(r)
	now := time.Now()

	if h.Router == nil {
		writeProxyError(w, http.StatusInternalServerError, "Gateway misconfigured", "internal_error")
		return
	}

	var prov router.Provider
	if routeMode == "fixed" && fixedProvider != "" {
		p, ok := h.Router.GetProviderBySlug(fixedProvider)
		if !ok {
			writeProxyError(w, http.StatusServiceUnavailable,
				"Fixed provider not available: "+fixedProvider, "no_provider")
			return
		}
		prov = p
	} else {
		p, err := h.Router.ResolveProvider(now)
		if err != nil {
			writeProxyError(w, http.StatusServiceUnavailable,
				"No upstream provider available", "no_provider")
			return
		}
		prov = p
	}

	// ── Per-provider passthrough gate ──
	if !prov.AllowPassthrough {
		writeProxyError(w, http.StatusForbidden,
			"Passthrough disabled for provider", "passthrough_disabled")
		return
	}

	// ── Multiplier + quota (reuse existing engines) ──
	if h.MultiplierEng == nil || h.QuotaChecker == nil {
		writeProxyError(w, http.StatusInternalServerError, "Gateway misconfigured", "internal_error")
		return
	}
	var multiplier float64
	var effectiveCalls int
	fixedMult, ferr := models.GetFixedMultiplier(h.QuotaChecker.DB(), userID)
	if ferr == nil && fixedMult.Valid {
		multiplier = fixedMult.Float64
		effectiveCalls = int(math.Ceil(fixedMult.Float64))
	} else {
		multiplier = h.MultiplierEng.GetEffectiveMultiplier(now)
		effectiveCalls = int(math.Ceil(1.0 * multiplier))
	}

	allowed, qerr := h.QuotaChecker.CheckAndDeduct(userID, effectiveCalls)
	if qerr != nil {
		log.Printf("ERROR: passthrough quota check for user %d: %v", userID, qerr)
		writeProxyError(w, http.StatusInternalServerError, "Internal server error", "internal_error")
		return
	}
	if !allowed {
		// Classify the cause so the client can tell a Token-cap hit apart.
		errType, errMsg := "quota_exceeded", "Quota exceeded"
		if q, gerr := models.GetQuota(h.QuotaChecker.DB(), userID); gerr == nil {
			if q.QuotaTokenTotalLimit != 0 && q.QuotaTokenTotalUsed >= q.QuotaTokenTotalLimit {
				errType, errMsg = "token_quota_exceeded", "Token 额度已用尽"
			}
		}
		callLog := &models.CallLog{
			UserID:         userID,
			Model:          methodPathModel(r.Method, subPath),
			ProviderID:     prov.ID,
			EffectiveCalls: effectiveCalls,
			MultiplierUsed: multiplier,
			StatusCode:     429,
			LatencyMs:      int(time.Since(startTime).Milliseconds()),
			ErrorMsg:       errMsg,
		}
		models.InsertCallLog(h.QuotaChecker.DB(), callLog)
		writeProxyError(w, http.StatusTooManyRequests, errMsg, errType)
		return
	}

	// ── Build upstream target + request (hides the client sub-key) ──
	targetURL := buildPassthroughTarget(prov.Endpoint, subPath, rawQuery)
	provEntry := providerEntryFromRouter(prov)
	upstreamReq, berr := buildUpstreamRequest(r, targetURL, bodyBytes, provEntry)
	if berr != nil {
		writeProxyError(w, http.StatusInternalServerError, "Failed to build upstream request", "internal_error")
		return
	}

	// StreamTimeout is used for both modes: we cannot know if the response is
	// streaming until after the request is sent, and a 10min ceiling is a safe
	// upper bound. The request body is already buffered, so the write is instant.
	timeout := h.StreamTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	client := &http.Client{Timeout: timeout}
	resp, derr := client.Do(upstreamReq)
	if derr != nil {
		latencyMs := int(time.Since(startTime).Milliseconds())
		callLog := &models.CallLog{
			UserID:         userID,
			Model:          methodPathModel(r.Method, subPath),
			ProviderID:     prov.ID,
			EffectiveCalls: effectiveCalls,
			MultiplierUsed: multiplier,
			StatusCode:     502,
			LatencyMs:      latencyMs,
			ErrorMsg:       derr.Error(),
		}
		models.InsertCallLog(h.QuotaChecker.DB(), callLog)
		writeProxyError(w, http.StatusBadGateway, "Upstream request failed", "upstream_error")
		return
	}
	defer resp.Body.Close()

	// ── Forward headers + body (generic streaming copy) ──
	// 4xx/5xx are forwarded verbatim (no wrapping) so MCP clients see the
	// upstream's own error shape.
	isStream := isStreamingResponse(resp)
	copyResponseHeaders(w, resp.Header, isStream)
	w.WriteHeader(resp.StatusCode)
	if cerr := streamCopy(w, r, resp.Body); cerr != nil {
		// Client disconnect (r.Context().Done()) or write error — we already
		// streamed what we could; log at WARN and stop.
		log.Printf("WARN: passthrough stream copy interrupted user=%d: %v", userID, cerr)
	}

	latencyMs := int(time.Since(startTime).Milliseconds())
	callLog := &models.CallLog{
		UserID:         userID,
		Model:          methodPathModel(r.Method, subPath),
		ProviderID:     prov.ID,
		EffectiveCalls: effectiveCalls,
		MultiplierUsed: multiplier,
		StatusCode:     resp.StatusCode,
		LatencyMs:      latencyMs,
	}
	models.InsertCallLog(h.QuotaChecker.DB(), callLog)
}

// methodPathModel renders the call_logs "model" column for a passthrough
// request. There is no chat model, so P0 convention records
// "<METHOD> <subPath>" (e.g. "POST /mcp" / "GET /sse") so the call is
// observable in the admin panel (see design §8). When subPath is empty we
// fall back to the bare method.
func methodPathModel(method, subPath string) string {
	if subPath == "" {
		return method
	}
	return method + " " + subPath
}

// providerEntryFromRouter converts a resolved router.Provider (which now carries
// the passthrough auth fields) into a provider.ProviderEntry for the request
// builders. The Router snapshot already holds the in-memory decrypted key.
func providerEntryFromRouter(p router.Provider) provider.ProviderEntry {
	return provider.ProviderEntry{
		Slug:             p.ID,
		Endpoint:         p.Endpoint,
		APIKey:           p.APIKey,
		AllowPassthrough: p.AllowPassthrough,
		AuthHeader:       p.AuthHeader,
		AuthScheme:       p.AuthScheme,
		ExtraHeaders:     p.ExtraHeaders,
	}
}

// isStreamingResponse reports whether the upstream response should be forwarded
// in streaming mode (MCP SSE / chunked). Used to drop Content-Length and
// disable nginx buffering.
func isStreamingResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") ||
		strings.Contains(ct, "application/stream") ||
		strings.Contains(ct, "application/x-ndjson") {
		return true
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Transfer-Encoding")), "chunked") {
		return true
	}
	// No Content-Length header -> likely streaming/chunked.
	if resp.ContentLength < 0 {
		return true
	}
	return false
}
