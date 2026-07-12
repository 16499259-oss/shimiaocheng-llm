package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"llm_api_gateway/internal/models"
)

// createRoutingRuleRequest is the JSON body for POST /admin/api/routing-rules.
type createRoutingRuleRequest struct {
	ProviderID string `json:"provider_id"`
	StartTime  string `json:"start_time"`
	EndTime    string `json:"end_time"`
	DaysOfWeek string `json:"days_of_week"`
	Timezone   string `json:"timezone"`
	Enabled    bool   `json:"enabled"`
}

// updateRoutingRuleRequest is the JSON body for PUT /admin/api/routing-rules/{id}.
type updateRoutingRuleRequest struct {
	ProviderID *string `json:"provider_id"`
	StartTime  *string `json:"start_time"`
	EndTime    *string `json:"end_time"`
	DaysOfWeek *string `json:"days_of_week"`
	Enabled    *bool   `json:"enabled"`
}

// HandleListRoutingRules handles GET /admin/api/routing-rules.
func (h *Handler) HandleListRoutingRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.ProviderStore.ListRoutingRules()
	if err != nil {
		log.Printf("ERROR: list routing rules: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list routing rules"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": rules})
}

// HandleCreateRoutingRule handles POST /admin/api/routing-rules.
func (h *Handler) HandleCreateRoutingRule(w http.ResponseWriter, r *http.Request) {
	var req createRoutingRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.ProviderID == "" || req.StartTime == "" || req.EndTime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider_id, start_time, and end_time are required"})
		return
	}

	if req.DaysOfWeek == "" {
		req.DaysOfWeek = "*"
	}
	if req.Timezone == "" {
		req.Timezone = "Asia/Shanghai"
	}

	rule := &models.RoutingRule{
		ProviderID: req.ProviderID,
		StartTime:  req.StartTime,
		EndTime:    req.EndTime,
		DaysOfWeek: req.DaysOfWeek,
		Timezone:   req.Timezone,
		Enabled:    req.Enabled,
	}

	if err := h.ProviderStore.CreateRoutingRule(rule); err != nil {
		log.Printf("ERROR: create routing rule: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Trigger hot-reload.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after create routing rule: %v", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": rule})
}

// HandleUpdateRoutingRule handles PUT /admin/api/routing-rules/{id}.
func (h *Handler) HandleUpdateRoutingRule(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid routing rule ID"})
		return
	}

	var req updateRoutingRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	updates := map[string]any{}
	if req.ProviderID != nil {
		updates["provider_id"] = *req.ProviderID
	}
	if req.StartTime != nil {
		updates["start_time"] = *req.StartTime
	}
	if req.EndTime != nil {
		updates["end_time"] = *req.EndTime
	}
	if req.DaysOfWeek != nil {
		updates["days_of_week"] = *req.DaysOfWeek
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}

	if len(updates) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No fields to update"})
		return
	}

	if err := h.ProviderStore.UpdateRoutingRule(id, updates); err != nil {
		log.Printf("ERROR: update routing rule %d: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Trigger hot-reload.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after update routing rule: %v", err)
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "Updated"})
}

// HandleDeleteRoutingRule handles DELETE /admin/api/routing-rules/{id}.
func (h *Handler) HandleDeleteRoutingRule(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid routing rule ID"})
		return
	}

	if err := h.ProviderStore.DeleteRoutingRule(id); err != nil {
		log.Printf("ERROR: delete routing rule %d: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Trigger hot-reload.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after delete routing rule: %v", err)
	}

	w.WriteHeader(http.StatusNoContent)
}
