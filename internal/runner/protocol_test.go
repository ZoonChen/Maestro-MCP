package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wire-shape tests against a mock Control Plane: every golden assertion
// checks the EXACT frozen runner.yaml field names, and the error contract
// (terminal vs retryable) is enforced by behavior, not error strings.

type mockCP struct {
	server *httptest.Server
}

func newMockCP(t *testing.T, custom func(w http.ResponseWriter, r *http.Request) bool) *mockCP {
	t.Helper()
	cp := &mockCP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if custom != nil && custom(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/runners/enroll":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"runner_id":"018f1000-0000-7000-8000-000000000001","state":"pending_approval","access_token":"device-token","expires_at":"2026-12-31T00:00:00Z"}`))
		case "/api/v3/runner-leases/claim":
			_, _ = w.Write([]byte(`{
				"id":"018f1000-0000-7000-8000-000000000002","version":3,"epoch":2,
				"execution_id":"018f1000-0000-7000-8000-000000000003",
				"work_item_id":"018f1000-0000-7000-8000-000000000004",
				"project_id":"018f1000-0000-7000-8000-000000000005",
				"repository":"https://gitlab.example/group/repo.git",
				"baseline_sha":"0123456789abcdef0123456789abcdef01234567",
				"workspace_generation":1,
				"push_intent":{"gitlab_instance_id":"018f1000-0000-7000-8000-000000000006","gitlab_project_id":42,"expected_host":"gitlab.example","source_branch":"maestro/proj/task-1","expected_old_sha":null},
				"expires_at":"2026-12-31T01:00:00Z",
				"command_profiles":[{"id":"go-test","version":"1.0.0","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]
			}`))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	cp.server = httptest.NewServer(mux)
	t.Cleanup(cp.server.Close)
	return cp
}

func TestEnrollWireShapeMatchesFrozenContract(t *testing.T) {
	cp := newMockCP(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v3/runners/enroll" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.ElementsMatch(t,
				[]string{"enrollment_code", "device_public_key", "display_name", "capabilities"},
				keysOf(body))
			assert.Equal(t, "enroll-code-0001", body["enrollment_code"])
			assert.Equal(t, "device-key-material", body["device_public_key"])
			assert.Empty(t, r.Header.Get("Authorization"), "enrollment is pre-credential")
		}
		return false
	})
	client, err := NewClient(cp.server.URL, cp.server.Client())
	require.NoError(t, err)

	credential, err := client.Enroll(context.Background(), EnrollmentRequest{
		EnrollmentCode:  "enroll-code-0001",
		DevicePublicKey: "device-key-material",
		DisplayName:     "runner-1",
		Capabilities:    []string{"rootless_oci", "no_new_privileges"},
	}, "idem-enroll-0000000001")
	require.NoError(t, err)
	assert.Equal(t, "pending_approval", credential.State)
	assert.Equal(t, "device-token", credential.AccessToken)
}

func TestClaimLeaseWireShapeAndNoWork(t *testing.T) {
	cp := newMockCP(t, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v3/runner-leases/claim" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			assert.ElementsMatch(t,
				[]string{"protocol_version", "connection_generation", "capabilities", "wait_seconds"},
				keysOf(body))
			assert.Equal(t, "Bearer device-token", r.Header.Get("Authorization"))
		}
		return false
	})
	client, err := NewClient(cp.server.URL, cp.server.Client())
	require.NoError(t, err)
	client.SetToken("device-token")

	lease, noWork, err := client.ClaimLease(context.Background(), ClaimRequest{
		ProtocolVersion:      ProtocolVersion,
		ConnectionGeneration: "018f1000-0000-7000-8000-00000000000f",
		Capabilities:         []string{"rootless_oci", "no_new_privileges", "resource_limits"},
		WaitSeconds:          25,
	}, "idem-claim-00000000001")
	require.NoError(t, err)
	require.NotNil(t, lease)
	assert.Nil(t, noWork)

	assert.Equal(t, int64(3), lease.Version)
	assert.Equal(t, int64(2), lease.Epoch)
	assert.Equal(t, int64(42), lease.PushIntent.GitLabProjectID)
	assert.Equal(t, "go-test", lease.CommandProfiles[0].ID)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, lease.CommandProfiles[0].Digest)

	cp2 := newMockCP(t, func(w http.ResponseWriter, _ *http.Request) bool {
		_, _ = w.Write([]byte(`{"available":false,"retry_after_seconds":20}`))
		return true
	})
	client2, _ := NewClient(cp2.server.URL, cp2.server.Client())
	lease2, noWork2, err := client2.ClaimLease(context.Background(), ClaimRequest{
		ProtocolVersion: ProtocolVersion, ConnectionGeneration: "gen", Capabilities: []string{"a", "b", "c"}, WaitSeconds: 1,
	}, "idem-claim-00000000002")
	require.NoError(t, err)
	assert.Nil(t, lease2)
	require.NotNil(t, noWork2)
	assert.Equal(t, 20, noWork2.RetryAfterSeconds)
}

