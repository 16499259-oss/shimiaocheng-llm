// Package models_test contains tests for the per-user concurrency cap at the
// data layer (T5/T6 fallback): CreateUser must persist the supplied
// max_concurrency verbatim (0 = unlimited), and UpdateUserMaxConcurrency must
// reject negative values.
package models_test

import (
	"testing"

	"llm_api_gateway/internal/models"
)

// TestCreateUser_MaxConcurrencyPersisted verifies the max_concurrency column
// is written exactly as supplied for the unlimited (0) and capped (>0) cases.
func TestCreateUser_MaxConcurrencyPersisted(t *testing.T) {
	database := newModelsTestDB(t)

	cases := []struct {
		name string
		in   int
	}{
		{"unlimited", 0},
		{"default", 10},
		{"capped", 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := models.CreateUser(database.Conn,
				"mc-"+tc.name, "pw", "skh-"+tc.name, "sk-"+tc.name,
				"user", "active", "", "auto", "",
				100, 1000, nil, 0, tc.in)
			if err != nil {
				t.Fatalf("CreateUser failed: %v", err)
			}
			got, err := models.GetUserByID(database.Conn, u.ID)
			if err != nil {
				t.Fatalf("GetUserByID failed: %v", err)
			}
			if got == nil {
				t.Fatal("user not found after create")
			}
			if got.MaxConcurrency != tc.in {
				t.Fatalf("max_concurrency DB value: want %d, got %d", tc.in, got.MaxConcurrency)
			}
		})
	}
}

// TestUpdateUserMaxConcurrency_NegativeRejected verifies the data-layer guard:
// a negative cap is rejected, while a valid (incl. 0) update persists.
func TestUpdateUserMaxConcurrency_NegativeRejected(t *testing.T) {
	database := newModelsTestDB(t)
	u, err := models.CreateUser(database.Conn,
		"neguser", "pw", "skh-neg", "sk-neg",
		"user", "active", "", "auto", "",
		100, 1000, nil, 0, 10)
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if err := models.UpdateUserMaxConcurrency(database.Conn, u.ID, -1); err == nil {
		t.Fatal("expected error for negative max_concurrency, got nil")
	}
	// A valid update (0 = unlimited) must still work and persist.
	if err := models.UpdateUserMaxConcurrency(database.Conn, u.ID, 0); err != nil {
		t.Fatalf("UpdateUserMaxConcurrency(0) failed: %v", err)
	}
	got, _ := models.GetUserByID(database.Conn, u.ID)
	if got.MaxConcurrency != 0 {
		t.Fatalf("expected persisted 0, got %d", got.MaxConcurrency)
	}
}
