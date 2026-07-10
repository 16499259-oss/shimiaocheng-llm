package models

import (
	"database/sql"
	"fmt"
	"time"
)

// AdminSession represents an admin login session.
type AdminSession struct {
	ID           int64  `json:"id"`
	SessionToken string `json:"session_token"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
}

// CreateSession creates a new admin session in the database.
func CreateSession(db *sql.DB, sessionToken string, expireHours int) (*AdminSession, error) {
	now := time.Now()
	expiresAt := now.Add(time.Duration(expireHours) * time.Hour)

	s := &AdminSession{
		SessionToken: sessionToken,
		CreatedAt:    now.Format(time.RFC3339),
		ExpiresAt:    expiresAt.Format(time.RFC3339),
	}

	result, err := db.Exec(
		`INSERT INTO admin_sessions (session_token, created_at, expires_at) VALUES (?, ?, ?)`,
		s.SessionToken, s.CreatedAt, s.ExpiresAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}

	s.ID, err = result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get session id: %w", err)
	}

	return s, nil
}

// GetValidSession retrieves a session by token, only if not expired.
func GetValidSession(db *sql.DB, sessionToken string) (*AdminSession, error) {
	s := &AdminSession{}
	now := time.Now().Format(time.RFC3339)

	err := db.QueryRow(
		`SELECT id, session_token, created_at, expires_at
		 FROM admin_sessions WHERE session_token = ? AND expires_at > ?`,
		sessionToken, now,
	).Scan(&s.ID, &s.SessionToken, &s.CreatedAt, &s.ExpiresAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get valid session: %w", err)
	}
	return s, nil
}

// DeleteSession removes a session from the database (logout).
func DeleteSession(db *sql.DB, sessionToken string) error {
	_, err := db.Exec(`DELETE FROM admin_sessions WHERE session_token = ?`, sessionToken)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// CleanExpiredSessions removes all expired sessions.
func CleanExpiredSessions(db *sql.DB) (int64, error) {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(`DELETE FROM admin_sessions WHERE expires_at <= ?`, now)
	if err != nil {
		return 0, fmt.Errorf("clean expired sessions: %w", err)
	}
	return result.RowsAffected()
}
