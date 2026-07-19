package admin

import (
	"io/fs"
	"log"
	"net/http"
	"strings"

	"llm_api_gateway/internal/models"
)

// HandleListProviderUsage handles GET /admin/api/provider-usage.
//
// It aggregates rolling-window usage for every provider and merges it with the
// provider records (name, limits) into a single ProviderUsageView slice. The
// rolling window is computed once (now-30d in Asia/Shanghai) and applied as a
// text comparison against call_logs.created_at.
func (h *Handler) HandleListProviderUsage(w http.ResponseWriter, r *http.Request) {
	windowStart := models.RollingWindowStart()

	usage, err := models.AggregateProviderUsage(h.DB, windowStart)
	if err != nil {
		log.Printf("ERROR: aggregate provider usage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to aggregate provider usage"})
		return
	}

	providers, err := h.ProviderStore.ListProviders()
	if err != nil {
		log.Printf("ERROR: list providers for usage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list providers"})
		return
	}

	views := make([]models.ProviderUsageView, 0, len(providers))
	for _, p := range providers {
		views = append(views, models.BuildProviderUsageView(p, usage[p.Slug], windowStart))
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": views})
}

// HandleGetProviderUsage handles GET /admin/api/providers/{slug}/usage.
//
// Returns the ProviderUsageView for a single provider (used by the account-
// creation form's live hint). A non-existent slug yields 404.
func (h *Handler) HandleGetProviderUsage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Missing provider slug"})
		return
	}

	p, err := h.ProviderStore.GetProvider(slug)
	if err != nil {
		log.Printf("ERROR: get provider %s: %v", slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to get provider"})
		return
	}
	if p == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Provider not found"})
		return
	}

	windowStart := models.RollingWindowStart()
	used, err := models.GetProviderUsage(h.DB, slug, windowStart)
	if err != nil {
		log.Printf("ERROR: get provider usage %s: %v", slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to get provider usage"})
		return
	}

	view := models.BuildProviderUsageView(*p, used, windowStart)
	writeJSON(w, http.StatusOK, map[string]any{"data": view})
}

// ServeProviderUsagePage handles GET /admin/provider-usage.
//
// It serves the same SPA (index.html) but injects window.__INIT_TAB__ so the
// dashboard tab opens directly — providing a deep-linkable, standalone quota
// dashboard page.
func (h *Handler) ServeProviderUsagePage(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(h.StaticFS, "index.html")
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}

	// Inject the initial-tab marker before </head> so DOMContentLoaded sees it.
	html := strings.Replace(
		string(data),
		"</head>",
		"<script>window.__INIT_TAB__='provider-usage';</script></head>",
		1,
	)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(html))
}
