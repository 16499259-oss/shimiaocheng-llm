package proxy

import (
	"bytes"
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
