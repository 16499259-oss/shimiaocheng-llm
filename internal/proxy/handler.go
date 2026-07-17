// Package proxy handles LLM API request proxying to upstream providers.
package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
)

// Handler handles the /v1/chat/completions endpoint.
type Handler struct {
	APIKeyGetter   func() string // dynamic API key getter (legacy fallback; used when Router == nil)
	EndpointGetter func() string // dynamic upstream endpoint getter (legacy fallback)
	QuotaChecker   *quota.Checker
	MultiplierEng  *quota.MultiplierEngine
	// Router resolves which upstream provider serves the request and rewrites
	// the model name. When nil, the handler falls back to APIKeyGetter /
	// EndpointGetter (gradual rollout safety).
	Router *router.Router
	// Compaction selects the over-budget behaviour for request bodies.
	// CompactionTrim (default) auto-compacts chat history to fit the user's
	// per-request body budget and forwards; CompactionOff restores the legacy
	// hard-413 behaviour when a request exceeds the per-user budget.
	Compaction CompactionMode
	// Debug, when true, dumps the raw request body to logs on JSON parse
	// failure — useful for troubleshooting malformed clients. OFF by default
	// to avoid leaking user conversation content into logs in production.
	Debug bool
}

// userConcurrency tracks the number of in-flight (concurrent) requests per
// user, so the gateway can enforce a per-user concurrent-request cap configured
// in the admin panel. A sync.Map of userID -> *int64 atomic counters. A cap of
// 0 means unlimited and is always allowed. The map is append-only (counters
// linger after a user is deleted) which is harmless for this use.
var userConcurrency sync.Map

// tryAcquireConcurrency increments the user's in-flight counter and returns
// false if doing so would exceed max (max <= 0 means unlimited, always
// allowed). On failure the counter is rolled back, so callers must NOT call
// releaseConcurrency for that attempt.
func tryAcquireConcurrency(userID int64, max int) bool {
	if max <= 0 {
		return true
	}
	v, _ := userConcurrency.LoadOrStore(userID, new(int64))
	c := atomic.AddInt64(v.(*int64), 1)
	if c > int64(max) {
		atomic.AddInt64(v.(*int64), -1)
		return false
	}
	return true
}

// releaseConcurrency decrements the user's in-flight counter.
func releaseConcurrency(userID int64) {
	if v, ok := userConcurrency.Load(userID); ok {
		atomic.AddInt64(v.(*int64), -1)
	}
}

// ChatCompletionRequest mirrors the upstream request structure.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// ChatMessage represents a single chat message.
// Content uses json.RawMessage to accept both string and array-of-content-parts formats.
type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ContentText extracts the text content from either string or array-of-parts format.
func (m ChatMessage) ContentText() string {
	// Try as plain string first
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	// Try as array of content parts (OpenAI multimodal format)
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(m.Content)
}

// normalizeContentArrays converts any array-format content fields in the JSON body
// to plain strings, so the upstream (Zhipu) receives simple string content.
func normalizeContentArrays(body []byte) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}

	msgsRaw, ok := raw["messages"]
	if !ok {
		return body
	}

	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return body
	}

	changed := false
	for i, msg := range msgs {
		contentRaw, ok := msg["content"]
		if !ok {
			continue
		}
		trimmed := bytes.TrimSpace(contentRaw)
		if len(trimmed) > 0 && trimmed[0] == '[' {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(contentRaw, &parts); err == nil {
				var texts []string
				for _, p := range parts {
					if p.Type == "text" && p.Text != "" {
						texts = append(texts, p.Text)
					}
				}
				newContent, _ := json.Marshal(strings.Join(texts, "\n"))
				msgs[i]["content"] = newContent
				changed = true
			}
		}
	}

	if !changed {
		return body
	}

	newMsgs, _ := json.Marshal(msgs)
	raw["messages"] = newMsgs
	result, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return result
}

// rewriteBodyModel replaces the "model" field inside a JSON request body with
// the given model name, preserving all other fields (including already
// normalized messages). If the body cannot be parsed, it is returned unchanged.
func rewriteBodyModel(body []byte, model string) []byte {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	newModel, err := json.Marshal(model)
	if err != nil {
		return body
	}
	raw["model"] = newModel
	out, err := json.Marshal(raw)
	if err != nil {
		return body
	}
	return out
}