func TestHeartbeatEvidenceCompletionCarryFencing(t *testing.T) {
	cp := newMockCP(t, func(w http.ResponseWriter, r *http.Request) bool {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			assert.ElementsMatch(t, []string{"lease_version", "connection_generation", "observed_at"}, keysOf(body))
			assert.Equal(t, float64(7), body["lease_version"])
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/evidence"):
			assert.Equal(t, "diagnostic", body["authority"])
			assert.Equal(t, "local_runner", body["producer"])
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/complete"):
			assert.Equal(t, "completed", body["outcome"])
			assert.Equal(t, "0123456789abcdef0123456789abcdef01234567", body["commit_sha"])
			w.WriteHeader(http.StatusAccepted)
		default:
			return false
		}
		return true
	})
	client, _ := NewClient(cp.server.URL, cp.server.Client())
	client.SetToken("device-token")
	ctx := context.Background()
	generation := "018f1000-0000-7000-8000-00000000000f"

	require.NoError(t, client.HeartbeatLease(ctx, "lease-1", HeartbeatRequest{
		LeaseVersion: 7, ConnectionGeneration: generation, ObservedAt: time.Now().UTC(),
	}, "idem-heartbeat-0000001"))

	require.NoError(t, client.UploadDiagnosticEvidence(ctx, "exec-1", DiagnosticEvidence{
		LeaseID: "lease-1", LeaseVersion: 7, ConnectionGeneration: generation,
		Type: "test", Authority: "diagnostic", Producer: "local_runner",
		Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CreatedAt: time.Now().UTC(),
	}, "idem-evidence-00000001"))

	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	require.NoError(t, client.CompleteExecution(ctx, "exec-1", ExecutionCompletion{
		LeaseID: "lease-1", LeaseVersion: 7, ConnectionGeneration: generation,
		WorkspaceGeneration: 1, Outcome: OutcomeCompleted, CommitSHA: &commitSHA, Summary: "done",
	}, "idem-complete-0000001"))
}

func TestErrorContractSeparatesTerminalFromRetryable(t *testing.T) {
	cp := newMockCP(t, func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"code":"RUNNER_REVOKED","message":"device revoked","retryable":false,"correlation_id":"c"}`))
		return true
	})
	client, _ := NewClient(cp.server.URL, cp.server.Client())
	_, _, err := client.ClaimLease(context.Background(), ClaimRequest{
		ProtocolVersion: ProtocolVersion, ConnectionGeneration: "g", Capabilities: []string{"a", "b", "c"}, WaitSeconds: 1,
	}, "idem-claim-00000000003")
	require.Error(t, err)

	protocolErr, ok := err.(*ProtocolError)
	require.True(t, ok, "error type: %T", err)
	assert.Equal(t, http.StatusGone, protocolErr.StatusCode)
	assert.Equal(t, "RUNNER_REVOKED", protocolErr.Code)
	assert.True(t, protocolErr.Terminal(), "revocation is terminal — the daemon must stop, not retry")

	cp2 := newMockCP(t, func(w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	client2, _ := NewClient(cp2.server.URL, cp2.server.Client())
	_, _, err = client2.ClaimLease(context.Background(), ClaimRequest{
		ProtocolVersion: ProtocolVersion, ConnectionGeneration: "g", Capabilities: []string{"a", "b", "c"}, WaitSeconds: 1,
	}, "idem-claim-00000000004")
	require.Error(t, err)
	assert.False(t, err.(*ProtocolError).Terminal())
}

func TestBackoffBoundedWithJitter(t *testing.T) {
	base := time.Second
	capLimit := 8 * time.Second
	for attempt := 1; attempt <= 10; attempt++ {
		wait := Backoff(attempt, base, capLimit)
		assert.GreaterOrEqual(t, wait, base, "attempt %d", attempt)
		assert.LessOrEqual(t, wait, capLimit+capLimit/4, "attempt %d must stay bounded", attempt)
	}
}

func TestClientRejectsNonHTTPBaseURL(t *testing.T) {
	_, err := NewClient("not-a-url", nil)
	assert.Error(t, err)
}

func keysOf(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	return keys
}
