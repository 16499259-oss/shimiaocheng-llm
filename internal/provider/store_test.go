package provider

import (
	"database/sql"
	"os"
	"testing"

	_ "modernc.org/sqlite"

	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/security"
)

// testStore creates an in-memory SQLite database and returns a ProviderStore.
func testStore(t *testing.T) (*ProviderStore, *sql.DB) {
	t.Helper()

	db, err := sql.Open("sqlite", ":memory:?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	// Run migrations for the tables we need.
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			username        TEXT    NOT NULL UNIQUE,
			password_hash   TEXT    NOT NULL,
			sub_key_hash    TEXT    NOT NULL UNIQUE,
			sub_key_preview TEXT    NOT NULL,
			role            TEXT    NOT NULL DEFAULT 'user',
			status          TEXT    NOT NULL DEFAULT 'active',
			created_at      TEXT    NOT NULL DEFAULT (datetime('now')),
			updated_at      TEXT    NOT NULL DEFAULT (datetime('now')),
			expires_at      TEXT    NOT NULL DEFAULT '',
			route_mode      TEXT    NOT NULL DEFAULT 'auto',
			fixed_provider  TEXT    NOT NULL DEFAULT ''
		)`,
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
		`CREATE TABLE IF NOT EXISTS model_mappings (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			external    TEXT    NOT NULL,
			provider_id TEXT    NOT NULL,
			real_model  TEXT    NOT NULL,
			created_at  TEXT    NOT NULL DEFAULT (datetime('now')),
			FOREIGN KEY (provider_id) REFERENCES providers(slug) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_model_mappings_ext_prov ON model_mappings(external, provider_id)`,
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
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			action      TEXT    NOT NULL,
			target_type TEXT    NOT NULL,
			target_id   TEXT    NOT NULL,
			detail      TEXT    NOT NULL DEFAULT '',
			created_at  TEXT    NOT NULL DEFAULT (datetime('now'))
		)`,
	}
	for i, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			t.Fatalf("migration %d: %v", i, err)
		}
	}

	os.Setenv("GATEWAY_KEK_ENV", "test-kek-for-unit-tests!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}

	store := NewProviderStore(db, kek)
	return store, db
}

func TestCreateAndGetProvider(t *testing.T) {
	store, _ := testStore(t)

	p, err := store.CreateProvider("Test Provider", "test-prov", "https://api.test.com/v1", "sk-test-key-123", true)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if p.Slug != "test-prov" {
		t.Errorf("expected slug test-prov, got %s", p.Slug)
	}
	if !p.IsDefault {
		t.Error("expected is_default=true")
	}

	// Get by slug.
	p2, err := store.GetProvider("test-prov")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if p2 == nil {
		t.Fatal("GetProvider returned nil")
	}
	if p2.Name != "Test Provider" {
		t.Errorf("expected name 'Test Provider', got %q", p2.Name)
	}

	// Verify the key can be decrypted.
	plaintext, err := store.DecryptKey("test-prov")
	if err != nil {
		t.Fatalf("DecryptKey: %v", err)
	}
	if plaintext != "sk-test-key-123" {
		t.Errorf("expected decrypted key 'sk-test-key-123', got %q", plaintext)
	}
}

func TestUpdateProvider(t *testing.T) {
	store, _ := testStore(t)

	_, err := store.CreateProvider("P1", "p1", "https://api.p1.com", "key-old", false)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	// Update name and key.
	updates := map[string]any{
		"name":          "P1 Updated",
		"encrypted_key": "key-new",
	}
	p, err := store.UpdateProvider("p1", updates)
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	if p.Name != "P1 Updated" {
		t.Errorf("expected name 'P1 Updated', got %q", p.Name)
	}

	plaintext, _ := store.DecryptKey("p1")
	if plaintext != "key-new" {
		t.Errorf("expected key 'key-new', got %q", plaintext)
	}
}

func TestDeleteProvider(t *testing.T) {
	store, _ := testStore(t)

	_, err := store.CreateProvider("P1", "p1", "https://api.p1.com", "key1", false)
	if err != nil {
		t.Fatalf("CreateProvider p1: %v", err)
	}
	_, err = store.CreateProvider("P2", "p2", "https://api.p2.com", "key2", true)
	if err != nil {
		t.Fatalf("CreateProvider p2: %v", err)
	}

	// Cannot delete the last provider.
	err = store.DeleteProvider("p1")
	if err != nil {
		t.Fatalf("DeleteProvider p1 (not last): %v", err)
	}

	// Now only p2 remains — cannot delete it.
	// But first, create a routing rule referencing p2 to test reference check.
	rule := &models.RoutingRule{
		ProviderID: "p2",
		StartTime:  "14:00",
		EndTime:    "18:00",
		DaysOfWeek: "*",
		Timezone:   "Asia/Shanghai",
		Enabled:    true,
	}
	if err := store.CreateRoutingRule(rule); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}

	err = store.DeleteProvider("p2")
	if err == nil {
		t.Error("expected error deleting provider referenced by routing rule")
	}
}

func TestCreateMapping_Duplicate(t *testing.T) {
	store, _ := testStore(t)

	_, err := store.CreateProvider("P1", "p1", "https://api.p1.com", "key1", true)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	_, err = store.CreateMapping("glm-5.2", "p1", "glm-5.2")
	if err != nil {
		t.Fatalf("CreateMapping first: %v", err)
	}

	// Duplicate (external, provider_id) should fail.
	_, err = store.CreateMapping("glm-5.2", "p1", "glm-5.2")
	if err == nil {
		t.Error("expected error for duplicate mapping")
	}
}

