package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Daemon lifecycle tests against a scripted mock Control Plane: happy
// claim→execute→complete, heartbeats carried with the connection
// generation, terminal refusals stop the loop, retryable trouble backs
// off, and the lease deadline bounds execution.

type scriptedCP struct {
	server *httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies []map[string]any
	// behavior returns (statusCode, responseBody); nil means default 202.
	behavior func(callIndex int, path string) (int, string)
}

func newScriptedCP(t *testing.T, behavior func(int, string) (int, string)) *scriptedCP {
	t.Helper()
	cp := &scriptedCP{behavior: behavior}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		cp.calls = append(cp.calls, r.Method+" "+r.URL.Path)
		index := len(cp.calls) - 1
		behavior := cp.behavior
		cp.mu.Unlock()

		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cp.mu.Lock()
		cp.bodies = append(cp.bodies, body)
		cp.mu.Unlock()

		if behavior != nil {
			if status, reply := behavior(index, r.URL.Path); status != 0 || reply != "" {
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(reply))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/runner-leases/claim":
			_, _ = w.Write([]byte(leaseJSON))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	cp.server = httptest.NewServer(mux)
	t.Cleanup(cp.server.Close)
	return cp
}

const leaseJSON = `{
	"id":"lease-1","version":5,"epoch":2,"execution_id":"exec-1",
	"work_item_id":"wi-1","project_id":"proj-1",
	"repository":"https://gitlab.example/g/r.git",
	"baseline_sha":"0123456789abcdef0123456789abcdef01234567",
	"workspace_generation":1,
	"push_intent":{"gitlab_instance_id":"inst-1","gitlab_project_id":7,"expected_host":"gitlab.example","source_branch":"maestro/p/t","expected_old_sha":null},
	"expires_at":"2999-01-01T00:00:00Z",
	"command_profiles":[{"id":"go-test","version":"1.0.0","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]
}`

type fakeExecutor struct {
	mu          sync.Mutex
	executed    []*Lease
	heartbeats  int
	outcome     ExecutionCompletion
	execErr     error
	onHeartbeat func()
}

func (f *fakeExecutor) Execute(_ context.Context, lease *Lease, heartbeat func() error) (ExecutionCompletion, error) {
	f.mu.Lock()
	f.executed = append(f.executed, lease)
	f.mu.Unlock()
	if f.onHeartbeat != nil {
		f.onHeartbeat()
	}
	if err := heartbeat(); err == nil {
		f.mu.Lock()
		f.heartbeats++
		f.mu.Unlock()
	}
	if f.execErr != nil {
		return ExecutionCompletion{}, f.execErr
	}
	completion := f.outcome
	completion.LeaseID = lease.ID
	completion.LeaseVersion = lease.Version
	completion.WorkspaceGeneration = lease.WorkspaceGeneration
	return completion, nil
}

func testDaemon(t *testing.T, cp *scriptedCP, executor Executor) (*Daemon, context.Context, context.CancelFunc) {
	t.Helper()
	client, err := NewClient(cp.server.URL, cp.server.Client())
	require.NoError(t, err)
	client.SetToken("device-token")
	daemon, err := NewDaemon(client, executor, DaemonConfig{
		ClaimWaitSeconds: 5,
		Capabilities:     []string{"rootless_oci", "no_new_privileges", "resource_limits"},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return daemon, ctx, cancel
}

func TestDaemonHappyPathClaimExecuteComplete(t *testing.T) {
	cp := newScriptedCP(t, nil)
	executor := &fakeExecutor{outcome: ExecutionCompletion{
		Outcome: OutcomeCompleted, CommitSHA: strPtr("0123456789abcdef0123456789abcdef01234567"),
	}}
	daemon, ctx, cancel := testDaemon(t, cp, executor)

	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx) }()
	// One lease completes, then the next claim gets no-work and we stop.
	cp.mu.Lock()
	cp.behavior = func(index int, path string) (int, string) {
		if path == "/api/v3/runner-leases/claim" && index >= 2 {
			return 0, `{"available":false,"retry_after_seconds":3600}`
		}
		return 0, ""
	}
	cp.mu.Unlock()

	// Wait for the completion to be reported.
	require.Eventually(t, func() bool {
		cp.mu.Lock()
		defer cp.mu.Unlock()
		for _, call := range cp.calls {
			if call == "POST /api/v3/executions/exec-1/complete" {
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "completion must be reported")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not stop on context cancel")
	}

	cp.mu.Lock()
	defer cp.mu.Unlock()
	completionBodies := cp.bodiesForPath("/api/v3/executions/exec-1/complete")
	require.Len(t, completionBodies, 1)
	body := completionBodies[0]
	assert.Equal(t, "completed", body["outcome"])
	assert.Equal(t, daemon.Generation(), body["connection_generation"])
	// The heartbeat the fake executor sent carries the same generation.
	heartbeatBodies := cp.bodiesForPath("/heartbeat")
	require.NotEmpty(t, heartbeatBodies)
	assert.Equal(t, daemon.Generation(), heartbeatBodies[0]["connection_generation"])
}

func TestDaemonStopsOnTerminalRefusal(t *testing.T) {
	cp := newScriptedCP(t, func(int, string) (int, string) {
		return http.StatusGone, `{"code":"RUNNER_REVOKED","message":"revoked","retryable":false,"correlation_id":"c"}`
	})
	executor := &fakeExecutor{}
	daemon, _, _ := testDaemon(t, cp, executor)

	err := daemon.Run(context.Background())
	require.Error(t, err)
	protocolErr, ok := err.(*ProtocolError)
	require.True(t, ok)
	assert.Equal(t, "RUNNER_REVOKED", protocolErr.Code)
	assert.Empty(t, executor.executed, "a revoked runner must never execute")
}

func TestDaemonBacksOffOnRetryableTroubleThenRecovers(t *testing.T) {
	var failClaims sync.Map // index -> fail
	failClaims.Store(0, true)
	failClaims.Store(1, true)
	cp := newScriptedCP(t, func(index int, path string) (int, string) {
		if path == "/api/v3/runner-leases/claim" {
			if fail, ok := failClaims.Load(index); ok && fail.(bool) {
				return http.StatusServiceUnavailable, `{"code":"DEPENDENCY_UNAVAILABLE","message":"down","retryable":true,"correlation_id":"c"}`
			}
			return 0, `{"available":false,"retry_after_seconds":3600}`
		}
		return 0, ""
	})
	executor := &fakeExecutor{}
	daemon, ctx, cancel := testDaemon(t, cp, executor)

	// Make backoff instant so the test does not really sleep.
	daemon.config.SleepFunc = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
			return nil
		}
	}
	// But let the third call (after two failures) reach no-work, then stop.
	go func() {
		require.Eventually(t, func() bool {
			cp.mu.Lock()
			defer cp.mu.Unlock()
			return len(cp.calls) >= 3
		}, 5*time.Second, 5*time.Millisecond)
		cancel()
	}()
	err := daemon.Run(ctx)
	require.NoError(t, err, "retryable trouble must not fail the session")
	cp.mu.Lock()
	defer cp.mu.Unlock()
	assert.GreaterOrEqual(t, len(cp.calls), 3, "daemon must retry after 503")
	assert.Empty(t, executor.executed, "no lease was ever granted")
}

func TestDaemonReportsExecutorFailureHonestly(t *testing.T) {
	cp := newScriptedCP(t, nil)
	executor := &fakeExecutor{execErr: context.DeadlineExceeded}
	daemon, ctx, cancel := testDaemon(t, cp, executor)

	cp.mu.Lock()
	cp.behavior = func(index int, path string) (int, string) {
		if path == "/api/v3/runner-leases/claim" && index >= 1 {
			return 0, `{"available":false,"retry_after_seconds":3600}`
		}
		return 0, ""
	}
	cp.mu.Unlock()

	go func() {
		require.Eventually(t, func() bool {
			cp.mu.Lock()
			defer cp.mu.Unlock()
			return len(cp.bodiesForPath("/api/v3/executions/exec-1/complete")) > 0
		}, 5*time.Second, 10*time.Millisecond)
		cancel()
	}()
	require.NoError(t, daemon.Run(ctx))

	cp.mu.Lock()
	defer cp.mu.Unlock()
	bodies := cp.bodiesForPath("/api/v3/executions/exec-1/complete")
	require.Len(t, bodies, 1)
	assert.Equal(t, OutcomeFailed, bodies[0]["outcome"], "an executor crash reports failed, never success")
}

func TestDaemonConfigValidation(t *testing.T) {
	client, err := NewClient("https://cp.example", nil)
	require.NoError(t, err)
	executor := &fakeExecutor{}

	_, err = NewDaemon(client, executor, DaemonConfig{ClaimWaitSeconds: 0, Capabilities: []string{"a", "b", "c"}})
	assert.Error(t, err, "claim wait below schema bound")

	_, err = NewDaemon(client, executor, DaemonConfig{ClaimWaitSeconds: 5, Capabilities: []string{"a"}})
	assert.Error(t, err, "below the frozen capability floor")

	_, err = NewDaemon(nil, executor, DaemonConfig{ClaimWaitSeconds: 5, Capabilities: []string{"a", "b", "c"}})
	assert.Error(t, err, "client required")

	_, err = NewDaemon(client, nil, DaemonConfig{ClaimWaitSeconds: 5, Capabilities: []string{"a", "b", "c"}})
	assert.Error(t, err, "executor required")
}

func (cp *scriptedCP) bodiesForPath(suffix string) []map[string]any {
	var matched []map[string]any
	for index, call := range cp.calls {
		if containsSuffix(call, suffix) && index < len(cp.bodies) {
			matched = append(matched, cp.bodies[index])
		}
	}
	return matched
}

func containsSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func strPtr(value string) *string { return &value }
