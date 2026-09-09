package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The M4-PILOT-001 rollout-flag surface: list one project's flags and
// record one rollout decision. The store owns the lifecycle guard and
// the same-transaction audit (pilot.decision.recorded); this handler
// owns the wire — actor always from the server-side principal, reason
// carried verbatim into the audit trail, gray percentage only with the
// gray stage.

// PilotStore is the store surface this handler consumes.
type PilotStore interface {
	ListFlags(ctx context.Context, projectID string) ([]store.PilotFlag, error)
	PutFlag(ctx context.Context, projectID, flag string, decision store.PilotDecision) (store.PilotFlag, bool, error)
}

// PilotHandler serves the pilot rollout-flag endpoints.
type PilotHandler struct {
	flags PilotStore
}

// NewPilotHandler builds the rollout-flag handler.
func NewPilotHandler(flags PilotStore) *PilotHandler {
	return &PilotHandler{flags: flags}
}

// pilotFlagWire is one flag on the wire (PilotFlag schema).
type pilotFlagWire struct {
	Flag        string `json:"flag"`
	Stage       string `json:"stage"`
	GrayPercent int    `json:"gray_percent"`
	ChangedBy   string `json:"changed_by"`
	Reason      string `json:"reason"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func pilotFlagToWire(flag store.PilotFlag) pilotFlagWire {
	return pilotFlagWire{
		Flag: flag.Flag, Stage: flag.Stage, GrayPercent: flag.GrayPercent,
		ChangedBy: flag.ChangedBy, Reason: flag.Reason,
		CreatedAt: flag.CreatedAt, UpdatedAt: flag.UpdatedAt,
	}
}

// ListPilotFlags answers the project's rollout flags in flag order.
func (h *PilotHandler) ListPilotFlags(c *gin.Context) {
	flags, err := h.flags.ListFlags(c.Request.Context(), c.Param("pid"))
	if err != nil {
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Pilot flags could not be listed")
		return
	}
	wire := make([]pilotFlagWire, 0, len(flags))
	for _, flag := range flags {
		wire = append(wire, pilotFlagToWire(flag))
	}
	c.JSON(http.StatusOK, gin.H{"project_id": c.Param("pid"), "flags": wire})
}

// PutPilotFlag records one rollout decision (putPilotFlag). Identical
// replays are no-ops; every recorded transition audited in the same
// transaction by the store.
func (h *PilotHandler) PutPilotFlag(c *gin.Context) {
	if c.GetHeader("Idempotency-Key") == "" {
		staticErrorReply(c, http.StatusBadRequest, "INVALID_PARAMETER", "A valid Idempotency-Key is required")
		return
	}
	principal := PrincipalFromContext(c)
	if principal == nil {
		staticErrorReply(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Authentication is required")
		return
	}
	flag := c.Param("flag")
	if flag == "" || len([]rune(flag)) > 128 {
		staticErrorReply(c, http.StatusUnprocessableEntity, "PILOT_DECISION_INVALID", "flag must be 1-128 characters")
		return
	}
	var request struct {
		Stage       string `json:"stage" binding:"required"`
		GrayPercent int    `json:"gray_percent"`
		Reason      string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		staticErrorReply(c, http.StatusBadRequest, "PILOT_DECISION_INVALID", "stage and reason are required")
		return
	}

	stored, created, err := h.flags.PutFlag(c.Request.Context(), c.Param("pid"), flag, store.PilotDecision{
		Stage: request.Stage, GrayPercent: request.GrayPercent,
		Actor: principal.PrincipalID, Reason: request.Reason,
	})
	switch {
	case errors.Is(err, store.ErrPilotDecisionInvalid):
		staticErrorReply(c, http.StatusUnprocessableEntity, "PILOT_DECISION_INVALID", err.Error())
		return
	case errors.Is(err, store.ErrPilotTransitionInvalid):
		staticErrorReply(c, http.StatusConflict, "PILOT_TRANSITION_INVALID", err.Error())
		return
	case err != nil:
		staticErrorReply(c, http.StatusInternalServerError, "INTERNAL_ERROR", "The rollout decision could not be recorded")
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, pilotFlagToWire(stored))
}
