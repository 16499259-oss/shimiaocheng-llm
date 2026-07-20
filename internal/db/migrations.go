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

		// NOTE: We deliberately do NOT seed a default routing rule. A hardcoded
		// rule pointing at a provider that may not exist (e.g. "openai") would,
		// under the strict no-fallback invariant (router/selector.go), cause a
		// 503 for every auto-routed request during its time window on a fresh
		// deploy that only configured a single real provider. With no rules
		// present, ResolveProvider safely falls back to the configured default
		// provider. Admins define time-based routing explicitly via the panel.

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
	// Add raw_total_tokens column to call_logs (idempotent: only when missing).
	// This stores the UNMULTIPLIED raw token sum (prompt_tokens +
	// completion_tokens) written at call-log insert time, so the "raw (no
	// multiplier) token statistics" can be displayed side-by-side with the
	// multiplier-inflated CallStats.Tokens without ever reverse-deriving it from
	// the multiplier. Distinct from total_tokens, which is the provider-reported
	// total (may include provider-specific extras such as reasoning tokens).
	if !columnExists(conn, "call_logs", "raw_total_tokens") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE call_logs ADD COLUMN raw_total_tokens INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter call_logs.raw_total_tokens failed: %w", err)
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
	// creation, backfill quota_token_total_used from historical call_logs as the
	// MULTIPLIER-AWARE sum (ceil((prompt+completion) * multiplier_used) per row)
	// so existing users' cumulative Token usage is preserved and consistent with
	// actual billing under the new quota-unlimited-by-default model. Idempotent:
	// only runs when the column does not yet exist.
	if !columnExists(conn, "quotas", "quota_token_total_limit") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_total_limit INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.quota_token_total_limit failed: %w", err)
		}
		// Backfill quota_token_total_used from historical call_logs, but
		// MULTIPLIER-AWARE: each row is billed as ceil((prompt_tokens +
		// completion_tokens) * multiplier_used), exactly matching the runtime
		// semantics of AddTokenUsage (which the 5h/week Token caps already use).
		// This keeps the cumulative Token cap ("Token 月总量") consistent with
		// the 5h/week caps and with actual billing. The multiplier-scaled sum is
		// computed by backfillQuotaTokenTotalUsed (set-based SQL below);
		// call_logs.multiplier_used defaults to 1.0 for pre-multiplier history,
		// so legacy rows are billed at 1x (matching the period when no
		// multiplier was in effect).
		if err := backfillQuotaTokenTotalUsed(conn); err != nil {
			return fmt.Errorf("migration backfill quotas.quota_token_total_used failed: %w", err)
		}
	}

	// ── One-time recompute of quota_token_total_used to include the billing
	// multiplier (2026-08-18) ──
