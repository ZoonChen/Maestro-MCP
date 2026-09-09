package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
	"github.com/gin-gonic/gin"
)

// Dead-letter replay control-plane endpoint (task brief E / UI-1): the
// human surface over the runbook §8/§9 dual-person replay contract that
// landed with the webhook store (ReplayDeadLetter). The HTTP layer owns
// exactly one decision the store cannot: the approver identity comes
// from the AUTHENTICATED principal, never from the body — you approve a
// replay as yourself, and you may not approve your own request.

// DeadLetterReplayStore is the consumed webhook-store contract.
type DeadLetterReplayStore interface {
	ReplayDeadLetter(ctx context.Context, inboxID string, approval webhook.ReplayApproval) (bool, error)
}

// DeadLetterHandler serves the DLQ replay endpoint.
type DeadLetterHandler struct {
	store DeadLetterReplayStore
}

// NewDeadLetterHandler binds the replay store.
func NewDeadLetterHandler(replayStore DeadLetterReplayStore) *DeadLetterHandler {
	return &DeadLetterHandler{store: replayStore}
}

type deadLetterReplayBody struct {
	RequestedBy string `json:"requested_by"`
	ApprovedBy  string `json:"approved_by"`
	Reason      string `json:"reason"`
}

// ReplayDeadLetter re-queues one quarantined webhook inbox row under
// its original event identity. The store performs the requeue, the
// dual-person approval validation and the audit row in one transaction;
// this handler maps the outcomes.
func (h *DeadLetterHandler) ReplayDeadLetter(c *gin.Context) {
	inboxID := c.Param("inbox_id")
	if inboxID == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A dead-letter inbox id is required")
		return
	}
	var body deadLetterReplayBody
	if err := c.ShouldBindJSON(&body); err != nil {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "Replay body does not match the contract")
		return
	}
	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	// The approver is the authenticated principal. A body naming a
	// different approver is an impersonation attempt, not a convenience.
	if body.ApprovedBy != "" && body.ApprovedBy != principal.PrincipalID {
		staticErrorReply(c, http.StatusForbidden, "APPROVER_IDENTITY_MISMATCH", "Only the authenticated principal may approve a replay")
		return
	}
	if body.RequestedBy == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "requested_by is required")
		return
	}
	// Dual-person control: the approver must differ from the requester
	// (runbook §9). The store re-checks this inside its transaction.
	if body.RequestedBy == principal.PrincipalID {
		staticErrorReply(c, http.StatusForbidden, "SEPARATION_OF_DUTIES", "The replay approver must differ from the requester")
		return
	}

	replayed, err := h.store.ReplayDeadLetter(c.Request.Context(), inboxID, webhook.ReplayApproval{
		RequestedBy: body.RequestedBy,
		ApprovedBy:  principal.PrincipalID,
		Reason:      body.Reason,
	})
	switch {
	case err == nil && replayed:
		c.JSON(http.StatusOK, gin.H{"inbox_id": inboxID, "replayed": true})
	case err == nil:
		// Nothing quarantined under this id: resource-hiding 404.
		staticErrorReply(c, http.StatusNotFound, "DEAD_LETTER_NOT_FOUND", "No quarantined delivery matches this id")
	case errors.Is(err, webhook.ErrReplayApprovalInvalid):
		staticErrorReply(c, http.StatusUnprocessableEntity, "REPLAY_APPROVAL_INVALID",
			"The replay approval is invalid (a substantive reason of at least 16 characters is required)")
	default:
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Replay could not be recorded")
	}
}