// rewriteResponseModel replaces the "model" field in an upstream JSON response
// body with the target model name (the original request model), preserving all
// other fields. If the body cannot be parsed, the upstream model is empty, or
// already matches the target model, the body is returned unchanged.
func rewriteResponseModel(body []byte, model string) []byte {
	if model == "" {
		return body
	}
	var respCheck struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &respCheck); err != nil {
		return body
	}
	if respCheck.Model == "" || respCheck.Model == model {
		return body
	}
	oldPattern := []byte(`"model":"` + respCheck.Model + `"`)
	newPattern := []byte(`"model":"` + model + `"`)
	return bytes.Replace(body, oldPattern, newPattern, 1)
}

// ServeHTTP handles the /v1/chat/completions request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	userID := auth.GetUserID(r)
	if userID == 0 {
		writeProxyError(w, http.StatusUnauthorized, "Not authenticated", "not_authenticated")
		return
	}

	// ── Per-user concurrent request cap ──
	// Reject bursts above the user's configured concurrency limit *before*
	// reading/forwarding the request, so a single misbehaving client cannot
	// exhaust the shared upstream rate-limit budget (all sub-users funnel
	// through the gateway's single upstream credential) or the gateway's own
	// resources (per-request body reads can be up to 32MB). 0 = unlimited.
	// Released on every subsequent return path via the deferred call below.
	maxConc := auth.GetMaxConcurrency(r)
	if !tryAcquireConcurrency(userID, int(maxConc)) {
		writeProxyError(w, http.StatusTooManyRequests,
			"并发请求数超过上限", "concurrency_limit_exceeded")
		return
	}
	defer releaseConcurrency(userID)

	// Determine the user's per-request body budget. The gateway always reads
	// the full request up to the absolute 32MB ceiling so it can inspect and,
	// when compaction is enabled, auto-trim oversized requests. Only requests
	// above the ceiling are rejected with 413 (abuse protection). When
	// compaction is disabled we fall back to reading at the per-user budget and
	// failing with 413 on overflow (legacy behaviour).
	budget := auth.GetMaxBodySize(r)
	if budget <= 0 {
		budget = models.DefaultMaxBodySize
	}
	if budget > models.MaxBodySizeCeiling {
		budget = models.MaxBodySizeCeiling
	}

	var readLimit int64 = models.MaxBodySizeCeiling
	if h.Compaction == CompactionOff {
		readLimit = budget
	}
	r.Body = http.MaxBytesReader(w, r.Body, readLimit)

	// Parse request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		// MaxBytesReader returns *http.MaxBytesError when the read limit is
		// exceeded. With compaction on this only happens above the 32MB ceiling;
		// with compaction off it happens above the per-user budget. Either way,
		// surface a generic 413 message (no numeric limit exposed) so clients
		// recognise "request too large".
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeProxyError(w, http.StatusRequestEntityTooLarge,
				"请求操作已超过模型最大请求限制",
				"request_entity_too_large")
			return
		}
		writeProxyError(w, http.StatusBadRequest, "Failed to read request body", "bad_request")
		return
	}

	var chatReq ChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		log.Printf("ERROR: Failed to parse JSON request body (len=%d): %v", len(bodyBytes), err)
		if h.Debug {
			// Only dump the body when debug mode is explicitly enabled, so we
			// never leak user conversation content into logs in production.
			if len(bodyBytes) > 500 {
				log.Printf("DEBUG: Raw body (first 500 bytes): %s", string(bodyBytes[:500]))
			} else {
				log.Printf("DEBUG: Raw body: %s", string(bodyBytes))
			}
		}
		writeProxyError(w, http.StatusBadRequest, "Invalid JSON request body", "bad_request")
		return
	}

	if chatReq.Model == "" {
		chatReq.Model = "glm-5.2"
	}

	// Normalize content arrays → strings (Cursor sends content as array-of-parts)
	bodyBytes = normalizeContentArrays(bodyBytes)

	// ── Per-user body budget: auto-compact history instead of failing ──
	// When the (normalized) request exceeds the user's budget, trim the chat
	// history down to fit and forward it, so large-context clients (e.g. ZCode)
	// keep working without a 413. Disabled only when compaction == "off".
	if h.Compaction != CompactionOff && int64(len(bodyBytes)) > budget {
		bodyBytes = compactMessagesToBudget(bodyBytes, budget)
		// compactMessagesToBudget guarantees the result fits the budget whenever
		// possible; if a single message alone exceeds it, the body is forwarded
		// best-effort and the upstream enforces its own context window.
	}

	// ── Route resolution ──
	// Read routing mode from context (injected by auth middleware — zero extra DB query).
	routeMode := auth.GetRouteMode(r)
	fixedProvider := auth.GetFixedProvider(r)

	providerID := "zhipu" // default fallback id when no Router is wired (legacy path)
	var endpoint, apiKey string

	if h.Router != nil {
		// Fixed route mode: bypass Router.ResolveProvider, use the specified provider directly.
		if routeMode == "fixed" && fixedProvider != "" {
			prov, ok := h.Router.GetProviderBySlug(fixedProvider)
			if !ok {
				writeProxyError(w, http.StatusServiceUnavailable, "Fixed provider not available: "+fixedProvider, "provider_unavailable")
				return
			}
			providerID = prov.ID
			endpoint = prov.Endpoint
			apiKey = prov.APIKey
		} else {
			// New routing path: resolve the upstream provider via time-window rules.
			prov, err := h.Router.ResolveProvider(time.Now())
			if err != nil {
				// Fallback: when the provider table is empty (e.g. not yet seeded),
				// try the legacy getter path so the gateway remains operational.
				if h.EndpointGetter != nil && h.APIKeyGetter != nil {
					providerID = "zhipu" // legacy default
					endpoint = h.EndpointGetter()
					apiKey = h.APIKeyGetter()
				} else {
					writeProxyError(w, http.StatusServiceUnavailable, "No upstream provider available", "no_provider")
					return
				}
			} else {
				providerID = prov.ID
				endpoint = prov.Endpoint
				apiKey = prov.APIKey
			}
		}
	} else {
		// Legacy fallback path (gradual rollout safety): only here do we invoke
		// the dynamic getters. This guarantees a nil getter can never panic when
		// a Router is wired (P2-1).
		if h.EndpointGetter == nil || h.APIKeyGetter == nil {
			writeProxyError(w, http.StatusInternalServerError, "Gateway misconfigured", "internal_error")
			return
		}
		endpoint = h.EndpointGetter()
		apiKey = h.APIKeyGetter()
	}

	// Rewrite the model name for the selected provider. Missing mapping ->
	// passthrough (original external name); never errors.
	rewrittenModel := chatReq.Model
	if h.Router != nil {
		rewrittenModel = h.Router.RewriteModel(chatReq.Model, providerID)
	}
	bodyBytes = rewriteBodyModel(bodyBytes, rewrittenModel)

	// Defensive (P2-2): the quota / multiplier engines must be configured. If
	// either is nil the gateway is misconfigured — fail fast with 500 instead of
	// panicking downstream.
	if h.MultiplierEng == nil || h.QuotaChecker == nil {
		writeProxyError(w, http.StatusInternalServerError, "Gateway misconfigured", "internal_error")
		return
	}

	// ── Multiplier resolution ──
	// Fixed multiplier takes priority over the global time-based multiplier.
	var multiplier float64
	var effectiveCalls int
	fixedMult, err := models.GetFixedMultiplier(h.QuotaChecker.DB(), userID)
	if err == nil && fixedMult.Valid {
		multiplier = fixedMult.Float64
		effectiveCalls = int(math.Ceil(fixedMult.Float64))
	} else {
		multiplier = h.MultiplierEng.GetEffectiveMultiplier(time.Now())
		effectiveCalls = int(math.Ceil(1.0 * multiplier))
	}

	// Quota check and atomic deduction
	allowed, err := h.QuotaChecker.CheckAndDeduct(userID, effectiveCalls)
	if err != nil {
		log.Printf("ERROR: quota check for user %d: %v", userID, err)
		writeProxyError(w, http.StatusInternalServerError, "Internal server error", "internal_error")
		return
	}
	if !allowed {
		// Log the rejected call (unified 429; the specific type is reported to
		// the client below after classifying the cause).
		callLog := &models.CallLog{
			UserID:         userID,
			Model:          rewrittenModel,
			ProviderID:     providerID,
			EffectiveCalls: effectiveCalls,
			MultiplierUsed: multiplier,
			StatusCode:     429,
			LatencyMs:      int(time.Since(startTime).Milliseconds()),
			ErrorMsg:       "Quota exceeded",
		}
		models.InsertCallLog(h.QuotaChecker.DB(), callLog)

		// Classify the rejection reason. A non-zero Token cap that has already
		// been reached yields token_quota_exceeded (中文「Token 额度已用尽」);
		// otherwise it is a call-count quota (quota_exceeded). If GetQuota fails
		// we conservatively fall back to the generic count message so the
		// request is always rejected.
		msg, errType := "Quota exceeded", "quota_exceeded"
		if q, qErr := models.GetQuota(h.QuotaChecker.DB(), userID); qErr == nil && q != nil {
			if q.QuotaTokenTotalLimit != 0 && q.QuotaTokenTotalUsed >= q.QuotaTokenTotalLimit {
				msg, errType = "Token 额度已用尽", "token_quota_exceeded"
			}
		}
		writeProxyError(w, http.StatusTooManyRequests, msg, errType)
		return
	}

	// Handle SSE streaming
	if chatReq.Stream {
		h.handleStream(w, r, bodyBytes, userID, rewrittenModel, chatReq.Model, providerID, endpoint, apiKey, effectiveCalls, multiplier, startTime)
		return
	}

	// Handle synchronous request
	h.handleSync(w, bodyBytes, userID, rewrittenModel, chatReq.Model, providerID, endpoint, apiKey, effectiveCalls, multiplier, startTime)
}

