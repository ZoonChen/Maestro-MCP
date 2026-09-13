// report-register (task brief S2C, slice S2C-4) files one shadow-phase
// weekly report as an INTERNAL ledger asset: retrospective class,
// digest+pointer mode, draft (register-only — recurring operational
// records carry no gate or signoff; the exit package is the dual-signed
// retrospective).
//
// It drives the REAL stdio MCP tool face with the bom-import (S2A)
// composition: the maestro substrate binary is always BUILT from this
// checkout into a temp dir the builder owns; the runner gets a throwaway
// SQLite baseline (migrate up) plus the live pilot PG overlay
// (MAESTRO_DATABASE_DSN); asset_register runs under an author-principal
// runner scoped to the peixun BOM 治理域 project. Registration is
// idempotent per week (project-namespaced idempotency key, W5-3), so a
// re-run of the same week replays to the same ledger entry.
//
// Env:
//
//	MAESTRO_TEST_POSTGRES_DSN  live pilot PG DSN (the maestro database)
//
// Run:
//
//	go run ./scripts/pilot/report-register --report deploy/gitlab/peixun/reports/shadow-report-W1-20260913.md --week 1
//
// (shadow-report.sh prints the pairing hint at the end of every run.)
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcp "github.com/mark3labs/mcp-go/mcp"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultProject = "018fb5b0-0000-7000-8000-000000000006" // peixun BOM 治理域
	assetIDPattern = `^ART-[a-z-]+-[0-9]{3,}$`              // frozen store rule (S2B F7 lesson: fail fast, with the rule in the error)
)

var assetIDRe = regexp.MustCompile(assetIDPattern)

type registration struct {
	AssetID     string
	Title       string
	ContentRef  string
	Digest      string
	SummaryJSON string // one JSON document serialized as a string
}

func main() {
	report := flag.String("report", "", "weekly report markdown path (required, inside this repo)")
	week := flag.Int("week", 0, "week number N of the report (required, >=1)")
	assetID := flag.String("asset-id", "", fmt.Sprintf("asset id (default ART-shadow-report-%%03d; must match %s)", assetIDPattern))
	title := flag.String("title", "", "ledger title (default: 影子期周报 W<N>（内部观察记录）)")
	dryRun := flag.Bool("dry-run", false, "print the registration payload and exit (no runner, no writes)")
	supersedes := flag.Bool("supersedes", false, "register a NEW version on top of the existing latest (digest changed); the tool contract requires supersedes_ref for any version > 1")
	flag.Parse()

	if *report == "" || *week < 1 {
		fatal("--report and --week (>=1) are required")
	}
	if *assetID == "" {
		*assetID = fmt.Sprintf("ART-shadow-report-%03d", *week)
	}

	reg, err := assemble(*report, *week, *assetID, *title)
	if err != nil {
		fatal("assemble: %v", err)
	}
	payload, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		fatal("marshal payload: %v", err)
	}
	fmt.Println(string(payload))
	if *dryRun {
		return
	}

	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		fatal("MAESTRO_TEST_POSTGRES_DSN must point at the LIVE pilot maestro database")
	}
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")

	// Replay short-circuit (read-only store check, the bom-import read
	// posture): the asset_register contract has no key replay — same
	// asset_id re-register means a version bump. A same-week re-run with
	// an UNCHANGED digest is a replay and must not error out.
	var supersedesRef string
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fatal("open pg: %v", err)
	}
	var latestVersion int
	var latestDigest string
	scanErr := db.QueryRow(
		"SELECT version, source_digest FROM assets WHERE project_id=$1 AND asset_id=$2 ORDER BY version DESC LIMIT 1",
		defaultProject, reg.AssetID).Scan(&latestVersion, &latestDigest)
	_ = db.Close()
	switch {
	case scanErr == nil && latestDigest == reg.Digest:
		fmt.Printf("already registered: %s@%d (digest match, replay)\n", reg.AssetID, latestVersion)
		return
	case scanErr == nil && !*supersedes:
		fatal("%s@%d exists with a different digest; re-run with --supersedes to register the new content as v%d", reg.AssetID, latestVersion, latestVersion+1)
	case scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows):
		fatal("latest-asset read: %v", scanErr)
	}
	if scanErr == nil && *supersedes {
		supersedesRef = fmt.Sprintf("%s@%d", reg.AssetID, latestVersion)
	}

	runner, err := startRunner(defaultProject)
	if err != nil {
		fatal("runner: %v", err)
	}
	defer runner.close()

	arguments := map[string]any{
		"asset_id":        reg.AssetID,
		"asset_type":      "retrospective",
		"title":           reg.Title,
		"sensitivity":     "internal",
		"source_digest":   reg.Digest,
		"content_ref":     reg.ContentRef,
		"summary":         reg.SummaryJSON,
		"idempotency_key": fmt.Sprintf("s2c-shadow-report-w%d-register", *week),
	}
	if supersedesRef != "" {
		arguments["supersedes_ref"] = supersedesRef
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := runner.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "asset_register", Arguments: arguments}})
	if err != nil {
		fatal("asset_register transport: %v", err)
	}
	var text strings.Builder
	for _, content := range result.Content {
		if typed, ok := content.(mcp.TextContent); ok {
			text.WriteString(typed.Text)
		}
	}
	if result.IsError {
		fatal("asset_register errored: %s", text.String())
	}
	fmt.Printf("registered: %s\n", text.String())
}

