package webhook

import (
	"context"
	"log/slog"
	"mime"
	"net/http"
	"strings"
)

// Ingestor owns the M2-WHK-001 receive decision: authenticate the raw
// delivery, classify it, and persist exactly one inbox/audit outcome.
// It never inspects business payload semantics beyond the contracted
// project identity probe, and it never executes business effects.
type Ingestor struct {
	Store   Store
	Secrets SecretResolver
	Cipher  *PayloadCipher
}

// ReceiveRequest is one HTTP delivery as received on the wire. The body
// is the exact raw bytes: verification and the digest run against it
// before any parsing.
type ReceiveRequest struct {
	InstanceID  string
	ContentType string
	EventHeader string // X-Gitlab-Event
	EventUUID   string // X-Gitlab-Event-UUID
	WebhookUUID string // X-Gitlab-Webhook-UUID
	Token       string // X-Gitlab-Token
	RawBody     []byte
}

// ReceiveResult is the receiver's verdict: the HTTP reply (status and
// stable code) plus the audit outcome that was durably recorded.
type ReceiveResult struct {
	Status  int
	Code    string
	Outcome string
}

// Receive applies the frozen receive contract
// (control-plane.yaml ingestGitLabWebhook):
//
//	202 — verified and durably persisted (accepted, duplicate or
//	      quarantined); duplicates stay idempotent, never a business effect
//	400 — malformed envelope (content type, event header, instance id)
//	401 — token or hook-identity verification failed
//	404 — unknown instance (hidden, unaudited: no attributable target)
//	503 — fail-closed configuration (suspended instance, unresolved secret)
func (i *Ingestor) Receive(ctx context.Context, req ReceiveRequest) ReceiveResult {
	auditKind := func() string { return clampText(req.EventHeader) }

	instance, found, err := i.Store.InstanceByID(ctx, req.InstanceID)
	if err != nil {
		return ReceiveResult{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR"}
	}
	if !found {
		// Unknown instances are indistinguishable from nonexistent ones
		// and have no audit target worth durable rows.
		return ReceiveResult{Status: http.StatusNotFound, Code: "INSTANCE_UNKNOWN"}
	}
	if instance.Status != "active" {
		i.deny(ctx, AuditRow{InstanceID: instance.ID, ExternalEventID: clampText(req.EventUUID),
			EventKind: auditKind(), Outcome: OutcomeRejected, RejectReason: ReasonInstanceSuspend})
		return ReceiveResult{Status: http.StatusServiceUnavailable, Code: "INSTANCE_SUSPENDED", Outcome: OutcomeRejected}
	}

	if !jsonContentType(req.ContentType) {
		i.deny(ctx, AuditRow{InstanceID: instance.ID, ExternalEventID: clampText(req.EventUUID),
			EventKind: auditKind(), Outcome: OutcomeRejected, RejectReason: ReasonContentType})
		return ReceiveResult{Status: http.StatusBadRequest, Code: "CONTENT_TYPE_UNSUPPORTED", Outcome: OutcomeRejected}
	}

	secret, err := i.Secrets.Resolve(ctx, instance.WebhookSecretRef)
	if err != nil {
		i.deny(ctx, AuditRow{InstanceID: instance.ID, ExternalEventID: clampText(req.EventUUID),
			EventKind: auditKind(), Outcome: OutcomeRejected, RejectReason: ReasonSecretUnresolved})
		return ReceiveResult{Status: http.StatusServiceUnavailable, Code: "SECRET_UNRESOLVED", Outcome: OutcomeRejected}
	}
	if !VerifyToken(req.Token, secret) {
		i.deny(ctx, AuditRow{InstanceID: instance.ID, ExternalEventID: clampText(req.EventUUID),
			EventKind: auditKind(), Outcome: OutcomeRejected, RejectReason: ReasonTokenMismatch})
		return ReceiveResult{Status: http.StatusUnauthorized, Code: "WEBHOOK_TOKEN_INVALID", Outcome: OutcomeRejected}
	}

	if strings.TrimSpace(req.EventHeader) == "" {
		i.deny(ctx, AuditRow{InstanceID: instance.ID, EventKind: "(none)",
			TokenVerified: true, Outcome: OutcomeRejected, RejectReason: ReasonEventHeader})
		return ReceiveResult{Status: http.StatusBadRequest, Code: "EVENT_HEADER_MISSING", Outcome: OutcomeRejected}
	}

	// Uncontracted event types are archived without business effect: the
	// audit row preserves the denial for forensics while the reply keeps
	// GitLab from retrying an event the contract never asked for.
	if _, contracted := Classify(req.EventHeader); !contracted {
		i.deny(ctx, AuditRow{InstanceID: instance.ID, ExternalEventID: clampText(req.EventUUID),
			EventKind: auditKind(), TokenVerified: true, Outcome: OutcomeRejected, RejectReason: ReasonUnsupportedKind})
		return ReceiveResult{Status: http.StatusAccepted, Code: "EVENT_KIND_ARCHIVED", Outcome: OutcomeRejected}
	}

	digest := BodyDigest(req.RawBody)
	gitlabProject, projectKnown := ProjectOf(req.RawBody)
	if !projectKnown {
		// Verified sender, unresolvable payload: quarantined durably
		// (dead_letter) for replay forensics — never silently dropped and
		// never auto-mapped.
		i.quarantine(ctx, instance, req, digest, ReasonPayloadInvalid)
		return ReceiveResult{Status: http.StatusAccepted, Code: "EVENT_QUARANTINED", Outcome: OutcomeDeadLetter}
	}

	// Hook-identity check: when the mapping registered a webhook uuid, a
	// delivery from a different hook on the same instance is rejected even
	// with a valid shared token (token rotation divides trust per hook).
	if req.WebhookUUID != "" {
		registered, mapped, err := i.Store.MappingWebhookUUID(ctx, instance.ID, gitlabProject)
		if err != nil {
			return ReceiveResult{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR"}
		}
		if mapped && registered != "" && registered != req.WebhookUUID {
			i.deny(ctx, AuditRow{InstanceID: instance.ID, ExternalEventID: clampText(req.EventUUID),
				EventKind: auditKind(), TokenVerified: true,
				Outcome: OutcomeRejected, RejectReason: "WEBHOOK_IDENTITY_MISMATCH"})
			return ReceiveResult{Status: http.StatusUnauthorized, Code: "WEBHOOK_IDENTITY_MISMATCH", Outcome: OutcomeRejected}
		}
	}

	key, compatibilityMode := DeliveryKey(instance.ID, gitlabProject, req.EventHeader, digest, req.EventUUID)
	if compatibilityMode {
		// WEBHOOK-RULE-002: digest-based compatibility dedup is weaker and
		// MUST raise the compatibility-mode alert.
		slog.WarnContext(ctx, "webhook ingest: compatibility-mode dedup (no X-Gitlab-Event-UUID)",
			"instance_id", instance.ID, "event_kind", auditKind())
	}

	sealed, err := i.Cipher.Seal(req.RawBody)
	if err != nil {
		return ReceiveResult{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR"}
	}
	result, err := i.Store.IngestDelivery(ctx, IngestRecord{
		InstanceID:       instance.ID,
		ExternalEventID:  key,
		EventKind:        string(classifyKind(req.EventHeader)),
		WebhookUUID:      clampText(req.WebhookUUID),
		PayloadDigest:    digest,
		RawBodyEncrypted: sealed,
	})
	if err != nil {
		return ReceiveResult{Status: http.StatusInternalServerError, Code: "INTERNAL_ERROR"}
	}
	if result.Outcome == OutcomeDuplicate {
		return ReceiveResult{Status: http.StatusAccepted, Code: "EVENT_DUPLICATE", Outcome: OutcomeDuplicate}
	}
	return ReceiveResult{Status: http.StatusAccepted, Code: "EVENT_PERSISTED", Outcome: OutcomeAccepted}
}

// quarantine ingests a dead_letter inbox row plus its audit entry.
func (i *Ingestor) quarantine(ctx context.Context, instance Instance, req ReceiveRequest, digest, reason string) {
	sealed, err := i.Cipher.Seal(req.RawBody)
	if err != nil {
		sealed = nil
	}
	key, _ := DeliveryKey(instance.ID, 0, req.EventHeader, digest, req.EventUUID)
	_, _ = i.Store.IngestDelivery(ctx, IngestRecord{
		InstanceID:       instance.ID,
		ExternalEventID:  key,
		EventKind:        string(classifyKind(req.EventHeader)),
		PayloadDigest:    digest,
		RawBodyEncrypted: sealed,
		Quarantine:       true,
		RejectReason:     reason,
	})
}

func (i *Ingestor) deny(ctx context.Context, audit AuditRow) {
	if err := i.Store.RecordDenial(ctx, audit); err != nil {
		slog.ErrorContext(ctx, "webhook ingest: denial audit write failed", "error", err.Error())
	}
}

// classifyKind maps a header already known to be contracted.
func classifyKind(eventHeader string) Kind {
	return headerKinds[eventHeader]
}

func jsonContentType(contentType string) bool {
	parsed, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return false
	}
	return parsed == "application/json"
}
