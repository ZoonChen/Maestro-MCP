package webhook

import (
	"context"
	"errors"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// ErrEnvelopeDuplicate reports that the outbox already carries this exact
// event identity — the idempotent re-drive outcome, not a failure.
var ErrEnvelopeDuplicate = errors.New("webhook envelope already emitted")

// ErrReplayApprovalInvalid rejects a replay attempt that does not carry
// the runbook §9 dual-person control (distinct requester and approver,
// a substantive reason) — a silent requeue is exactly what it refuses.
var ErrReplayApprovalInvalid = errors.New("webhook replay approval invalid")

// ReplayApproval carries the runbook §8/§9 replay contract: the
// original event identity is preserved by the requeue itself, while
// the approval and the attempt land in one audited transaction.
type ReplayApproval struct {
	RequestedBy string
	ApprovedBy  string
	Reason      string
}

// AuditRow appends one entry to the webhook_deliveries audit trail.
// InboxID is empty for pre-ingest denials (no inbox row exists).
type AuditRow struct {
	InboxID         string
	InstanceID      string
	ExternalEventID string
	EventKind       string
	TokenVerified   bool
	Outcome         string
	RejectReason    string
}

// Store is the persistence contract for the receive and dispatch paths.
// Implementations must keep each method's writes atomic: IngestDelivery
// persists the inbox row and its audit entry in one transaction, and
// ApplyUnit settles a claim and its audit entry in one transaction.
type Store interface {
	// InstanceByID resolves the receiver's instance projection; found is
	// false for unknown or removed instances (resource hiding).
	InstanceByID(ctx context.Context, instanceID string) (Instance, bool, error)

	// MappingWebhookUUID reads the registered hook identity for an
	// instance/project mapping; found is false when no mapping exists.
	MappingWebhookUUID(ctx context.Context, instanceID string, gitlabProjectID int64) (string, bool, error)

	// RecordDenial appends a denial/replay audit entry without any inbox
	// row (token mismatch, unresolved secret, archived event kind, ...).
	RecordDenial(ctx context.Context, audit AuditRow) error

	// IngestDelivery persists one verified delivery and its audit entry
	// atomically and reports the durable outcome.
	IngestDelivery(ctx context.Context, rec IngestRecord) (IngestResult, error)

	// ClaimInbox leases the oldest eligible row (FOR UPDATE SKIP LOCKED,
	// bounded stale-claim reclaim) and commits the claim immediately:
	// a dispatcher crash never strands work past the stale window.
	ClaimInbox(ctx context.Context, owner string) (*InboxRow, error)

	// BeginApply opens the settle transaction for one claimed row.
	BeginApply(ctx context.Context) (ApplyUnit, error)

	// ReplayDeadLetter re-queues a quarantined row under its ORIGINAL
	// event identity: no re-verification bypass and no side-effect
	// duplication (re-emission collapses on the outbox unique key).
	// The dual-person approval is mandatory (runbook §9): requester
	// and approver must be distinct principals, and the approval, the
	// attempt and the original event identity are recorded atomically.
	ReplayDeadLetter(ctx context.Context, inboxID string, approval ReplayApproval) (bool, error)
}

// ApplyUnit settles one claimed inbox row transactionally.
type ApplyUnit interface {
	// ProjectForMapping resolves the Maestro project uuid for an
	// instance/GitLab-project pair; "" means unmapped.
	ProjectForMapping(ctx context.Context, instanceID string, gitlabProjectID int64) (string, error)

	// EnqueueOutbox appends the envelope, reporting ErrEnvelopeDuplicate
	// when the identity already exists.
	EnqueueOutbox(ctx context.Context, event *model.OutboxEvent) error

	MarkProcessed(ctx context.Context, inboxID, owner string) error
	MarkRetry(ctx context.Context, inboxID, owner string, availableIn time.Duration) error
	MarkDeadLetter(ctx context.Context, inboxID, owner, reason string) error
	Commit() error
	Rollback() error
}