// assemble validates inputs and derives the registration payload. The
// digest covers the report file exactly as produced by shadow-report.sh;
// the content pointer stays repo-relative so the ledger entry survives
// worktree moves.
func assemble(reportPath string, week int, assetID, title string) (*registration, error) {
	if !assetIDRe.MatchString(assetID) {
		return nil, fmt.Errorf("asset id %q does not match the frozen rule %s (S2B F7: the rule is documented here because the team hit it)", assetID, assetIDPattern)
	}
	if title == "" {
		title = fmt.Sprintf("影子期周报 W%d（内部观察记录）", week)
	}
	raw, err := os.ReadFile(reportPath) // #nosec G703 -- the operator names the report
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	repoRoot, err := repoRoot()
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(reportPath)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(repoRoot, abs)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("report %s is outside the repo root %s", reportPath, repoRoot)
	}

	summary := map[string]any{
		"note":     "影子期周报（brief-S2C S2C-4）：每周报=internal 资产，digest 指向 scripts/pilot/shadow-report.sh 产物",
		"week":     week,
		"report":   filepath.ToSlash(rel),
		"sha256":   digest,
		"producer": "scripts/pilot/shadow-report.sh + scripts/pilot/report-register",
	}
	summaryDoc, err := json.Marshal(summary)
	if err != nil {
		return nil, err
	}
	summaryString, err := json.Marshal(string(summaryDoc)) // one JSON document serialized as a string
	if err != nil {
		return nil, err
	}
	return &registration{
		AssetID:     assetID,
		Title:       title,
		ContentRef:  filepath.ToSlash(rel),
		Digest:      digest,
		SummaryJSON: string(summaryString),
	}, nil
}

func repoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(cwd, "go.mod")); err == nil {
			return cwd, nil
		}
		parent := filepath.Dir(cwd)
		if parent == cwd {
			return "", errors.New("go.mod not found from cwd")
		}
		cwd = parent
	}
}

// ---------------------------------------------------------------------------
// The bom-import stdio runner composition, reduced to the one call this
// driver needs. SQLite baseline (throwaway, migrated fresh) + live PG
// overlay via MAESTRO_DATABASE_DSN; remote write stays off.
// ---------------------------------------------------------------------------

