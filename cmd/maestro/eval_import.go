package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ZoonChen/Maestro-MCP/internal/eval"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// runEvalImport wires the harness JSONL output into evaluation_records
// (M4-EVAL-001 ingest). Two phases so a bad file never half-imports:
// every line is parsed and wire-validated first, then appended. The
// store is unique per (run, case, layer) while the harness emits one
// record per trial under the case's dataset id, so repeated keys in a
// file are that case's trials: each occurrence after the first is
// stored under a deterministic "#t<k>" suffix (the ingest projection
// of the trial ordinal — the wire itself carries no trial field). A
// key that already exists in the store is a duplicate — counted and
// skipped, never an update; the post-import verdict counts are read
// back from the store as the round-trip evidence. One file per run is
// the expected shape; PostgreSQL-only, fail-closed otherwise.
func runEvalImport(ctx context.Context, args []string, ioStreams streams) error {
	fs := flag.NewFlagSet("eval-import", flag.ContinueOnError)
	fs.SetOutput(ioStreams.err)
	var common commonFlags
	bindCommonFlags(fs, &common)
	var filePath, projectID string
	var jsonOutput bool
	fs.StringVar(&filePath, "file", "", "harness records.jsonl to import (one frozen-wire record per line)")
	fs.StringVar(&projectID, "project", "", "owning project uuid (the wire carries no project_id)")
	fs.BoolVar(&jsonOutput, "json", false, "write a machine-readable import summary")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fail(exitUsage, "USAGE_ERROR", err)
	}
	if fs.NArg() != 0 {
		return fail(exitUsage, "USAGE_ERROR", fmt.Errorf("unexpected eval-import arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if strings.TrimSpace(filePath) == "" {
		return fail(exitUsage, "USAGE_ERROR", errors.New("--file PATH is required"))
	}
	if _, err := uuid.Parse(strings.TrimSpace(projectID)); err != nil {
		return fail(exitUsage, "USAGE_ERROR", errors.New("--project must be a uuid"))
	}
	projectID = strings.TrimSpace(projectID)

	cfg, err := loadRuntimeConfig(common)
	if err != nil {
		return err
	}
	configureLogger(cfg, ioStreams.err)
	if !cfg.PostgresEnabled() {
		return fail(exitUsage, "CONFIG_INVALID", errors.New("eval-import requires the postgres database configuration"))
	}

	records, err := parseRecordFile(filePath)
	if err != nil {
		return fail(exitInternal, "IMPORT_REJECTED", err)
	}

	database, err := store.OpenPostgres(ctx, cfg.Database.DSN)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}
	defer database.Close()
	if err := store.ValidatePostgresSchema(ctx, database); err != nil {
		return postgresMigrationFailure(err)
	}
	pgStore, err := store.NewPostgresStore(database)
	if err != nil {
		return fail(exitDependency, "DEPENDENCY_UNAVAILABLE", err)
	}

	storeEval := pgStore.Evaluation()
	appended, duplicates, disambiguated := 0, 0, 0
	runOrder := []string{}
	seenKeys := make(map[string]int, len(records))
	for _, record := range records {
		runOrder = appendRunID(runOrder, record.RunID)
		key := record.RunID + "\x00" + record.CaseID + "\x00" + string(record.Layer)
		seenKeys[key]++
		if seenKeys[key] > 1 {
			record.CaseID = trialCaseID(record.CaseID, seenKeys[key])
			disambiguated++
		}
		record.ProjectID = projectID
		if _, appendErr := storeEval.AppendRecord(ctx, record); appendErr != nil {
			if errors.Is(appendErr, store.ErrEvaluationDuplicate) {
				duplicates++
				continue
			}
			return fail(exitInternal, "IMPORT_FAILED", appendErr)
		}
		appended++
	}

	runs := make([]evalImportRunSummary, 0, len(runOrder))
	for _, runID := range runOrder {
		counts, countsErr := storeEval.VerdictCounts(ctx, runID)
		if countsErr != nil {
			return fail(exitInternal, "IMPORT_FAILED", countsErr)
		}
		wire := make(map[string]int64, len(counts))
		for verdict, count := range counts {
			wire[string(verdict)] = count
		}
		runs = append(runs, evalImportRunSummary{RunID: runID, VerdictCounts: wire})
	}

	summary := evalImportSummary{
		File: filePath, Project: projectID,
		Lines: len(records), Appended: appended, DuplicatesSkipped: duplicates,
		TrialsDisambiguated: disambiguated,
		Runs:                runs,
	}
	if jsonOutput {
		encoder := json.NewEncoder(ioStreams.out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(summary); err != nil {
			return fail(exitInternal, "IMPORT_FAILED", err)
		}
		return nil
	}
	fmt.Fprintf(ioStreams.out, "eval-import complete file=%s project=%s lines=%d appended=%d duplicates=%d trials_disambiguated=%d\n",
		summary.File, summary.Project, summary.Lines, summary.Appended, summary.DuplicatesSkipped, summary.TrialsDisambiguated)
	for _, run := range runs {
		fmt.Fprintf(ioStreams.out, "run %s: %s\n", run.RunID, formatVerdictCounts(run.VerdictCounts))
	}
	return nil
}

type evalImportRunSummary struct {
	RunID         string           `json:"run_id"`
	VerdictCounts map[string]int64 `json:"verdict_counts"`
}

type evalImportSummary struct {
	File                string                 `json:"file"`
	Project             string                 `json:"project"`
	Lines               int                    `json:"lines"`
	Appended            int                    `json:"appended"`
	DuplicatesSkipped   int                    `json:"duplicates_skipped"`
	TrialsDisambiguated int                    `json:"trials_disambiguated"`
	Runs                []evalImportRunSummary `json:"runs"`
}

// trialCaseID projects the k-th occurrence of a (run, case, layer) key
// onto the store's case_id budget: "#t<k>" appended, base truncated
// from the right when the wire's 128-char limit would be exceeded (by
// runes — the wire counts characters, not bytes). Deterministic per
// file, so re-imports land on the same keys.
func trialCaseID(base string, occurrence int) string {
	suffix := fmt.Sprintf("#t%d", occurrence)
	if utf8.RuneCountInString(base)+len(suffix) > 128 {
		base = string([]rune(base)[:128-utf8.RuneCountInString(suffix)])
	}
	return base + suffix
}

// parseRecordFile reads the whole file through parse + wire validation
// before anything persists; blank lines are ignored, every other line
// must be one frozen-wire record (1-based line numbers in errors).
func parseRecordFile(path string) ([]eval.Record, error) {
	file, err := os.Open(path) // #nosec G304 -- the operator names the file to import
	if err != nil {
		return nil, fmt.Errorf("open records file: %w", err)
	}
	defer file.Close()

	records := []eval.Record{}
	scanner := bufio.NewScanner(file)
	// Trial records carry bounded-but-large arrays (notes ≤ 4000 chars,
	// constraint/action lists ≤ 200 entries); 1 MiB is headroom, not a limit.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		record, parseErr := eval.ParseRecord(line)
		if parseErr != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, lineNumber, parseErr)
		}
		if validateErr := record.Validate(); validateErr != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, lineNumber, validateErr)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read records file: %w", err)
	}
	return records, nil
}

// appendRunID keeps the first-seen order without duplicates so the
// summary's runs array is deterministic.
func appendRunID(order []string, runID string) []string {
	for _, existing := range order {
		if existing == runID {
			return order
		}
	}
	return append(order, runID)
}

func formatVerdictCounts(counts map[string]int64) string {
	verdicts := make([]string, 0, len(counts))
	for verdict := range counts {
		verdicts = append(verdicts, verdict)
	}
	sort.Strings(verdicts)
	parts := make([]string, 0, len(verdicts))
	for _, verdict := range verdicts {
		parts = append(parts, fmt.Sprintf("%s=%d", verdict, counts[verdict]))
	}
	return strings.Join(parts, " ")
}
