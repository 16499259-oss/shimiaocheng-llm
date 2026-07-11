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

	// Override API key from environment if set
	if envKey := os.Getenv("ZHIPU_API_KEY"); envKey != "" {
		cfg.API.ZhipuAPIKey = envKey
	}

	return cfg, nil
}
