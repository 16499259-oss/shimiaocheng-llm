package db

import (
	"path/filepath"
	"testing"
)

func openMigrated(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mig.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := RunMigrations(database); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func tableExists(t *testing.T, database *DB, name string) bool {
	t.Helper()
	var cnt int
	if err := database.Conn.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&cnt); err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	return cnt == 1
}

func TestRunMigrations_CreatesAllTables(t *testing.T) {
	database := openMigrated(t)
	want := []string{
		"users", "quotas", "call_logs", "admin_sessions",
		"time_multipliers", "provider_routing_rules", "providers",
		"model_mappings", "audit_logs",
	}
	for _, tbl := range want {
		if !tableExists(t, database, tbl) {
			t.Errorf("expected table %q to exist after migrations", tbl)
		}
	}
}

func TestRunMigrations_SeedsDefaultRoutingRule(t *testing.T) {
	database := openMigrated(t)
	var cnt int
	if err := database.Conn.QueryRow(`SELECT COUNT(*) FROM provider_routing_rules`).Scan(&cnt); err != nil {
		t.Fatalf("count rules: %v", err)
	}
	if cnt != 1 {
		t.Errorf("expected exactly 1 seeded routing rule, got %d", cnt)
	}
	var pid string
	if err := database.Conn.QueryRow(`SELECT provider_id FROM provider_routing_rules`).Scan(&pid); err != nil {
		t.Fatalf("select rule: %v", err)
	}
	if pid != "openai" {
		t.Errorf("seeded rule provider_id = %q, want openai", pid)
	}
}

func TestRunMigrations_AddsExpectedColumns(t *testing.T) {
	database := openMigrated(t)
	checks := []struct {
		table, column string
	}{
		{"call_logs", "provider_id"},
		{"users", "expires_at"},
		{"users", "route_mode"},
		{"users", "fixed_provider"},
		{"quotas", "fixed_multiplier"},
	}
	for _, c := range checks {
		if !columnExists(database, c.table, c.column) {
			t.Errorf("expected column %s.%s to exist", c.table, c.column)
		}
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mig_idem.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()
	if err := RunMigrations(database); err != nil {
		t.Fatalf("first RunMigrations: %v", err)
	}
	if err := RunMigrations(database); err != nil {
		t.Fatalf("second RunMigrations should be idempotent, got: %v", err)
	}
	// Idempotency invariant: all 9 domain tables still present, and the seed
	// routing rule is NOT duplicated (still exactly 1 row).
	for _, tbl := range []string{
		"users", "quotas", "call_logs", "admin_sessions",
		"time_multipliers", "provider_routing_rules", "providers",
		"model_mappings", "audit_logs",
	} {
		if !tableExists(t, database, tbl) {
			t.Errorf("table %q missing after re-migration", tbl)
		}
	}
	var rules int
	if err := database.Conn.QueryRow(`SELECT COUNT(*) FROM provider_routing_rules`).Scan(&rules); err != nil {
		t.Fatalf("count rules: %v", err)
	}
	if rules != 1 {
		t.Errorf("expected 1 seed routing rule after re-migration, got %d (duplicated?)", rules)
	}
}

func TestRunMigrations_UniqueConstraints(t *testing.T) {
	database := openMigrated(t)
	insert := `INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at)
	           VALUES (?, 'phash', ?, 'sk-prev', 'user', 'active', datetime('now'), datetime('now'))`
	if _, err := database.Conn.Exec(insert, "alice", "hash1"); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	// Duplicate username must violate the UNIQUE constraint.
	if _, err := database.Conn.Exec(insert, "alice", "hash2"); err == nil {
		t.Error("expected UNIQUE(username) violation on duplicate insert, got nil")
	}
	// Duplicate sub_key_hash must also violate UNIQUE.
	if _, err := database.Conn.Exec(insert, "bob", "hash1"); err == nil {
		t.Error("expected UNIQUE(sub_key_hash) violation on duplicate insert, got nil")
	}
}
