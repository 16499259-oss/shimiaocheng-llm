package admin

import (
	"log"
	"net/http"
	"strconv"
)

// HandleListAuditLogs handles GET /admin/api/audit-logs?page=1&limit=50.
func (h *Handler) HandleListAuditLogs(w http.ResponseWriter, r *http.Request) {
	page := 1
	limit := 50

	if p := r.URL.Query().Get("page"); p != "" {
		if v, err := strconv.Atoi(p); err == nil && v > 0 {
			page = v
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 100 {
			limit = v
		}
	}

	logs, total, err := h.ProviderStore.ListAuditLogs(page, limit)
	if err != nil {
		log.Printf("ERROR: list audit logs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list audit logs"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"data":  logs,
		"total": total,
		"page":  page,
		"limit": limit,
	})
}