type stdioRunner struct {
	*mcpclient.Client
	command *exec.Cmd
	stdin   io.WriteCloser
	stderr  *bytes.Buffer
	done    chan error
}

// maestroCommand builds the argv for one maestro subprocess with the
// SAME security posture as the committed bom-import driver: exec.Command
// receives a literal name plus an argument LIST (no shell is ever
// involved), and the executable is ALWAYS the binary this driver just
// built into its own temp dir — cmd.Path is re-pointed explicitly so the
// process never resolves through PATH and no environment-supplied
// executable is honored (the recorded PATH-lookup error is cleared;
// Path is authoritative here).
func (r *registrar) maestroCommand(args ...string) *exec.Cmd {
	command := exec.Command("maestro", args...) // #nosec G204 -- literal name + argv list; Path re-pointed below
	command.Path = r.binary
	command.Err = nil
	return command
}

type registrar struct {
	binary string
}

func startRunner(projectID string) (*stdioRunner, error) {
	reg := &registrar{}
	repo, err := repoRoot()
	if err != nil {
		return nil, err
	}
	binDir, err := os.MkdirTemp("", "maestro-s2c-binary-")
	if err != nil {
		return nil, err
	}
	reg.binary = filepath.Join(binDir, "maestro")
	build := exec.Command("go", "build", "-trimpath", "-o", reg.binary, "./cmd/maestro") // #nosec G204 -- argv literals
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build maestro: %v: %s", err, out)
	}

	runnerID := "s2c-report-author"
	localDB := filepath.Join(os.TempDir(), "s2c-runner-"+runnerID+".db")
	configPath := filepath.Join(os.TempDir(), "s2c-runner-"+runnerID+".yaml")
	// Throwaway baseline: remove any stale local db so the baseline
	// migration is always fresh (the live state lives in PG, not here).
	if err := os.Remove(localDB); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if out, err := reg.maestroCommand("migrate", "up", "--db", localDB).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("migrate local baseline: %v: %s", err, out)
	}
	config := fmt.Sprintf("db_path: %q\nhttp_addr: \"127.0.0.1:0\"\nremote_write: false\ndatabase:\n  driver: postgres\n", localDB)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil { // #nosec G306 -- credentials-free runner config, matches bom-import
		return nil, err
	}

	child := reg.maestroCommand("runner", "--config", configPath, "--runner-id", runnerID, "--project", projectID)
	child.Env = append(os.Environ(),
		"MAESTRO_DATABASE_DSN="+os.Getenv("MAESTRO_TEST_POSTGRES_DSN"),
		"MAESTRO_REMOTE_WRITE=false",
	)
	stdin, err := child.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := child.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := child.Start(); err != nil {
		return nil, fmt.Errorf("start runner %s: %w", runnerID, err)
	}
	runner := &stdioRunner{command: child, stdin: stdin, stderr: &bytes.Buffer{}, done: make(chan error, 1)}
	go func() { _, _ = io.Copy(runner.stderr, stderrPipe); close(runner.done) }()
	runner.Client = mcpclient.NewClient(mcptransport.NewIO(stdout, stdin, io.NopCloser(bytes.NewReader(nil))))
	if err := runner.Start(context.Background()); err != nil {
		return nil, err
	}
	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "s2c-report-register", Version: "1.0.0"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runner.Initialize(ctx, initRequest); err != nil {
		return nil, fmt.Errorf("initialize %s: %w", runnerID, err)
	}
	return runner, nil
}

func (r *stdioRunner) close() {
	if r == nil {
		return
	}
	_ = r.Close()
	_ = r.command.Process.Kill()
	<-r.done
	if r.stderr.Len() > 0 {
		tail := r.stderr.String()
		if len(tail) > 400 {
			tail = "..." + tail[len(tail)-400:]
		}
		fmt.Fprintf(os.Stderr, "runner stderr tail: %s\n", tail)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "report-register: "+format+"\n", args...)
	os.Exit(1)
}
