package db

import (
	"path/filepath"
	"testing"

	"llm_api_gateway/internal/models"
)

// openBaseSchemaDB opens a raw SQLite DB and creates ONLY the pre-Token-column
// schema for users/quotas/call_logs (verbatim from migrations.go, minus the two
// token columns, fixed_multiplier and the newer user columns which are added by
// later ALTER steps). This lets us then run the real RunMigrations and assert
// that the one-time backfill fires exactly once.
func openBaseSchemaDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backfill_base.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	stmts := []string{
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
	}
	for i, s := range stmts {
		if _, err := database.Conn.Exec(s); err != nil {
			t.Fatalf("base schema stmt %d: %v", i, err)
		}
	}
	return database
}

// insertBaseUser inserts a user + quota row using the stripped base schema
// (no token columns / fixed_multiplier) and returns the new user ID.
func insertBaseUser(t *testing.T, database *DB, username, subKeyHash, subKeyPreview string) int64 {
	t.Helper()
	res, err := database.Conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at)
		 VALUES (?, 'pw', ?, ?, 'user', 'active', datetime('now'), datetime('now'))`,
		username, subKeyHash, subKeyPreview,
	)
	if err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	if _, err := database.Conn.Exec(
		`INSERT INTO quotas (user_id, quota_5h_limit, quota_5h_used, quota_total_limit, quota_total_used, window_start, updated_at)
		 VALUES (?, 1000, 0, 1000, 0, datetime('now'), datetime('now'))`,
		id,
	); err != nil {
		t.Fatalf("insert quota %s: %v", username, err)
	}
	return id
}

// TestRunMigrations_BackfillsTokenUsageFromCallLogs verifies the one-time
// backfill: when the cumulative Token columns are first introduced, historical
// call_logs (prompt_tokens + completion_tokens) are summed per user into
// quota_token_total_used, while the cap defaults to 0 (unlimited).
func TestRunMigrations_BackfillsTokenUsageFromCallLogs(t *testing.T) {
	database := openBaseSchemaDB(t)

	// Two users so we can confirm the backfill is computed per-user.
	u1 := insertBaseUser(t, database, "bk1", "hash-bk1", "sk-bk1...")
	u2 := insertBaseUser(t, database, "bk2", "hash-bk2", "sk-bk2...")

	// Insert historical call logs: user1 -> 40 tokens, user2 -> 150 tokens.
	insertLog := func(userID int64, prompt, completion int) {
		t.Helper()
		if _, err := database.Conn.Exec(
			`INSERT INTO call_logs (user_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, multiplier_used, status_code)
			 VALUES (?, ?, ?, ?, 1, 1.0, 200)`,
			userID, prompt, completion, prompt+completion,
		); err != nil {
			t.Fatalf("insert call log: %v", err)
		}
	}
	insertLog(u1, 10, 20)  // 30
	insertLog(u1, 5, 5)    // +10 = 40
	insertLog(u2, 100, 50) // 150

	// Run the real migrations -> token columns are absent, so the one-time
	// backfill fires and sums historical call_logs per user.
	if err := RunMigrations(database); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	q1, err := models.GetQuota(database.Conn, u1)
	if err != nil {
		t.Fatalf("get quota1: %v", err)
	}
	if q1.QuotaTokenTotalUsed != 40 {
		t.Fatalf("user1: expected backfilled used == 40, got %d", q1.QuotaTokenTotalUsed)
	}
	if q1.QuotaTokenTotalLimit != 0 {
		t.Fatalf("user1: expected token limit default 0 (unlimited), got %d", q1.QuotaTokenTotalLimit)
	}

	q2, err := models.GetQuota(database.Conn, u2)
	if err != nil {
		t.Fatalf("get quota2: %v", err)
	}
	if q2.QuotaTokenTotalUsed != 150 {
		t.Fatalf("user2: expected backfilled used == 150, got %d", q2.QuotaTokenTotalUsed)
	}

	// Idempotency: a second migration must NOT overwrite the backfilled value
	// (the backfill is guarded by column-existence and runs only once).
	if err := RunMigrations(database); err != nil {
		t.Fatalf("second migrations: %v", err)
	}
	q1b, _ := models.GetQuota(database.Conn, u1)
	if q1b.QuotaTokenTotalUsed != 40 {
		t.Fatalf("user1: backfilled used must survive re-migration, got %d", q1b.QuotaTokenTotalUsed)
	}
	q2b, _ := models.GetQuota(database.Conn, u2)
	if q2b.QuotaTokenTotalUsed != 150 {
		t.Fatalf("user2: backfilled used must survive re-migration, got %d", q2b.QuotaTokenTotalUsed)
	}
}

// TestRunMigrations_BackfillAppliesMultiplier verifies the one-time backfill is
// MULTIPLIER-AWARE: historical call_logs are billed as ceil((prompt+completion)
// * multiplier_used), matching the runtime AddTokenUsage semantics used by the
// 5h/week Token caps. This is the regression guard for the "Token 月总量 must
// include the billing multiplier" requirement.
func TestRunMigrations_BackfillAppliesMultiplier(t *testing.T) {
	database := openBaseSchemaDB(t)

	u1 := insertBaseUser(t, database, "mult1", "hash-mult1", "sk-mult1...")
	u2 := insertBaseUser(t, database, "mult2", "hash-mult2", "sk-mult2...")

	// user1: two calls under a 3x peak window (10+20 -> 30*3=90; 5+5 -> 10*3=30) = 120
	insertLogWithMult := func(userID int64, prompt, completion int, mult float64) {
		t.Helper()
		if _, err := database.Conn.Exec(
			`INSERT INTO call_logs (user_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, multiplier_used, status_code)
			 VALUES (?, ?, ?, ?, 1, ?, 200)`,
			userID, prompt, completion, prompt+completion, mult,
		); err != nil {
			t.Fatalf("insert call log: %v", err)
		}
	}
	insertLogWithMult(u1, 10, 20, 3.0) // ceil(30*3) = 90
	insertLogWithMult(u1, 5, 5, 3.0)   // ceil(10*3) = 30  -> total 120
	// user2: mixed windows: 1x (100+50=150) + 2x (40+10 -> 50*2=100) = 250
	insertLogWithMult(u2, 100, 50, 1.0) // 150
	insertLogWithMult(u2, 40, 10, 2.0)  // ceil(50*2) = 100 -> total 250

	if err := RunMigrations(database); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	q1, err := models.GetQuota(database.Conn, u1)
	if err != nil {
		t.Fatalf("get quota1: %v", err)
	}
	if q1.QuotaTokenTotalUsed != 120 {
		t.Fatalf("user1: expected multiplier-scaled backfill == 120, got %d", q1.QuotaTokenTotalUsed)
	}

	q2, err := models.GetQuota(database.Conn, u2)
	if err != nil {
		t.Fatalf("get quota2: %v", err)
	}
	if q2.QuotaTokenTotalUsed != 250 {
		t.Fatalf("user2: expected multiplier-scaled backfill == 250, got %d", q2.QuotaTokenTotalUsed)
	}
}

// openFullTokenSchemaDB opens a raw SQLite DB that already carries the CURRENT
// production schema for users/quotas/call_logs (every Token column present,
// including fixed_multiplier), but deliberately WITHOUT the one-time recompute
// marker column quota_token_total_mult_v1. This simulates an existing production
// database so we can assert that the multiplier recompute fires exactly once.
func openFullTokenSchemaDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recompute_base.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	stmts := []string{
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
		`CREATE TABLE IF NOT EXISTS quotas (
			id                  INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id             INTEGER NOT NULL UNIQUE,
			quota_5h_limit      INTEGER NOT NULL DEFAULT 100,
			quota_5h_used       INTEGER NOT NULL DEFAULT 0,
			quota_total_limit   INTEGER NOT NULL DEFAULT 10000,
			quota_total_used    INTEGER NOT NULL DEFAULT 0,
			quota_token_total_limit INTEGER NOT NULL DEFAULT 0,
			quota_token_total_used  INTEGER NOT NULL DEFAULT 0,
			quota_token_5h_limit    INTEGER NOT NULL DEFAULT 0,
			quota_token_5h_used     INTEGER NOT NULL DEFAULT 0,
			quota_token_week_limit  INTEGER NOT NULL DEFAULT 0,
			quota_token_week_used   INTEGER NOT NULL DEFAULT 0,
			week_start          TEXT NOT NULL DEFAULT '',
			window_start        TEXT NOT NULL,
			updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
			fixed_multiplier    REAL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
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
	}
	for i, s := range stmts {
		if _, err := database.Conn.Exec(s); err != nil {
			t.Fatalf("full schema stmt %d: %v", i, err)
		}
	}
	return database
}

