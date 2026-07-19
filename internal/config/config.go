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
	// Compaction selects the over-budget behaviour for request bodies.
	// "trim" (default) auto-compacts chat history to fit each user's per-request
	// body budget and forwards; "off" restores the legacy hard-413 behaviour.
	Compaction string `yaml:"compaction"`
	// Debug enables verbose request logging (e.g. raw body dump on JSON parse
	// failure). OFF by default to avoid leaking user content into logs.
	Debug bool `yaml:"debug"`
	// Proxy holds the global switchboard for the wildcard passthrough endpoint
	// (/v1/passthrough/). See internal/proxy/passthrough*.go.
	Proxy ProxyConfig `yaml:"proxy"`
	// ProviderQuota holds the global default low-balance thresholds for
	// upstream providers. token 与 调用次数 各自独立；每 provider 可在 DB 中
	// 覆盖（0 = 继承全局默认）。
	ProviderQuota ProviderQuotaConfig `yaml:"provider_quota"`
}

// ProviderQuotaConfig holds the global default low-balance thresholds for
// upstream providers, expressed as a REMAINING ratio (e.g. 0.10 = "flag red
// when < 10% remaining"). The two dimensions (token / call-count) are
// configured independently. A per-provider value of 0 means "inherit this
// global default".
type ProviderQuotaConfig struct {
	DefaultTokenLowRatio float64 `yaml:"default_token_low_ratio"`
	DefaultCallLowRatio  float64 `yaml:"default_call_low_ratio"`
}

// ProxyConfig holds the global proxy / passthrough switchboard settings.
type ProxyConfig struct {
	// PassthroughEnabled is the GLOBAL master switch for the wildcard
	// /v1/passthrough/ endpoint. It is OFF by default; a request is only
	// forwarded when BOTH this flag AND the target provider's allow_passthrough
	// are true (defence in depth against an open-proxy / SSRF surface).
	// See docs/design-mcp-passthrough.md §8.
	PassthroughEnabled bool `yaml:"passthrough_enabled"`
}

// ProviderConfig describes a single upstream LLM provider.
type ProviderConfig struct {
	ID        string `yaml:"id"`          // unique provider id, e.g. "zhipu", "openai"
	Endpoint  string `yaml:"endpoint"`    // upstream chat-completions endpoint URL
	APIKeyEnv string `yaml:"api_key_env"` // env var that injects this provider's key at startup
	IsDefault bool   `yaml:"is_default"`  // true for the global default provider
	// ── Passthrough / MCP support ──
	// AllowPassthrough enables this provider as a wildcard passthrough target
	// (e.g. MCP / arbitrary upstream paths). Effective only when the global
	// Proxy.PassthroughEnabled master switch is also on.
	AllowPassthrough bool `yaml:"allow_passthrough"`
	// AuthHeader is the upstream auth header name. Empty defaults to
	// "Authorization" (bearer) or "X-Api-Key" (x-api-key).
	AuthHeader string `yaml:"auth_header"`
	// AuthScheme selects the upstream auth injection: "bearer" (default),
	// "x-api-key", or "none" (inject only when AuthHeader is set).
	AuthScheme string `yaml:"auth_scheme"`
	// ExtraHeaders are static extra headers injected verbatim on every
	// passthrough request (e.g. {"anthropic-version": "2023-06-01"}).
	ExtraHeaders map[string]string `yaml:"extra_headers"`
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
	cfg.Compaction = "trim" // auto-compact over-budget requests; "off" = legacy hard 413

	// Global default low-balance thresholds (remaining ratio). Set BEFORE
	// yaml.Unmarshal so a missing provider_quota section or a missing field
	// falls back to 0.10 instead of the zero value (0 would mean "flag
	// immediately"). Unmarshal only overwrites the sub-fields present in YAML.
	cfg.ProviderQuota.DefaultTokenLowRatio = 0.10
	cfg.ProviderQuota.DefaultCallLowRatio = 0.10

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
