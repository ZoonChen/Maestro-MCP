package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

type validationPolicy struct {
	ProfileID      string   `json:"profile_id"`
	ProfileVersion string   `json:"profile_version"`
	ProfileDigest  string   `json:"profile_digest"`
	CoverageFormat string   `json:"coverage_format"`
	CoveragePath   string   `json:"coverage_path"`
	MinCoverage    *float64 `json:"min_coverage"`
}

type projectValidationDefaults struct {
	DefaultProfileID      string   `json:"default_command_profile_id"`
	DefaultProfileVersion string   `json:"default_command_profile_version"`
	DefaultProfileDigest  string   `json:"default_command_profile_digest"`
	DefaultCoverageFormat string   `json:"default_coverage_format"`
	DefaultCoveragePath   string   `json:"default_coverage_path"`
	DefaultMinCoverage    *float64 `json:"default_min_coverage"`
}

func resolveValidationPolicy(task *model.Task, project *model.Project, cfg TestExecutionConfig) (validationPolicy, error) {
	if cfg.PolicyVersion == "" || !imageDigestRe.MatchString(cfg.PolicyDigest) {
		return validationPolicy{}, fmt.Errorf("active quality policy version/digest is missing or invalid")
	}

	raw := bytes.TrimSpace(task.TestRequirements)
	if len(raw) > 0 && !bytes.Equal(raw, []byte("{}")) && !bytes.Equal(raw, []byte("null")) {
		policy, err := decodeValidationPolicy(raw)
		if err != nil {
			return validationPolicy{}, err
		}
		if err := validateResolvedValidationPolicy(policy); err != nil {
			return validationPolicy{}, err
		}
		return policy, nil
	}

	if project == nil || len(bytes.TrimSpace(project.Config)) == 0 {
		return validationPolicy{}, fmt.Errorf("required validation policy/profile evidence is missing")
	}
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(project.Config, &rawConfig); err != nil {
		return validationPolicy{}, fmt.Errorf("project validation config is invalid JSON: %w", err)
	}
	if _, exists := rawConfig["default_test_command"]; exists {
		return validationPolicy{}, fmt.Errorf("default_test_command is forbidden; use an approved command profile reference")
	}
	if _, exists := rawConfig["allowed_test_commands"]; exists {
		return validationPolicy{}, fmt.Errorf("allowed_test_commands is forbidden; use an approved command profile registry")
	}
	var defaults projectValidationDefaults
	if err := json.Unmarshal(project.Config, &defaults); err != nil {
		return validationPolicy{}, fmt.Errorf("project validation defaults are invalid: %w", err)
	}
	policy := validationPolicy{
		ProfileID:      defaults.DefaultProfileID,
		ProfileVersion: defaults.DefaultProfileVersion,
		ProfileDigest:  defaults.DefaultProfileDigest,
		CoverageFormat: defaults.DefaultCoverageFormat,
		CoveragePath:   defaults.DefaultCoveragePath,
		MinCoverage:    defaults.DefaultMinCoverage,
	}
	if err := validateResolvedValidationPolicy(policy); err != nil {
		return validationPolicy{}, err
	}
	return policy, nil
}

// ValidateTaskTestRequirements rejects arbitrary command material at API/MCP
// boundaries. Empty input is allowed at creation time only because trusted
// project defaults may provide the complete policy; submission still fails if
// neither source yields a complete policy.
func ValidateTaskTestRequirements(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	policy, err := decodeValidationPolicy(trimmed)
	if err != nil {
		return err
	}
	return validateResolvedValidationPolicy(policy)
}

// ValidateProjectConfig prevents legacy command strings from being persisted
// through any transport and validates a complete set of profile defaults when
// one is configured.
func ValidateProjectConfig(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return fmt.Errorf("project config is invalid JSON: %w", err)
	}
	if _, exists := fields["default_test_command"]; exists {
		return fmt.Errorf("default_test_command is forbidden; use default_command_profile_id/version/digest")
	}
	if _, exists := fields["allowed_test_commands"]; exists {
		return fmt.Errorf("allowed_test_commands is forbidden; profiles are server-owned")
	}
	var defaults projectValidationDefaults
	if err := json.Unmarshal(trimmed, &defaults); err != nil {
		return fmt.Errorf("project validation defaults are invalid: %w", err)
	}
	configured := defaults.DefaultProfileID != "" || defaults.DefaultProfileVersion != "" || defaults.DefaultProfileDigest != "" ||
		defaults.DefaultCoverageFormat != "" || defaults.DefaultCoveragePath != "" || defaults.DefaultMinCoverage != nil
	if !configured {
		return nil
	}
	return validateResolvedValidationPolicy(validationPolicy{
		ProfileID:      defaults.DefaultProfileID,
		ProfileVersion: defaults.DefaultProfileVersion,
		ProfileDigest:  defaults.DefaultProfileDigest,
		CoverageFormat: defaults.DefaultCoverageFormat,
		CoveragePath:   defaults.DefaultCoveragePath,
		MinCoverage:    defaults.DefaultMinCoverage,
	})
}

func decodeValidationPolicy(raw []byte) (validationPolicy, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return validationPolicy{}, fmt.Errorf("test_requirements is invalid JSON: %w", err)
	}
	if _, exists := fields["command"]; exists {
		return validationPolicy{}, fmt.Errorf("test_requirements.command is forbidden; reference an approved command profile")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var policy validationPolicy
	if err := decoder.Decode(&policy); err != nil {
		return validationPolicy{}, fmt.Errorf("test_requirements schema is invalid: %w", err)
	}
	if err := ensureJSONDecoderEOF(decoder); err != nil {
		return validationPolicy{}, err
	}
	return policy, nil
}

func validateResolvedValidationPolicy(policy validationPolicy) error {
	if !commandProfileIDRe.MatchString(policy.ProfileID) || !commandProfileVersionRe.MatchString(policy.ProfileVersion) {
		return fmt.Errorf("profile_id/profile_version is missing or invalid")
	}
	if !imageDigestRe.MatchString(policy.ProfileDigest) {
		return fmt.Errorf("profile_digest is missing or invalid")
	}
	if policy.CoverageFormat != "go-cover" && policy.CoverageFormat != "cobertura" && policy.CoverageFormat != "jacoco" && policy.CoverageFormat != "istanbul" {
		return fmt.Errorf("coverage_format is missing or unsupported")
	}
	coveragePath, err := normalizeRepositoryPath(policy.CoveragePath, false)
	if err != nil || isSystemPath(coveragePath) {
		return fmt.Errorf("coverage_path is missing or unsafe")
	}
	if policy.MinCoverage == nil || math.IsNaN(*policy.MinCoverage) || math.IsInf(*policy.MinCoverage, 0) || *policy.MinCoverage < 80 || *policy.MinCoverage > 100 {
		return fmt.Errorf("min_coverage must be within the enforced 80..100 range")
	}
	return nil
}

func ensureJSONDecoderEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains multiple values")
		}
		return fmt.Errorf("JSON trailing data: %w", err)
	}
	return nil
}

func validationProfileReference(policy validationPolicy) string {
	return strings.Join([]string{policy.ProfileID, policy.ProfileVersion, policy.ProfileDigest}, "@")
}
