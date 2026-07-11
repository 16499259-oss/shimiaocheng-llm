package db

import (
	"database/sql"
	"fmt"
	"log"
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
