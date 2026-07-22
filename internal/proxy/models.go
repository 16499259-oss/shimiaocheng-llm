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

// allowedModelCORSOrigins restricts the OpenAI-compatible /v1/models endpoint's
// CORS to the gateway's own web origins. A wildcard "*" would let any website
// probe the model list on behalf of a victim's browser; we only echo origins we
// actually serve the admin/user panels from. Server-side API clients (Cursor,
// etc.) send no Origin header, so this does not affect them (audit LOW: CORS
// 通配 /v1/models).
var allowedModelCORSOrigins = map[string]bool{
	"https://ai.shimiaocheng.top": true,
}

// ServeHTTP serves the /v1/models response.
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if origin := r.Header.Get("Origin"); origin != "" && allowedModelCORSOrigins[origin] {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	resp := ModelsResponse{
		Object: "list",
		Data:   supportedModels(),
	}
	json.NewEncoder(w).Encode(resp)
}
