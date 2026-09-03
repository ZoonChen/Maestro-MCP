package defect

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fingerprint identity: run-specific noise must not fork the defect
// space; identity-relevant parts must.
func TestFingerprintIdentity(t *testing.T) {
	base := FingerprintInput{
		ProjectID: "p1", Repository: "acme/orders", Branch: "main",
		CheckID:        "TestCheckout",
		ErrorSignature: "checkout failed\n  at server.go:42 (dial 10.0.0.7:5432)\n  goroutine 7 [running]",
	}

	// Noise-only differences collapse.
	noisy := base
	noisy.ErrorSignature = "checkout failed\n  at server.go:42 (dial 10.0.0.9:5432)\n  goroutine 912 [running]\n  2026-09-02T10:00:00Z 3s"
	assert.Equal(t, Fingerprint(base), Fingerprint(noisy), "addresses/IPs/goroutine ids/timestamps/durations are noise")

	// Real differences fork.
	for name, mutate := range map[string]func(*FingerprintInput){
		"branch":     func(in *FingerprintInput) { in.Branch = "release" },
		"check":      func(in *FingerprintInput) { in.CheckID = "TestPayment" },
		"signature":  func(in *FingerprintInput) { in.ErrorSignature = "payment declined\n  at pay.go:1" },
		"repository": func(in *FingerprintInput) { in.Repository = "acme/billing" },
	} {
		variant := base
		mutate(&variant)
		assert.NotEqual(t, Fingerprint(base), Fingerprint(variant), name)
	}

	// Path normalization: absolute and ./-prefixed forms are one identity.
	assert.Equal(t,
		Fingerprint(FingerprintInput{CheckID: "/build/tests/x_test.go"}),
		Fingerprint(FingerprintInput{CheckID: "build/tests/x_test.go"}),
		"absolute paths collapse to relative")
}

func TestNormalizeSignature(t *testing.T) {
	stable := NormalizeSignature(
		"Error: boom\n" +
			"    at main.run() 0x104abcf0\n" +
			"    2026-09-02T10:00:00.123Z dial 10.1.2.3:5432 timeout after 3s\n" +
			"    goroutine 42 [running]:")
	assert.Contains(t, stable, "Error: boom")
	assert.NotContains(t, stable, "0x104abcf0")
	assert.NotContains(t, stable, "10.1.2.3")
	assert.NotContains(t, stable, "2026-09-02")
	assert.NotContains(t, stable, "3s")
	assert.NotContains(t, stable, "goroutine 42")
	assert.Contains(t, stable, "goroutine N")
	assert.Contains(t, stable, "main.run()")
}

func TestAdaptersNormalizeAndFailClosed(t *testing.T) {
	t.Run("pipeline", func(t *testing.T) {
		finding, input, err := FromPipeline(PipelineFinding{
			ProjectID: "p", Repository: "r", Branch: "main",
			JobName: "unit", Stage: "test", LogExcerpt: "FAIL: TestX",
			ExitCode: 1, PipelineID: "50", JobID: "500", SourceSHA: "abc123",
		})
		require.NoError(t, err)
		assert.Equal(t, SourcePipeline, finding.SourceType)
		assert.Equal(t, SeverityHigh, finding.Severity)
		assert.Equal(t, "pipeline:50:job:500", finding.SourceEventID)
		assert.Equal(t, "unit", input.CheckID)

		_, _, err = FromPipeline(PipelineFinding{JobName: "x", PipelineID: "1", JobID: "2", ExitCode: 0})
		require.Error(t, err, "exit 0 is not a failure")
		_, _, err = FromPipeline(PipelineFinding{ExitCode: 1})
		require.Error(t, err)
	})

	t.Run("junit", func(t *testing.T) {
		finding, input, err := FromJUnit(JUnitFinding{
			ProjectID: "p", Repository: "r", Branch: "main",
			Suite: "s", TestClass: "./pkg/checkout_test", TestName: "TestRetry",
			Message: "expected 200, got 500",
		})
		require.NoError(t, err)
		assert.Equal(t, SourceJUnit, finding.SourceType)
		assert.Equal(t, "pkg/checkout_test.TestRetry", input.CheckID, "class/name with ./ stripped")

		_, _, err = FromJUnit(JUnitFinding{TestClass: "x"})
		require.Error(t, err)
	})

	t.Run("contract", func(t *testing.T) {
		finding, input, err := FromContract(ContractFinding{
			ProjectID: "p", Repository: "r", Branch: "main",
			Service: "orders", Location: "responses.200.properties.count",
			Detail: "schema type changed", Provider: "backend", Consumer: "web",
		})
		require.NoError(t, err)
		assert.Equal(t, SourceContract, finding.SourceType)
		assert.Equal(t, "contract:orders:responses.200.properties.count", input.CheckID)

		_, _, err = FromContract(ContractFinding{Service: "orders"})
		require.Error(t, err)
	})

	t.Run("sast", func(t *testing.T) {
		finding, input, err := FromScan(ScanFinding{
			ProjectID: "p", Repository: "r", Branch: "main",
			Tool: "semgrep", RuleID: "go-sql-injection",
			FilePath: "internal/db/query.go", Line: 42, Excerpt: "db.Query(userInput)",
		})
		require.NoError(t, err)
		assert.Equal(t, SourceSAST, finding.SourceType)
		assert.Equal(t, SeverityHigh, finding.Severity)
		assert.Equal(t, "semgrep:go-sql-injection", input.CheckID, "sast rules are rule-scoped, not path-scoped")
	})

	t.Run("secret is path-scoped and critical", func(t *testing.T) {
		finding, input, err := FromScan(ScanFinding{
			ProjectID: "p", Repository: "r", Branch: "main",
			Tool: "gitleaks", RuleID: "aws-access-key",
			FilePath: "deploy/prod.env", IsSecret: true,
			Excerpt: "AKIA[REDACTED]",
		})
		require.NoError(t, err)
		assert.Equal(t, SourceSecret, finding.SourceType)
		assert.Equal(t, SeverityCritical, finding.Severity)
		assert.Equal(t, "gitleaks:aws-access-key:deploy/prod.env", input.CheckID,
			"the same leaked rule at a new path is a new exposure")
		// The prefix label stays (it identifies the credential TYPE);
		// the material after it is gone.
		assert.Equal(t, "AKIA[REDACTED]", finding.Repro)
	})

	t.Run("manual qa requires repro", func(t *testing.T) {
		_, _, err := FromManualQA(ManualQAFinding{Reporter: "qa", Title: "x", Severity: SeverityLow})
		require.Error(t, err, "repro is mandatory")

		finding, _, err := FromManualQA(ManualQAFinding{
			ProjectID: "p", Reporter: "qa", Title: "checkout hangs",
			Repro: "steps 1..3", Severity: SeverityMedium,
		})
		require.NoError(t, err)
		assert.Equal(t, SourceManualQA, finding.SourceType)
		assert.Contains(t, finding.SourceEventID, "manual:qa:")
	})
}

func TestAggregateSeverity(t *testing.T) {
	severity, err := AggregateSeverity([]Severity{SeverityLow, SeverityCritical, SeverityMedium})
	require.NoError(t, err)
	assert.Equal(t, SeverityCritical, severity, "a defect is as severe as its worst finding")

	_, err = AggregateSeverity(nil)
	require.Error(t, err)
	_, err = AggregateSeverity([]Severity{SeverityLow, "extreme"})
	require.Error(t, err)
}
