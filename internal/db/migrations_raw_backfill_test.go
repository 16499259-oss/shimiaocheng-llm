package db

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

// TestBackfillRawTotalTokens verifies that the historical backfill of
// call_logs.raw_total_tokens recomputes each row as prompt_tokens +
// completion_tokens, matching the InsertCallLog invariant for new rows.
func TestBackfillRawTotalTokens(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	// Minimal call_logs schema with only the columns the backfill touches.
	if _, err := db.Exec(`CREATE TABLE call_logs (
		id INTEGER PRIMARY KEY,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		raw_total_tokens INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create table failed: %v", err)
	}

	// Historical rows: raw_total_tokens deliberately 0 (pre-deploy default).
	if _, err := db.Exec(`INSERT INTO call_logs (id, prompt_tokens, completion_tokens, raw_total_tokens) VALUES
		(1, 100, 50, 0),
		(2, 0, 0, 0),
		(3, 200, 30, 0)`); err != nil {
		t.Fatalf("insert rows failed: %v", err)
	}

	// Execute the backfill.
	if _, err := db.Exec(backfillRawTotalTokensSQL); err != nil {
		t.Fatalf("backfill exec failed: %v", err)
	}

	want := map[int]int{
		1: 150, // 100 + 50
		2: 0,   // 0 + 0  (zero-token call)
		3: 230, // 200 + 30
	}
	for id, expected := range want {
		var raw, prompt, completion int
		row := db.QueryRow(`SELECT raw_total_tokens, prompt_tokens, completion_tokens FROM call_logs WHERE id = ?`, id)
		if err := row.Scan(&raw, &prompt, &completion); err != nil {
			t.Fatalf("query row id=%d failed: %v", id, err)
		}
		if raw != expected {
			t.Errorf("row id=%d: raw_total_tokens = %d, want %d (prompt=%d + completion=%d)",
				id, raw, expected, prompt, completion)
		}
	}
}

// TestBackfillRawTotalTokens_Idempotent ensures re-running the backfill does
// not change the already-correct values (the UPDATE is a full in-place SET).
func TestBackfillRawTotalTokens_Idempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec(`CREATE TABLE call_logs (
		id INTEGER PRIMARY KEY,
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		raw_total_tokens INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("create table failed: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO call_logs (id, prompt_tokens, completion_tokens, raw_total_tokens) VALUES
		(1, 100, 50, 0),
		(2, 0, 0, 0),
		(3, 200, 30, 0)`); err != nil {
		t.Fatalf("insert rows failed: %v", err)
	}

	// First run.
	if _, err := db.Exec(backfillRawTotalTokensSQL); err != nil {
		t.Fatalf("backfill exec (1st) failed: %v", err)
	}

	// Second run must be a no-op with respect to the resulting values.
	if _, err := db.Exec(backfillRawTotalTokensSQL); err != nil {
		t.Fatalf("backfill exec (2nd) failed: %v", err)
	}

	want := map[int]int{
		1: 150,
		2: 0,
		3: 230,
	}
	for id, expected := range want {
		var raw int
		row := db.QueryRow(`SELECT raw_total_tokens FROM call_logs WHERE id = ?`, id)
		if err := row.Scan(&raw); err != nil {
			t.Fatalf("query row id=%d failed: %v", id, err)
		}
		if raw != expected {
			t.Errorf("row id=%d after re-run: raw_total_tokens = %d, want %d", id, raw, expected)
		}
	}
}
