package admin

import (
	"io/fs"
	"log"
	"net/http"
	"strings"

	"llm_api_gateway/internal/models"
)

// globalTokenLowRatio returns the global default token low-balance threshold
// (remaining ratio) from config, falling back to 0.10 when Config is nil or
// the configured value is zero (defensive for tests and missing config).
func (h *Handler) globalTokenLowRatio() float64 {
	if h.Config != nil {
		if r := h.Config.ProviderQuota.DefaultTokenLowRatio; r > 0 {
			return r
		}
	}
	return 0.10
}

// globalCallLowRatio returns the global default call-count low-balance
// threshold (remaining ratio), with the same 0.10 fallback as
// globalTokenLowRatio.
func (h *Handler) globalCallLowRatio() float64 {
	if h.Config != nil {
		if r := h.Config.ProviderQuota.DefaultCallLowRatio; r > 0 {
			return r
		}
	}
	return 0.10
}

// HandleListProviderUsage handles GET /admin/api/provider-usage.
//
// It iterates over every provider, computes the fixed 30-day cycle window from
// the provider's cycle_start_date, aggregates usage within that window, fetches
// the cross-table allocation, and merges everything into ProviderUsageView.
func (h *Handler) HandleListProviderUsage(w http.ResponseWriter, r *http.Request) {
	providers, err := h.ProviderStore.ListProviders()
	if err != nil {
		log.Printf("ERROR: list providers for usage: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list providers"})
		return
	}

	views := make([]models.ProviderUsageView, 0, len(providers))
	for _, p := range providers {
		// Compute the current fixed 30-day cycle window for this provider.
		cycleStart, _ := models.CurrentCycleWindow(p.CycleStartDate)

		// Convert "2006-01-02" DATE to RFC3339 for call_logs.created_at comparison.
		windowRFC3339 := cycleStart + "T00:00:00+08:00"

		// Aggregate usage within the cycle window.
		used, err := models.GetProviderUsage(h.DB, p.Slug, windowRFC3339)
		if err != nil {
			log.Printf("ERROR: get provider usage %s: %v", p.Slug, err)
			// Degrade gracefully: zero usage.
			used = nil
		}

		// Fetch cross-table allocation.
		alloc, err := models.GetProviderAllocation(h.DB, p.Slug)
		if err != nil {
			log.Printf("ERROR: get provider allocation %s: %v", p.Slug, err)
			// Degrade gracefully: nil allocation.
			alloc = nil
		}

		view := models.BuildProviderUsageView(p, used, alloc,
			h.globalTokenLowRatio(), h.globalCallLowRatio())
		views = append(views, view)
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

	// Compute the current fixed 30-day cycle window.
	cycleStart, _ := models.CurrentCycleWindow(p.CycleStartDate)
	windowRFC3339 := cycleStart + "T00:00:00+08:00"

	used, err := models.GetProviderUsage(h.DB, slug, windowRFC3339)
	if err != nil {
		log.Printf("ERROR: get provider usage %s: %v", slug, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to get provider usage"})
		return
	}

	alloc, err := models.GetProviderAllocation(h.DB, slug)
	if err != nil {
		log.Printf("ERROR: get provider allocation %s: %v", slug, err)
		// Degrade gracefully: nil allocation.
		alloc = nil
	}

	view := models.BuildProviderUsageView(*p, used, alloc,
		h.globalTokenLowRatio(), h.globalCallLowRatio())
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
