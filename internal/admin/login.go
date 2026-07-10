package admin

import (
	"encoding/json"
	"log"
	"net/http"

	"golang.org/x/crypto/bcrypt"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
)

// loginRequest is the JSON body for POST /admin/login.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// HandleLogin processes the admin login request.
func (h *Handler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Username and password are required"})
		return
	}

	// Find user by username
	user, err := models.GetUserByUsername(h.DB, req.Username)
	if err != nil {
		log.Printf("ERROR: login lookup: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Internal server error"})
		return
	}
	if user == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid username or password"})
		return
	}

	// Check role
	if user.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Not an admin account"})
		return
	}

	// Check status
	if user.Status == "disabled" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Account is disabled"})
		return
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid username or password"})
		return
	}

	// Create session
	sessionToken := auth.GenerateSessionToken()
	session, err := models.CreateSession(h.DB, sessionToken, h.SessionExpHours)
	if err != nil {
		log.Printf("ERROR: create session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create session"})
		return
	}

	// Set cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    session.SessionToken,
		Path:     "/",
		MaxAge:   h.SessionExpHours * 3600,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})

	writeJSON(w, http.StatusOK, map[string]string{
		"message":  "Login successful",
		"username": user.Username,
	})
}

// HandleLogout processes the admin logout request.
func (h *Handler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("admin_session")
	if err == nil && cookie != nil {
		models.DeleteSession(h.DB, cookie.Value)
	}

	// Clear cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	writeJSON(w, http.StatusOK, map[string]string{"message": "Logged out"})
}

// writeJSON is a helper for writing JSON responses.
func writeJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}
