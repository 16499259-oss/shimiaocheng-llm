// Package proxy handles LLM API request proxying to upstream providers.
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
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

// ServeHTTP handles the /v1/chat/completions request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	userID := auth.GetUserID(r)
	if userID == 0 {
		writeProxyError(w, http.StatusUnauthorized, "Not authenticated", "not_authenticated")
		return
	}

	// Limit request body to 1MB to prevent OOM attacks
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Parse request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "Failed to read request body", "bad_request")
		return
	}

	var chatReq ChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		log.Printf("ERROR: Failed to parse JSON request body (len=%d): %v", len(bodyBytes), err)
		if len(bodyBytes) > 500 {
			log.Printf("DEBUG: Raw body (first 500 bytes): %s", string(bodyBytes[:500]))
		} else {
			log.Printf("DEBUG: Raw body: %s", string(bodyBytes))
		}
		writeProxyError(w, http.StatusBadRequest, "Invalid JSON request body", "bad_request")
		return
	}

	if chatReq.Model == "" {
		chatReq.Model = "glm-5.2"
	}

	// Normalize content arrays → strings (Cursor sends content as array-of-parts)
	bodyBytes = normalizeContentArrays(bodyBytes)

	// Resolve the upstream provider ONCE for this request (time-window routing).
	// When no Router is wired, fall back to the legacy getters (gradual rollout).
	providerID := "zhipu"
	endpoint := h.EndpointGetter()
	apiKey := h.APIKeyGetter()
	if h.Router != nil {
		prov, err := h.Router.ResolveProvider(time.Now())
		if err != nil {
			writeProxyError(w, http.StatusServiceUnavailable, "No upstream provider available", "no_provider")
			return
		}
		providerID = prov.ID
		endpoint = prov.Endpoint
		apiKey = prov.APIKey
	}

	// Rewrite the model name for the selected provider. Missing mapping ->
	// passthrough (original external name); never errors.
	rewrittenModel := chatReq.Model
	if h.Router != nil {
		rewrittenModel = h.Router.RewriteModel(chatReq.Model, providerID)
	}
	bodyBytes = rewriteBodyModel(bodyBytes, rewrittenModel)

	// Get effective multiplier for the current time (Asia/Shanghai)
	multiplier := h.MultiplierEng.GetEffectiveMultiplier(time.Now())
	effectiveCalls := int(math.Ceil(1.0 * multiplier))

	// Quota check and atomic deduction
	allowed, err := h.QuotaChecker.CheckAndDeduct(userID, effectiveCalls)
	if err != nil {
		log.Printf("ERROR: quota check for user %d: %v", userID, err)
		writeProxyError(w, http.StatusInternalServerError, "Internal server error", "internal_error")
		return
	}
	if !allowed {
		// Log the rejected call
		callLog := &models.CallLog{
			UserID:         userID,
			Model:          chatReq.Model,
			ProviderID:     providerID,
			EffectiveCalls: effectiveCalls,
			MultiplierUsed: multiplier,
			StatusCode:     429,
			LatencyMs:      int(time.Since(startTime).Milliseconds()),
			ErrorMsg:       "Quota exceeded",
		}
		models.InsertCallLog(h.QuotaChecker.DB(), callLog)

		writeProxyError(w, http.StatusTooManyRequests, "Quota exceeded", "quota_exceeded")
		return
	}

	// Handle SSE streaming
	if chatReq.Stream {
		h.handleStream(w, r, bodyBytes, userID, chatReq.Model, providerID, endpoint, apiKey, effectiveCalls, multiplier, startTime)
		return
	}

	// Handle synchronous request
	h.handleSync(w, bodyBytes, userID, chatReq.Model, providerID, endpoint, apiKey, effectiveCalls, multiplier, startTime)
}

// handleSync processes a non-streaming chat completion request.
// endpoint/apiKey/providerID are the resolved upstream target for this request.
func (h *Handler) handleSync(w http.ResponseWriter, bodyBytes []byte, userID int64, model, providerID, endpoint, apiKey string, effectiveCalls int, multiplier float64, startTime time.Time) {
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
