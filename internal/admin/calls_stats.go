package admin

import (
	"log"
	"net/http"
	"strconv"

	"llm_api_gateway/internal/models"
)

// ListCalls handles GET /admin/api/calls — a global (multi-user) call-log
// list. Query params: user_id (0/empty = all), provider_id, model, from, to
// (RFC3339, SH-normalized server side), page, limit.
//
// It is a pure read; no quota / billing / routing state is touched.
func (h *Handler) ListCalls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	userID, _ := strconv.ParseInt(q.Get("user_id"), 10, 64) // 0 = all users
	providerID := q.Get("provider_id")
	model := q.Get("model")
	from := models.NormalizeToShanghaiRFC3339(q.Get("from"))
	to := models.NormalizeToShanghaiRFC3339(q.Get("to"))
	page, _ := strconv.Atoi(q.Get("page"))
	limit, _ := strconv.Atoi(q.Get("limit"))

	filter := models.CallLogFilter{
		UserID:     userID,
		ProviderID: providerID,
		Model:      model,
		From:       from,
		To:         to,
		Page:       page,
		Limit:      limit,
	}

	page_, err := models.QueryCallLogsGlobal(h.DB, filter)
	if err != nil {
		log.Printf("ERROR: query global calls: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to query calls"})
		return
	}
	writeJSON(w, http.StatusOK, page_)
}

// CallsStats handles GET /admin/api/calls/stats — the aggregated summary for
// the same filter set (page/limit are ignored; aggregation is over the full
// filtered result). Pure read; no state mutation.
func (h *Handler) CallsStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	userID, _ := strconv.ParseInt(q.Get("user_id"), 10, 64) // 0 = all users
	providerID := q.Get("provider_id")
	model := q.Get("model")
	from := models.NormalizeToShanghaiRFC3339(q.Get("from"))
	to := models.NormalizeToShanghaiRFC3339(q.Get("to"))

	filter := models.CallLogFilter{
		UserID:     userID,
		ProviderID: providerID,
		Model:      model,
		From:       from,
		To:         to,
	}

	stats, err := models.AggregateCallStats(h.DB, filter)
	if err != nil {
		log.Printf("ERROR: aggregate call stats: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to aggregate call stats"})
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ListCallModels handles GET /admin/api/calls/models — returns the distinct,
// sorted model names used to populate the model filter dropdown. The query has
// no user input, so it is inherently injection-safe. Pure read.
func (h *Handler) ListCallModels(w http.ResponseWriter, r *http.Request) {
	models_, err := models.DistinctModels(h.DB)
	if err != nil {
		log.Printf("ERROR: query distinct call models: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to query models"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": models_})
}
