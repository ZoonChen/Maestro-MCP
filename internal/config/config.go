package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/service"
	"gopkg.in/yaml.v3"
)

// ValidationConfig is trusted, process-owned M0 validation configuration.
// It deliberately has no environment overrides: enabling local host execution
// requires an explicit, reviewable configuration file and cannot be requested
// by an MCP/HTTP caller.
type ValidationConfig struct {
	AllowHostExecution bool                       `yaml:"allow_host_execution"`
	DefaultTimeoutSec  int                        `yaml:"default_timeout_sec"`
	MaxOutputBytes     int                        `yaml:"max_output_bytes"`
	PolicyVersion      string                     `yaml:"policy_version"`
	PolicyDigest       string                     `yaml:"policy_digest"`
	CommandProfiles    []ValidationCommandProfile `yaml:"command_profiles"`
}

// ValidationCommandProfile is the YAML representation of the service-owned
// immutable CommandProfile. Conversion always passes through
// service.NewCommandProfileRegistry, which is the validation authority.
type ValidationCommandProfile struct {
	ID               string                            `yaml:"id"`
	Version          string                            `yaml:"version"`
	ImageDigest      string                            `yaml:"image_digest"`
	Argv             []string                          `yaml:"argv"`
	WorkingDirectory string                            `yaml:"working_directory"`
	Network          ValidationCommandProfileNetwork   `yaml:"network"`
	Resources        ValidationCommandProfileResources `yaml:"resources"`
	OutputLimitBytes int64                             `yaml:"output_limit_bytes"`
	TimeoutSeconds   int                               `yaml:"timeout_seconds"`
	Environment      map[string]string                 `yaml:"environment,omitempty"`
}

type ValidationCommandProfileNetwork struct {
	Mode       string   `yaml:"mode"`
	AllowHosts []string `yaml:"allow_hosts"`
}

type ValidationCommandProfileResources struct {
	CPUMillis int `yaml:"cpu_millis"`
	MemoryMB  int `yaml:"memory_mb"`
	DiskMB    int `yaml:"disk_mb"`
	PIDs      int `yaml:"pids"`
}

// Config holds all configuration for the M0 local Maestro runtime. M1 replaces
// the SQLite/shared-token fields with the v3 Control Plane and Runner schema.
type Config struct {
	DBPath   string `yaml:"db_path"`
	HTTPAddr string `yaml:"http_addr"`

	MaxConnections int `yaml:"max_connections"`

	LogLevel  string `yaml:"log_level"`
	LogFormat string `yaml:"log_format"`

	StaleTimeout int `yaml:"stale_timeout_sec"`

	WorktreeGCInterval int `yaml:"worktree_gc_interval_sec"`
	DataGCInterval     int `yaml:"data_gc_interval_hours"`

	ActivityLogRetention int `yaml:"activity_log_retention_days"`
	AuditLogRetention    int `yaml:"audit_log_retention_days"`

	AuthToken      string   `yaml:"-"`               // process Secret: MAESTRO_AUTH_TOKEN only
	AllowedOrigins []string `yaml:"allowed_origins"` // exact browser Origin values
	RemoteWrite    bool     `yaml:"remote_write"`    // disabled by default throughout M0

	Validation ValidationConfig `yaml:"validation"`
}

// DefaultConfig returns the safe M0 development baseline.
func DefaultConfig() *Config {
	return &Config{
		DBPath:   "data/maestro.db",
		HTTPAddr: "127.0.0.1:8080",

		MaxConnections: 50,

		LogLevel:  "info",
		LogFormat: "text",

		StaleTimeout: 120,

		WorktreeGCInterval: 600,
		DataGCInterval:     24,

		ActivityLogRetention: 90,
		AuditLogRetention:    365,

		AllowedOrigins: []string{},
		RemoteWrite:    false,

		Validation: ValidationConfig{
			DefaultTimeoutSec: 120,
			MaxOutputBytes:    64 << 10,
			CommandProfiles:   []ValidationCommandProfile{},
		},
	}
}