// handleSync processes a non-streaming chat completion request.
// endpoint/apiKey/providerID are the resolved upstream target for this request.
func (h *Handler) handleSync(w http.ResponseWriter, bodyBytes []byte, userID int64, model, requestModel, providerID, endpoint, apiKey string, effectiveCalls int, multiplier float64, startTime time.Time) {
	// Build upstream request
	upstreamReq, err := BuildUpstreamRequest(endpoint, apiKey, bodyBytes)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "Failed to build upstream request", "internal_error")
		return
	}

	// Execute upstream request
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(upstreamReq)
	if err != nil {
		latencyMs := int(time.Since(startTime).Milliseconds())
		callLog := &models.CallLog{
			UserID:         userID,
			Model:          model,
			ProviderID:     providerID,
			EffectiveCalls: effectiveCalls,
			MultiplierUsed: multiplier,
			StatusCode:     502,
			LatencyMs:      latencyMs,
			ErrorMsg:       err.Error(),
		}
		models.InsertCallLog(h.QuotaChecker.DB(), callLog)

		writeProxyError(w, http.StatusBadGateway, "Upstream request failed", "upstream_error")
		return
	}
	defer resp.Body.Close()

	// Read upstream response
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		latencyMs := int(time.Since(startTime).Milliseconds())
		callLog := &models.CallLog{
			UserID:         userID,
			Model:          model,
			ProviderID:     providerID,
			EffectiveCalls: effectiveCalls,
			MultiplierUsed: multiplier,
			StatusCode:     502,
			LatencyMs:      latencyMs,
			ErrorMsg:       err.Error(),
		}
		models.InsertCallLog(h.QuotaChecker.DB(), callLog)

		writeProxyError(w, http.StatusBadGateway, "Failed to read upstream response", "upstream_error")
		return
	}

	// Rewrite model in upstream response back to the original request model
	// so that the client sees the model name it originally requested (transparent proxy).
	respBody = rewriteResponseModel(respBody, requestModel)

	latencyMs := int(time.Since(startTime).Milliseconds())

	// Parse usage from response for logging
	promptTokens, completionTokens, totalTokens := ExtractTokenUsage(respBody)

	// Log the call
	callLog := &models.CallLog{
		UserID:           userID,
		Model:            model,
		ProviderID:       providerID,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		EffectiveCalls:   effectiveCalls,
		MultiplierUsed:   multiplier,
		StatusCode:       resp.StatusCode,
		LatencyMs:        latencyMs,
	}
	models.InsertCallLog(h.QuotaChecker.DB(), callLog)

	// Account the Token usage toward the user's cumulative Token quota. This is
	// fire-and-forget bookkeeping that must NOT break the already-built response,
	// so any error is logged only (no client impact).
	if err := models.AddTokenUsage(h.QuotaChecker.DB(), userID, promptTokens+completionTokens); err != nil {
		log.Printf("ERROR: add token usage (sync) for user %d: %v", userID, err)
	}

	// Forward response to client
	for key, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)
}

