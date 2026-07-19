package db

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

// RunMigrations creates all tables if they do not exist.
func RunMigrations(conn *DB) error {
	migrations := []string{
		// users table
		`CREATE TABLE IF NOT EXISTS users (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			username        TEXT    NOT NULL UNIQUE,
			password_hash   TEXT    NOT NULL,
			sub_key_hash    TEXT    NOT NULL UNIQUE,
			sub_key_preview TEXT    NOT NULL,
			role            TEXT    NOT NULL DEFAULT 'user',
			status          TEXT    NOT NULL DEFAULT 'active',
			created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
			updated_at      TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE INDEX IF NOT EXISTS idx_users_sub_key_hash ON users(sub_key_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_users_status ON users(status)`,

		// quotas table
		`CREATE TABLE IF NOT EXISTS quotas (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id           INTEGER NOT NULL UNIQUE,
			quota_5h_limit    INTEGER NOT NULL DEFAULT 100,
			quota_5h_used     INTEGER NOT NULL DEFAULT 0,
			quota_total_limit INTEGER NOT NULL DEFAULT 10000,
			quota_total_used  INTEGER NOT NULL DEFAULT 0,
			window_start      TEXT    NOT NULL,
			updated_at        TEXT    NOT NULL DEFAULT (datetime('now')),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,

		`CREATE INDEX IF NOT EXISTS idx_quotas_window_start ON quotas(window_start)`,

		// call_logs table
		`CREATE TABLE IF NOT EXISTS call_logs (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id           INTEGER NOT NULL,
			model             TEXT    NOT NULL DEFAULT 'glm-5.2',
			prompt_tokens     INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			total_tokens      INTEGER NOT NULL DEFAULT 0,
			effective_calls   INTEGER NOT NULL DEFAULT 1,
			multiplier_used   REAL    NOT NULL DEFAULT 1.0,
			status_code       INTEGER NOT NULL,
			latency_ms        INTEGER NOT NULL DEFAULT 0,
			error_msg         TEXT,
			created_at        TEXT    NOT NULL DEFAULT (datetime('now')),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,

		`CREATE INDEX IF NOT EXISTS idx_call_logs_user_id ON call_logs(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_call_logs_created_at ON call_logs(created_at)`,
		// Composite index (user_id, created_at) for the global call-stats panel.
		// user_id exists in the call_logs CREATE TABLE above, so this is safe here.
		// The provider_id-based composite index is created further below, after
		// the provider_id column is added (it is not part of the CREATE TABLE).
		`CREATE INDEX IF NOT EXISTS idx_call_logs_user_created ON call_logs(user_id, created_at)`,

		// admin_sessions table
		`CREATE TABLE IF NOT EXISTS admin_sessions (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			session_token   TEXT    NOT NULL UNIQUE,
			created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
			expires_at      TEXT    NOT NULL
		)`,

		`CREATE INDEX IF NOT EXISTS idx_admin_sessions_token ON admin_sessions(session_token)`,

		// time_multipliers table
		`CREATE TABLE IF NOT EXISTS time_multipliers (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			start_time    TEXT    NOT NULL,
			end_time      TEXT    NOT NULL,
			multiplier    REAL    NOT NULL DEFAULT 1.0,
			days_of_week  TEXT    NOT NULL DEFAULT '*',
			enabled       INTEGER NOT NULL DEFAULT 1,
			created_at    TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE INDEX IF NOT EXISTS idx_time_multipliers_enabled ON time_multipliers(enabled)`,

		// provider_routing_rules table — drives multi-upstream time-based routing.
		// Schema mirrors time_multipliers for operational familiarity.
		// default_provider_id is reserved for future per-rule fallback (P2); the
		// global default is always taken from config.Providers[IsDefault].
		`CREATE TABLE IF NOT EXISTS provider_routing_rules (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			provider_id         TEXT    NOT NULL,
			start_time          TEXT    NOT NULL,
			end_time            TEXT    NOT NULL,
			days_of_week        TEXT    NOT NULL DEFAULT '*',
			timezone            TEXT    NOT NULL DEFAULT 'Asia/Shanghai',
			enabled             INTEGER NOT NULL DEFAULT 1,
			default_provider_id TEXT
		)`,

		`CREATE INDEX IF NOT EXISTS idx_provider_routing_rules_enabled ON provider_routing_rules(enabled)`,

		// Seed a default rule (14:00-18:01 -> openai) only when the table is empty.
		`INSERT INTO provider_routing_rules (provider_id, start_time, end_time, days_of_week, timezone, enabled)
		 SELECT 'openai', '14:00', '18:01', '*', 'Asia/Shanghai', 1
		 WHERE NOT EXISTS (SELECT 1 FROM provider_routing_rules)`,

		// ── providers table: upstream LLM providers ──
		`CREATE TABLE IF NOT EXISTS providers (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			name           TEXT    NOT NULL,
			slug           TEXT    NOT NULL UNIQUE,
			endpoint       TEXT    NOT NULL,
			encrypted_key  BLOB    NOT NULL,
			is_default     INTEGER NOT NULL DEFAULT 0,
			enabled        INTEGER NOT NULL DEFAULT 1,
			created_at     TEXT    NOT NULL DEFAULT (datetime('now')),
			updated_at     TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE UNIQUE INDEX IF NOT EXISTS idx_providers_slug ON providers(slug)`,
		`CREATE INDEX IF NOT EXISTS idx_providers_enabled ON providers(enabled)`,

		// ── model_mappings table: external model → provider real model ──
		`CREATE TABLE IF NOT EXISTS model_mappings (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			external    TEXT    NOT NULL,
			provider_id TEXT    NOT NULL,
			real_model  TEXT    NOT NULL,
			created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
			FOREIGN KEY (provider_id) REFERENCES providers(slug) ON DELETE CASCADE
		)`,

		`CREATE UNIQUE INDEX IF NOT EXISTS idx_model_mappings_ext_prov
			ON model_mappings(external, provider_id)`,

		// ── audit_logs table: operation audit trail ──
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			action      TEXT    NOT NULL,
			target_type TEXT    NOT NULL,
			target_id   TEXT    NOT NULL,
			detail      TEXT    NOT NULL DEFAULT '',
			created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,

		`CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_logs_action ON audit_logs(action)`,
	}

	for i, m := range migrations {
		if _, err := conn.Conn.Exec(m); err != nil {
			return fmt.Errorf("migration %d failed: %w", i, err)
		}
	}

	// Add provider_id column to call_logs (idempotent: only when missing).
	if !columnExists(conn, "call_logs", "provider_id") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE call_logs ADD COLUMN provider_id TEXT NOT NULL DEFAULT 'zhipu'`,
		); err != nil {
			return fmt.Errorf("migration alter call_logs.provider_id failed: %w", err)
		}
	}
	// Composite index (provider_id, created_at) for the global call-stats panel.
	// Must run AFTER the provider_id column exists (it is added above, not in the
	// CREATE TABLE). Idempotent so re-runs are safe.
	if _, err := conn.Conn.Exec(
		`CREATE INDEX IF NOT EXISTS idx_call_logs_provider_created ON call_logs(provider_id, created_at)`,
	); err != nil {
		return fmt.Errorf("migration create idx_call_logs_provider_created failed: %w", err)
	}

	// Add expires_at column to users (idempotent).
	if !columnExists(conn, "users", "expires_at") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE users ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migration alter users.expires_at failed: %w", err)
		}
	}

	// Add route_mode column to users (idempotent).
	if !columnExists(conn, "users", "route_mode") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE users ADD COLUMN route_mode TEXT NOT NULL DEFAULT 'auto'`,
		); err != nil {
			return fmt.Errorf("migration alter users.route_mode failed: %w", err)
		}
	}

	// Add fixed_provider column to users (idempotent).
	if !columnExists(conn, "users", "fixed_provider") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE users ADD COLUMN fixed_provider TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migration alter users.fixed_provider failed: %w", err)
		}
	}

	// Add max_body_size column to users (idempotent). Per-user request body cap
	// in bytes; default 1MB. Enforced by Go after auth (nginx only provides a
	// high global ceiling so per-user caps can be applied downstream).
	if !columnExists(conn, "users", "max_body_size") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE users ADD COLUMN max_body_size INTEGER NOT NULL DEFAULT 1048576`,
		); err != nil {
			return fmt.Errorf("migration alter users.max_body_size failed: %w", err)
		}
	}

	// Add max_concurrency column to users (idempotent). Per-user cap on the
	// number of simultaneous in-flight requests; 0 means unlimited. Enforced in
	// proxy.Handler via an atomic per-user counter so a single misbehaving
	// client cannot exhaust the shared upstream rate-limit budget (all sub-users
	// funnel through the gateway's single upstream credential). Default 10.
	if !columnExists(conn, "users", "max_concurrency") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE users ADD COLUMN max_concurrency INTEGER NOT NULL DEFAULT 10`,
		); err != nil {
			return fmt.Errorf("migration alter users.max_concurrency failed: %w", err)
		}
	}

	// Add fixed_multiplier column to quotas (idempotent).
	if !columnExists(conn, "quotas", "fixed_multiplier") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN fixed_multiplier REAL`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.fixed_multiplier failed: %w", err)
		}
	}

	// Add quota_token_total_used column to quotas (idempotent). Added BEFORE
	// quota_token_total_limit so the one-time backfill below (run when the
	// limit column is first created) can safely reference this column.
	if !columnExists(conn, "quotas", "quota_token_total_used") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_total_used INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.quota_token_total_used failed: %w", err)
		}
	}

	// Add priority column to provider_routing_rules (idempotent). Drives
	// routing precedence: when multiple enabled rules' windows overlap, the
	// higher-priority rule wins; ties are broken by narrower window then id.
	// Default 0 preserves legacy first-match-by-id behaviour for existing rows.
	if !columnExists(conn, "provider_routing_rules", "priority") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE provider_routing_rules ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter provider_routing_rules.priority failed: %w", err)
		}
	}

	// Add quota_token_total_limit column to quotas (idempotent). On first
	// creation, backfill quota_token_total_used from historical call_logs
	// (SUM(prompt_tokens + completion_tokens) per user) — a one-time migration
	// so existing users' cumulative Token usage is preserved under the new
	// quota-unlimited-by-default model. Idempotent: only runs when the column
	// does not yet exist.
	if !columnExists(conn, "quotas", "quota_token_total_limit") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_total_limit INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.quota_token_total_limit failed: %w", err)
		}
		// Backfill only where not already incremented (defensive against a
		// partial prior run). quota_token_total_used now exists (added above).
		if _, err := conn.Conn.Exec(
			`UPDATE quotas SET quota_token_total_used = COALESCE(
				(SELECT SUM(prompt_tokens + completion_tokens) FROM call_logs WHERE call_logs.user_id = quotas.user_id), 0
			) WHERE quota_token_total_used = 0`,
		); err != nil {
			return fmt.Errorf("migration backfill quotas.quota_token_total_used failed: %w", err)
		}
	}

	// ── One-time data fix (2026-08-16) ──
	// User "大仙撸车" (id=39) was mistakenly set to permanent (expires_at='').
	// Correct to 2026-08-15 (Beijing time, end of day). Guarded so it only
	// applies while still permanent; idempotent and safe across restarts.
	if _, err := conn.Conn.Exec(
		`UPDATE users SET expires_at = ?, updated_at = ? WHERE username = ? AND (expires_at IS NULL OR expires_at = '')`,
		"2026-08-15T23:59:59+08:00", time.Now().Format(time.RFC3339), "大仙撸车",
	); err != nil {
		return fmt.Errorf("data fix daxian expiry: %w", err)
	}

	// ── Passthrough / MCP support (idempotent) ──
	// Per-provider flags enabling wildcard passthrough (e.g. MCP / arbitrary path):
	//   allow_passthrough : whether this provider may be used as a passthrough target.
	//   auth_header       : upstream auth header name (default "Authorization").
	//   auth_scheme       : "bearer" | "x-api-key" | "none" (default "bearer").
	//   extra_headers     : JSON object of static extra headers (e.g. anthropic-version).
	// All default to "chat-compatible" values so existing providers keep working.
	if !columnExists(conn, "providers", "allow_passthrough") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN allow_passthrough INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter providers.allow_passthrough failed: %w", err)
		}
	}
	if !columnExists(conn, "providers", "auth_header") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN auth_header TEXT NOT NULL DEFAULT 'Authorization'`,
		); err != nil {
			return fmt.Errorf("migration alter providers.auth_header failed: %w", err)
		}
	}
	if !columnExists(conn, "providers", "auth_scheme") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN auth_scheme TEXT NOT NULL DEFAULT 'bearer'`,
		); err != nil {
			return fmt.Errorf("migration alter providers.auth_scheme failed: %w", err)
		}
	}
	if !columnExists(conn, "providers", "extra_headers") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN extra_headers TEXT NOT NULL DEFAULT '{}'`,
		); err != nil {
			return fmt.Errorf("migration alter providers.extra_headers failed: %w", err)
		}
	}

	// ── Provider monthly quota (idempotent) ──
	// 0 = unlimited / no limit. Token limit may exceed 2^31 (hundreds of
	// millions) so the columns are INTEGER (64-bit in SQLite, fine for int64).
	if !columnExists(conn, "providers", "monthly_token_limit") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN monthly_token_limit INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter providers.monthly_token_limit failed: %w", err)
		}
	}
	if !columnExists(conn, "providers", "monthly_call_limit") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN monthly_call_limit INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter providers.monthly_call_limit failed: %w", err)
		}
	}

	// ── Provider low-balance thresholds (idempotent) ──
	// Per-provider override of the global default low-balance threshold,
	// expressed as a remaining ratio (0.10 = flag when < 10% remaining).
	// token 与 call-count 各自独立。0 = 继承全局默认（见 config.ProviderQuota）。
	if !columnExists(conn, "providers", "monthly_token_low_ratio") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN monthly_token_low_ratio REAL NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter providers.monthly_token_low_ratio failed: %w", err)
		}
	}
	if !columnExists(conn, "providers", "monthly_call_low_ratio") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN monthly_call_low_ratio REAL NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter providers.monthly_call_low_ratio failed: %w", err)
		}
	}

	log.Println("Database migrations completed successfully")
	return nil
}

// columnExists reports whether a column exists in the given table.
// Used to make schema alterations idempotent across restarts.
func columnExists(conn *DB, table, column string) bool {
	rows, err := conn.Conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}