func TestRoutingRuleCRUD(t *testing.T) {
	store, _ := testStore(t)

	rule := &models.RoutingRule{
		ProviderID: "zhipu",
		StartTime:  "14:00",
		EndTime:    "18:00",
		DaysOfWeek: "1,2,3,4,5",
		Timezone:   "Asia/Shanghai",
		Enabled:    true,
	}
	if err := store.CreateRoutingRule(rule); err != nil {
		t.Fatalf("CreateRoutingRule: %v", err)
	}
	if rule.ID == 0 {
		t.Error("expected rule ID to be set")
	}

	rules, err := store.ListRoutingRules()
	if err != nil {
		t.Fatalf("ListRoutingRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}

	// Update.
	err = store.UpdateRoutingRule(rule.ID, map[string]any{"enabled": false})
	if err != nil {
		t.Fatalf("UpdateRoutingRule: %v", err)
	}

	rules, _ = store.ListRoutingRules()
	if rules[0].Enabled {
		t.Error("expected rule to be disabled")
	}

	// Delete.
	err = store.DeleteRoutingRule(rule.ID)
	if err != nil {
		t.Fatalf("DeleteRoutingRule: %v", err)
	}

	rules, _ = store.ListRoutingRules()
	if len(rules) != 0 {
		t.Errorf("expected 0 rules after delete, got %d", len(rules))
	}
}

func TestAuditLogs(t *testing.T) {
	store, _ := testStore(t)

	store.WriteAudit("test.action", "test_type", "42", `{"key":"value"}`)
	store.WriteAudit("test.action2", "test_type", "43", "")

	logs, total, err := store.ListAuditLogs(1, 10)
	if err != nil {
		t.Fatalf("ListAuditLogs: %v", err)
	}
	if total != 2 {
		t.Errorf("expected total 2, got %d", total)
	}
	if len(logs) != 2 {
		t.Errorf("expected 2 logs, got %d", len(logs))
	}
}

func TestBuildProviderTable(t *testing.T) {
	store, _ := testStore(t)

	// Create two providers.
	_, err := store.CreateProvider("Zhipu", "zhipu", "https://api.zhipu.com/v1", "sk-zhipu", true)
	if err != nil {
		t.Fatalf("CreateProvider zhipu: %v", err)
	}
	_, err = store.CreateProvider("OpenAI", "openai", "https://api.openai.com/v1", "sk-openai", false)
	if err != nil {
		t.Fatalf("CreateProvider openai: %v", err)
	}

	// Create mappings.
	_, err = store.CreateMapping("glm-5.2", "zhipu", "glm-5.2")
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	_, err = store.CreateMapping("glm-5.2", "openai", "gpt-4o")
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	// Build table.
	table, err := store.BuildProviderTable()
	if err != nil {
		t.Fatalf("BuildProviderTable: %v", err)
	}

	if len(table.Providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(table.Providers))
	}
	if table.Default != "zhipu" {
		t.Errorf("expected default 'zhipu', got %q", table.Default)
	}

	// Check keys are decrypted.
	zhipu := table.Providers["zhipu"]
	if zhipu.APIKey != "sk-zhipu" {
		t.Errorf("expected zhipu key 'sk-zhipu', got %q", zhipu.APIKey)
	}

	// Check mappings.
	if table.Mappings["glm-5.2"]["zhipu"] != "glm-5.2" {
		t.Error("mapping mismatch")
	}
	if table.Mappings["glm-5.2"]["openai"] != "gpt-4o" {
		t.Error("mapping mismatch")
	}
}

func TestSeedFromConfig(t *testing.T) {
	store, _ := testStore(t)

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://api.zhipu.com", APIKeyEnv: "TEST_ZHIPU_KEY", IsDefault: true},
			{ID: "openai", Endpoint: "https://api.openai.com", APIKeyEnv: "TEST_OPENAI_KEY", IsDefault: false},
		},
		ModelMappings: []config.ModelMapping{
			{External: "glm-5.2", PerProvider: map[string]string{"zhipu": "glm-5.2", "openai": "gpt-4o"}},
		},
	}

	os.Setenv("TEST_ZHIPU_KEY", "zhipu-key-from-env")
	os.Setenv("TEST_OPENAI_KEY", "openai-key-from-env")
	defer os.Unsetenv("TEST_ZHIPU_KEY")
	defer os.Unsetenv("TEST_OPENAI_KEY")

	// First seed.
	if err := store.SeedFromConfig(cfg); err != nil {
		t.Fatalf("SeedFromConfig first: %v", err)
	}

	providers, err := store.ListProviders()
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if len(providers) != 2 {
		t.Errorf("expected 2 providers after seed, got %d", len(providers))
	}

	// Verify keys were encrypted and can be decrypted.
	key1, _ := store.DecryptKey("zhipu")
	if key1 != "zhipu-key-from-env" {
		t.Errorf("expected 'zhipu-key-from-env', got %q", key1)
	}

	// Second seed should be idempotent (no duplicates).
	if err := store.SeedFromConfig(cfg); err != nil {
		t.Fatalf("SeedFromConfig second: %v", err)
	}
	providers2, _ := store.ListProviders()
	if len(providers2) != 2 {
		t.Errorf("expected still 2 providers after second seed, got %d", len(providers2))
	}
}
