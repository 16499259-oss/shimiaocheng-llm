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
	"sync"
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
)

// apiKeyHolder provides thread-safe runtime access to the upstream API key.
type apiKeyHolder struct {
	mu  sync.RWMutex
	key string
}

func (h *apiKeyHolder) Get() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.key
}

func (h *apiKeyHolder) Set(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.key = key
}

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

	// Key + endpoint holders — support runtime update via admin panel
	keyHolder := &apiKeyHolder{key: cfg.API.ZhipuAPIKey}
	endpointHolder := &apiKeyHolder{key: cfg.API.ZhipuEndpoint}

	// Validate required config
	if keyHolder.Get() == "" {
		log.Println("WARNING: zhipu_api_key is not set. Set it via admin panel.")
	}

	// Initialize database
	database, err := db.Open(cfg.Database.Path)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

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

	// Proxy handler — reads API key + endpoint dynamically via holders
	proxyHandler := &proxy.Handler{
		APIKeyGetter:    keyHolder.Get,
		EndpointGetter:  endpointHolder.Get,
		QuotaChecker:    quotaChecker,
		MultiplierEng:   multiplierEng,
	}

	// Quota query handler
	quotaQueryHandler := &handler.QuotaHandler{
		DB:            database.Conn,
		MultEng:       multiplierEng,
		ResetInterval: cfg.Quota.ResetIntervalHours,
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
		APIKeyConfigured:  func() bool { return keyHolder.Get() != "" },
		EndpointGetter:    endpointHolder.Get,
	}

	// Build routes
	mux := http.NewServeMux()

	// Public API endpoints (with sub-key auth)
	mux.Handle("POST /v1/chat/completions", authMW.SubKeyAuth(proxyHandler))
	mux.Handle("GET /v1/quota", authMW.SubKeyAuth(quotaQueryHandler))

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
