package handler

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ZoonChen/Maestro-MCP/internal/audit"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The M4-OBS-001 export surface over the frozen audit-export schema.
// The store owns the chain computation over the immutable
// audit_events table; this handler owns the wire projection —
// deterministic uuid event ids (uuidv5 over project+seq, stable
// across exports so recipients can diff), RFC3339 timestamps and the
// redaction record. Token hashes and free-text reasons are absent by
// the store's projection, not by handler convention.

// AuditExporter is the store surface this handler consumes.
type AuditExporter interface {
	AuditExport(ctx context.Context, projectID string, fromSeq, toSeq int64) (rows []audit.Row, digests []string, chain string, err error)
	AuditChainVerify(ctx context.Context, projectID string, fromSeq, toSeq int64, claimed []string) error
}

// redactionPolicyVersion identifies the frozen export allowlist.
const redactionPolicyVersion = "audit-export-v3"

// ObservabilityHandler serves the frozen audit-export endpoints.
type ObservabilityHandler struct {
	exports AuditExporter
}

// NewObservabilityHandler builds the export/verify handler.
func NewObservabilityHandler(exports AuditExporter) *ObservabilityHandler {
	return &ObservabilityHandler{exports: exports}
}

// auditEntry is one wire entry (audit-export.schema.json).
type auditEntry struct {
	Seq           int64  `json:"seq"`
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	Principal     string `json:"principal"`
	OccurredAt    string `json:"occurred_at"`
	CorrelationID string `json:"correlation_id,omitempty"`
	Resource      string `json:"resource,omitempty"`
	Action        string `json:"action,omitempty"`
	Decision      string `json:"decision,omitempty"`
	PrevDigest    string `json:"prev_digest,omitempty"`
	EntryDigest   string `json:"entry_digest"`
}

type auditRangeWire struct {
	FromSeq int64 `json:"from_seq"`
	ToSeq   int64 `json:"to_seq"`
}

type auditRedactionWire struct {
	PolicyVersion string   `json:"policy_version"`
	FieldsMasked  []string `json:"fields_masked"`
}

type auditChainExport struct {
	SchemaVersion string              `json:"schema_version"`
	ExportID      string              `json:"export_id"`
	ProjectID     string              `json:"project_id"`
	Range         auditRangeWire      `json:"range"`
	Entries       []auditEntry        `json:"entries"`
	ChainDigest   string              `json:"chain_digest"`
	Redaction     *auditRedactionWire `json:"redaction"`
}

// ExportAuditChain walks the immutable audit events in seq order and
// returns the frozen export wire with the per-entry and rolling chain
// digests.
func (h *ObservabilityHandler) ExportAuditChain(c *gin.Context) {
	projectID := c.Param("pid")
	fromSeq, toSeq, ok := parseAuditRange(c)
	if !ok {
		return
	}
	rows, digests, chain, err := h.exports.AuditExport(c.Request.Context(), projectID, fromSeq, toSeq)
	if errors.Is(err, store.ErrAuditRangeEmpty) {
		staticErrorReply(c, http.StatusBadRequest, "AUDIT_RANGE_INVALID", "to_seq must be greater than or equal to from_seq")
		return
	}
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Audit export failed")
		return
	}

	entries := make([]auditEntry, 0, len(rows))
	for index, row := range rows {
		occurredAt, timeErr := normalizeAuditTimestamp(row.OccurredAt)
		if timeErr != nil {
			// Fail closed: an unparseable timestamp would corrupt the
			// wire contract, never a best-effort string.
			staticErrorReply(c, http.StatusInternalServerError, "AUDIT_TIMESTAMP_INVALID", "An audit timestamp could not be normalized")
			return
		}
		entry := auditEntry{
			Seq:         row.Seq,
			EventID:     auditEventID(projectID, row.Seq),
			EventType:   row.EventType,
			Principal:   row.Principal,
			OccurredAt:  occurredAt,
			Decision:    row.Decision,
			EntryDigest: digests[index],
		}
		if row.CorrelationID != "" {
			entry.CorrelationID = row.CorrelationID
		}
		if row.Resource != "" {
			entry.Resource = row.Resource
		}
		if row.Action != "" {
			entry.Action = row.Action
		}
		if index > 0 {
			entry.PrevDigest = digests[index-1]
		}
		entries = append(entries, entry)
	}

	c.JSON(http.StatusOK, auditChainExport{
		SchemaVersion: "3.0",
		ExportID:      uuid.NewString(),
		ProjectID:     projectID,
		Range:         auditRangeWire{FromSeq: fromSeq, ToSeq: toSeq},
		Entries:       entries,
		ChainDigest:   chain,
		Redaction: &auditRedactionWire{
			PolicyVersion: redactionPolicyVersion,
			FieldsMasked:  []string{"token_hash", "reason"},
		},
	})
}

