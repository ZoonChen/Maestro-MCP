// bom-import (task brief S2A) turns the peixun BOM workbook into
// Maestro governance objects on the LIVE pilot control plane:
//
//	S2A-1  phase-1 44 entries → claimable work-item nodes under the
//	       sealed peixun-m1 milestone graph (letter/numbered domain
//	       packages, 17 intra-group requires edges, BOM consume flows)
//	S2A-2  every phase-1 flat work item bound to locked_gate "bom" →
//	       ART-bom-001@2 (draft-bind denial reproduced, then the
//	       approved re-registration with the true workbook digest)
//	S2A-3  phase-2/3 61 entries → blocked placeholder nodes on the
//	       peixun-m23 graph (brief said 46 — BOM errata, see
//	       DERIVATION.md) + blocked flat items; claim stays closed
//	S2A-4  manual Jira anchors for all 105 entries (blueprint
//	       fallback; Jira unreachable from this network) + the empty
//	       reconcile list
//	S2A-5  this program IS the import: idempotent, replay-safe, its
//	       replay source is the committed manifest whose digest is
//	       anchored into the ART-bom-001 ledger summary
//
// It is an OPERATIONAL importer, not a hermetic test: it writes the
// pilot control plane for real. Replay re-asserts the settled state.
//
// Env:
//
//	MAESTRO_TEST_POSTGRES_DSN  live pilot PG DSN (the maestro database)
//	S2A_PILOT_API              pilot server base URL (default :8080)
//	S2A_PILOT_TOKEN            fresh OIDC bearer (fetch-token.sh)
//	S2A_GITLAB_API / S2A_GITLAB_PAT   sandbox GitLab (baseline SHA;
//	                           PAT falls back to deploy/gitlab/.root-pat)
//	S2A_JIRA_BASE_URL          Jira re-probe target
//	S2A_EVIDENCE_OUT           evidence JSON path (default
//	                           deploy/gitlab/peixun/s2a-evidence.json)
//
// The runner substrate binary is always BUILT from this checkout into a
// temp dir the builder owns; maestroCommand re-points cmd.Path at it
// explicitly — no environment-supplied executable, no PATH lookup.
//
// Run: go run ./scripts/pilot/bom-import
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcptransport "github.com/mark3labs/mcp-go/client/transport"
	mcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// ---------------------------------------------------------------------------
// Environment / boilerplate.
// ---------------------------------------------------------------------------

type importer struct {
	db       *sql.DB
	pg       *store.PostgresStore
	rest     *restClient
	claims   jwtClaims
	userID   string
	ev       *evidence
	binary   string
	repoRoot string

	manifest        BomManifest
	manifestDigest  string
	baselineSHA     string
	bomAssetVersion int // the version the gates bind (v2)

	planM1ID  string
	planM23ID string
	m1RootID  string
	m23RootID string

	author *stdioRunner
	tech   *stdioRunner
}

func main() {
	manifestPath := flag.String("manifest", filepath.Join("data", "bom-manifest-20260909.json"), "BOM manifest (replay source)")
	flag.Parse()

	imp, cleanup, err := openImporter(*manifestPath)
	if err != nil {
		fatal("open: %v", err)
	}
	defer cleanup()

	steps := []struct {
		name string
		run  func() error
	}{
		{"S0 seeds", imp.stepSeeds},
		{"S0b plans", imp.stepPlans},
		{"S1 bom asset walk", imp.stepBOMAsset},
		{"S1 m1 proposals + seal", imp.stepM1Proposals},
		{"S2 flat items + claim probe", imp.stepFlatItems},
		{"S2 gate bindings", imp.stepGateBindings},
		{"S3 m23 placeholders", imp.stepM23},
		{"S4 jira anchors", imp.stepJira},
		{"S5 evidence + audit", imp.stepEvidence},
	}
	for _, step := range steps {
		started := time.Now()
		if err := step.run(); err != nil {
			imp.ev.record("failed_step", map[string]any{"step": step.name, "error": err.Error()})
			imp.ev.flush()
			fatal("%s: %v", step.name, err)
		}
		fmt.Printf("✓ %s (%.1fs)\n", step.name, time.Since(started).Seconds())
	}
	delete(imp.ev.data, "failed_step") // a completed run clears any stale failure marker
	imp.ev.flush()
	imp.stepSummary()
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "bom-import: "+format+"\n", args...)
	os.Exit(1)
}

