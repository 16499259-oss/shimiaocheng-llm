// Package admin provides the admin panel HTTP handlers.
package admin

import (
	"database/sql"
	"io/fs"
	"log"
	"net/http"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
)

// Handler manages all admin panel routes and sub-handlers.
type Handler struct {
	DB                *sql.DB
	AuthMW            *auth.Middleware
	MultiplierEng     *quota.MultiplierEngine
	StaticFS          fs.FS
	SessionExpHours   int
	SubKeySalt        string
	Default5hLimit    int
	DefaultTotalLimit int
	// Settings
	ConfigPath       string
	APIKeyConfigured func() bool
	EndpointGetter   func() string
	APIKeySetter     func(string) // updates the in-memory API key (no disk persistence)

	// Provider management (ADR-0007)
	ProviderStore *provider.ProviderStore
	Router        *router.Router
}

// RegisterRoutes registers all admin routes on the given ServeMux.
// Uses a sub-mux under /admin/ to avoid Go 1.22 pattern conflicts.
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	// Admin login/logout (no auth required) — top-level
	mux.HandleFunc("POST /admin/login", h.HandleLogin)
	mux.HandleFunc("POST /admin/logout", h.HandleLogout)
	mux.HandleFunc("GET /admin/login", h.ServeLoginPage)

	// Sub-mux for all /admin/ routes with auth
	adminMux := http.NewServeMux()

	// Dashboard page (auth required)
	adminMux.HandleFunc("/", h.ServeDashboardPage)

	// API endpoints (auth required)
	adminMux.HandleFunc("GET /api/users", h.ListUsers)
	adminMux.HandleFunc("POST /api/users", h.CreateUser)
	adminMux.HandleFunc("PUT /api/users/{id}", h.UpdateUser)
	adminMux.HandleFunc("DELETE /api/users/{id}", h.DeleteUser)
	adminMux.HandleFunc("GET /api/users/{id}/calls", h.GetUserCalls)
	adminMux.HandleFunc("GET /api/overview", h.GetOverview)
	adminMux.HandleFunc("GET /api/multipliers", h.ListMultipliers)
	adminMux.HandleFunc("POST /api/multipliers", h.CreateMultiplier)
	adminMux.HandleFunc("PUT /api/multipliers/{id}", h.UpdateMultiplier)
	adminMux.HandleFunc("DELETE /api/multipliers/{id}", h.DeleteMultiplier)
	adminMux.HandleFunc("GET /api/settings", h.HandleGetSettings)
	adminMux.HandleFunc("PUT /api/settings", h.HandleUpdateSettings)

	// Provider management routes (ADR-0007)
	adminMux.HandleFunc("GET /api/providers", h.HandleListProviders)
	adminMux.HandleFunc("POST /api/providers", h.HandleCreateProvider)
	adminMux.HandleFunc("PUT /api/providers/{slug}", h.HandleUpdateProvider)
	adminMux.HandleFunc("DELETE /api/providers/{slug}", h.HandleDeleteProvider)

	// Model mapping routes
	adminMux.HandleFunc("GET /api/mappings", h.HandleListMappings)
	adminMux.HandleFunc("POST /api/mappings", h.HandleCreateMapping)
	adminMux.HandleFunc("DELETE /api/mappings/{id}", h.HandleDeleteMapping)

	// Routing rules routes
	adminMux.HandleFunc("GET /api/routing-rules", h.HandleListRoutingRules)
	adminMux.HandleFunc("POST /api/routing-rules", h.HandleCreateRoutingRule)
	adminMux.HandleFunc("PUT /api/routing-rules/{id}", h.HandleUpdateRoutingRule)
	adminMux.HandleFunc("DELETE /api/routing-rules/{id}", h.HandleDeleteRoutingRule)

	// Audit log routes
	adminMux.HandleFunc("GET /api/audit-logs", h.HandleListAuditLogs)

	// Static files (no auth needed)
	adminMux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(h.StaticFS))))

	// Auth middleware: session for page routes, API auth for API routes
	authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static files — no auth required
		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/static/" {
			adminMux.ServeHTTP(w, r)
			return
		}
		// API routes need JSON auth
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			h.AuthMW.AdminSessionAuthAPI(adminMux).ServeHTTP(w, r)
		} else {
			h.AuthMW.AdminSessionAuth(adminMux).ServeHTTP(w, r)
		}
	})

	mux.Handle("/admin/", http.StripPrefix("/admin", authHandler))
	log.Println("Admin routes registered")
}

// ServeLoginPage serves the admin login HTML page.
func (h *Handler) ServeLoginPage(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(h.StaticFS, "login.html")
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ServeDashboardPage serves the admin dashboard HTML page.
func (h *Handler) ServeDashboardPage(w http.ResponseWriter, r *http.Request) {
	// Only serve for root path (sub-mux with StripPrefix, so path is "/" or "")
	if r.URL.Path != "/" && r.URL.Path != "" {
		http.NotFound(w, r)
		return
	}

	data, err := fs.ReadFile(h.StaticFS, "index.html")
	if err != nil {
		http.Error(w, "Page not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}
