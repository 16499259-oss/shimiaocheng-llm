package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// examplePath points at the real shipped example config at the repo root.
const examplePath = "../../config.yaml.example"

func TestLoad_ExampleFile(t *testing.T) {
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load(%s): %v", examplePath, err)
	}

	if cfg.Server.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("Server.ListenAddr = %q, want 127.0.0.1:8080", cfg.Server.ListenAddr)
	}
	if cfg.Database.Path != "./llm_gateway.db" {
		t.Errorf("Database.Path = %q, want ./llm_gateway.db", cfg.Database.Path)
	}
	if cfg.Auth.SubKeySalt != "llm-gateway-salt-2025" {
		t.Errorf("Auth.SubKeySalt = %q, want llm-gateway-salt-2025", cfg.Auth.SubKeySalt)
	}
	if cfg.Quota.Default5hLimit != 100 || cfg.Quota.DefaultTotalLimit != 10000 || cfg.Quota.ResetIntervalHours != 5 {
		t.Errorf("Quota defaults = %+v, want 100/10000/5", cfg.Quota)
	}

	if len(cfg.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(cfg.Providers))
	}
	if cfg.Providers[0].ID != "zhipu" || !cfg.Providers[0].IsDefault {
		t.Errorf("Providers[0] = %+v, want zhipu+is_default", cfg.Providers[0])
	}
	if cfg.Providers[1].ID != "openai" || cfg.Providers[1].IsDefault {
		t.Errorf("Providers[1] = %+v, want openai+not-default", cfg.Providers[1])
	}

	if len(cfg.ModelMappings) != 2 {
		t.Fatalf("len(ModelMappings) = %d, want 2", len(cfg.ModelMappings))
	}
	g := cfg.ModelMappings[0]
	if g.External != "glm-5.2" || g.PerProvider["zhipu"] != "glm-5.2" || g.PerProvider["openai"] != "gpt-4o" {
		t.Errorf("ModelMappings[0] = %+v, want glm-5.2 -> {zhipu:glm-5.2, openai:gpt-4o}", g)
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	// Only listen_addr is set; everything else must fall back to documented defaults.
	src := "server:\n  listen_addr: \"0.0.0.0:80\"\n"
	path := filepath.Join(t.TempDir(), "minimal.yaml")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write minimal config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load minimal: %v", err)
	}
	if cfg.Server.ReadTimeout != 120*time.Second {
		t.Errorf("ReadTimeout = %v, want 120s", cfg.Server.ReadTimeout)
	}
	if cfg.Server.WriteTimeout != 300*time.Second {
		t.Errorf("WriteTimeout = %v, want 300s", cfg.Server.WriteTimeout)
	}
	if cfg.Database.Path != "./llm_gateway.db" {
		t.Errorf("Database.Path = %q, want default", cfg.Database.Path)
	}
	if cfg.Auth.SessionExpireHours != 24 {
		t.Errorf("Auth.SessionExpireHours = %d, want 24", cfg.Auth.SessionExpireHours)
	}
	if cfg.API.ZhipuEndpoint != "https://api.zhipuai.cn/api/paas/v4/chat/completions" {
		t.Errorf("API.ZhipuEndpoint = %q, want default zhipu endpoint", cfg.API.ZhipuEndpoint)
	}
	if cfg.Quota.Default5hLimit != 100 || cfg.Quota.DefaultTotalLimit != 10000 || cfg.Quota.ResetIntervalHours != 5 {
		t.Errorf("Quota defaults = %+v, want 100/10000/5", cfg.Quota)
	}
	if len(cfg.Providers) != 0 {
		t.Errorf("expected no providers injected by Load (DB-first), got %d", len(cfg.Providers))
	}
}

func TestLoad_EnvOverride(t *testing.T) {
	t.Setenv("ZHIPU_API_KEY", "env-injected-secret")
	cfg, err := Load(examplePath)
	if err != nil {
		t.Fatalf("Load(%s): %v", examplePath, err)
	}
	if cfg.API.ZhipuAPIKey != "env-injected-secret" {
		t.Errorf("ZhipuAPIKey = %q, want env-injected-secret (env override)", cfg.API.ZhipuAPIKey)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Error("expected error loading nonexistent config file, got nil")
	}
}
