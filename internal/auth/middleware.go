package auth

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strings"

	"llm_api_gateway/internal/models"
)

// contextKey is a private type for context keys to avoid collisions.
type contextKey string

const (
	// CtxKeyUserID is the context key for the authenticated user ID.
	CtxKeyUserID contextKey = "user_id"
	// CtxKeyUserRole is the context key for the user's role.
	CtxKeyUserRole contextKey = "user_role"
)

// Middleware provides authentication middleware functions.
type Middleware struct {
	DB *sql.DB
}

// NewMiddleware creates a new auth middleware instance.
func NewMiddleware(db *sql.DB) *Middleware {
	return &Middleware{DB: db}
}

// SubKeyAuth is a middleware that authenticates requests using a Bearer sub-key.
// It sets user_id and user_role in the request context.
func (m *Middleware) SubKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract Bearer token
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			writeAuthError(w, http.StatusUnauthorized, "Missing or invalid Authorization header", "invalid_api_key")
			return
		}

		subKey := strings.TrimPrefix(authHeader, "Bearer ")
		if subKey == "" || !strings.HasPrefix(subKey, SubKeyPrefix) {
			writeAuthError(w, http.StatusUnauthorized, "Invalid API key format", "invalid_api_key")
			return
		}

		// Hash and look up
		keyHash := HashSubKey(subKey)
		user, err := models.GetUserBySubKeyHash(m.DB, keyHash)
		if err != nil {
			log.Printf("ERROR: sub key lookup: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "Internal server error", "internal_error")
			return
		}
		if user == nil {
			writeAuthError(w, http.StatusUnauthorized, "Invalid API key", "invalid_api_key")
			return
		}

		// Check if user is disabled or deleted
		if user.Status == "disabled" || user.Status == "deleted" {
			msg := "API key has been disabled"
			if user.Status == "deleted" {
				msg = "API key has been revoked"
			}
			writeAuthError(w, http.StatusForbidden, msg, "key_revoked")
			return
		}

		// Set user info in context
		ctx := context.WithValue(r.Context(), CtxKeyUserID, user.ID)
		ctx = context.WithValue(ctx, CtxKeyUserRole, user.Role)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AdminSessionAuth is a middleware that authenticates admin panel requests via session cookie.
func (m *Middleware) AdminSessionAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract session cookie
		cookie, err := r.Cookie("admin_session")
		if err != nil {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		session, err := models.GetValidSession(m.DB, cookie.Value)
		if err != nil {
			log.Printf("ERROR: session lookup: %v", err)
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if session == nil {
			// Clear expired cookie
			http.SetCookie(w, &http.Cookie{
				Name:     "admin_session",
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				HttpOnly: true,
				SameSite: http.SameSiteStrictMode,
			})
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), CtxKeyUserRole, "admin")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AdminSessionAuthAPI is like AdminSessionAuth but returns JSON errors instead of redirects.
// Used for /admin/api/* endpoints.
func (m *Middleware) AdminSessionAuthAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("admin_session")
		if err != nil {
			writeAuthError(w, http.StatusUnauthorized, "Not authenticated", "not_authenticated")
			return
		}

		session, err := models.GetValidSession(m.DB, cookie.Value)
		if err != nil {
			log.Printf("ERROR: session lookup: %v", err)
			writeAuthError(w, http.StatusInternalServerError, "Internal server error", "internal_error")
			return
		}
		if session == nil {
			writeAuthError(w, http.StatusUnauthorized, "Session expired or invalid", "session_expired")
			return
		}

		ctx := context.WithValue(r.Context(), CtxKeyUserRole, "admin")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetUserID extracts the authenticated user ID from the request context.
func GetUserID(r *http.Request) int64 {
	v, ok := r.Context().Value(CtxKeyUserID).(int64)
	if !ok {
		return 0
	}
	return v
}

// writeAuthError writes a JSON error response for authentication failures.
func writeAuthError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	body := `{"error":{"message":"` + message + `","type":"` + errType + `","code":"` + errType + `"}}`
	w.Write([]byte(body))
}
