package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
)

// CallsHandler handles GET /v1/calls for user self-service call log queries.
type CallsHandler struct {
	DB *sql.DB
}

// ServeHTTP handles GET /v1/calls. Only returns the authenticated user's own calls.
func (h *CallsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	userID := auth.GetUserID(r)
	if userID == 0 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Not authenticated"})
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if page <= 0 {
		page = 1
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	filter := models.CallLogFilter{
		UserID: userID,
		Page:   page,
		Limit:  limit,
	}

	result, err := models.QueryCallLogs(h.DB, filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to query call logs"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func writeJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}
