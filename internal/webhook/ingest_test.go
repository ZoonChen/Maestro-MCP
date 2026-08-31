package webhook

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeStore records ingest decisions for the receiver decision table.
type fakeStore struct {
	instances  map[string]Instance
	mappingIDs map[int64]string // gitlab project id -> registered webhook uuid
	denials    []AuditRow
	ingested   []IngestRecord
	outcomes   []string // per-ingest outcomes handed back
}

func (f *fakeStore) InstanceByID(_ context.Context, id string) (Instance, bool, error) {
	instance, ok := f.instances[id]
	if ok && instance.Status == "removed" {
		return Instance{}, false, nil
	}
	return instance, ok, nil
}

func (f *fakeStore) MappingWebhookUUID(_ context.Context, _ string, gitlabProjectID int64) (string, bool, error) {
	uuid, ok := f.mappingIDs[gitlabProjectID]
	return uuid, ok, nil
}

func (f *fakeStore) RecordDenial(_ context.Context, audit AuditRow) error {
	f.denials = append(f.denials, audit)
	return nil
}

func (f *fakeStore) IngestDelivery(_ context.Context, rec IngestRecord) (IngestResult, error) {
	f.ingested = append(f.ingested, rec)
	outcome := OutcomeAccepted
	if len(f.outcomes) > 0 {
		outcome = f.outcomes[0]
		f.outcomes = f.outcomes[1:]
	}
	return IngestResult{Outcome: outcome, InboxID: "018f6000-0000-7000-8000-0000000000ff"}, nil
}

func (f *fakeStore) ClaimInbox(context.Context, string) (*InboxRow, error) {
	return nil, nil
}
func (f *fakeStore) BeginApply(context.Context) (ApplyUnit, error) {
	return nil, assert.AnError
}
func (f *fakeStore) ReplayDeadLetter(context.Context, string) (bool, error) {
	return false, nil
}

const (
	testInstance = "018f6200-0000-7000-8000-000000000001"
	otherSecret  = "other-secret"
)

// secretRefA names an environment variable, never a credential value.
const secretRefA = "env:MAESTRO_WEBHOOK_SECRET_A" //nolint:gosec // reference, not a credential

func newTestIngestor(store Store) *Ingestor {
	cipher, err := NewPayloadCipher("ingest-test-key")
	if err != nil {
		panic(err)
	}
	return &Ingestor{Store: store, Secrets: EnvSecretResolver{}, Cipher: cipher}
}

func baseRequest() ReceiveRequest {
	return ReceiveRequest{
		InstanceID:  testInstance,
		ContentType: "application/json",
		EventHeader: "Push Hook",
		EventUUID:   "evt-1",
		Token:       "secret-a",
		RawBody:     []byte(`{"project_id": 42, "ref": "refs/heads/main"}`),
	}
}

