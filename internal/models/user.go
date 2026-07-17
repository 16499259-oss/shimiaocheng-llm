// Package models provides data structures and CRUD operations for the LLM API Gateway.
package models

import (
	"database/sql"
	"fmt"
	"time"
)

// DefaultMaxBodySize is the per-user request body cap (1MB) applied when a
// user has no explicit max_body_size set.
const DefaultMaxBodySize = 1 << 20

// MaxBodySizeCeiling is the hard ceiling for any per-user cap. It MUST match the
// nginx /v1/ client_max_body_size (currently 32MB): a per-user cap above this is
// unreachable because nginx rejects larger bodies before Go sees them.
const MaxBodySizeCeiling = 32 << 20

// DefaultMaxConcurrency is the per-user concurrent request cap applied when a
// user has no explicit max_concurrency set. 0 means unlimited.
const DefaultMaxConcurrency = 10

// MaxConcurrencyHardLimit is the absolute ceiling for any per-user concurrency
// cap configured via the admin panel (create or edit). It bounds the in-process
// atomic counter map so a single user cannot register an arbitrarily large cap
// that would defeat the protective intent of the limit.
const MaxConcurrencyHardLimit = 200

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
	ExpiresAt      string `json:"expires_at"`
	RouteMode      string `json:"route_mode"`      // "auto" | "fixed"
	FixedProvider  string `json:"fixed_provider"`  // provider slug when route_mode=fixed
	MaxBodySize    int    `json:"max_body_size"`   // per-request body cap in bytes (default 1MB)
	MaxConcurrency int    `json:"max_concurrency"` // max simultaneous in-flight requests (0 = unlimited, default 10)
}

// UserWithQuota combines user info with quota info for API responses.
type UserWithQuota struct {
	User
	Quota5hLimit         int      `json:"quota_5h_limit"`
	Quota5hUsed          int      `json:"quota_5h_used"`
	QuotaTotalLimit      int      `json:"quota_total_limit"`
	QuotaTotalUsed       int      `json:"quota_total_used"`
	QuotaTokenTotalLimit int      `json:"quota_token_total_limit"` // 0 = unlimited
	QuotaTokenTotalUsed  int      `json:"quota_token_total_used"`  // cumulative used
	TotalTokens          int64    `json:"total_tokens"`
	SubKey               string   `json:"sub_key,omitempty"`
	FixedMultiplier      *float64 `json:"fixed_multiplier"` // nil = global
}