// TestRunMigrations_RecomputeTokenTotalWithMultiplier simulates an existing
// production DB: the Token columns already exist (so the original raw backfill
// is SKIPPED), but the cumulative used value was seeded with the stale RAW sum
// (no multiplier). Running RunMigrations must trigger the one-time recompute
// (marker column absent), rewriting quota_token_total_used to the
// multiplier-scaled sum — and a second run must NOT touch it again (idempotent).
func TestRunMigrations_RecomputeTokenTotalWithMultiplier(t *testing.T) {
	database := openFullTokenSchemaDB(t)

	u1 := insertBaseUser(t, database, "rec1", "hash-rec1", "sk-rec1...")
	u2 := insertBaseUser(t, database, "rec2", "hash-rec2", "sk-rec2...")

	// Seed logs under a 3x peak window.
	if _, err := database.Conn.Exec(
		`INSERT INTO call_logs (user_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, multiplier_used, status_code)
		 VALUES (?, 10, 20, 30, 1, 3.0, 200)`, u1); err != nil {
		t.Fatalf("insert log u1: %v", err)
	}
	if _, err := database.Conn.Exec(
		`INSERT INTO call_logs (user_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, multiplier_used, status_code)
		 VALUES (?, 100, 50, 150, 1, 3.0, 200)`, u2); err != nil {
		t.Fatalf("insert log u2: %v", err)
	}

	// Pre-seed the STALE raw (un-multiplied) cumulative used, as the old
	// backfill would have written it: u1 = 30, u2 = 150. A user with no logs
	// (u3) is also seeded to prove it is reset to 0.
	u3 := insertBaseUser(t, database, "rec3", "hash-rec3", "sk-rec3...")
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_total_used = ? WHERE user_id = ?`, 30, u1); err != nil {
		t.Fatalf("seed u1 stale: %v", err)
	}
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_total_used = ? WHERE user_id = ?`, 150, u2); err != nil {
		t.Fatalf("seed u2 stale: %v", err)
	}
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_total_used = ? WHERE user_id = ?`, 999, u3); err != nil {
		t.Fatalf("seed u3 stale: %v", err)
	}

	// First deploy: the one-time recompute must fire.
	if err := RunMigrations(database); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	q1, _ := models.GetQuota(database.Conn, u1)
	if q1.QuotaTokenTotalUsed != 90 { // ceil(30*3)
		t.Fatalf("u1: expected recomputed used == 90 (3x), got %d", q1.QuotaTokenTotalUsed)
	}
	q2, _ := models.GetQuota(database.Conn, u2)
	if q2.QuotaTokenTotalUsed != 450 { // ceil(150*3)
		t.Fatalf("u2: expected recomputed used == 450 (3x), got %d", q2.QuotaTokenTotalUsed)
	}
	q3, _ := models.GetQuota(database.Conn, u3)
	if q3.QuotaTokenTotalUsed != 0 { // no logs -> 0
		t.Fatalf("u3: expected recomputed used == 0 (no logs), got %d", q3.QuotaTokenTotalUsed)
	}

	// The marker column must now exist (guards future runs).
	if !columnExists(database, "quotas", "quota_token_total_mult_v1") {
		t.Fatalf("expected marker column quota_token_total_mult_v1 to be created")
	}

	// Change the underlying logs AFTER the first recompute to prove the second
	// run does NOT re-aggregate (it would otherwise pick up the new log).
	if _, err := database.Conn.Exec(
		`INSERT INTO call_logs (user_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, multiplier_used, status_code)
		 VALUES (?, 10, 10, 20, 1, 3.0, 200)`, u1); err != nil {
		t.Fatalf("insert extra log u1: %v", err)
	}

	if err := RunMigrations(database); err != nil {
		t.Fatalf("second migrations: %v", err)
	}
	q1b, _ := models.GetQuota(database.Conn, u1)
	if q1b.QuotaTokenTotalUsed != 90 {
		t.Fatalf("u1: recompute must be one-time; expected 90, got %d", q1b.QuotaTokenTotalUsed)
	}
}
