package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all configuration for the Maestro server.
type Config struct {
	DBPath    string `yaml:"db_path"`
	HTTPAddr  string `yaml:"http_addr"`
	SSEAddr   string `yaml:"sse_addr"`
	Transport string `yaml:"transport"`

	// Connection limits
	MaxConnections int `yaml:"max_connections"`

	// Logging
	LogLevel  string `yaml:"log_level"`  // debug, info, warn, error
	LogFormat string `yaml:"log_format"` // text, json

	// Session management
	StaleTimeout int `yaml:"stale_timeout_sec"` // default 120

	// Garbage collection intervals
	WorktreeGCInterval int `yaml:"worktree_gc_interval_sec"` // default 600
	DataGCInterval     int `yaml:"data_gc_interval_hours"`   // default 24

	// Data retention (days)
	ActivityLogRetention int `yaml:"activity_log_retention_days"` // default 90
	AuditLogRetention    int `yaml:"audit_log_retention_days"`    // default 180

	// Security
	AuthToken string `yaml:"auth_token"` // Bearer token for REST API auth; empty = disabled
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		DBPath:    "data/maestro.db",
		HTTPAddr:  ":8080",
		SSEAddr:   ":3000",
		Transport: "stdio",

		MaxConnections: 50,

		LogLevel:  "info",
		LogFormat: "text",

		StaleTimeout: 120,

		WorktreeGCInterval: 600,
		DataGCInterval:     24,

		ActivityLogRetention: 90,
		AuditLogRetention:    180,
	}
}

// Load reads the config file at the given path. If path is empty, returns defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	// Simple YAML parsing: line by line, key: value
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.Trim(strings.TrimSpace(parts[1]), `"'`)

		switch key {
		case "db_path":
			cfg.DBPath = value
		case "http_addr":
			cfg.HTTPAddr = value
		case "sse_addr":
			cfg.SSEAddr = value
		case "transport":
			cfg.Transport = value
		case "max_connections":
			cfg.MaxConnections = atoiOrDefault(value, cfg.MaxConnections)
		case "log_level":
			cfg.LogLevel = value
		case "log_format":
			cfg.LogFormat = value
		case "stale_timeout_sec":
			cfg.StaleTimeout = atoiOrDefault(value, cfg.StaleTimeout)
		case "worktree_gc_interval_sec":
			cfg.WorktreeGCInterval = atoiOrDefault(value, cfg.WorktreeGCInterval)
		case "data_gc_interval_hours":
			cfg.DataGCInterval = atoiOrDefault(value, cfg.DataGCInterval)
		case "activity_log_retention_days":
			cfg.ActivityLogRetention = atoiOrDefault(value, cfg.ActivityLogRetention)
		case "audit_log_retention_days":
			cfg.AuditLogRetention = atoiOrDefault(value, cfg.AuditLogRetention)
		case "auth_token":
			cfg.AuthToken = value
		}
	}

	return cfg, nil
}

// atoiOrDefault parses an int from string, returning the default on failure.
func atoiOrDefault(s string, def int) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ApplyEnvOverrides allows overriding config values via environment variables.
// Supported: MAESTRO_AUTH_TOKEN, MAESTRO_DB_PATH, MAESTRO_HTTP_ADDR, MAESTRO_SSE_ADDR.
func ApplyEnvOverrides(cfg *Config) {
	if v := os.Getenv("MAESTRO_AUTH_TOKEN"); v != "" {
		cfg.AuthToken = v
	}
	if v := os.Getenv("MAESTRO_DB_PATH"); v != "" {
		cfg.DBPath = v
	}
	if v := os.Getenv("MAESTRO_HTTP_ADDR"); v != "" {
		cfg.HTTPAddr = v
	}
	if v := os.Getenv("MAESTRO_SSE_ADDR"); v != "" {
		cfg.SSEAddr = v
	}
}