// VerifyAuditChain recomputes the chain for a range and compares it
// against the claimed digests — the auditor's tamper check over an
// export. The If-Match precondition carries the export's chain digest
// being verified.
func (h *ObservabilityHandler) VerifyAuditChain(c *gin.Context) {
	if c.GetHeader("If-Match") == "" {
		staticErrorReply(c, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED", "If-Match must carry the export chain digest")
		return
	}
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Idempotency-Key header is required")
		return
	}
	var request struct {
		FromSeq        int64    `json:"from_seq" binding:"required"`
		ToSeq          int64    `json:"to_seq" binding:"required"`
		ClaimedDigests []string `json:"claimed_digests" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		staticErrorReply(c, http.StatusBadRequest, "AUDIT_VERIFY_INVALID", "from_seq, to_seq and claimed_digests are required")
		return
	}
	if request.FromSeq < 1 || request.ToSeq < request.FromSeq {
		staticErrorReply(c, http.StatusBadRequest, "AUDIT_RANGE_INVALID", "from_seq must be >= 1 and to_seq >= from_seq")
		return
	}
	err := h.exports.AuditChainVerify(c.Request.Context(), c.Param("pid"), request.FromSeq, request.ToSeq, request.ClaimedDigests)
	if errors.Is(err, store.ErrAuditRangeEmpty) {
		staticErrorReply(c, http.StatusBadRequest, "AUDIT_RANGE_INVALID", "to_seq must be greater than or equal to from_seq")
		return
	}
	if err != nil {
		staticErrorReply(c, http.StatusConflict, "AUDIT_CHAIN_MISMATCH", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"verified": true})
}

func parseAuditRange(c *gin.Context) (int64, int64, bool) {
	fromSeq, err := strconv.ParseInt(c.Query("from_seq"), 10, 64)
	if err != nil || fromSeq < 1 {
		staticErrorReply(c, http.StatusBadRequest, "AUDIT_RANGE_INVALID", "from_seq must be an integer >= 1")
		return 0, 0, false
	}
	toSeq, err := strconv.ParseInt(c.Query("to_seq"), 10, 64)
	if err != nil || toSeq < fromSeq {
		staticErrorReply(c, http.StatusBadRequest, "AUDIT_RANGE_INVALID", "to_seq must be an integer >= from_seq")
		return 0, 0, false
	}
	return fromSeq, toSeq, true
}

// auditEventID derives the deterministic wire event id: uuidv5 over
// the project and seq, so the same row always exports the same id and
// recipients can diff consecutive exports.
func auditEventID(projectID string, seq int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte("https://maestro.internal/audit-export/"+projectID+"/"+strconv.FormatInt(seq, 10))).String()
}

var auditTimestampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999",
}

func normalizeAuditTimestamp(raw string) (string, error) {
	for _, layout := range auditTimestampLayouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano), nil
		}
	}
	return "", errors.New("unparseable audit timestamp")
}
