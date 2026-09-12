//go:build p5b

// Package p5b_test is the P5b PoC governance first-run harness
// (brief-P5b): it drives the LIVE pilot control plane — the real
// PostgreSQL store, the real stdio MCP runners and the real OIDC
// bearer REST surface — through the whole 0→1 chain: baseline
// re-seed, WorkPattern/plan, decomposition proposal, plan seal
// (the J1 functional-plane first use), the artifact ledger walk with
// the locked-gate lifecycle, Jira manual anchoring (blueprint
// fallback) and the audit-chain export/verify.
//
// It is an OPERATIONAL harness, not a hermetic test: it reads its
// targets from the environment and writes the pilot control plane for
// real. Every stage is idempotent so the run can be replayed.
//
//	MAESTRO_TEST_POSTGRES_DSN   pilot PG DSN (the maestro database)
//	P5B_PILOT_API               pilot server base URL (default
//	                             http://127.0.0.1:8080)
//	P5B_PILOT_TOKEN             fresh OIDC bearer (fetch-token.sh)
//	P5B_ASSETS_DIR              local peixun-backend clone with the
//	                             artifact content files (digest source)
//	P5B_JIRA_BASE_URL           Jira to re-probe (blueprint fallback
//	                             when unreachable)
//	P5B_EVIDENCE_OUT            evidence JSON output path
//	P5B_PILOT_GITLAB_PAT        sandbox GitLab root PAT (profile A/B)
//
// Run:  go test -tags p5b ./tests/p5b/ -run TestP5bStage -v -count=1
// It is excluded from CI (no p5b tag there).

package p5b_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

var (
	p5bRepositoryRoot string
	p5bMaestroBinary  string
)

func TestMain(m *testing.M) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "cannot resolve p5b harness location")
		os.Exit(1)
	}
	p5bRepositoryRoot = filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))

	if configured := os.Getenv("MAESTRO_BINARY"); configured != "" {
		resolved, err := filepath.Abs(configured)
		if err != nil {
			fmt.Fprintf(os.Stderr, "resolve MAESTRO_BINARY: %v\n", err)
			os.Exit(1)
		}
		p5bMaestroBinary = resolved
	} else {
		buildDir, err := os.MkdirTemp("", "maestro-p5b-binary-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "create build directory: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = os.RemoveAll(buildDir) }()
		p5bMaestroBinary = filepath.Join(buildDir, "maestro")
		build := exec.Command("go", "build", "-trimpath", "-o", p5bMaestroBinary, "./cmd/maestro")
		build.Dir = p5bRepositoryRoot
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build real Maestro binary: %v\n%s", err, out)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Evidence recorder: structured facts per stage, merged into one JSON
// document at P5B_EVIDENCE_OUT so the release-note asset can cite them.
// ---------------------------------------------------------------------------

type p5bEvidence struct {
	mu   sync.Mutex
	path string
	data map[string]any
}

func p5bNewEvidence(t *testing.T) *p5bEvidence {
	t.Helper()
	ev := &p5bEvidence{path: os.Getenv("P5B_EVIDENCE_OUT"), data: map[string]any{}}
	if ev.path == "" {
		return ev
	}
	if raw, err := os.ReadFile(ev.path); err == nil {
		_ = json.Unmarshal(raw, &ev.data)
	}
	return ev
}

func (e *p5bEvidence) record(t *testing.T, key string, value any) {
	t.Helper()
	if e == nil || e.path == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.data[key] = value
	raw, err := json.MarshalIndent(e.data, "", "  ")
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if err := os.WriteFile(e.path, raw, 0o600); err != nil {
		t.Fatalf("write evidence %s: %v", e.path, err)
	}
}

// ---------------------------------------------------------------------------
// stdio MCP runner bootstrap (the m0 managed-client pattern, trimmed to
// what the first run needs: start, initialize, call, close).
// ---------------------------------------------------------------------------

type p5bStdioRunner struct {
	*client.Client

	command *exec.Cmd
	stdin   io.WriteCloser
	stderr  *bytes.Buffer
	done    chan error
}

func p5bStartRunner(t *testing.T, configPath, projectID, runnerID string) *p5bStdioRunner {
	t.Helper()
	child := exec.Command(p5bMaestroBinary, "runner", "--config", configPath,
		"--runner-id", runnerID, "--project", projectID)
	child.Env = append(os.Environ(),
		"MAESTRO_DATABASE_DSN="+os.Getenv("MAESTRO_TEST_POSTGRES_DSN"),
		"MAESTRO_REMOTE_WRITE=false",
	)
	stdin, err := child.StdinPipe()
	require.NoError(t, err, "create runner stdin")
	stdout, err := child.StdoutPipe()
	require.NoError(t, err, "create runner stdout")
	stderrPipe, err := child.StderrPipe()
	require.NoError(t, err, "create runner stderr")
	require.NoError(t, child.Start(), "start runner "+runnerID)

	runner := &p5bStdioRunner{command: child, stdin: stdin, stderr: &bytes.Buffer{}, done: make(chan error, 1)}
	go func() { _, _ = io.Copy(runner.stderr, stderrPipe); close(runner.done) }()

	// stderr is intentionally left open: the transport only owns stdout.
	runner.Client = client.NewClient(transport.NewIO(stdout, stdin, io.NopCloser(bytes.NewReader(nil))))
	require.NoError(t, runner.Client.Start(context.Background()), "start MCP transport for "+runnerID)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "p5b-first-run", Version: "1.0.0"}
	_, initErr := runner.Client.Initialize(ctx, initRequest)
	require.NoError(t, initErr, "initialize MCP for "+runnerID)
	t.Cleanup(func() {
		_ = runner.Client.Close()
		_ = child.Process.Kill()
		<-runner.done
		if runner.stderr.Len() > 0 {
			t.Logf("runner %s stderr tail: %s", runnerID, tailOf(runner.stderr.String(), 400))
		}
	})
	return runner
}

