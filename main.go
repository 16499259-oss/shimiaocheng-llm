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
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/proxy"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
	"llm_api_gateway/internal/security"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	adminPasswd := flag.String("passwd", "", "Initialize admin password (hashes and stores)")
	flag.Parse()

	// ── Phase 0: Load KEK (must come before any provider operations) ──
	kek, err := security.DeriveKEK()
	if err != nil {
		log.Fatalf("FATAL: GATEWAY_KEK_ENV is not set. This environment variable is required "+
			"for provider key encryption. Set it before starting the gateway. "+
			"Error: %v", err)
	}
	log.Println("KEK loaded from GATEWAY_KEK_ENV")

	// ── Phase 1: Load configuration ──
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	log.Printf("Configuration loaded from %s", *configPath)

	// ── Phase 2: Initialize database ──
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	// Run migrations (creates tables including providers, model_mappings, audit_logs).
	if err := db.RunMigrations(database); err != nil {
		log.Fatalf("Failed to run migrations: %v", err)
	}

	// ── Phase 3: ProviderStore + Seed + Router ──
	providerStore := provider.NewProviderStore(database.Conn, kek)

	// Seed from config.yaml if providers table is empty (idempotent).
	if err := providerStore.SeedFromConfig(cfg); err != nil {
		log.Fatalf("Failed to seed providers from config: %v", err)
	}

	// Router resolves the upstream provider per request (time-window routing).
	// Now reads from the ProviderStore via atomic snapshots instead of config.yaml.
	routerInst := router.NewRouter(database.Conn, providerStore)

	// ── Phase 4: Legacy credential store (backward compat) ──
	// Credential store is retained for backward compatibility (admin settings
	// panel may still read/write to it). New code paths use ProviderStore/Router.
	creds := router.NewCredentialStore()
	for _, p := range cfg.Providers {
		initial := ""
		if p.APIKeyEnv != "" {
			initial = os.Getenv(p.APIKeyEnv)
		}
		creds.Set(p.ID, initial)
	}

	// Determine the default endpoint for the legacy fallback path.
	defaultEndpoint := cfg.API.ZhipuEndpoint
	for _, p := range cfg.Providers {
		if p.IsDefault {
			defaultEndpoint = p.Endpoint
		}
	}

	// ── Phase 5: Handle -passwd flag ──
	if *adminPasswd != "" {
		initAdminPassword(database.Conn, *adminPasswd, cfg.Quota.Default5hLimit, cfg.Quota.DefaultTotalLimit)
		return
	}

	// ── Phase 6: Static file systems ──
	adminFS, err := fs.Sub(adminStaticFS, "web/admin")
	if err != nil {
		log.Fatalf("Failed to create admin static FS: %v", err)
	}

	userFS, err := fs.Sub(userStaticFS, "web/user")
	if err != nil {
		log.Fatalf("Failed to create user static FS: %v", err)
	}

	// ── Phase 7: Initialize components ──
	authMW := auth.NewMiddleware(database.Conn)

	multiplierEng := quota.NewMultiplierEngine(database.Conn)

	quotaChecker := quota.NewChecker(database.Conn, multiplierEng, cfg.Quota.ResetIntervalHours)

	_ = quota.NewManager(quotaChecker) // quota manager for streaming

	quotaScheduler := quota.NewScheduler(database.Conn, cfg.Quota.ResetIntervalHours)
	quotaScheduler.Start()
	defer quotaScheduler.Stop()

	// Proxy handler — resolves the upstream provider via the Router.
	// Legacy getters remain as a fallback when Router is nil (gradual rollout safety).
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

	// ── Phase 8: Admin handler with ProviderStore + Router ──
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
		// New fields for provider management.
		ProviderStore: providerStore,
		Router:        routerInst,
	}

	// ── Phase 9: Build routes ──
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

	// ── Phase 10: Start server ──
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
			"admin", "active", "", "auto", "", 1000000, 100000000, nil,
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