// CreateUser inserts a new user and associated quota record.
// maxBodySize is the per-request body cap in bytes; <=0 falls back to DefaultMaxBodySize.
// maxConcurrency is the per-user concurrent request cap written at creation: 0 means
// unlimited (no cap); a positive N caps simultaneous in-flight requests.
func CreateUser(db *sql.DB, username, passwordHash, subKeyHash, subKeyPreview, role, status, expiresAt, routeMode, fixedProvider string, quota5hLimit, quotaTotalLimit int, fixedMultiplier *float64, maxBodySize, maxConcurrency int) (*UserWithQuota, error) {
	now := time.Now().Format(time.RFC3339)

	// Admin users never expire and always use auto route mode.
	if role == "admin" {
		expiresAt = ""
		routeMode = "auto"
		fixedProvider = ""
	}

	// Default route_mode to "auto" if not specified.
	if routeMode == "" {
		routeMode = "auto"
	}

	// Default per-request body cap to 1MB when unset or invalid.
	if maxBodySize <= 0 {
		maxBodySize = DefaultMaxBodySize
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Insert user
	result, err := tx.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, expires_at, route_mode, fixed_provider, max_body_size, max_concurrency, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		username, passwordHash, subKeyHash, subKeyPreview, role, status, expiresAt, routeMode, fixedProvider, maxBodySize, maxConcurrency, now, now,
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
		`INSERT INTO quotas (user_id, quota_5h_limit, quota_5h_used, quota_total_limit, quota_total_used, window_start, fixed_multiplier, updated_at)
		 VALUES (?, ?, 0, ?, 0, ?, ?, ?)`,
		userID, quota5hLimit, quotaTotalLimit, windowStart, fixedMultiplier, now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert quota: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return &UserWithQuota{
		User: User{
			ID:             userID,
			Username:       username,
			PasswordHash:   passwordHash,
			SubKeyHash:     subKeyHash,
			SubKeyPreview:  subKeyPreview,
			Role:           role,
			Status:         status,
			CreatedAt:      now,
			UpdatedAt:      now,
			ExpiresAt:      expiresAt,
			RouteMode:      routeMode,
			FixedProvider:  fixedProvider,
			MaxBodySize:    maxBodySize,
			MaxConcurrency: maxConcurrency,
		},
		Quota5hLimit:         quota5hLimit,
		Quota5hUsed:          0,
		QuotaTotalLimit:      quotaTotalLimit,
		QuotaTotalUsed:       0,
		QuotaTokenTotalLimit: 0,
		QuotaTokenTotalUsed:  0,
	}, nil
}

// GetUserBySubKeyHash finds a user by their sub_key_hash.
func GetUserBySubKeyHash(db *sql.DB, subKeyHash string) (*User, error) {
	u := &User{}
	err := db.QueryRow(
		`SELECT id, username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at, expires_at, route_mode, fixed_provider, max_body_size, max_concurrency
		 FROM users WHERE sub_key_hash = ? AND status != 'deleted'`, subKeyHash,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.SubKeyHash, &u.SubKeyPreview, &u.Role, &u.Status, &u.CreatedAt, &u.UpdatedAt, &u.ExpiresAt, &u.RouteMode, &u.FixedProvider, &u.MaxBodySize, &u.MaxConcurrency)

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
		`SELECT id, username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at, expires_at, route_mode, fixed_provider, max_body_size, max_concurrency
		 FROM users WHERE id = ?`, id,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.SubKeyHash, &u.SubKeyPreview, &u.Role, &u.Status, &u.CreatedAt, &u.UpdatedAt, &u.ExpiresAt, &u.RouteMode, &u.FixedProvider, &u.MaxBodySize, &u.MaxConcurrency)

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
		`SELECT id, username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at, expires_at, route_mode, fixed_provider, max_body_size, max_concurrency
		 FROM users WHERE username = ?`, username,
	).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.SubKeyHash, &u.SubKeyPreview, &u.Role, &u.Status, &u.CreatedAt, &u.UpdatedAt, &u.ExpiresAt, &u.RouteMode, &u.FixedProvider, &u.MaxBodySize, &u.MaxConcurrency)

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
		`SELECT u.id, u.username, u.sub_key_preview, u.role, u.status, u.created_at, u.updated_at, u.expires_at, u.route_mode, u.fixed_provider, u.max_body_size, u.max_concurrency,
		        q.quota_5h_limit, q.quota_5h_used, q.quota_total_limit, q.quota_total_used, q.quota_token_total_limit, q.quota_token_total_used, q.fixed_multiplier,
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
		var fixedMult sql.NullFloat64
		err := rows.Scan(
			&uwq.ID, &uwq.Username, &uwq.SubKeyPreview, &uwq.Role, &uwq.Status,
			&uwq.CreatedAt, &uwq.UpdatedAt, &uwq.ExpiresAt, &uwq.RouteMode, &uwq.FixedProvider, &uwq.MaxBodySize, &uwq.MaxConcurrency,
			&uwq.Quota5hLimit, &uwq.Quota5hUsed, &uwq.QuotaTotalLimit, &uwq.QuotaTotalUsed,
			&uwq.QuotaTokenTotalLimit, &uwq.QuotaTokenTotalUsed,
			&fixedMult, &uwq.TotalTokens,
		)
		if err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		if fixedMult.Valid {
			uwq.FixedMultiplier = &fixedMult.Float64
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

// ExtendUserExpiry updates a user's expires_at and sets status to active.
// Returns an error if the user is an admin (admins never expire).
func ExtendUserExpiry(db *sql.DB, userID int64, newExpiresAt string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE users SET expires_at = ?, status = 'active', updated_at = ? WHERE id = ?`,
		newExpiresAt, now, userID,
	)
	if err != nil {
		return fmt.Errorf("extend user expiry: %w", err)
	}
	return nil
}

// UpdateUserRoute updates a user's route_mode and fixed_provider fields.
func UpdateUserRoute(db *sql.DB, userID int64, routeMode, fixedProvider string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE users SET route_mode = ?, fixed_provider = ?, updated_at = ? WHERE id = ?`,
		routeMode, fixedProvider, now, userID,
	)
	if err != nil {
		return fmt.Errorf("update user route: %w", err)
	}
	return nil
}

// UpdateUserMaxBodySize updates a user's per-request body cap (bytes).
func UpdateUserMaxBodySize(db *sql.DB, userID int64, maxBodySize int) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE users SET max_body_size = ?, updated_at = ? WHERE id = ?`,
		maxBodySize, now, userID,
	)
	if err != nil {
		return fmt.Errorf("update user max body size: %w", err)
	}
	return nil
}

// UpdateUserMaxConcurrency updates a user's per-user concurrent request cap.
// 0 means unlimited (no cap); a positive N caps simultaneous in-flight requests.
func UpdateUserMaxConcurrency(db *sql.DB, userID int64, maxConcurrency int) error {
	// Guard at the data layer: a negative cap is invalid. The admin handlers
	// also validate this, but rejecting here makes models the single gatekeeper.
	if maxConcurrency < 0 {
		return fmt.Errorf("max_concurrency must be >= 0, got %d", maxConcurrency)
	}
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE users SET max_concurrency = ?, updated_at = ? WHERE id = ?`,
		maxConcurrency, now, userID,
	)
	if err != nil {
		return fmt.Errorf("update user max concurrency: %w", err)
	}
	return nil
}

// GetUsersByFixedProvider returns usernames of all non-deleted users whose
// fixed_provider matches the given provider slug. Used by DeleteProvider to
// prevent deletion of providers that are pinned by users.
func GetUsersByFixedProvider(db *sql.DB, providerSlug string) ([]string, error) {
	rows, err := db.Query(
		`SELECT username FROM users WHERE fixed_provider = ? AND status != 'deleted'`,
		providerSlug,
	)
	if err != nil {
		return nil, fmt.Errorf("get users by fixed provider: %w", err)
	}
	defer rows.Close()

	var usernames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan username: %w", err)
		}
		usernames = append(usernames, name)
	}
	return usernames, rows.Err()
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
