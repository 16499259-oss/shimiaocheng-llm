// Package models_test contains tests for the models package, focusing on the
// security-hardening behavior that deleted users must never be returned by lookups.
package models_test

import (
	"os"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
)

// newModelsTestDB opens an isolated temp-file SQLite database (zero CGO, modernc driver),
// runs migrations, and registers cleanup so the file is removed automatically.
func newModelsTestDB(t *testing.T) *db.DB {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "models_test_*.db")
	if err != nil {
		t.Fatalf("failed to create temp db file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close temp db file: %v", err)
	}

	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})
	return database
}

// TestGetUserBySubKeyHash_ExcludesDeleted verifies the security-hardening filter:
// a user whose status is "deleted" must NOT be retrievable by sub_key_hash, while an
// active user with the same lookup must be returned normally.
func TestGetUserBySubKeyHash_ExcludesDeleted(t *testing.T) {
	database := newModelsTestDB(t)

	// Active user: create with a known sub-key, then hash it for lookup.
	activeSubKey := "sk-active-0123456789abcdef0123456789abcdef"
	activeHash := auth.HashSubKey(activeSubKey)
	_, err := models.CreateUser(
		database.Conn,
		"activeuser", "pw-hash", activeHash, "sk-activ...",
		"user", "active",
		100, 1000,
	)
	if err != nil {
		t.Fatalf("CreateUser(active) failed: %v", err)
	}

	// To-be-deleted user: created active, then transitioned to "deleted".
	deletedSubKey := "sk-deleted-0123456789abcdef0123456789abc"
	deletedHash := auth.HashSubKey(deletedSubKey)
	deleted, err := models.CreateUser(
		database.Conn,
		"deluser", "pw-hash", deletedHash, "sk-delet...",
		"user", "active",
		100, 1000,
	)
	if err != nil {
		t.Fatalf("CreateUser(deleted) failed: %v", err)
	}
	if err := models.UpdateUserStatus(database.Conn, deleted.ID, "deleted"); err != nil {
		t.Fatalf("UpdateUserStatus(deleted) failed: %v", err)
	}

	// 1) Deleted user must be invisible to the lookup (SQL filters status != 'deleted').
	gotDeleted, err := models.GetUserBySubKeyHash(database.Conn, deletedHash)
	if err != nil {
		t.Fatalf("GetUserBySubKeyHash(deleted) returned error: %v", err)
	}
	if gotDeleted != nil {
		t.Fatalf("expected nil for deleted user, but got: %+v", gotDeleted)
	}

	// 2) Active user must still be retrievable with the correct username.
	gotActive, err := models.GetUserBySubKeyHash(database.Conn, activeHash)
	if err != nil {
		t.Fatalf("GetUserBySubKeyHash(active) returned error: %v", err)
	}
	if gotActive == nil {
		t.Fatalf("expected active user to be found, got nil")
	}
	if gotActive.Username != "activeuser" {
		t.Fatalf("expected username %q, got %q", "activeuser", gotActive.Username)
	}
	if gotActive.Status != "active" {
		t.Fatalf("expected status %q, got %q", "active", gotActive.Status)
	}

	// Sanity: the deleted user still exists in the table (it was only filtered out).
	byID, err := models.GetUserByID(database.Conn, deleted.ID)
	if err != nil {
		t.Fatalf("GetUserByID(deleted) returned error: %v", err)
	}
	if byID == nil {
		t.Fatalf("deleted user should still exist by ID (test setup issue)")
	}
}