// Load decodes one strict YAML document over safe defaults. Unknown fields,
// duplicate mapping keys, malformed values, and additional documents fail
// closed instead of silently retaining defaults.
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, cfg.Validate()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, errors.New("read config file failed")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return cfg, errors.New("decode config: YAML does not match the configuration schema")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple YAML documents are not allowed")
		}
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ApplyEnvOverrides applies environment values atomically. The caller receives
// an error and the original Config is preserved if any override is invalid.
func ApplyEnvOverrides(cfg *Config) error {
	if cfg == nil {
		return errors.New("config is nil")
	}
	next := *cfg
	next.AllowedOrigins = append([]string(nil), cfg.AllowedOrigins...)

	applyString := func(environmentName string, destination *string) {
		if value, ok := os.LookupEnv(environmentName); ok && value != "" {
			*destination = value
		}
	}
	applyInt := func(environmentName string, destination *int) error {
		value, ok := os.LookupEnv(environmentName)
		if !ok || value == "" {
			return nil
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be an integer", environmentName)
		}
		*destination = parsed
		return nil
	}

	applyString("MAESTRO_AUTH_TOKEN", &next.AuthToken)
	applyString("MAESTRO_DB_PATH", &next.DBPath)
	applyString("MAESTRO_HTTP_ADDR", &next.HTTPAddr)
	applyString("MAESTRO_LOG_LEVEL", &next.LogLevel)
	applyString("MAESTRO_LOG_FORMAT", &next.LogFormat)

	for _, item := range []struct {
		name        string
		destination *int
	}{
		{"MAESTRO_MAX_CONNECTIONS", &next.MaxConnections},
		{"MAESTRO_STALE_TIMEOUT_SEC", &next.StaleTimeout},
		{"MAESTRO_WORKTREE_GC_INTERVAL_SEC", &next.WorktreeGCInterval},
		{"MAESTRO_DATA_GC_INTERVAL_HOURS", &next.DataGCInterval},
		{"MAESTRO_ACTIVITY_LOG_RETENTION_DAYS", &next.ActivityLogRetention},
		{"MAESTRO_AUDIT_LOG_RETENTION_DAYS", &next.AuditLogRetention},
	} {
		if err := applyInt(item.name, item.destination); err != nil {
			return err
		}
	}

	if value, ok := os.LookupEnv("MAESTRO_ALLOWED_ORIGINS"); ok && value != "" {
		origins, err := parseStringList(value)
		if err != nil {
			return fmt.Errorf("MAESTRO_ALLOWED_ORIGINS: %w", err)
		}
		next.AllowedOrigins = origins
	}
	if value, ok := os.LookupEnv("MAESTRO_REMOTE_WRITE"); ok && value != "" {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return errors.New("MAESTRO_REMOTE_WRITE must be true or false")
		}
		next.RemoteWrite = enabled
	}

	if err := next.Validate(); err != nil {
		return err
	}
	*cfg = next
	return nil
}

// Validate checks cross-field and range constraints used by every CLI command.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is nil")
	}
	if strings.TrimSpace(c.DBPath) == "" {
		return errors.New("db_path must not be empty")
	}
	if err := validateDBPath(c.DBPath); err != nil {
		return err
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return errors.New("http_addr must not be empty")
	}
	if _, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
		return errors.New("http_addr must be a valid host:port value")
	}
	if c.MaxConnections < 1 || c.MaxConnections > 10_000 {
		return errors.New("max_connections must be between 1 and 10000")
	}
	if !oneOf(c.LogLevel, "debug", "info", "warn", "error") {
		return errors.New("log_level must be debug, info, warn, or error")
	}
	if !oneOf(c.LogFormat, "text", "json") {
		return errors.New("log_format must be text or json")
	}
	if c.StaleTimeout < 1 {
		return errors.New("stale_timeout_sec must be positive")
	}
	if c.WorktreeGCInterval < 1 {
		return errors.New("worktree_gc_interval_sec must be positive")
	}
	if c.DataGCInterval < 1 {
		return errors.New("data_gc_interval_hours must be positive")
	}
	if c.ActivityLogRetention < 1 {
		return errors.New("activity log retention days must be positive")
	}
	// Audit events are append-only in M0. The fixed value is a minimum
	// export/backup retention policy, never authorization to delete rows.
	if c.AuditLogRetention != 365 {
		return errors.New("audit_log_retention_days must be 365")
	}

	seenOrigins := make(map[string]struct{}, len(c.AllowedOrigins))
	for index, origin := range c.AllowedOrigins {
		normalized, err := validateOrigin(origin)
		if err != nil {
			return fmt.Errorf("allowed_origins[%d]: %w", index, err)
		}
		if _, exists := seenOrigins[normalized]; exists {
			return fmt.Errorf("allowed_origins[%d]: duplicate origin", index)
		}
		seenOrigins[normalized] = struct{}{}
	}
	if err := c.validateValidation(); err != nil {
		return err
	}
	return nil
}

