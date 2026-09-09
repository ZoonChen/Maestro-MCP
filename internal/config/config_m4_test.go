package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// M4-OBS-001 `telemetry` section: absent keeps the producer unstarted;
// present values must be explicit and require the PostgreSQL driver
// (telemetry_aggregates is PostgreSQL-only — SQLite deployments fail
// closed instead of silently dropping every flush).

func writeTelemetryConfig(t *testing.T, body string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "maestro.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := Load(path)
	if err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func TestTelemetrySectionAbsentStaysUnstarted(t *testing.T) {
	cfg, err := writeTelemetryConfig(t, "db_path: data/maestro.db\n")
	require.NoError(t, err)
	assert.Nil(t, cfg.Telemetry)
}

func TestTelemetrySectionValidation(t *testing.T) {
	postgresBase := "db_path: data/maestro.db\ndatabase:\n  dsn_secret_ref: docker-secret://maestro-postgres-dsn\n"
	valid := postgresBase + "telemetry:\n" +
		"  producer_interval_seconds: 60\n" +
		"  redaction_version: telemetry-v3\n" +
		"  platform_project_id: 018f7e00-0000-7000-8000-0000000000f0\n"
	cfg, err := writeTelemetryConfig(t, valid)
	require.NoError(t, err)
	require.NotNil(t, cfg.Telemetry)
	assert.True(t, cfg.PostgresEnabled())

	invalid := map[string]string{
		"interval below 1": postgresBase + "telemetry:\n  producer_interval_seconds: 0\n" +
			"  redaction_version: v\n  platform_project_id: p\n",
		"interval above 3600": postgresBase + "telemetry:\n  producer_interval_seconds: 3601\n" +
			"  redaction_version: v\n  platform_project_id: p\n",
		"empty redaction version": postgresBase + "telemetry:\n  producer_interval_seconds: 60\n" +
			"  redaction_version: \"\"\n  platform_project_id: p\n",
		"empty platform project": postgresBase + "telemetry:\n  producer_interval_seconds: 60\n" +
			"  redaction_version: v\n  platform_project_id: \"\"\n",
		"sqlite driver": "db_path: data/maestro.db\ntelemetry:\n" +
			"  producer_interval_seconds: 60\n  redaction_version: v\n  platform_project_id: p\n",
	}
	for name, body := range invalid {
		_, err := writeTelemetryConfig(t, body)
		assert.Error(t, err, name)
	}
}

func TestTelemetrySectionUnknownFieldsFailClosed(t *testing.T) {
	body := "db_path: data/maestro.db\ndatabase:\n  dsn_secret_ref: docker-secret://maestro-postgres-dsn\n" +
		"telemetry:\n  producer_interval_seconds: 60\n  redaction_version: v\n" +
		"  platform_project_id: p\n  extra_field: 1\n"
	_, err := writeTelemetryConfig(t, body)
	require.Error(t, err, "strict decoding rejects unknown telemetry fields")
}