func TestReceiveDecisionTable(t *testing.T) {
	t.Setenv("MAESTRO_WEBHOOK_SECRET_A", "secret-a")

	cases := []struct {
		name       string
		mutate     func(*ReceiveRequest)
		instance   Instance
		wantStatus int
		wantCode   string
		wantDenial string // expected reject_reason on the audit row, if any
	}{
		{
			name:       "unknown instance is hidden and unaudited",
			mutate:     func(r *ReceiveRequest) { r.InstanceID = "018f6200-0000-7000-8000-00000000dead" },
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 404, wantCode: "INSTANCE_UNKNOWN",
		},
		{
			name:       "removed instance is hidden like an unknown one",
			mutate:     func(r *ReceiveRequest) { r.InstanceID = "018f6200-0000-7000-8000-00000000dead" },
			instance:   Instance{ID: "018f6200-0000-7000-8000-00000000dead", Status: "removed"},
			wantStatus: 404, wantCode: "INSTANCE_UNKNOWN",
		},
		{
			name:       "suspended instance answers 503",
			instance:   Instance{ID: testInstance, Status: "suspended", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 503, wantCode: "INSTANCE_SUSPENDED", wantDenial: ReasonInstanceSuspend,
		},
		{
			name:       "non-json content type is rejected",
			mutate:     func(r *ReceiveRequest) { r.ContentType = "text/plain" },
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 400, wantCode: "CONTENT_TYPE_UNSUPPORTED", wantDenial: ReasonContentType,
		},
		{
			name:       "unresolvable secret fails closed with 503",
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: "env:MAESTRO_WEBHOOK_SECRET_UNSET"}, //nolint:gosec // reference, not a credential
			wantStatus: 503, wantCode: "SECRET_UNRESOLVED", wantDenial: ReasonSecretUnresolved,
		},
		{
			name:       "token mismatch has no business effect",
			mutate:     func(r *ReceiveRequest) { r.Token = "forged" },
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 401, wantCode: "WEBHOOK_TOKEN_INVALID", wantDenial: ReasonTokenMismatch,
		},
		{
			name:       "absent token never verifies",
			mutate:     func(r *ReceiveRequest) { r.Token = "" },
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 401, wantCode: "WEBHOOK_TOKEN_INVALID", wantDenial: ReasonTokenMismatch,
		},
		{
			name:       "missing event header is rejected",
			mutate:     func(r *ReceiveRequest) { r.EventHeader = "" },
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 400, wantCode: "EVENT_HEADER_MISSING", wantDenial: ReasonEventHeader,
		},
		{
			name:       "uncontracted event kind is archived without business effect",
			mutate:     func(r *ReceiveRequest) { r.EventHeader = "Note Hook" },
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 202, wantCode: "EVENT_KIND_ARCHIVED", wantDenial: ReasonUnsupportedKind,
		},
		{
			name:       "payload without project identity is quarantined",
			mutate:     func(r *ReceiveRequest) { r.RawBody = []byte(`{"ref": "refs/heads/main"}`) },
			instance:   Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA}, //nolint:gosec // reference, not a credential
			wantStatus: 202, wantCode: "EVENT_QUARANTINED",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{
				instances:  map[string]Instance{tc.instance.ID: tc.instance},
				mappingIDs: map[int64]string{},
			}
			req := baseRequest()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			result := newTestIngestor(store).Receive(t.Context(), req)

			assert.Equal(t, tc.wantStatus, result.Status, "status")
			assert.Equal(t, tc.wantCode, result.Code, "code")
			if tc.wantDenial != "" {
				require.Len(t, store.denials, 1)
				assert.Equal(t, tc.wantDenial, store.denials[0].RejectReason)
			}
			if tc.wantCode == "EVENT_QUARANTINED" {
				require.Len(t, store.ingested, 1)
				assert.True(t, store.ingested[0].Quarantine)
				assert.Equal(t, ReasonPayloadInvalid, store.ingested[0].RejectReason)
				assert.Empty(t, store.denials)
			}
			if tc.wantCode == "INSTANCE_UNKNOWN" {
				assert.Empty(t, store.denials, "hidden instances leave no audit rows")
				assert.Empty(t, store.ingested)
			}
		})
	}
}

