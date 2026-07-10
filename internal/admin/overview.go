package admin

import (
	"log"
	"net/http"

	"llm_api_gateway/internal/models"
)

// GetOverview handles GET /admin/api/overview.
func (h *Handler) GetOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := models.GetDashboardOverview(h.DB)
	if err != nil {
		log.Printf("ERROR: dashboard overview: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to get overview"})
		return
	}

	writeJSON(w, http.StatusOK, overview)
}
