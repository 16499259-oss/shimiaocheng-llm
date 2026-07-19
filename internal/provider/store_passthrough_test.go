package provider

import (
	"os"
	"testing"

	"llm_api_gateway/internal/config"
)

// TestCreateProvider_PassthroughFields verifies the extended CreateProvider
// persists and round-trips the passthrough / auth fields through
// GetProvider and the BuildProviderTable snapshot (which parses the
// extra_headers JSON into a map).
func TestCreateProvider_PassthroughFields(t *testing.T) {
	store, _ := testStore(t)

	p, err := store.CreateProvider("Anthropic", "anthropic", "https://api.anthropic.com",
		"sk-ant-key", false, true, "X-Api-Key", "x-api-key",
		map[string]string{"anthropic-version": "2023-06-01"}, 0, 0)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if !p.AllowPassthrough {
		t.Error("expected allow_passthrough=true")
	}
	if p.AuthScheme != "x-api-key" {
		t.Errorf("expected auth_scheme x-api-key, got %q", p.AuthScheme)
	}
	if p.AuthHeader != "X-Api-Key" {
		t.Errorf("expected auth_header X-Api-Key, got %q", p.AuthHeader)
	}
	if p.ExtraHeaders != `{"anthropic-version":"2023-06-01"}` {
		t.Errorf("unexpected extra_headers stored: %q", p.ExtraHeaders)
	}

	// GetProvider round-trips the JSON string verbatim.
	g, err := store.GetProvider("anthropic")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if !g.AllowPassthrough || g.AuthScheme != "x-api-key" {
		t.Error("GetProvider did not round-trip passthrough fields")
	}

	// BuildProviderTable parses extra_headers into a map.
	table, err := store.BuildProviderTable()
	if err != nil {
		t.Fatalf("BuildProviderTable: %v", err)
	}
	entry, ok := table.Providers["anthropic"]
	if !ok {
		t.Fatal("anthropic not in provider table")
	}
	if !entry.AllowPassthrough {
		t.Error("snapshot allow_passthrough not true")
	}
	if entry.AuthScheme != "x-api-key" {
		t.Errorf("snapshot auth_scheme = %q", entry.AuthScheme)
	}
	if entry.ExtraHeaders["anthropic-version"] != "2023-06-01" {
		t.Errorf("snapshot extra_headers not parsed: %#v", entry.ExtraHeaders)
	}
}

// TestUpdateProvider_PassthroughFields verifies UpdateProvider applies the
// new passthrough fields via the updates map.
func TestUpdateProvider_PassthroughFields(t *testing.T) {
	store, _ := testStore(t)
	_, err := store.CreateProvider("P1", "p1", "https://api.p1.com", "key1", true, false, "Authorization", "bearer", nil, 0, 0)
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}

	updates := map[string]any{
		"allow_passthrough": true,
		"auth_scheme":       "x-api-key",
		"auth_header":       "X-Api-Key",
		"extra_headers":     map[string]string{"x-foo": "bar"},
	}
	p, err := store.UpdateProvider("p1", updates)
	if err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	if !p.AllowPassthrough {
		t.Error("expected allow_passthrough=true after update")
	}
	if p.AuthScheme != "x-api-key" || p.AuthHeader != "X-Api-Key" {
		t.Errorf("auth not updated: scheme=%q header=%q", p.AuthScheme, p.AuthHeader)
	}
	if p.ExtraHeaders != `{"x-foo":"bar"}` {
		t.Errorf("extra_headers not stored: %q", p.ExtraHeaders)
	}
}

// TestSeedFromConfig_PassthroughFields verifies SeedFromConfig propagates
// the passthrough / auth fields into the DB and snapshot.
func TestSeedFromConfig_PassthroughFields(t *testing.T) {
	store, _ := testStore(t)
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{
				ID: "anthropic", Endpoint: "https://api.anthropic.com",
				APIKeyEnv: "TEST_ANTHROPIC_KEY", IsDefault: true,
				AllowPassthrough: true, AuthHeader: "X-Api-Key",
				AuthScheme:   "x-api-key",
				ExtraHeaders: map[string]string{"anthropic-version": "2023-06-01"},
			},
		},
	}
	os.Setenv("TEST_ANTHROPIC_KEY", "ant-key")
	defer os.Unsetenv("TEST_ANTHROPIC_KEY")

	if err := store.SeedFromConfig(cfg); err != nil {
		t.Fatalf("SeedFromConfig: %v", err)
	}
	g, err := store.GetProvider("anthropic")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if !g.AllowPassthrough || g.AuthScheme != "x-api-key" {
		t.Error("seed did not propagate passthrough fields")
	}
	table, err := store.BuildProviderTable()
	if err != nil {
		t.Fatalf("BuildProviderTable: %v", err)
	}
	entry := table.Providers["anthropic"]
	if !entry.AllowPassthrough || entry.ExtraHeaders["anthropic-version"] != "2023-06-01" {
		t.Error("snapshot missing seeded passthrough fields")
	}
}