func TestReceiveAcceptedAndDuplicate(t *testing.T) {
	t.Setenv("MAESTRO_WEBHOOK_SECRET_A", "secret-a")
	instance := Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA} //nolint:gosec // reference, not a credential

	t.Run("accepted persists under the event uuid", func(t *testing.T) {
		store := &fakeStore{instances: map[string]Instance{instance.ID: instance}}
		result := newTestIngestor(store).Receive(t.Context(), baseRequest())
		assert.Equal(t, 202, result.Status)
		assert.Equal(t, "EVENT_PERSISTED", result.Code)
		require.Len(t, store.ingested, 1)
		assert.Equal(t, "evt-1", store.ingested[0].ExternalEventID)
		assert.Equal(t, "push", store.ingested[0].EventKind)
		assert.Equal(t, BodyDigest(baseRequest().RawBody), store.ingested[0].PayloadDigest)
		assert.NotEmpty(t, store.ingested[0].RawBodyEncrypted)
		assert.NotContains(t, string(store.ingested[0].RawBodyEncrypted), "project_id", "raw body must be sealed")
	})

	t.Run("duplicate stays idempotent", func(t *testing.T) {
		store := &fakeStore{
			instances: map[string]Instance{instance.ID: instance},
			outcomes:  []string{OutcomeDuplicate},
		}
		result := newTestIngestor(store).Receive(t.Context(), baseRequest())
		assert.Equal(t, 202, result.Status)
		assert.Equal(t, "EVENT_DUPLICATE", result.Code)
		assert.Equal(t, OutcomeDuplicate, result.Outcome)
	})

	t.Run("missing uuid falls back to the composite compatibility key", func(t *testing.T) {
		store := &fakeStore{instances: map[string]Instance{instance.ID: instance}}
		req := baseRequest()
		req.EventUUID = ""
		result := newTestIngestor(store).Receive(t.Context(), req)
		assert.Equal(t, 202, result.Status)
		require.Len(t, store.ingested, 1)
		assert.Equal(t,
			"compat:"+testInstance+":42:Push Hook:"+BodyDigest(req.RawBody),
			store.ingested[0].ExternalEventID)
	})
}

func TestReceiveWebhookIdentityMismatch(t *testing.T) {
	t.Setenv("MAESTRO_WEBHOOK_SECRET_A", "secret-a")
	instance := Instance{ID: testInstance, Status: "active", WebhookSecretRef: secretRefA} //nolint:gosec // reference, not a credential
	store := &fakeStore{
		instances:  map[string]Instance{instance.ID: instance},
		mappingIDs: map[int64]string{42: "registered-hook-uuid"},
	}

	req := baseRequest()
	req.WebhookUUID = "a-different-hook"
	result := newTestIngestor(store).Receive(t.Context(), req)
	assert.Equal(t, 401, result.Status)
	assert.Equal(t, "WEBHOOK_IDENTITY_MISMATCH", result.Code)
	require.Len(t, store.denials, 1)
	assert.Equal(t, "WEBHOOK_IDENTITY_MISMATCH", store.denials[0].RejectReason)
	assert.Empty(t, store.ingested)

	// A matching hook identity, or a mapping without a registered uuid,
	// or no mapping at all, must all pass the identity gate.
	match := baseRequest()
	match.WebhookUUID = "registered-hook-uuid"
	assert.Equal(t, 202, newTestIngestor(store).Receive(t.Context(), match).Status)
}

func TestReceivedEnvelopeShape(t *testing.T) {
	row := &InboxRow{
		ID:              "018f6300-0000-7000-8000-000000000001",
		InstanceID:      testInstance,
		ExternalEventID: "evt-9",
		EventKind:       "merge_request",
		PayloadDigest:   "sha256:" + "ab",
		ReceivedAt:      "2026-08-31T10:00:00Z",
	}
	envelope, err := ReceivedEnvelope(row, "018f6300-0000-7000-8000-000000000002")
	require.NoError(t, err)

	assert.Equal(t, EventTypeWebhookReceived, envelope.EventType)
	assert.Equal(t, 1, envelope.EventVersion)
	assert.Equal(t, row.ID, envelope.EventID, "the envelope identity is the inbox identity")
	assert.Equal(t, "018f6300-0000-7000-8000-000000000002", envelope.ProjectID)
	assert.Equal(t, row.PayloadDigest, envelope.PayloadDigest)
	assert.Equal(t, SensitivityWebhook, envelope.Sensitivity)
	assert.Contains(t, string(envelope.Payload), `"delivery_key":"evt-9"`)
	assert.Contains(t, string(envelope.Payload), `"event_kind":"merge_request"`)
}
