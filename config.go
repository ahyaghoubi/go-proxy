package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AuthConfig specifies authentication for private upstreams
type AuthConfig struct {
	Type  string `yaml:"type" json:"type"`   // "basic", "bearer", or "header"
	Value string `yaml:"value" json:"value"` // token, user:pass, or header value
	Name  string `yaml:"name" json:"name"`   // for type "header": header name
}

// PrivateUpstream routes requests for matching module paths to a specific upstream with auth
type PrivateUpstream struct {
	Pattern string     `yaml:"pattern" json:"pattern"`
	URL     string     `yaml:"url" json:"url"`
	Auth    AuthConfig `yaml:"auth" json:"auth"`
}

// Config holds proxy configuration (from file, env, flags)
type Config struct {
	Port              string           `yaml:"port" json:"port"`
	CacheDir          string           `yaml:"cache_dir" json:"cache_dir"`
	Upstreams         []string         `yaml:"upstreams" json:"upstreams"`
	PrivateUpstreams  []PrivateUpstream `yaml:"private_upstreams" json:"private_upstreams"`
	Proxy             string           `yaml:"proxy" json:"proxy"`
	DNSServer         string           `yaml:"dns" json:"dns"`
	WriteTimeout      string           `yaml:"write_timeout" json:"write_timeout"`
	MaxCacheAge       string           `yaml:"max_cache_age" json:"max_cache_age"`
	CleanupInterval   string           `yaml:"cleanup_interval" json:"cleanup_interval"`
	GOSUMDB           string           `yaml:"gosumdb" json:"gosumdb"`
	GONOSUMDB         string           `yaml:"gonosumdb" json:"gonosumdb"`
	RetryAttempts     int              `yaml:"retry_attempts" json:"retry_attempts"`
	RetryBackoff      string           `yaml:"retry_backoff" json:"retry_backoff"`
	DownloadConnections   int          `yaml:"download_connections" json:"download_connections"`
	MaxConcurrentDownloads int         `yaml:"max_concurrent_downloads" json:"max_concurrent_downloads"`
}

// LoadConfig loads configuration from a YAML or JSON file (auto-detected by extension)
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".yaml", ".yml":
		var cfg Config
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse YAML config: %w", err)
		}
		return &cfg, nil
	case ".json":
		var cfg Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("parse JSON config: %w", err)
		}
		return &cfg, nil
	default:
		return nil, fmt.Errorf("unsupported config format: %s (use .yaml, .yml, or .json)", ext)
	}
}

// DefaultConfig returns default configuration values
func DefaultConfig() Config {
	return Config{
		Port:                   "12345",
		CacheDir:               "./cache",
		Upstreams:              []string{"https://proxy.golang.org"},
		WriteTimeout:           "30m",
		MaxCacheAge:            "0s",
		CleanupInterval:        "24h",
		GOSUMDB:                "off",
		RetryAttempts:          3,
		RetryBackoff:           "100ms",
		DownloadConnections:    4,
		MaxConcurrentDownloads: 4,
	}
}

// ParseRetryBackoff parses RetryBackoff as duration
func (c *Config) ParseRetryBackoff() (time.Duration, error) {
	backoff := c.RetryBackoff
	if backoff == "" {
		backoff = "100ms"
	}
	return time.ParseDuration(backoff)
}
