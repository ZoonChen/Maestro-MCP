package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateTaskTestRequirementsSchemaAndLimits(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", "  "} {
		require.NoError(t, ValidateTaskTestRequirements([]byte(raw)))
	}
	valid := `{"profile_id":"go-unit","profile_version":"3.0.0","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"go-cover","coverage_path":"reports/coverage.out","min_coverage":80}`
	require.NoError(t, ValidateTaskTestRequirements([]byte(valid)))

	invalid := []string{
		`{`,
		valid + `{}`,
		`{"command":"go test ./..."}`,
		`{"profile_id":"go-unit","unknown":true}`,
		`{"profile_id":"UPPER","profile_version":"3.0.0","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"go-cover","coverage_path":"coverage.out","min_coverage":80}`,
		`{"profile_id":"go-unit","profile_version":"latest","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"go-cover","coverage_path":"coverage.out","min_coverage":80}`,
		`{"profile_id":"go-unit","profile_version":"3.0.0","profile_digest":"mutable","coverage_format":"go-cover","coverage_path":"coverage.out","min_coverage":80}`,
		`{"profile_id":"go-unit","profile_version":"3.0.0","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"auto","coverage_path":"coverage.out","min_coverage":80}`,
		`{"profile_id":"go-unit","profile_version":"3.0.0","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"go-cover","coverage_path":"../coverage.out","min_coverage":80}`,
		`{"profile_id":"go-unit","profile_version":"3.0.0","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"go-cover","coverage_path":".git/config","min_coverage":80}`,
		`{"profile_id":"go-unit","profile_version":"3.0.0","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"go-cover","coverage_path":"coverage.out","min_coverage":79.9}`,
		`{"profile_id":"go-unit","profile_version":"3.0.0","profile_digest":"sha256:` + strings.Repeat("a", 64) + `","coverage_format":"go-cover","coverage_path":"coverage.out","min_coverage":101}`,
	}
	for _, raw := range invalid {
		require.Error(t, ValidateTaskTestRequirements([]byte(raw)), raw)
	}
}

func TestValidateProjectConfigRejectsLegacyCommandsAndPartialDefaults(t *testing.T) {
	for _, raw := range []string{"", "{}", "null", `{"max_worktrees":3}`} {
		require.NoError(t, ValidateProjectConfig([]byte(raw)))
	}
	valid := `{"default_command_profile_id":"go-unit","default_command_profile_version":"3.0.0","default_command_profile_digest":"sha256:` + strings.Repeat("b", 64) + `","default_coverage_format":"istanbul","default_coverage_path":"coverage/final.json","default_min_coverage":85}`
	require.NoError(t, ValidateProjectConfig([]byte(valid)))

	for _, raw := range []string{
		`{`,
		`{"default_test_command":"go test ./..."}`,
		`{"allowed_test_commands":["go test ./..."]}`,
		`{"default_command_profile_id":"go-unit"}`,
		`{"default_coverage_format":"go-cover"}`,
	} {
		require.Error(t, ValidateProjectConfig([]byte(raw)), raw)
	}
}

func TestResolveValidationPolicyUsesTaskOrCompleteProjectDefaults(t *testing.T) {
	cfg := TestExecutionConfig{PolicyVersion: "3.0.0", PolicyDigest: "sha256:" + strings.Repeat("c", 64)}
	min := 82.0
	taskPolicy := validationPolicy{
		ProfileID: "go-unit", ProfileVersion: "3.0.0", ProfileDigest: "sha256:" + strings.Repeat("d", 64),
		CoverageFormat: "go-cover", CoveragePath: "coverage.out", MinCoverage: &min,
	}
	rawTask, err := json.Marshal(taskPolicy)
	require.NoError(t, err)
	resolved, err := resolveValidationPolicy(&model.Task{TestRequirements: rawTask}, nil, cfg)
	require.NoError(t, err)
	assert.Equal(t, taskPolicy, resolved)

	project := &model.Project{Config: json.RawMessage(`{
		"default_command_profile_id":"ts-unit",
		"default_command_profile_version":"1.2.3",
		"default_command_profile_digest":"sha256:` + strings.Repeat("e", 64) + `",
		"default_coverage_format":"istanbul",
		"default_coverage_path":"coverage/final.json",
		"default_min_coverage":90
	}`)}
	resolved, err = resolveValidationPolicy(&model.Task{}, project, cfg)
	require.NoError(t, err)
	assert.Equal(t, "ts-unit", resolved.ProfileID)
	assert.Equal(t, 90.0, *resolved.MinCoverage)

	for _, badProject := range []*model.Project{
		nil,
		{Config: json.RawMessage(`{`)},
		{Config: json.RawMessage(`{"default_test_command":"go test"}`)},
		{Config: json.RawMessage(`{"allowed_test_commands":[]}`)},
		{Config: json.RawMessage(`{}`)},
	} {
		_, err := resolveValidationPolicy(&model.Task{}, badProject, cfg)
		require.Error(t, err)
	}
	_, err = resolveValidationPolicy(&model.Task{TestRequirements: rawTask}, project, TestExecutionConfig{})
	require.Error(t, err)
}
