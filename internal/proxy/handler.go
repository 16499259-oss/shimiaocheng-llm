// Package proxy handles LLM API request proxying to upstream providers.
package proxy

import (
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// Handler handles the /v1/chat/completions endpoint.
type Handler struct {
	APIKeyGetter   func() string // dynamic API key getter (supports runtime updates)
	EndpointGetter func() string // dynamic upstream endpoint getter
	QuotaChecker   *quota.Checker
	MultiplierEng  *quota.MultiplierEngine
}

// ChatCompletionRequest mirrors the upstream request structure.
type ChatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

// ChatMessage represents a single chat message.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ServeHTTP handles the /v1/chat/completions request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	userID := auth.GetUserID(r)
	if userID == 0 {
		writeProxyError(w, http.StatusUnauthorized, "Not authenticated", "not_authenticated")
		return
	}

	// Parse request body
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeProxyError(w, http.StatusBadRequest, "Failed to read request body", "bad_request")
		return
	}

	var chatReq ChatCompletionRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		writeProxyError(w, http.StatusBadRequest, "Invalid JSON request body", "bad_request")
		return
	}

	if chatReq.Model == "" {
		chatReq.Model = "glm-5.2"
	}

	// Get effective multiplier for the current time
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
		h.handleStream(w, r, bodyBytes, userID, chatReq.Model, effectiveCalls, multiplier, startTime)
		return
	}

	// Handle synchronous request
	h.handleSync(w, bodyBytes, userID, chatReq.Model, effectiveCalls, multiplier, startTime)
}

// handleSync processes a non-streaming chat completion request.
func (h *Handler) handleSync(w http.ResponseWriter, bodyBytes []byte, userID int64, model string, effectiveCalls int, multiplier float64, startTime time.Time) {
	// Build upstream request
	upstreamReq, err := BuildUpstreamRequest(h.EndpointGetter(), h.APIKeyGetter(), bodyBytes)
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
