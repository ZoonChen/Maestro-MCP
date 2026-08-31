// Package webhook implements the M2-WHK-001 receive path: raw-byte token
// verification (GitLab CE mode), durable Inbox persistence with composite
// dedup, quarantine/DLQ transitions, and the gitlab.webhook.received
// outbox envelope from the frozen events.yaml 3.0.0 catalog.
//
// The payload is UNTRUSTED input. GitLab CE offers no request signing, so
// verification is the constant-time shared-token compare plus TLS plus the
// (instance_id, external_event_id) dedup key — the S4 CE deviations
// absorbed by the frozen contract. Nothing in this package executes
// business logic from a payload field: payloads are classified, digested,
// encrypted and dispatched as events for downstream sync (S4a).
package webhook

// Kind enumerates the contracted event kinds (webhook_inbox.event_kind
// CHECK and events.yaml GitLabWebhookPayload.event_kind).
type Kind string

const (
	KindPush         Kind = "push"
	KindMergeRequest Kind = "merge_request"
	KindPipeline     Kind = "pipeline"
	KindJob          Kind = "job"
)

// Inbox row lifecycle (webhook_inbox.status CHECK). dead_letter is the
// DLQ: quarantine at ingest or retry exhaustion at dispatch.
const (
	StatusReceived   = "received"
	StatusProcessing = "processing"
	StatusProcessed  = "processed"
	StatusRetryWait  = "retry_wait"
	StatusDeadLetter = "dead_letter"
)

// Delivery audit outcomes (webhook_deliveries.outcome CHECK).
const (
	OutcomeAccepted   = "accepted"
	OutcomeDuplicate  = "duplicate"
	OutcomeRejected   = "rejected"
	OutcomeDeadLetter = "dead_letter"
)

// Reject reasons recorded on webhook_deliveries rows.
const (
	ReasonTokenMismatch    = "TOKEN_MISMATCH"
	ReasonSecretUnresolved = "SECRET_UNRESOLVED"
	ReasonInstanceSuspend  = "INSTANCE_SUSPENDED"
	ReasonUnsupportedKind  = "UNSUPPORTED_EVENT_KIND"
	ReasonContentType      = "CONTENT_TYPE_UNSUPPORTED"
	ReasonEventHeader      = "EVENT_HEADER_MISSING"
	ReasonPayloadInvalid   = "PAYLOAD_PROJECT_MISSING"
	ReasonUnmappedProject  = "UNMAPPED_PROJECT"
	ReasonDecryptFailed    = "PAYLOAD_DECRYPT_FAILED"
	ReasonRetryExhausted   = "RETRY_EXHAUSTED"
)

// Instance is the receiver's projection of gitlab_instances.
type Instance struct {
	ID               string
	BaseURL          string
	Status           string // active | suspended | removed
	WebhookSecretRef string
}

// IngestRecord carries one token-verified HTTP delivery into the store.
// Quarantine rows enter the inbox directly as dead_letter (payload fields
// unresolvable at ingest); every record also appends its audit row in the
// same transaction.
type IngestRecord struct {
	InstanceID       string
	ExternalEventID  string
	EventKind        string // contracted Kind, or the raw header for archive rows
	WebhookUUID      string
	PayloadDigest    string // sha256:<hex64>
	RawBodyEncrypted []byte
	Quarantine       bool
	RejectReason     string // set when Quarantine
}

// IngestResult reports the durable outcome of one ingest.
type IngestResult struct {
	Outcome string // accepted | duplicate | dead_letter
	InboxID string
}

// InboxRow is a claimed dispatch unit.
type InboxRow struct {
	ID               string
	InstanceID       string
	ExternalEventID  string
	EventKind        string
	WebhookUUID      string
	PayloadDigest    string
	RawBodyEncrypted []byte
	ReceivedAt       string // RFC3339
	Attempts         int
}

// Audit visibility cap for header-derived text stored in the audit trail;
// protects the append-only table from pathological sender input.
const maxAuditText = 512

func clampText(value string) string {
	if len(value) <= maxAuditText {
		return value
	}
	return value[:maxAuditText]
}
