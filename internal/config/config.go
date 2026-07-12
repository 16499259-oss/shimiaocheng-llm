// Package config provides YAML configuration parsing for the LLM API Gateway.
package config

import (
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds all configuration for the gateway.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DatabaseConfig `yaml:"database"`
	API      APIConfig      `yaml:"api"`
	Auth     AuthConfig     `yaml:"auth"`
	Quota    QuotaConfig    `yaml:"quota"`
	// Providers lists the upstream LLM providers the gateway can route to.
	// When empty, a single default Zhipu provider is injected in Load.
	Providers []ProviderConfig `yaml:"providers"`
	// ModelMappings maps an external (user-facing) model name to the
	// per-provider real model name. A missing mapping means the external name
	// is passed through unchanged (passthrough).
	ModelMappings []ModelMapping `yaml:"model_mappings"`
}

// ProviderConfig describes a single upstream LLM provider.
type ProviderConfig struct {
	ID        string `yaml:"id"`          // unique provider id, e.g. "zhipu", "openai"
	Endpoint  string `yaml:"endpoint"`    // upstream chat-completions endpoint URL
	APIKeyEnv string `yaml:"api_key_env"` // env var that injects this provider's key at startup
	IsDefault bool   `yaml:"is_default"`  // true for the global default provider
}

// ModelMapping maps an external model name to per-provider real model names.
type ModelMapping struct {
	External    string            `yaml:"external"`     // external (user-facing) model name
	PerProvider map[string]string `yaml:"per_provider"` // providerID -> real model name
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	ListenAddr   string        `yaml:"listen_addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
}

// DatabaseConfig holds SQLite database settings.
type DatabaseConfig struct {
	Path string `yaml:"path"`
}

// APIConfig holds upstream API settings.
type APIConfig struct {
	ZhipuEndpoint string `yaml:"zhipu_endpoint"`
	ZhipuAPIKey   string `yaml:"zhipu_api_key"`
}

// AuthConfig holds authentication settings.
type AuthConfig struct {
	SubKeySalt         string `yaml:"sub_key_salt"`
	SessionExpireHours int    `yaml:"session_expire_hours"`
}

// QuotaConfig holds quota default settings.
type QuotaConfig struct {
	Default5hLimit     int `yaml:"default_5h_limit"`
	DefaultTotalLimit  int `yaml:"default_total_limit"`
	ResetIntervalHours int `yaml:"reset_interval_hours"`
}

// Load reads configuration from a YAML file path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	// Set defaults
	cfg.Server.ListenAddr = "127.0.0.1:8080"
	cfg.Server.ReadTimeout = 120 * time.Second
	cfg.Server.WriteTimeout = 300 * time.Second
	cfg.Database.Path = "./llm_gateway.db"
	cfg.API.ZhipuEndpoint = "https://api.zhipuai.cn/api/paas/v4/chat/completions"
	cfg.Auth.SessionExpireHours = 24
	cfg.Quota.Default5hLimit = 100
	cfg.Quota.DefaultTotalLimit = 10000
	cfg.Quota.ResetIntervalHours = 5

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	// NOTE: Default provider injection and is_default auto-promotion have been
	// moved to ProviderStore.SeedFromConfig. The config.Load function no longer
	// mutates providers/model_mappings — those are now loaded from the database
	// at runtime (DB-first, see ADR-0007).

	// Override API key from environment if set (legacy fallback).
	if envKey := os.Getenv("ZHIPU_API_KEY"); envKey != "" {
		cfg.API.ZhipuAPIKey = envKey
	}

	return cfg, nil
}
