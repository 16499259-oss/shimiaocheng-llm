// Package proxy — /v1/models endpoint for OpenAI-compatible client discovery.
package proxy

import (
	"encoding/json"
	"net/http"
)

// ModelEntry describes a single model in the /v1/models response.
type ModelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// ModelsResponse is the OpenAI-compatible /v1/models payload.
type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// ModelsHandler serves GET /v1/models to list supported models.
// This is required by Cursor, Continue, and other OpenAI-compatible
// clients that probe the endpoint during connection setup.
type ModelsHandler struct{}

// supportedModels returns the list of advertised models.
// Note: model names should match what the upstream (Zhipu Coding Plan / GLM API) accepts.
func supportedModels() []ModelEntry {
	return []ModelEntry{
		{ID: "glm-5.2", Object: "model", Created: 1735689600, OwnedBy: "zhipu"},
		{ID: "glm-4", Object: "model", Created: 1735689600, OwnedBy: "zhipu"},
		{ID: "glm-4-flash", Object: "model", Created: 1735689600, OwnedBy: "zhipu"},
		{ID: "glm-4v", Object: "model", Created: 1735689600, OwnedBy: "zhipu"},
		{ID: "glm-4-plus", Object: "model", Created: 1735689600, OwnedBy: "zhipu"},
	}
}

// ServeHTTP serves the /v1/models response.
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	resp := ModelsResponse{
		Object: "list",
		Data:   supportedModels(),
	}
	json.NewEncoder(w).Encode(resp)
}
