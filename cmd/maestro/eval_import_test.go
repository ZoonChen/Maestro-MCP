package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/eval"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The M4-EVAL-001 ingest surface. Non-PG usage tests run everywhere;
// the three ingest behaviors (legal / illegal / duplicate) and the
// harness round-trip are PG-gated, matching the store's evaluation
// tests. eval-import is fail-closed off PostgreSQL.

func TestEvalImportUsageFailures(t *testing.T) {
	t.Setenv("MAESTRO_DB_DRIVER", "sqlite")
	t.Setenv("MAESTRO_DATABASE_DSN", "")
	project := "018f7e00-0000-7000-8000-0000000000e2"
	jsonL := `{"schema_version":"3.0","layer":"quality","case_id":"c","run_id":"018f7e00-0000-7000-8000-00000000e101","dataset_digest":"sha256:` + strings.Repeat("a", 64) + `","verdict":"pass","score":100,"version":1}`
	file := filepath.Join(t.TempDir(), "run.jsonl")
	require.NoError(t, os.WriteFile(file, []byte(jsonL+"\n"), 0o600))

	for name, tc := range map[string]struct {
		args     []string
		exitCode int
		code     string
		message  string
	}{
		"missing file":   {args: []string{"eval-import", "--project", project}, exitCode: exitUsage, code: "USAGE_ERROR", message: "--file PATH is required"},
		"bad project":    {args: []string{"eval-import", "--file", file, "--project", "not-a-uuid"}, exitCode: exitUsage, code: "USAGE_ERROR", message: "--project must be a uuid"},
		"not postgres":   {args: []string{"eval-import", "--file", file, "--project", project}, exitCode: exitUsage, code: "CONFIG_INVALID", message: "requires the postgres database configuration"},
		"stray argument": {args: []string{"eval-import", "--file", file, "--project", project, "extra"}, exitCode: exitUsage, code: "USAGE_ERROR", message: "unexpected eval-import arguments"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := execute(context.Background(), tc.args, streams{in: strings.NewReader(""), out: &stdout, err: &stderr})
			require.Equal(t, tc.exitCode, code, stderr.String())
			assert.Contains(t, stderr.String(), "code="+tc.code)
			assert.Contains(t, stderr.String(), tc.message)
			assert.Empty(t, stdout.String(), "a rejected import writes no summary")
		})
	}

	var stdout, stderr bytes.Buffer
	assert.Equal(t, exitOK, execute(context.Background(), []string{"eval-import", "--help"}, streams{in: strings.NewReader(""), out: &stdout, err: &stderr}))
	assert.Contains(t, stderr.String(), "records.jsonl", "flag usage goes to the error stream")
}

