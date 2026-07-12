package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// createMappingRequest is the JSON body for POST /admin/api/mappings.
type createMappingRequest struct {
	External   string `json:"external"`
	ProviderID string `json:"provider_id"`
	RealModel  string `json:"real_model"`
}

// HandleListMappings handles GET /admin/api/mappings.
func (h *Handler) HandleListMappings(w http.ResponseWriter, r *http.Request) {
	mappings, err := h.ProviderStore.ListMappings()
	if err != nil {
		log.Printf("ERROR: list mappings: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list mappings"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": mappings})
}

// HandleCreateMapping handles POST /admin/api/mappings.
func (h *Handler) HandleCreateMapping(w http.ResponseWriter, r *http.Request) {
	var req createMappingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.External == "" || req.ProviderID == "" || req.RealModel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "external, provider_id, and real_model are required"})
		return
	}

	mapping, err := h.ProviderStore.CreateMapping(req.External, req.ProviderID, req.RealModel)
	if err != nil {
		log.Printf("ERROR: create mapping: %v", err)
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}

	// Trigger hot-reload so the mapping takes effect.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after create mapping: %v", err)
	}

	writeJSON(w, http.StatusCreated, map[string]any{"data": mapping})
}

// HandleDeleteMapping handles DELETE /admin/api/mappings/{id}.
func (h *Handler) HandleDeleteMapping(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid mapping ID"})
		return
	}

	if err := h.ProviderStore.DeleteMapping(id); err != nil {
		log.Printf("ERROR: delete mapping %d: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Trigger hot-reload.
	if err := h.Router.Reload(); err != nil {
		log.Printf("ERROR: router reload after delete mapping: %v", err)
	}

	w.WriteHeader(http.StatusNoContent)
}
