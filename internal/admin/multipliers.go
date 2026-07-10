package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// createMultiplierRequest is the JSON body for POST /admin/api/multipliers.
type createMultiplierRequest struct {
	StartTime  string  `json:"start_time"`
	EndTime    string  `json:"end_time"`
	Multiplier float64 `json:"multiplier"`
	DaysOfWeek string  `json:"days_of_week"`
}

// updateMultiplierRequest is the JSON body for PUT /admin/api/multipliers/{id}.
type updateMultiplierRequest struct {
	StartTime  *string  `json:"start_time"`
	EndTime    *string  `json:"end_time"`
	Multiplier *float64 `json:"multiplier"`
	DaysOfWeek *string  `json:"days_of_week"`
	Enabled    *bool    `json:"enabled"`
}

// ListMultipliers handles GET /admin/api/multipliers.
func (h *Handler) ListMultipliers(w http.ResponseWriter, r *http.Request) {
	rules, err := h.MultiplierEng.FindAll()
	if err != nil {
		log.Printf("ERROR: list multipliers: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list multipliers"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": rules})
}

// CreateMultiplier handles POST /admin/api/multipliers.
func (h *Handler) CreateMultiplier(w http.ResponseWriter, r *http.Request) {
	var req createMultiplierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.StartTime == "" || req.EndTime == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "start_time and end_time are required"})
		return
	}
	if req.Multiplier < 1.0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multiplier must be >= 1.0"})
		return
	}
	if req.DaysOfWeek == "" {
		req.DaysOfWeek = "*"
	}

	rule, err := h.MultiplierEng.Create(req.StartTime, req.EndTime, req.Multiplier, req.DaysOfWeek)
	if err != nil {
		log.Printf("ERROR: create multiplier: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create multiplier"})
		return
	}

	writeJSON(w, http.StatusCreated, rule)
}

// UpdateMultiplier handles PUT /admin/api/multipliers/{id}.
func (h *Handler) UpdateMultiplier(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid multiplier ID"})
		return
	}

	var req updateMultiplierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	updates := map[string]any{}
	if req.StartTime != nil {
		updates["start_time"] = *req.StartTime
	}
	if req.EndTime != nil {
		updates["end_time"] = *req.EndTime
	}
	if req.Multiplier != nil {
		if *req.Multiplier < 1.0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "multiplier must be >= 1.0"})
			return
		}
		updates["multiplier"] = *req.Multiplier
	}
	if req.DaysOfWeek != nil {
		updates["days_of_week"] = *req.DaysOfWeek
	}
	if req.Enabled != nil {
		if *req.Enabled {
			updates["enabled"] = 1
		} else {
			updates["enabled"] = 0
		}
	}

	if len(updates) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "No fields to update"})
		return
	}

	if err := h.MultiplierEng.Update(id, updates); err != nil {
		log.Printf("ERROR: update multiplier: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update multiplier"})
		return
	}

	// Return updated rule
	rule, err := h.MultiplierEng.GetByID(id)
	if err != nil || rule == nil {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Updated"})
		return
	}

	writeJSON(w, http.StatusOK, rule)
}

// DeleteMultiplier handles DELETE /admin/api/multipliers/{id}.
func (h *Handler) DeleteMultiplier(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid multiplier ID"})
		return
	}

	if err := h.MultiplierEng.Delete(id); err != nil {
		log.Printf("ERROR: delete multiplier: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to delete multiplier"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
