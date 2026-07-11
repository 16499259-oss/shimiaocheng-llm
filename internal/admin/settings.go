package admin

import (
	"encoding/json"
	"net/http"
	"os"

	"gopkg.in/yaml.v3"
)

// HandleGetSettings returns current API key status and upstream endpoint.
func (h *Handler) HandleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"api_key_configured": h.APIKeyConfigured(),
		"endpoint":           h.EndpointGetter(),
	})
}

// HandleUpdateSettings updates the API key and/or endpoint, persists to config file.
func (h *Handler) HandleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ZhipuAPIKey   string `json:"zhipu_api_key"`
		ZhipuEndpoint string `json:"zhipu_endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Update the API key in memory immediately (no plaintext key persisted to disk).
	if req.ZhipuAPIKey != "" {
		h.APIKeySetter(req.ZhipuAPIKey)
	}

	// Persist endpoint only; never write the plaintext API key to disk.
	if err := h.persistToYAML(req.ZhipuAPIKey, req.ZhipuEndpoint); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "failed to save config: " + err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "API Key 已即时生效（内存态，重启将失效；建议用环境变量 ZHIPU_API_KEY 持久化）。Endpoint 已保存。",
	})
}

// persistToYAML reads the config file, updates the api fields, and writes it back.
func (h *Handler) persistToYAML(apiKey, endpoint string) error {
	data, err := os.ReadFile(h.ConfigPath)
	if err != nil {
		return err
	}

	var cfg map[string]interface{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return err
	}

	if cfg["api"] == nil {
		cfg["api"] = make(map[string]interface{})
	}
	apiSection := cfg["api"].(map[string]interface{})

	if apiKey != "" {
		// Never persist the plaintext API key to disk; strip it from the
		// on-disk config if it was previously stored in plaintext.
		delete(apiSection, "zhipu_api_key")
	}
	if endpoint != "" {
		apiSection["zhipu_endpoint"] = endpoint
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}

	return os.WriteFile(h.ConfigPath, out, 0600)
}