func (c *Config) validateValidation() error {
	if c.Validation.DefaultTimeoutSec < 1 || c.Validation.DefaultTimeoutSec > 3600 {
		return errors.New("validation.default_timeout_sec must be between 1 and 3600")
	}
	if c.Validation.MaxOutputBytes < 1024 || c.Validation.MaxOutputBytes > 10<<20 {
		return errors.New("validation.max_output_bytes must be between 1024 and 10485760")
	}

	configured := len(c.Validation.CommandProfiles) > 0 || c.Validation.PolicyVersion != "" || c.Validation.PolicyDigest != ""
	if configured {
		if strings.TrimSpace(c.Validation.PolicyVersion) == "" {
			return errors.New("validation.policy_version is required when validation profiles are configured")
		}
		if !isSHA256Digest(c.Validation.PolicyDigest) {
			return errors.New("validation.policy_digest must be an immutable sha256 digest")
		}
	}
	if c.Validation.AllowHostExecution {
		if len(c.Validation.CommandProfiles) == 0 {
			return errors.New("validation.command_profiles must not be empty when host execution is enabled")
		}
		if c.RemoteWrite {
			return errors.New("validation.allow_host_execution requires remote_write=false")
		}
		host, _, err := net.SplitHostPort(c.HTTPAddr)
		if err != nil || !isLoopbackIP(host) {
			return errors.New("validation.allow_host_execution requires http_addr to use an explicit loopback IP")
		}
	}
	_, err := c.TestExecutionConfig()
	return err
}

// TestExecutionConfig converts validated process configuration into the
// application-service contract. No request data participates in this step.
func (c *Config) TestExecutionConfig() (service.TestExecutionConfig, error) {
	result := service.TestExecutionConfig{
		DefaultTimeout:     time.Duration(c.Validation.DefaultTimeoutSec) * time.Second,
		MaxOutputBytes:     c.Validation.MaxOutputBytes,
		PolicyVersion:      c.Validation.PolicyVersion,
		PolicyDigest:       c.Validation.PolicyDigest,
		AllowHostExecution: c.Validation.AllowHostExecution,
	}
	if len(c.Validation.CommandProfiles) == 0 {
		return result, nil
	}
	profiles := make([]service.CommandProfile, 0, len(c.Validation.CommandProfiles))
	for _, profile := range c.Validation.CommandProfiles {
		profiles = append(profiles, service.CommandProfile{
			ID:               profile.ID,
			Version:          profile.Version,
			ImageDigest:      profile.ImageDigest,
			Argv:             append([]string(nil), profile.Argv...),
			WorkingDirectory: profile.WorkingDirectory,
			Network: service.CommandProfileNetwork{
				Mode:       profile.Network.Mode,
				AllowHosts: append([]string(nil), profile.Network.AllowHosts...),
			},
			Resources: service.CommandProfileResources{
				CPUMillis: profile.Resources.CPUMillis,
				MemoryMB:  profile.Resources.MemoryMB,
				DiskMB:    profile.Resources.DiskMB,
				PIDs:      profile.Resources.PIDs,
			},
			OutputLimitBytes: profile.OutputLimitBytes,
			TimeoutSeconds:   profile.TimeoutSeconds,
			Environment:      cloneStringMap(profile.Environment),
		})
	}
	registry, err := service.NewCommandProfileRegistry(profiles)
	if err != nil {
		return service.TestExecutionConfig{}, fmt.Errorf("validation.command_profiles: %w", err)
	}
	result.Profiles = registry
	return result, nil
}