func openImporter(manifestPath string) (*importer, func(), error) {
	imp := &importer{}

	// Repo root (three levels above this package).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return nil, nil, errors.New("resolve source location")
	}
	imp.repoRoot = filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))

	// Manifest (the committed replay source) + its digest.
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("manifest: %w", err)
	}
	if err := json.Unmarshal(raw, &imp.manifest); err != nil {
		return nil, nil, fmt.Errorf("manifest parse: %w", err)
	}
	if len(imp.manifest.Items) != 105 {
		return nil, nil, fmt.Errorf("manifest carries %d items, want 105", len(imp.manifest.Items))
	}
	sum := sha256.Sum256(raw)
	imp.manifestDigest = "sha256:" + hex.EncodeToString(sum[:])

	// Structural self-checks before touching anything live (per plan,
	// with the real code bases).
	selfChecks := []struct {
		phase string
		opts  proposalOptions
	}{
		{"一期", proposalOptions{Phase: "一期", LetterCodeBase: s2aM1LetterCodeBase, DomainCodeBase: s2aM1DomainCodeBase,
			Repo: "peixun-backend", BaselineSHA: strings.Repeat("0", 40), WorkspacePaths: []string{"doc"},
			BOMAssetRef: s2aBOMAssetID + "@2", IncludeFlows: true, IncludeEdges: true}},
		{"二三期", proposalOptions{Phase: "二三期", LetterCodeBase: s2aM23LetterCodeBase, DomainCodeBase: s2aM23DomainCodeBase,
			Repo: "peixun-backend", BaselineSHA: strings.Repeat("0", 40), WorkspacePaths: []string{"doc"}}},
	}
	for _, check := range selfChecks {
		if err := checkDerived(deriveLetterProposals(imp.manifest, check.opts)); err != nil {
			return nil, nil, fmt.Errorf("derive %s: %w", check.phase, err)
		}
	}

	// Maestro binary for the runner substrate — always built from THIS
	// checkout into a temp dir the builder owns.
	binary, err := buildBinary(imp.repoRoot)
	if err != nil {
		return nil, nil, err
	}
	imp.binary = binary

	// Live PG store.
	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	if dsn == "" {
		return nil, nil, errors.New("MAESTRO_TEST_POSTGRES_DSN must point at the LIVE pilot maestro database")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	if err := db.Ping(); err != nil {
		return nil, nil, fmt.Errorf("ping pilot PG: %w", err)
	}
	pg, err := store.NewPostgresStore(db)
	if err != nil {
		return nil, nil, err
	}
	imp.db, imp.pg = db, pg

	// OIDC REST surface + the server-side user row.
	token := os.Getenv("S2A_PILOT_TOKEN")
	if token == "" {
		return nil, nil, errors.New("S2A_PILOT_TOKEN must carry a fresh pilot-admin bearer (fetch-token.sh)")
	}
	api := os.Getenv("S2A_PILOT_API")
	if api == "" {
		api = "http://127.0.0.1:8080"
	}
	imp.rest = &restClient{base: strings.TrimRight(api, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
	claims, err := decodeClaims(token)
	if err != nil {
		return nil, nil, err
	}
	imp.claims = claims

	// Evidence recorder.
	out := os.Getenv("S2A_EVIDENCE_OUT")
	if out == "" {
		out = filepath.Join(imp.repoRoot, "deploy", "gitlab", "peixun", "s2a-evidence.json")
	}
	imp.ev = newEvidence(out)

	// Baseline SHA for the work-item atomicity pins (peixun-backend main).
	sha, err := gitlabMainSHA(imp.repoRoot, 3)
	if err != nil {
		return nil, nil, fmt.Errorf("baseline sha: %w", err)
	}
	imp.baselineSHA = sha

	cleanupRunners := func() {
		if imp.author != nil {
			imp.author.close()
		}
		if imp.tech != nil {
			imp.tech.close()
		}
	}
	return imp, func() {
		cleanupRunners()
		_ = os.RemoveAll(filepath.Dir(binary))
		_ = db.Close()
	}, nil
}

func buildBinary(repoRoot string) (string, error) {
	dir, err := os.MkdirTemp("", "maestro-s2a-binary-")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(dir, "maestro")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, "./cmd/maestro")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build maestro: %v: %s", err, out)
	}
	return binary, nil
}

// maestroCommand builds the argv for one maestro subprocess. The
// executable is ALWAYS the binary this importer just built into its own
// temp dir; cmd.Path is re-pointed explicitly so the process never
// resolves through PATH and no environment-supplied executable is ever
// honored (the recorded PATH-lookup error is cleared — Path is
// authoritative here).
func (imp *importer) maestroCommand(args ...string) *exec.Cmd {
	command := exec.Command("maestro", args...)
	command.Path = imp.binary
	command.Err = nil
	return command
}

// ---------------------------------------------------------------------------
// Evidence recorder (the P5b pattern).
// ---------------------------------------------------------------------------

type evidence struct {
	path string
	data map[string]any
}

func newEvidence(path string) *evidence {
	ev := &evidence{path: path, data: map[string]any{"scenario": "S2A BOM→WorkGraph 全量导入"}}
	raw, err := os.ReadFile(path) // #nosec G703 -- the operator names the evidence file via S2A_EVIDENCE_OUT
	if err == nil {
		_ = json.Unmarshal(raw, &ev.data)
	}
	return ev
}

