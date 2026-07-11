// LLM API Gateway — Main entry point.
//
// Usage:
//
//	llm_api_gateway -config config.yaml
//	llm_api_gateway -config config.yaml -passwd <admin-password>   # Initialize admin account
package main

import (
	"database/sql"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/bcrypt"

	"llm_api_gateway/internal/admin"
	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/handler"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/proxy"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	adminPasswd := flag.String("passwd", "", "Initialize admin password (hashes and stores)")
	flag.Parse()

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Configuration loaded from %s", *configPath)

	// Credential store (in-memory only). Provider keys are injected from their
	// configured environment variables at startup and NEVER written to disk
	// (see ADR-0002 / AGENTS.md §5). The same holder instance is shared between
	// the router and the admin panel so admin key updates propagate immediately.
	creds := router.NewCredentialStore()
	for _, p := range cfg.Providers {
		initial := ""
		if p.APIKeyEnv != "" {
			initial = os.Getenv(p.APIKeyEnv)
		}
		creds.Set(p.ID, initial)
	}

	// Determine the default (zhipu) endpoint for the legacy fallback path.
	defaultEndpoint := cfg.API.ZhipuEndpoint
	for _, p := range cfg.Providers {
		if p.IsDefault {
			defaultEndpoint = p.Endpoint
		}
	}

	// Validate required config
	if creds.Get("zhipu") == "" {
		log.Println("WARNING: zhipu upstream key (ZHIPU_API_KEY) is not set. Set it via systemd env or admin panel.")
	}

	// Initialize database
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Router resolves the upstream provider per request (time-window routing).
	// Built after the DB is open so it can load rules from provider_routing_rules.
	routerInst := router.NewRouter(database.Conn, cfg, creds)

	// Run migrations
	if err := db.RunMigrations(database); err != nil {
		log.Fatalf("Failed to run migrations: %v", err)
	}

	// Handle -passwd flag (admin password initialization)
	if *adminPasswd != "" {
		initAdminPassword(database.Conn, *adminPasswd, cfg.Quota.Default5hLimit, cfg.Quota.DefaultTotalLimit)
		return
	}

	// Get sub-FS for admin static files (strip "web/admin/" prefix)
	adminFS, err := fs.Sub(adminStaticFS, "web/admin")
	if err != nil {
		log.Fatalf("Failed to create admin static FS: %v", err)
	}

	// Get sub-FS for user static files
	userFS, err := fs.Sub(userStaticFS, "web/user")
	if err != nil {
		log.Fatalf("Failed to create user static FS: %v", err)
	}

	// Initialize components
	authMW := auth.NewMiddleware(database.Conn)

	multiplierEng := quota.NewMultiplierEngine(database.Conn)

	quotaChecker := quota.NewChecker(database.Conn, multiplierEng, cfg.Quota.ResetIntervalHours)

	_ = quota.NewManager(quotaChecker) // quota manager for streaming

	quotaScheduler := quota.NewScheduler(database.Conn, cfg.Quota.ResetIntervalHours)
	quotaScheduler.Start()
	defer quotaScheduler.Stop()

	// Proxy handler — resolves the upstream provider via the Router.
	// Legacy getters remain as a fallback when Router is nil (gradual rollout).
	proxyHandler := &proxy.Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return defaultEndpoint },
		QuotaChecker:   quotaChecker,
		MultiplierEng:  multiplierEng,
		Router:         routerInst,
	}

	// Quota query handler
	quotaQueryHandler := &handler.QuotaHandler{
		DB:            database.Conn,
		MultEng:       multiplierEng,
		ResetInterval: cfg.Quota.ResetIntervalHours,
	}

	// Call logs handler (user self-service)
	callsHandler := &handler.CallsHandler{
		DB: database.Conn,
	}

	// Admin handler
	adminHandler := &admin.Handler{
		DB:                database.Conn,
		AuthMW:            authMW,
		MultiplierEng:     multiplierEng,
		StaticFS:          adminFS,
		SessionExpHours:   cfg.Auth.SessionExpireHours,
		SubKeySalt:        cfg.Auth.SubKeySalt,
		Default5hLimit:    cfg.Quota.Default5hLimit,
		DefaultTotalLimit: cfg.Quota.DefaultTotalLimit,
		ConfigPath:        *configPath,
		APIKeyConfigured:  func() bool { return creds.Get("zhipu") != "" },
		EndpointGetter:    func() string { return defaultEndpoint },
		APIKeySetter:      func(k string) { creds.Holder("zhipu").Set(k) },
	}

	// Build routes
	mux := http.NewServeMux()

	// Public API endpoints
	modelsHandler := &proxy.ModelsHandler{}
	mux.Handle("GET /v1/models", modelsHandler) // OpenAI-compatible model discovery (no auth)

	// API endpoints requiring sub-key auth
	mux.Handle("POST /v1/chat/completions", authMW.SubKeyAuth(proxyHandler))
	mux.Handle("GET /v1/quota", authMW.SubKeyAuth(quotaQueryHandler))
	mux.Handle("GET /v1/calls", authMW.SubKeyAuth(callsHandler))

	// Admin routes
	adminHandler.RegisterRoutes(mux)

	// User self-service console (public, no auth required)
	userFileHandler := http.FileServer(http.FS(userFS))
	mux.Handle("GET /user/", http.StripPrefix("/user/", userFileHandler))
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/user/", http.StatusMovedPermanently)
	})

	// Create HTTP server
	server := &http.Server{
		Addr:         cfg.Server.ListenAddr,
		Handler:      withLogging(mux),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("Shutting down server...")
		server.Close()
	}()

	log.Printf("LLM API Gateway starting on %s", cfg.Server.ListenAddr)
	log.Printf("Admin panel: http://%s/admin/", cfg.Server.ListenAddr)
	log.Printf("API endpoint: http://%s/v1/chat/completions", cfg.Server.ListenAddr)

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	log.Println("Server stopped")
}

// initAdminPassword creates or updates the admin user with the given password.
func initAdminPassword(conn *sql.DB, password string, default5hLimit, defaultTotalLimit int) {
	if password == "" {
		log.Fatal("Password cannot be empty")
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("Failed to hash password: %v", err)
	}

	// Check if admin user exists using models
	existingUser, err := models.GetUserByUsername(conn, "admin")
	if err != nil {
		log.Fatalf("Failed to check admin user: %v", err)
	}

	now := db.Now()

	if existingUser != nil {
		// Update existing admin password
		_, err = conn.Exec(
			`UPDATE users SET password_hash = ?, updated_at = ? WHERE username = 'admin'`,
			string(hashedPassword), now,
		)
		if err != nil {
			log.Fatalf("Failed to update admin password: %v", err)
		}
		log.Println("Admin password updated successfully")
	} else {
		// Create admin user with a placeholder sub-key
		placeholderSubKey := auth.GenerateSubKey("admin-init", 0)
		placeholderHash := auth.HashSubKey(placeholderSubKey)
		placeholderPreview := auth.SubKeyPreview(placeholderSubKey)

		_, err := models.CreateUser(
			conn, "admin", string(hashedPassword), placeholderHash, placeholderPreview,
			"admin", "active", 1000000, 100000000,
		)
		if err != nil {
			log.Fatalf("Failed to create admin user: %v", err)
		}
		log.Println("Admin user created successfully")
	}
}

// withLogging wraps an http.Handler with request logging.
func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}