// ValidationProfileReports returns only non-sensitive immutable identities for
// diagnostics; argv, environment and host paths are intentionally omitted.
func (c *Config) ValidationProfileReports() ([]ValidationProfileReport, error) {
	reports := make([]ValidationProfileReport, 0, len(c.Validation.CommandProfiles))
	for _, raw := range c.Validation.CommandProfiles {
		profile := service.CommandProfile{
			ID: raw.ID, Version: raw.Version, ImageDigest: raw.ImageDigest,
			Argv: append([]string(nil), raw.Argv...), WorkingDirectory: raw.WorkingDirectory,
			Network:          service.CommandProfileNetwork{Mode: raw.Network.Mode, AllowHosts: append([]string(nil), raw.Network.AllowHosts...)},
			Resources:        service.CommandProfileResources{CPUMillis: raw.Resources.CPUMillis, MemoryMB: raw.Resources.MemoryMB, DiskMB: raw.Resources.DiskMB, PIDs: raw.Resources.PIDs},
			OutputLimitBytes: raw.OutputLimitBytes, TimeoutSeconds: raw.TimeoutSeconds,
			Environment: cloneStringMap(raw.Environment),
		}
		digest, err := profile.Digest()
		if err != nil {
			return nil, err
		}
		reports = append(reports, ValidationProfileReport{Ref: raw.ID + "@" + raw.Version, Digest: digest})
	}
	return reports, nil
}

type ValidationProfileReport struct {
	Ref    string `json:"ref"`
	Digest string `json:"digest"`
}

func isLoopbackIP(host string) bool {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func validateOrigin(origin string) (string, error) {
	origin = strings.TrimSpace(origin)
	if origin == "" || origin == "null" || strings.ContainsAny(origin, "\r\n") {
		return "", errors.New("origin must be a non-empty HTTP(S) origin")
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("origin must be a valid HTTP(S) origin")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("origin must not contain credentials, path, query, or fragment")
	}
	return strings.ToLower(parsed.Scheme) + "://" + strings.ToLower(parsed.Host), nil
}

func validateDBPath(raw string) error {
	path := strings.TrimSpace(raw)
	if path == ":memory:" {
		return nil
	}
	if strings.IndexByte(path, 0) >= 0 {
		return errors.New("db_path must be a NUL-free filesystem path")
	}
	// modernc.org/sqlite accepts file: DSNs and query parameters that can
	// select a different database identity or connection mode than the runtime
	// lock protects. M0 accepts one ordinary filesystem path only.
	if strings.HasPrefix(strings.ToLower(path), "file:") || strings.ContainsAny(path, "?#") {
		return errors.New("db_path must be a filesystem path, not a SQLite URI or query")
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// parseStringList accepts an inline YAML/JSON-style list or a comma-separated
// environment value. It rejects empty items and duplicates.
func parseStringList(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "[]" {
		return []string{}, nil
	}
	if strings.HasPrefix(value, "[") {
		if !strings.HasSuffix(value, "]") {
			return nil, errors.New("unterminated list")
		}
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(value, "["), "]"))
	}

	seen := make(map[string]struct{})
	result := make([]string, 0)
	for index, item := range strings.Split(value, ",") {
		item = strings.Trim(strings.TrimSpace(item), `"'`)
		if item == "" {
			return nil, fmt.Errorf("origin list item %d is empty", index)
		}
		if _, exists := seen[item]; exists {
			return nil, fmt.Errorf("origin list item %d is duplicated", index)
		}
		seen[item] = struct{}{}
		result = append(result, item)
	}
	return result, nil
}