func (e *evidence) record(key string, value any) {
	e.data[key] = value
	e.flush()
}

func (e *evidence) flush() {
	raw, err := json.MarshalIndent(e.data, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(e.path, raw, 0o600)
}

// ---------------------------------------------------------------------------
// stdio MCP runners (the only honest write path for proposals and the
// asset register; the P5b composition: SQLite baseline + PG overlay).
// ---------------------------------------------------------------------------

type stdioRunner struct {
	*mcpclient.Client
	command *exec.Cmd
	stdin   io.WriteCloser
	stderr  *bytes.Buffer
	done    chan error
}

func (imp *importer) startRunner(runnerID string) (*stdioRunner, error) {
	localDB := filepath.Join(os.TempDir(), "s2a-runner-"+runnerID+".db")
	if out, err := imp.maestroCommand("migrate", "up", "--db", localDB).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("migrate local baseline for %s: %v: %s", runnerID, err, out)
	}
	configPath := filepath.Join(os.TempDir(), "s2a-runner-"+runnerID+".yaml")
	config := fmt.Sprintf("db_path: %q\nhttp_addr: \"127.0.0.1:0\"\nremote_write: false\ndatabase:\n  driver: postgres\n", localDB)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return nil, err
	}
	child := imp.maestroCommand("runner", "--config", configPath, "--runner-id", runnerID, "--project", s2aProjectID)
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
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "s2a-bom-import", Version: "1.0.0"}
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

// call invokes one tool and returns (errored, wire text).
func (r *stdioRunner) call(name string, arguments map[string]any) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := r.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: arguments}})
	if err != nil {
		return false, "", err
	}
	var text strings.Builder
	for _, content := range result.Content {
		if typed, ok := content.(mcp.TextContent); ok {
			text.WriteString(typed.Text)
		}
	}
	return result.IsError, text.String(), nil
}

func (r *stdioRunner) callOK(name string, arguments map[string]any) (string, error) {
	isErr, text, err := r.call(name, arguments)
	if err != nil {
		return "", err
	}
	if isErr {
		return "", fmt.Errorf("tool %s errored: %s", name, text)
	}
	return text, nil
}

// ensureRunners lazily starts the author/tech pair (separation of duties
// for the register→review walk).
func (imp *importer) ensureRunners() error {
	if imp.author == nil {
		runner, err := imp.startRunner("s2a-author")
		if err != nil {
			return err
		}
		imp.author = runner
	}
	if imp.tech == nil {
		runner, err := imp.startRunner("s2a-tech")
		if err != nil {
			return err
		}
		imp.tech = runner
	}
	return nil
}

// ---------------------------------------------------------------------------
// REST surface (the P5b minimal client).
// ---------------------------------------------------------------------------

type restClient struct {
	base  string
	token string
	http  *http.Client
}

func (r *restClient) do(method, path string, headers map[string]string, body []byte) (int, string, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, r.base+path, reader)
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("Authorization", "Bearer "+r.token)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := r.http.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return response.StatusCode, "", err
	}
	return response.StatusCode, string(raw), nil
}

type jwtClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
}

func decodeClaims(token string) (jwtClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return jwtClaims{}, errors.New("bearer is not a JWS compact token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}, err
	}
	var claims jwtClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return jwtClaims{}, err
	}
	if claims.Iss == "" || claims.Sub == "" {
		return jwtClaims{}, errors.New("bearer carries no iss/sub")
	}
	return claims, nil
}

// gitlabMainSHA resolves one repo's default-branch HEAD (the atomicity
// baseline pin). The PAT comes from the env or the sandbox root PAT file.
func gitlabMainSHA(repoRoot string, gitlabProject int) (string, error) {
	api := os.Getenv("S2A_GITLAB_API")
	if api == "" {
		api = "http://127.0.0.1:8181"
	}
	pat := os.Getenv("S2A_GITLAB_PAT")
	if pat == "" {
		raw, err := os.ReadFile(filepath.Join(repoRoot, "deploy", "gitlab", ".root-pat"))
		if err == nil {
			pat = strings.TrimSpace(string(raw))
		}
	}
	if pat == "" {
		return "", errors.New("no GitLab PAT (S2A_GITLAB_PAT or deploy/gitlab/.root-pat)")
	}
	// #nosec G704 -- the GitLab API base is operator-supplied (S2A_GITLAB_API), the sandbox endpoint by default.
	request, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/api/v4/projects/%d/repository/commits/main", api, gitlabProject), nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("PRIVATE-TOKEN", pat)
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request) // #nosec G704 -- operator-supplied target
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gitlab commit lookup: %d: %s", response.StatusCode, raw)
	}
	var commit struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &commit); err != nil {
		return "", err
	}
	return commit.ID, nil
}