// The cumulative-Token backfill that already ran on production (when
// quota_token_total_limit was first created, on an earlier build) summed RAW
// prompt+completion from call_logs (no multiplier). Going forward,
	// AddTokenUsage accrues the multiplier-scaled billed delta into
	// quota_token_total_used — exactly like the 5h/week Token caps — so the raw
	// backfill left existing production rows under-counted versus actual billing.
	// This one-time step recomputes quota_token_total_used for every user as the
	// multiplier-scaled sum of historical call_logs, in lock-step with the
	// 5h/week caps. It is guarded by the presence of the marker column
	// quota_token_total_mult_v1 so it runs exactly once (on the first deploy that
	// introduces this migration); subsequent deploys skip it (column already
	// present). The recompute is a full SET from call_logs (not an increment), so
	// it safely corrects a column that already holds a raw or partially-billed
	// value. The lead triggers it simply by deploying this build.
	if !columnExists(conn, "quotas", "quota_token_total_mult_v1") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_total_mult_v1 INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration add marker quotas.quota_token_total_mult_v1 failed: %w", err)
		}
		if err := backfillQuotaTokenTotalUsed(conn); err != nil {
			return fmt.Errorf("migration recompute quotas.quota_token_total_used (multiplier) failed: %w", err)
		}
	}

	// ── Token window columns (5h + weekly rolling) for the dual Token soft gate ──
	// quota_token_5h_limit / quota_token_5h_used: Token cap inside the 5h count
	//   window (reuses the existing 5h window reset path via Reset5hQuota /
	//   CompensateQuotaReset; 0 = unlimited).
	// quota_token_week_limit / quota_token_week_used: Token cap inside a rolling-7-day
	//   (lazy-reset) bucket. The reset is performed inside AtomicDeductQuota's
	//   atomic UPDATE (CASE on week_start), so no separate migration backfill is
	//   needed — and that is precisely why the gate does NOT accumulate token
	//   counts at request time (only the response-time AddTokenUsage does).
	// week_start: the rolling-7-day bucket anchor (RFC3339/UTC default; newly
	//   created users write local now). The gate resets it when it is older than
	//   7 days. Each column is guarded by columnExists so re-runs are idempotent.
	if !columnExists(conn, "quotas", "quota_token_5h_limit") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_5h_limit INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.quota_token_5h_limit failed: %w", err)
		}
	}
	if !columnExists(conn, "quotas", "quota_token_5h_used") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_5h_used INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.quota_token_5h_used failed: %w", err)
		}
	}
	if !columnExists(conn, "quotas", "quota_token_week_limit") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_week_limit INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.quota_token_week_limit failed: %w", err)
		}
	}
	if !columnExists(conn, "quotas", "quota_token_week_used") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN quota_token_week_used INTEGER NOT NULL DEFAULT 0`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.quota_token_week_used failed: %w", err)
		}
	}
	if !columnExists(conn, "quotas", "week_start") {
		// NOTE: SQLite forbids ALTER TABLE ADD COLUMN with a NON-CONSTANT
		// default (e.g. datetime('now')) once the table already contains rows,
		// raising "Cannot add a column with non-constant default". To stay
		// idempotent and compatible with non-empty databases we add the column
		// with a constant empty default and then backfill existing rows with
		// the current UTC time as the rolling-7-day bucket anchor. New rows
		// (via CreateUser) write time.Now() directly, so they never hit this
		// empty default. The gate (AtomicDeductQuota) treats an empty week_start
		// as "expired" ('' < any ISO timestamp), which is the safe first-touch
		// behaviour for legacy rows.
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN week_start TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.week_start failed: %w", err)
		}
		// Backfill existing rows; idempotent (only touches empty defaults).
		if _, err := conn.Conn.Exec(
			`UPDATE quotas SET week_start = (datetime('now')) WHERE week_start = ''`,
		); err != nil {
			return fmt.Errorf("migration backfill quotas.week_start failed: %w", err)
		}
	}
	// ── Month-window anchor (rolling 30-day Token bucket) ──
	// month_start: the rolling-30-day bucket anchor for the cumulative Token cap
	// (quota_token_total_used). Added symmetrically to week_start so the gate's
	// lazy-reset (CASE on month_start, cutoff = now-30d) can reset the
	// monthly Token bucket inside the SAME atomic UPDATE — exactly like the
	// weekly bucket already does for week_start. Same idempotent ALTER-with
	// constant-empty-default + backfill pattern as week_start: SQLite forbids a
	// non-constant default on a non-empty table, so we default to '' and
	// backfill existing rows with the current UTC time. The gate treats an
	// empty month_start as "expired" ('' < any ISO timestamp), the safe
	// first-touch behaviour for legacy rows. New rows (via CreateUser) write
	// time.Now() directly. Existing production rows are NOT zeroed — only the
	// window anchor is set — so their already-accumulated
	// quota_token_total_used is preserved (decision R2).
	if !columnExists(conn, "quotas", "month_start") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE quotas ADD COLUMN month_start TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migration alter quotas.month_start failed: %w", err)
		}
		// Backfill existing rows; idempotent (only touches empty defaults).
		if _, err := conn.Conn.Exec(
			`UPDATE quotas SET month_start = (datetime('now')) WHERE month_start = ''`,
		); err != nil {
			return fmt.Errorf("migration backfill quotas.month_start failed: %w", err)
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

	// ── Provider cycle start date (上游额度 V3: 固定30天周期) ──
	// cycle_start_date marks the anchor date for fixed 30-day billing cycles.
	// New providers default to today; existing rows are backfilled from
	// DATE(created_at). The column is a "2006-01-02" DATE string.
	if !columnExists(conn, "providers", "cycle_start_date") {
		if _, err := conn.Conn.Exec(
			`ALTER TABLE providers ADD COLUMN cycle_start_date TEXT NOT NULL DEFAULT ''`,
		); err != nil {
			return fmt.Errorf("migration alter providers.cycle_start_date failed: %w", err)
		}
		// Backfill existing rows: cycle_start_date = DATE(created_at).
		if _, err := conn.Conn.Exec(
			`UPDATE providers SET cycle_start_date = DATE(created_at) WHERE cycle_start_date = ''`,
		); err != nil {
			return fmt.Errorf("migration backfill providers.cycle_start_date failed: %w", err)
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

// backfillQuotaTokenTotalUsed recomputes quotas.quota_token_total_used for every
// user as the multiplier-scaled sum of historical call_logs, mirroring the
// runtime billing semantics of models.AddTokenUsage: each request is billed
// ceil((prompt_tokens + completion_tokens) * multiplier_used) Tokens. This keeps
// the cumulative Token cap ("Token 月总量") consistent with the 5h/week Token
// caps, which are already accrued with the same multiplier.
//
// It is a single set-based UPDATE (no app-side iteration), so it scales to large
// production call_logs tables. We use the CAST(x + 0.999999 AS INTEGER) idiom
// for ceil because modernc.org/sqlite does not guarantee a CEIL math function;
// the +0.999999 offset makes the integer truncation equal ceil(x) for every
// non-negative billed x. call_logs.multiplier_used is NOT NULL DEFAULT 1.0, so
// pre-multiplier history is billed at 1x — matching the period when no
// multiplier was in effect. COALESCE(...,0) keeps users with no call_logs at 0.
//
// This is the SAME statement documented (and regression-tested) in
// internal/models/token_total_recalc_test.go, which the lead may also run
// manually as a belt-and-suspenders check. Because it is a full SET from the
// authoritative call_logs source, it is idempotent and safe to call on a column
// that already holds a raw or partially-billed value — e.g. the one-time
// migration backfill and the one-time multiplier recompute both delegate to it.
func backfillQuotaTokenTotalUsed(conn *DB) error {
	if _, err := conn.Conn.Exec(
		`UPDATE quotas SET quota_token_total_used = COALESCE(
			(SELECT SUM(CAST((prompt_tokens + completion_tokens) * multiplier_used + 0.999999 AS INTEGER))
			 FROM call_logs WHERE call_logs.user_id = quotas.user_id), 0)`,
	); err != nil {
		return fmt.Errorf("recompute quota_token_total_used (multiplier): %w", err)
	}
	return nil
}