// setupEvalImportDB provisions a migrated throwaway database with one
// team/project pair the records can reference (project_id carries a
// foreign key). Returns the store and the target DSN.
func setupEvalImportDB(t *testing.T, databaseName string) (*store.PostgresStore, string) {
	t.Helper()
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, databaseName))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, databaseName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, databaseName))
		_ = admin.Close()
	})
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	targetDSN := dsn[:strings.LastIndex(dsn, "/")+1] + databaseName
	db, err := store.OpenPostgres(context.Background(), targetDSN)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `INSERT INTO teams (id, name) VALUES ('018f7e00-0000-7000-8000-0000000000e1', 'eval-import')`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO projects (id, team_id, key, name, status) VALUES ('018f7e00-0000-7000-8000-0000000000e2', '018f7e00-0000-7000-8000-0000000000e1', 'evalimp', 'EVAL-IMPORT', 'active')`)
	require.NoError(t, err)
	return pg, targetDSN
}

const evalImportProject = "018f7e00-0000-7000-8000-0000000000e2"

func caseIDs(records []eval.Record) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.CaseID)
	}
	return ids
}

func evalImportRecordLine(layer, caseID, verdict, notes string) string {
	risk := ""
	if layer == "security" {
		risk = `,"risk":"elevated"`
	}
	noteField := ""
	if notes != "" {
		noteField = fmt.Sprintf(`,"notes":%q`, notes)
	}
	return fmt.Sprintf(`{"schema_version":"3.0","layer":%q,"case_id":%q,"run_id":"018f7e00-0000-7000-8000-00000000e101","dataset_digest":"sha256:%s","verdict":%q,"score":100,"version":1%s,"scorer":{"kind":"rule_based","version":"v1"}%s}`,
		layer, caseID, strings.Repeat("a", 64), verdict, risk, noteField)
}

func TestEvalImportIngestBehaviors(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	pg, targetDSN := setupEvalImportDB(t, "maestro_eval_import_cmd_test")
	t.Setenv("MAESTRO_DB_DRIVER", "postgres")
	t.Setenv("MAESTRO_DATABASE_DSN", targetDSN)

	legalFile := filepath.Join(t.TempDir(), "legal.jsonl")
	legalJSONL := strings.Join([]string{
		evalImportRecordLine("quality", "case-pass", "pass", ""),
		evalImportRecordLine("quality", "case-fail", "fail", "two assertions missed the fixed error code"),
		evalImportRecordLine("security", "inj-refused", "pass", ""),
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(legalFile, []byte(legalJSONL), 0o600))

	runImport := func(args ...string) (int, evalImportSummary, string) {
		var stdout, stderr bytes.Buffer
		code := execute(context.Background(), append([]string{"eval-import"}, args...), streams{
			in: strings.NewReader(""), out: &stdout, err: &stderr})
		var summary evalImportSummary
		if code == exitOK {
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &summary), stdout.String())
		}
		return code, summary, stderr.String()
	}

	// Legal: every valid line persists once and the read-back counts match.
	code, summary, stderr := runImport("--file", legalFile, "--project", evalImportProject, "--json")
	require.Equal(t, exitOK, code, stderr)
	assert.Equal(t, 3, summary.Lines)
	assert.Equal(t, 3, summary.Appended)
	assert.Equal(t, 0, summary.DuplicatesSkipped)
	require.Len(t, summary.Runs, 1)
	assert.Equal(t, map[string]int64{"pass": 2, "fail": 1}, summary.Runs[0].VerdictCounts)

	records, err := pg.Evaluation().RecordsByRun(context.Background(), "018f7e00-0000-7000-8000-00000000e101", eval.LayerQuality)
	require.NoError(t, err)
	require.Len(t, records, 2)
	for _, record := range records {
		assert.Equal(t, evalImportProject, record.ProjectID, "--project stamps the owning project")
	}

	// Illegal: one invalid line rejects the whole file — nothing from it
	// persists, not even the valid lines before it.
	illegalFile := filepath.Join(t.TempDir(), "illegal.jsonl")
	illegalJSONL := strings.Join([]string{
		evalImportRecordLine("quality", "case-other", "pass", ""),
		strings.Replace(evalImportRecordLine("security", "inj-no-risk", "pass", ""), `,"risk":"elevated"`, "", 1),
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(illegalFile, []byte(illegalJSONL), 0o600))
	code, _, stderr = runImport("--file", illegalFile, "--project", evalImportProject, "--json")
	require.Equal(t, exitInternal, code, stderr)
	assert.Contains(t, stderr, "code=IMPORT_REJECTED")
	assert.Contains(t, stderr, "line 2")
	assert.Contains(t, stderr, "security records require")
	counts, err := pg.Evaluation().VerdictCounts(context.Background(), "018f7e00-0000-7000-8000-00000000e101")
	require.NoError(t, err)
	assert.Equal(t, int64(3), counts[eval.VerdictPass]+counts[eval.VerdictFail], "the rejected file contributed zero rows")

	// Malformed JSON and unknown properties are rejected the same way.
	for name, line := range map[string]string{
		"not json":        `{"schema_version":`,
		"unknown field":   `{"schema_version":"3.0","layer":"quality","case_id":"x","run_id":"018f7e00-0000-7000-8000-00000000e101","dataset_digest":"sha256:` + strings.Repeat("a", 64) + `","verdict":"pass","score":1,"version":1,"owner":"someone"}`,
		"fail no notes":   evalImportRecordLine("quality", "case-nonotes", "fail", ""),
		"digest violated": `{"schema_version":"3.0","layer":"quality","case_id":"x","run_id":"018f7e00-0000-7000-8000-00000000e101","dataset_digest":"md5:abc","verdict":"pass","score":1,"version":1}`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			badFile := filepath.Join(t.TempDir(), "bad.jsonl")
			require.NoError(t, os.WriteFile(badFile, []byte(line+"\n"), 0o600))
			var stdout, stderr2 bytes.Buffer
			importCode := execute(context.Background(), []string{"eval-import", "--file", badFile, "--project", evalImportProject, "--json"}, streams{
				in: strings.NewReader(""), out: &stdout, err: &stderr2})
			require.Equal(t, exitInternal, importCode, stderr2.String())
			assert.Contains(t, stderr2.String(), "code=IMPORT_REJECTED")
			assert.Contains(t, stderr2.String(), "line 1")
			assert.Empty(t, stdout.String())
		})
	}

	// Duplicate: re-importing the same file skips every (run, case,
	// layer) key and leaves the counts unchanged.
	code, summary, stderr = runImport("--file", legalFile, "--project", evalImportProject, "--json")
	require.Equal(t, exitOK, code, stderr)
	assert.Equal(t, 3, summary.Lines)
	assert.Equal(t, 0, summary.Appended)
	assert.Equal(t, 3, summary.DuplicatesSkipped)
	assert.Equal(t, map[string]int64{"pass": 2, "fail": 1}, summary.Runs[0].VerdictCounts)

	// Trials: the harness emits repeated trials under the case's dataset
	// id — every occurrence persists, disambiguated by its ordinal, and
	// re-import stays idempotent.
	trialsFile := filepath.Join(t.TempDir(), "trials.jsonl")
	trialsJSONL := strings.Join([]string{
		evalImportRecordLine("quality", "case-trials", "pass", ""),
		evalImportRecordLine("quality", "case-trials", "pass", ""),
		evalImportRecordLine("quality", "case-trials", "fail", "one flaky trial"),
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(trialsFile, []byte(trialsJSONL), 0o600))
	code, summary, stderr = runImport("--file", trialsFile, "--project", evalImportProject, "--json")
	require.Equal(t, exitOK, code, stderr)
	assert.Equal(t, 3, summary.Appended)
	assert.Equal(t, 0, summary.DuplicatesSkipped)
	assert.Equal(t, 2, summary.TrialsDisambiguated)
	records, err = pg.Evaluation().RecordsByRun(context.Background(), "018f7e00-0000-7000-8000-00000000e101", eval.LayerQuality)
	require.NoError(t, err)
	assert.Equal(t, []string{"case-fail", "case-pass", "case-trials", "case-trials#t2", "case-trials#t3"},
		caseIDs(records), "trial ordinals project onto distinct store keys")
	code, summary, stderr = runImport("--file", trialsFile, "--project", evalImportProject, "--json")
	require.Equal(t, exitOK, code, stderr)
	assert.Equal(t, 0, summary.Appended)
	assert.Equal(t, 3, summary.DuplicatesSkipped)
	assert.Equal(t, 2, summary.TrialsDisambiguated)

	// The human summary carries the same facts for operator evidence.
	var humanOut, humanErr bytes.Buffer
	code = execute(context.Background(), []string{"eval-import", "--file", legalFile, "--project", evalImportProject}, streams{
		in: strings.NewReader(""), out: &humanOut, err: &humanErr})
	require.Equal(t, exitOK, code, humanErr.String())
	assert.Contains(t, humanOut.String(), "lines=3 appended=0 duplicates=3 trials_disambiguated=0")
	assert.Contains(t, humanOut.String(), "fail=2 pass=4", "read-back counts cover the whole run, not just this file")
}

// TestEvalImportRoundTrip closes M4-EVAL-001's ingest loop against a
// fixture produced by the real tests/eval harness (datasets/seed.json,
// 24 trials): the local JSONL tally equals the store's read-back
// VerdictCounts for the imported run.
func TestEvalImportRoundTrip(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	pg, targetDSN := setupEvalImportDB(t, "maestro_eval_roundtrip_test")
	t.Setenv("MAESTRO_DB_DRIVER", "postgres")
	t.Setenv("MAESTRO_DATABASE_DSN", targetDSN)

	fixture := filepath.Join("testdata", "eval", "records.jsonl")
	raw, err := os.ReadFile(fixture)
	require.NoError(t, err)
	localTally := map[string]int64{}
	runID := ""
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		record, parseErr := eval.ParseRecord([]byte(line))
		require.NoError(t, parseErr)
		localTally[string(record.Verdict)]++
		require.NotEmpty(t, record.RunID)
		if runID == "" {
			runID = record.RunID
		}
		require.Equal(t, runID, record.RunID, "the harness fixture is one run")
	}
	require.Equal(t, int64(24), localTally["pass"]+localTally["fail"]+localTally["error"])

	var stdout, stderr bytes.Buffer
	code := execute(context.Background(), []string{"eval-import", "--file", fixture, "--project", evalImportProject, "--json"}, streams{
		in: strings.NewReader(""), out: &stdout, err: &stderr})
	require.Equal(t, exitOK, code, stderr.String())

	var summary evalImportSummary
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &summary))
	assert.Equal(t, 24, summary.Appended)
	require.Len(t, summary.Runs, 1)
	assert.Equal(t, runID, summary.Runs[0].RunID)
	assert.Equal(t, localTally, summary.Runs[0].VerdictCounts)

	readBack, err := pg.Evaluation().VerdictCounts(context.Background(), runID)
	require.NoError(t, err)
	normalized := map[string]int64{}
	for verdict, count := range readBack {
		normalized[string(verdict)] = count
	}
	assert.Equal(t, localTally, normalized, "store VerdictCounts equals the local JSONL tally")
}
