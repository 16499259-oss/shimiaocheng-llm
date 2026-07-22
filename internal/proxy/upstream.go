package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// BuildUpstreamRequest creates an HTTP request to the upstream LLM API.
// It injects the real API key and sets required headers.
func BuildUpstreamRequest(upstreamURL, realAPIKey string, bodyBytes []byte) (*http.Request, error) {
	// Normalize upstreamURL: auto-append /chat/completions if missing.
	if !strings.HasSuffix(upstreamURL, "/chat/completions") {
		upstreamURL = strings.TrimSuffix(upstreamURL, "/") + "/chat/completions"
	}

	// For streaming chat completions, force include_usage so the upstream
	// returns token usage in the final SSE frame (required for accurate Token
	// accounting on the streaming path). No-op for non-streaming bodies.
	bodyBytes = ensureStreamUsage(bodyBytes)

	req, err := http.NewRequest(http.MethodPost, upstreamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}

	// Set headers — note: real API key is only in this request, never logged
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+realAPIKey)
	req.Header.Set("Accept", "application/json")

	return req, nil
}

// ensureStreamUsage injects stream_options.include_usage=true into a JSON
// request body that has stream:true, so the upstream emits a usage chunk
// (OpenAI-compatible providers only send usage in the final frame when this is
// set). Only an explicit user-provided false is honored. Non-streaming,
// non-JSON, or unparsable bodies are returned unchanged.
func ensureStreamUsage(body []byte) []byte {
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return body
	}
	stream, ok := req["stream"].(bool)
	if !ok || !stream {
		return body
	}
	opts, ok := req["stream_options"].(map[string]interface{})
	if !ok {
		opts = make(map[string]interface{})
	}
	if v, ok := opts["include_usage"].(bool); ok && !v {
		return body
	}
	opts["include_usage"] = true
	req["stream_options"] = opts
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}
