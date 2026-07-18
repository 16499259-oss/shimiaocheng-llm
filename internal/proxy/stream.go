package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strings"
	"time"

	"llm_api_gateway/internal/models"
)

// handleStream handles SSE streaming response from the upstream.
// The quota has already been deducted in the handler before calling this method.
// endpoint/apiKey/providerID are the resolved upstream target for this request.
func (h *Handler) handleStream(w http.ResponseWriter, r *http.Request, bodyBytes []byte, userID int64, model, requestModel, providerID, endpoint, apiKey string, effectiveCalls int, multiplier float64, startTime time.Time) {
	// Build upstream request
	upstreamReq, err := BuildUpstreamRequest(endpoint, apiKey, bodyBytes)
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "Failed to build upstream request", "internal_error")
		return
	}

	// Execute upstream request
	// Streaming responses can run long, so we avoid a tight overall Timeout, but
	// we cap the total stream duration at 10 minutes to prevent a hung/malicious
	// upstream from occupying a worker connection indefinitely.
	client := &http.Client{
		Timeout: 10 * time.Minute,
	}
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

	// Check upstream status
	if resp.StatusCode != http.StatusOK {
		latencyMs := int(time.Since(startTime).Milliseconds())
		callLog := &models.CallLog{
			UserID:         userID,
			Model:          model,
			ProviderID:     providerID,
			EffectiveCalls: effectiveCalls,
			MultiplierUsed: multiplier,
			StatusCode:     resp.StatusCode,
			LatencyMs:      latencyMs,
			ErrorMsg:       fmt.Sprintf("Upstream returned %d", resp.StatusCode),
		}
		models.InsertCallLog(h.QuotaChecker.DB(), callLog)

		// Forward the error response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		return
	}

	// Set SSE response headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // Disable Nginx buffering
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		log.Printf("ERROR: ResponseWriter does not support Flusher")
		return
	}

	// Scan upstream SSE response line by line
	scanner := bufio.NewScanner(resp.Body)
	// Increase buffer size for large SSE chunks
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	var totalTokens int
	var promptTokens int
	var completionTokens int

	for scanner.Scan() {
		// Check if client disconnected
		select {
		case <-r.Context().Done():
			return
		default:
		}

		line := scanner.Text()

		// Rewrite model in SSE data lines back to the original request model
		// for transparent proxying (e.g. "astron-code-latest" → "glm-5.2").
		if requestModel != "" && strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
			dataStr := strings.TrimPrefix(line, "data:")
			dataStr = strings.TrimSpace(dataStr)
			if dataStr != "" {
				var sseData struct {
					Model string `json:"model"`
				}
				if json.Unmarshal([]byte(dataStr), &sseData) == nil && sseData.Model != "" && sseData.Model != requestModel {
					oldPattern := []byte(`"model":"` + sseData.Model + `"`)
					newPattern := []byte(`"model":"` + requestModel + `"`)
					rewritten := bytes.Replace([]byte(dataStr), oldPattern, newPattern, 1)
					line = "data: " + string(rewritten)
				}
			}
		}

		// Write line to client
		fmt.Fprintf(w, "%s\n", line)
		flusher.Flush()

		// Parse usage from the final data frame
		if strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
			dataStr := strings.TrimPrefix(line, "data:")
			dataStr = strings.TrimSpace(dataStr)

			if dataStr == "" {
				continue
			}

			var sseChunk struct {
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
					TotalTokens      int `json:"total_tokens"`
				} `json:"usage"`
			}

			if err := json.Unmarshal([]byte(dataStr), &sseChunk); err == nil && sseChunk.Usage != nil {
				promptTokens = sseChunk.Usage.PromptTokens
				completionTokens = sseChunk.Usage.CompletionTokens
				totalTokens = sseChunk.Usage.TotalTokens
			}
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("ERROR: scanning SSE stream: %v", err)
	}

	latencyMs := int(time.Since(startTime).Milliseconds())

	// Log the call with token statistics
	statusCode := http.StatusOK
	callLog := &models.CallLog{
		UserID:           userID,
		Model:            model,
		ProviderID:       providerID,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      totalTokens,
		EffectiveCalls:   effectiveCalls,
		MultiplierUsed:   multiplier,
		StatusCode:       statusCode,
		LatencyMs:        latencyMs,
	}
	if _, err := models.InsertCallLog(h.QuotaChecker.DB(), callLog); err != nil {
		log.Printf("ERROR: logging stream call: %v", err)
	}

	// Account the Token usage toward the user's cumulative Token quota
	// (fire-and-forget: a failure is logged only and must not break the
	// already-flushed SSE response).
	//
	// Apply the active multiplier to the BILLED Token counter (mirrors the sync
	// path in handler.go): a request under a 2x window costs 2x Tokens toward
	// the cap, exactly like the call-count quota does. The real upstream usage
	// is still recorded verbatim in call_logs for honest auditing.
	billedTokens := int(math.Ceil(float64(promptTokens+completionTokens) * multiplier))
	if err := models.AddTokenUsage(h.QuotaChecker.DB(), userID, billedTokens); err != nil {
		log.Printf("ERROR: add token usage (stream) for user %d: %v", userID, err)
	}
}
