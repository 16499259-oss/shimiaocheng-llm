package admin

import (
	"encoding/json"
	"log"
	"net/http"
)

// listProvidersResponse is the JSON response for GET /admin/api/providers.
type listProvidersResponse struct {
	Data any `json:"data"`
}

// createProviderRequest is the JSON body for POST /admin/api/providers.
type createProviderRequest struct {
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	Endpoint  string `json:"endpoint"`
	APIKey    string `json:"api_key"`
	IsDefault bool   `json:"is_default"`
	// ── Passthrough / MCP support ──
	AllowPassthrough bool              `json:"allow_passthrough"`
	AuthHeader       string            `json:"auth_header"`
	AuthScheme       string            `json:"auth_scheme"`
	ExtraHeaders     map[string]string `json:"extra_headers"`
	// ── Monthly quota (0 = unlimited) ──
	MonthlyTokenLimit int64 `json:"monthly_token_limit"`
	MonthlyCallLimit  int64 `json:"monthly_call_limit"`
	// ── Low-balance thresholds (remaining ratio; 0 = inherit global default) ──
	MonthlyTokenLowRatio float64 `json:"monthly_token_low_ratio"`
	MonthlyCallLowRatio  float64 `json:"monthly_call_low_ratio"`
	// ── V3: Fixed 30-day cycle anchor ──
	CycleStartDate string `json:"cycle_start_date"` // "2006-01-02" DATE, defaults to today if empty
}

// updateProviderRequest is the JSON body for PUT /admin/api/providers/{slug}.
// Optional fields use pointers so "not provided" can be distinguished from
// "set to zero value".
type updateProviderRequest struct {
	Name      *string `json:"name"`
	Endpoint  *string `json:"endpoint"`
	APIKey    *string `json:"api_key"`
	IsDefault *bool   `json:"is_default"`
	Enabled   *bool   `json:"enabled"`
	// ── Passthrough / MCP support ──
	AllowPassthrough *bool              `json:"allow_passthrough"`
	AuthHeader       *string            `json:"auth_header"`
	AuthScheme       *string            `json:"auth_scheme"`
	ExtraHeaders     *map[string]string `json:"extra_headers"`
	// ── Monthly quota (0 = unlimited) ──
	MonthlyTokenLimit *int64 `json:"monthly_token_limit"`
	MonthlyCallLimit  *int64 `json:"monthly_call_limit"`
	// ── Low-balance thresholds (remaining ratio; nil = do not change) ──
	MonthlyTokenLowRatio *float64 `json:"monthly_token_low_ratio"`
	MonthlyCallLowRatio  *float64 `json:"monthly_call_low_ratio"`
	// ── V3: Fixed 30-day cycle anchor ──
	CycleStartDate *string `json:"cycle_start_date"` // nil = do not change
}

// HandleListProviders handles GET /admin/api/providers.
func (h *Handler) HandleListProviders(w http.ResponseWriter, r *http.Request) {
	providers, err := h.ProviderStore.BuildMaskedProviders()
	if err != nil {
		log.Printf("ERROR: list providers: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list providers"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": providers})
}

// HandleCreateProvider handles POST /admin/api/providers.
func (h *Handler) HandleCreateProvider(w http.ResponseWriter, r *http.Request) {
	var req createProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.Name == "" || req.Slug == "" || req.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name, slug, and endpoint are required"})
		return
	}

	// Defense: low-balance ratios must be a remaining ratio in [0, 1.0].
	// Out-of-range values are rejected outright (the frontend constrains input
	// via min/max but the backend must never trust the client).
	if req.MonthlyTokenLowRatio < 0 || req.MonthlyTokenLowRatio > 1.0 ||
		req.MonthlyCallLowRatio < 0 || req.MonthlyCallLowRatio > 1.0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid low ratio"})
		return
	}

	authHeader := req.AuthHeader
	if authHeader == "" {
		authHeader = "Authorization"
	}
	authScheme := req.AuthScheme
	if authScheme == "" {
		authScheme = "bearer"
	}

	prov, err := h.ProviderStore.CreateProvider(
		req.Name, req.Slug, req.Endpoint, req.APIKey, req.IsDefault,
		req.AllowPassthrough, authHeader, authScheme, req.ExtraHeaders,
		req.MonthlyTokenLimit, req.MonthlyCallLimit,
		req.MonthlyTokenLowRatio, req.MonthlyCallLowRatio,
		req.CycleStartDate,
	)
	if err != nil {
		log.Printf("ERROR: create provider: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Trigger hot-reload so the new provider is immediately available.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after create provider: %v", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": prov})
}

// HandleUpdateProvider handles PUT /admin/api/providers/{slug}.
func (h *Handler) HandleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing provider slug"})
		return
	}

	var req updateProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	updates := map[string]any{}
	if req.Name != nil {
		updates["name"] = *req.Name
	}
	if req.Endpoint != nil {
		updates["endpoint"] = *req.Endpoint
	}
	if req.APIKey != nil && *req.APIKey != "" {
		updates["encrypted_key"] = *req.APIKey
	}
	if req.IsDefault != nil {
		updates["is_default"] = *req.IsDefault
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}
	if req.AllowPassthrough != nil {
		updates["allow_passthrough"] = *req.AllowPassthrough
	}
	if req.AuthHeader != nil {
		updates["auth_header"] = *req.AuthHeader
	}
	if req.AuthScheme != nil {
		updates["auth_scheme"] = *req.AuthScheme
	}
	if req.ExtraHeaders != nil {
		updates["extra_headers"] = *req.ExtraHeaders
	}
	if req.MonthlyTokenLimit != nil {
		updates["monthly_token_limit"] = *req.MonthlyTokenLimit
	}
	if req.MonthlyCallLimit != nil {
		updates["monthly_call_limit"] = *req.MonthlyCallLimit
	}
	if req.MonthlyTokenLowRatio != nil {
		// Defense: reject out-of-range ratios even on partial updates.
		if *req.MonthlyTokenLowRatio < 0 || *req.MonthlyTokenLowRatio > 1.0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid low ratio"})
			return
		}
		updates["monthly_token_low_ratio"] = *req.MonthlyTokenLowRatio
	}
	if req.MonthlyCallLowRatio != nil {
		if *req.MonthlyCallLowRatio < 0 || *req.MonthlyCallLowRatio > 1.0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid low ratio"})
			return
		}
		updates["monthly_call_low_ratio"] = *req.MonthlyCallLowRatio
	}
	if req.CycleStartDate != nil {
		updates["cycle_start_date"] = *req.CycleStartDate
	}

	if len(updates) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No fields to update"})
		return
	}

	prov, err := h.ProviderStore.UpdateProvider(slug, updates)
	if err != nil {
		log.Printf("ERROR: update provider %s: %v", slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if prov == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Provider not found"})
		return
	}

	// Trigger hot-reload.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after update provider: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": prov})
}

// HandleDeleteProvider handles DELETE /admin/api/providers/{slug}.
func (h *Handler) HandleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing provider slug"})
		return
	}

	if err := h.ProviderStore.DeleteProvider(slug); err != nil {
		log.Printf("ERROR: delete provider %s: %v", slug, err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	// Trigger hot-reload.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after delete provider: %v", err)
	}

	w.WriteHeader(http.StatusNoContent)
}