// ExtractTokenUsage parses token usage from an upstream JSON response body.
func ExtractTokenUsage(body []byte) (promptTokens, completionTokens, totalTokens int) {
	var resp struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, 0, 0
	}
	return resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens
}

// writeProxyError writes a JSON error response for proxy errors.
func writeProxyError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
			"code":    errType,
		},
	})
}

// CompactionMode selects the over-budget behaviour for request bodies.
type CompactionMode string

const (
	// CompactionTrim auto-compacts chat history so the request fits the user's
	// per-request body budget, then forwards it (no 413 for normal users).
	CompactionTrim CompactionMode = "trim"
	// CompactionOff restores the legacy hard 413 when a request exceeds the
	// user's per-request body budget (rollback / strict mode).
	CompactionOff CompactionMode = "off"
)

// compactMessagesToBudget trims the chat history in a request body so the
// serialized request fits within maxBody bytes. It always preserves all system
// messages and keeps as many of the most recent (newest) turns as fit; older
// messages are dropped from the front. If the body cannot be parsed, or is
// already within budget, it is returned unchanged.
func compactMessagesToBudget(body []byte, maxBody int64) []byte {
	if maxBody <= 0 || int64(len(body)) <= maxBody {
		return body
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return body
	}
	msgsRaw, ok := raw["messages"]
	if !ok {
		return body
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(msgsRaw, &msgs); err != nil {
		return body
	}
	if len(msgs) == 0 {
		return body
	}

	// Partition: system messages are always kept; everything else (the chat
	// history) is trimmed from the oldest end until the whole request fits.
	var system, others []json.RawMessage
	for _, m := range msgs {
		var probe struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(m, &probe) == nil && probe.Role == "system" {
			system = append(system, m)
		} else {
			others = append(others, m)
		}
	}

	// tryBuild re-marshals the full request keeping the newest keepN non-system
	// messages (plus all system messages) and reports whether it fits maxBody.
	// Re-marshalling the whole body avoids fragile envelope/comma arithmetic —
	// the size check is always against the real serialized bytes.
	tryBuild := func(keepN int) ([]byte, bool) {
		var kept []json.RawMessage
		if keepN > 0 && len(others) > 0 {
			start := len(others) - keepN
			if start < 0 {
				start = 0
			}
			kept = others[start:]
		}
		compacted := make([]json.RawMessage, 0, len(system)+len(kept))
		compacted = append(compacted, system...)
		compacted = append(compacted, kept...)
		newMsgs, err := json.Marshal(compacted)
		if err != nil {
			return nil, false
		}
		raw["messages"] = newMsgs
		out, err := json.Marshal(raw)
		if err != nil {
			return nil, false
		}
		return out, int64(len(out)) <= maxBody
	}

	// Always keep at least the single newest non-system message. Drop the oldest
	// turns first: try the most messages that fit, walking down to the minimum.
	minKeep := 0
	if len(others) > 0 {
		minKeep = 1
	}
	for keepN := len(others); keepN >= minKeep; keepN-- {
		if out, ok := tryBuild(keepN); ok {
			return out
		}
	}
	// Even the minimal set (system + newest message) exceeds the budget — a
	// single message is larger than maxBody. Forward best-effort with that
	// minimal set; the upstream enforces its own context window if needed.
	if out, _ := tryBuild(minKeep); out != nil {
		return out
	}
	return body
}
