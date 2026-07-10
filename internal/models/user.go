// Package models provides data structures and CRUD operations for the LLM API Gateway.
package models

import (
	"database/sql"
	"fmt"
	"time"
)

// User represents a user in the system.
type User struct {
	ID             int64  `json:"id"`
	Username       string `json:"username"`
	PasswordHash   string `json:"-"` // never exposed in JSON
	SubKeyHash     string `json:"-"` // never exposed in JSON
	SubKeyPreview  string `json:"sub_key_preview"`
	Role           string `json:"role"`
	Status         string `json:"status"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
}

// UserWithQuota combines user info with quota info for API responses.
type UserWithQuota struct {
	User
	Quota5hLimit   int    `json:"quota_5h_limit"`
	Quota5hUsed    int    `json:"quota_5h_used"`
	QuotaTotalLimit int   `json:"quota_total_limit"`
	QuotaTotalUsed int    `json:"quota_total_used"`
	TotalTokens    int64  `json:"total_tokens"`
	SubKey         string `json:"sub_key,omitempty"`
}

// CreateUser inserts a new user and associated quota record.
func CreateUser(db *sql.DB, username, passwordHash, subKeyHash, subKeyPreview, role, status string, quota5hLimit, quotaTotalLimit int) (*UserWithQuota, error) {
	now := time.Now().Format(time.RFC3339)

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert user
	result, err := tx.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		username, passwordHash, subKeyHash, subKeyPreview, role, status, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	userID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("get last insert id: %w", err)
	}

	// Calculate next window start (align to 5h boundaries)
	windowStart := calculateWindowStart(5)

	// Insert quota
	_, err = tx.Exec(
		`INSERT INTO quotas (user_id, quota_5h_limit, quota_5h_used, quota_total_limit, quota_total_used, window_start, updated_at)
		 VALUES (?, ?, 0, ?, 0, ?, ?)`,
		userID, quota5hLimit, quotaTotalLimit, windowStart, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert quota: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &UserWithQuota{
		User: User{
			ID:            userID,
			Username:      username,
			PasswordHash:  passwordHash,
			SubKeyHash:    subKeyHash,
			SubKeyPreview: subKeyPreview,
			Role:          role,
			Status:        status,
			CreatedAt:     now,
			UpdatedAt:     now,
		},
		Quota5hLimit:   quota5hLimit,
		Quota5hUsed:    0,
		QuotaTotalLimit: quotaTotalLimit,
		QuotaTotalUsed:  0,
	}, nil
}

// GetUserBySubKeyHash finds a user by their sub_key_hash.
func GetUserBySubKeyHash(db *sql.DB, subKeyHash string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id, username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at
		 FROM users WHERE sub_key_hash = ? AND status != 'deleted'`, subKeyHash,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.SubKeyHash, &u.SubKeyPreview, &u.Role, &u.Status, &u.CreatedAt, &u.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by sub key hash: %w", err)
	}
	return u, nil
}

// GetUserByID finds a user by their ID.
func GetUserByID(db *sql.DB, id int64) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id, username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at
		 FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.SubKeyHash, &u.SubKeyPreview, &u.Role, &u.Status, &u.CreatedAt, &u.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by id: %w", err)
	}
	return u, nil
}

// GetUserByUsername finds a user by username.
func GetUserByUsername(db *sql.DB, username string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id, username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at
		 FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.SubKeyHash, &u.SubKeyPreview, &u.Role, &u.Status, &u.CreatedAt, &u.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	return u, nil
}

// ListUsers returns all users with their quota information (for admin).
func ListUsers(db *sql.DB) ([]UserWithQuota, error) {
	rows, err := db.Query(
		`SELECT u.id, u.username, u.sub_key_preview, u.role, u.status, u.created_at, u.updated_at,
		        q.quota_5h_limit, q.quota_5h_used, q.quota_total_limit, q.quota_total_used,
		        COALESCE(t.total_tokens, 0) AS total_tokens
		 FROM users u
		 LEFT JOIN quotas q ON u.id = q.user_id
		 LEFT JOIN (SELECT user_id, SUM(total_tokens) AS total_tokens FROM call_logs GROUP BY user_id) t ON u.id = t.user_id
		 WHERE u.status != 'deleted'
		 ORDER BY u.id DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []UserWithQuota
	for rows.Next() {
		var uwq UserWithQuota
		err := rows.Scan(
			&uwq.ID, &uwq.Username, &uwq.SubKeyPreview, &uwq.Role, &uwq.Status,
			&uwq.CreatedAt, &uwq.UpdatedAt,
			&uwq.Quota5hLimit, &uwq.Quota5hUsed, &uwq.QuotaTotalLimit, &uwq.QuotaTotalUsed,
			&uwq.TotalTokens,
		)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, uwq)
	}
	return users, rows.Err()
}

// UpdateUserStatus updates a user's status (active/disabled).
func UpdateUserStatus(db *sql.DB, userID int64, status string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE users SET status = ?, updated_at = ? WHERE id = ?`, status, now, userID)
	if err != nil {
		return fmt.Errorf("update user status: %w", err)
	}
	return nil
}

// RegenerateUserKey updates the user's sub_key_hash and sub_key_preview.
// The new sub_key in plaintext is NOT stored — caller must return it to the admin.
func RegenerateUserKey(db *sql.DB, userID int64, newSubKeyHash, newSubKeyPreview string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE users SET sub_key_hash = ?, sub_key_preview = ?, updated_at = ? WHERE id = ?`,
		newSubKeyHash, newSubKeyPreview, now, userID,
	)
	if err != nil {
		return fmt.Errorf("regenerate user key: %w", err)
	}
	return nil
}

// calculateWindowStart computes the start of the current 5h window.
func calculateWindowStart(intervalHours int) string {
	now := time.Now()
	// Truncate to the window boundary
	windowIndex := now.Hour() / intervalHours
	windowHour := windowIndex * intervalHours
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), windowHour, 0, 0, 0, now.Location())
	return windowStart.Format(time.RFC3339)
}