// call invokes one real MCP tool and returns (result, wire text).
func (r *p5bStdioRunner) call(t *testing.T, name string, arguments map[string]any) (*mcp.CallToolResult, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := r.Client.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: arguments},
	})
	require.NoError(t, err, "call tool %s", name)
	var text strings.Builder
	for _, content := range result.Content {
		if typed, ok := content.(mcp.TextContent); ok {
			text.WriteString(typed.Text)
		}
	}
	return result, text.String()
}

// callOK invokes one tool and requires a non-error reply.
func (r *p5bStdioRunner) callOK(t *testing.T, name string, arguments map[string]any) string {
	t.Helper()
	result, text := r.call(t, name, arguments)
	require.False(t, result.IsError, "tool %s errored: %s", name, text)
	return text
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// ---------------------------------------------------------------------------
// Pilot REST surface (the live OIDC server).
// ---------------------------------------------------------------------------

type p5bREST struct {
	base  string
	token string
	http  *http.Client
}

func p5bNewREST(t *testing.T) *p5bREST {
	t.Helper()
	token := os.Getenv("P5B_PILOT_TOKEN")
	require.NotEmpty(t, token, "P5B_PILOT_TOKEN must carry a fresh pilot-admin bearer (fetch-token.sh)")
	base := os.Getenv("P5B_PILOT_API")
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	return &p5bREST{base: strings.TrimRight(base, "/"), token: token, http: &http.Client{Timeout: 30 * time.Second}}
}

// p5bClaims decodes the unverified JWT payload — the harness only needs
// iss/sub to seed the matching user row (the server itself verifies).
type p5bClaims struct {
	Iss string `json:"iss"`
	Sub string `json:"sub"`
}

func (r *p5bREST) claims(t *testing.T) p5bClaims {
	t.Helper()
	parts := strings.Split(r.token, ".")
	require.Len(t, parts, 3, "bearer is not a JWS compact token")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var claims p5bClaims
	require.NoError(t, json.Unmarshal(raw, &claims))
	require.NotEmpty(t, claims.Iss)
	require.NotEmpty(t, claims.Sub)
	return claims
}

// do performs one REST request; body may be nil. Returns status and body.
func (r *p5bREST) do(t *testing.T, method, path string, headers map[string]string, body []byte) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, r.base+path, reader)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+r.token)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := r.http.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(raw)
}

// probeHTTPS reports whether an HTTPS endpoint answers within the budget
// (the Jira re-test; any response counts as reachable).
func p5bProbeHTTPS(target string, budget time.Duration) error {
	client := &http.Client{Timeout: budget}
	response, err := client.Get(target)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}
