package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/timeutil"
)

// contextKey is a private type for context keys to avoid collisions.
type contextKey string

const (
	// CtxKeyUserID is the context key for the authenticated user ID.
	CtxKeyUserID contextKey = "user_id"
	// CtxKeyUserRole is the context key for the user's role.
	CtxKeyUserRole contextKey = "user_role"
	// CtxKeyRouteMode is the context key for the user's routing mode ("auto" | "fixed").
	CtxKeyRouteMode contextKey = "route_mode"
	// CtxKeyFixedProvider is the context key for the user's fixed provider slug.
	CtxKeyFixedProvider contextKey = "fixed_provider"
	// CtxKeyMaxBodySize is the context key for the user's per-request body cap (bytes).
	CtxKeyMaxBodySize contextKey = "max_body_size"
	// CtxKeyMaxConcurrency is the context key for the user's per-user
	// concurrent request cap (0 = unlimited).
	CtxKeyMaxConcurrency contextKey = "max_concurrency"
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
// It sets user_id, user_role, route_mode and fixed_provider in the request context.
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

		// Check if user's API key has expired (admin users are exempt).
		// A non-empty but malformed expiry is treated as already expired
		// (fail-closed): a typo'd value can never silently grant a permanent key.
		if user.Role != "admin" && user.ExpiresAt != "" {
			expiresAt, ok := models.ParseExpiry(user.ExpiresAt)
			if !ok || time.Now().In(timeutil.ShanghaiTZ).After(expiresAt) {
				writeAuthError(w, http.StatusForbidden, "API key has expired", "key_expired")
				return
			}
		}

		// Set user info in context (including route_mode and fixed_provider).
		ctx := context.WithValue(r.Context(), CtxKeyUserID, user.ID)
		ctx = context.WithValue(ctx, CtxKeyUserRole, user.Role)
		ctx = context.WithValue(ctx, CtxKeyRouteMode, user.RouteMode)
		ctx = context.WithValue(ctx, CtxKeyFixedProvider, user.FixedProvider)
		ctx = context.WithValue(ctx, CtxKeyMaxBodySize, int64(user.MaxBodySize))
		ctx = context.WithValue(ctx, CtxKeyMaxConcurrency, int64(user.MaxConcurrency))
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

// GetRouteMode extracts the route_mode from the request context.
func GetRouteMode(r *http.Request) string {
	v, ok := r.Context().Value(CtxKeyRouteMode).(string)
	if !ok {
		return "auto"
	}
	return v
}

// GetFixedProvider extracts the fixed_provider from the request context.
func GetFixedProvider(r *http.Request) string {
	v, ok := r.Context().Value(CtxKeyFixedProvider).(string)
	if !ok {
		return ""
	}
	return v
}

// GetMaxBodySize extracts the user's per-request body cap (bytes) from the
// request context. Returns 0 if unset, so callers must fall back to
// models.DefaultMaxBodySize.
func GetMaxBodySize(r *http.Request) int64 {
	v, ok := r.Context().Value(CtxKeyMaxBodySize).(int64)
	if !ok {
		return 0
	}
	return v
}

// GetMaxConcurrency extracts the user's per-user concurrent request cap from
// the request context. Returns 0 if unset, meaning unlimited — callers should
// allow the request through.
func GetMaxConcurrency(r *http.Request) int64 {
	v, ok := r.Context().Value(CtxKeyMaxConcurrency).(int64)
	if !ok {
		return 0
	}
	return v
}

// writeAuthError writes a JSON error response for authentication failures.
// It uses json.Encoder so that message / errType are correctly escaped (a
// quote or backslash in either field can no longer produce invalid JSON).
func writeAuthError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
			"code":    errType,
		},
	})
}
