package store

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
)

// M4-PILOT-001 rollout semantics over the migration 0012 pilot_flags
// table. The table CHECKs freeze the structural invariants (stage enum,
// gray-only percentage, one flag per project); this file owns the pure
// decision algebra — which stage moves are legal, and what effect a
// stored flag produces for a consuming feature face. The pilot book
// (docs/testing/pilot-acceptance.md §4/§13) fixes shadow first, then
// percentage grayscale; rollback is the terminal stop.

// Pilot flag stages (the migration 0012 enum).
const (
	PilotStageOff        = "off"
	PilotStageShadow     = "shadow"
	PilotStageGray       = "gray"
	PilotStageFull       = "full"
	PilotStageRolledBack = "rolled_back"
)

// PilotStages is the frozen stage enumeration, in lifecycle order.
var PilotStages = []string{PilotStageOff, PilotStageShadow, PilotStageGray, PilotStageFull, PilotStageRolledBack}

// pilotTransitions is the legal stage-move matrix. Same-stage moves are
// not transitions: only gray carries a meaningful same-stage change
// (percentage adjustment), handled by the decision guard below.
//
// Lifecycle: off → shadow (pilot starts read-only); shadow → gray
// (grayscale begins); gray → gray (percentage moves); gray → full
// (grayscale completes); any active stage → rolled_back (the kill
// switch, terminal — re-piloting a face needs a new flag name).
// off can only move to shadow: a flag that never started has nothing
// to roll back and cannot skip the shadow phase.
var pilotTransitions = map[string]map[string]struct{}{
	PilotStageOff:        {PilotStageShadow: {}},
	PilotStageShadow:     {PilotStageGray: {}, PilotStageRolledBack: {}},
	PilotStageGray:       {PilotStageFull: {}, PilotStageRolledBack: {}},
	PilotStageFull:       {PilotStageRolledBack: {}},
	PilotStageRolledBack: {},
}

// PilotCanTransition reports whether one recorded stage move is legal.
func PilotCanTransition(from, to string) bool {
	targets, ok := pilotTransitions[from]
	if !ok {
		return false
	}
	_, legal := targets[to]
	return legal
}

// ValidPilotStage reports whether the stage is part of the frozen enum.
func ValidPilotStage(stage string) bool {
	for _, candidate := range PilotStages {
		if stage == candidate {
			return true
		}
	}
	return false
}

// PilotEffect is what a consuming feature face applies for one flag.
type PilotEffect string

const (
	// PilotEffectOff: the face stays inert (also the meaning of the
	// terminal rolled_back stage).
	PilotEffectOff PilotEffect = "off"
	// PilotEffectShadow: the face computes alongside the human process
	// but creates no remote change (PILOT-RULE shadow phase).
	PilotEffectShadow PilotEffect = "shadow"
	// PilotEffectGray: the face acts for the percentage bucket only.
	PilotEffectGray PilotEffect = "gray"
	// PilotEffectFull: the face acts for everyone in the project.
	PilotEffectFull PilotEffect = "full"
)

// PilotEffectOf maps a stored flag stage to the effect a consumer
// applies. rolled_back is terminal and therefore inert — off; the gray
// percentage travels separately (see PilotGrayActive).
func PilotEffectOf(stage string) PilotEffect {
	switch stage {
	case PilotStageShadow:
		return PilotEffectShadow
	case PilotStageGray:
		return PilotEffectGray
	case PilotStageFull:
		return PilotEffectFull
	default: // off, rolled_back, unknown: fail inert
		return PilotEffectOff
	}
}

// PilotGrayBucket maps one subject to its stable 0–99 bucket inside a
// flag. The bucket derives from sha256(flag + ":" + subject) so the
// same subject never flips buckets between calls or restarts, and
// different flags distribute the same subjects independently.
func PilotGrayBucket(flag, subject string) int {
	sum := sha256.Sum256([]byte(flag + ":" + subject))
	return int(binary.BigEndian.Uint16(sum[:2])) % 100
}

// PilotGrayActive answers whether one subject is inside the gray
// bucket. percent 0 disables everyone, 100 enables everyone; values
// outside 0–100 disable everyone (fail closed).
func PilotGrayActive(flag, subject string, grayPercent int) bool {
	if grayPercent <= 0 || grayPercent > 100 {
		return false
	}
	return PilotGrayBucket(flag, subject) < grayPercent
}

// PilotDecision is one recorded rollout decision. Actor comes from the
// server-side authorization context, never from the request body.
type PilotDecision struct {
	Stage       string
	GrayPercent int
	Actor       string
	Reason      string
}

// PilotFlagState is the current stored state one decision is checked
// against; a flag that does not exist yet is the virtual off state.
type PilotFlagState struct {
	Stage       string
	GrayPercent int
	Exists      bool
}

// PilotDecisionCheck is the guarded outcome for one decision.
type PilotDecisionCheck int

const (
	// PilotDecisionApply: a real transition — record it and audit.
	PilotDecisionApply PilotDecisionCheck = iota
	// PilotDecisionNoop: the target already holds; nothing to record
	// (this is the idempotent replay path).
	PilotDecisionNoop
	// PilotDecisionReject: the decision is invalid or the move illegal.
	PilotDecisionReject
)

// Pilot decision sentinels.
var (
	ErrPilotDecisionInvalid   = errors.New("pilot decision fields are invalid")
	ErrPilotTransitionInvalid = errors.New("pilot stage transition is not allowed")
)

// PilotValidateDecision guards one rollout decision against the current
// flag state: field validity first (stage enum, reason 16–2000 runes —
// the table floor is 8 but a recorded rollout decision must be
// substantive, gray-only percentage), then the lifecycle matrix.
func PilotValidateDecision(current PilotFlagState, decision PilotDecision) (PilotDecisionCheck, error) {
	if !ValidPilotStage(decision.Stage) {
		return PilotDecisionReject, fmt.Errorf("%w: unknown stage %q", ErrPilotDecisionInvalid, decision.Stage)
	}
	if decision.Actor == "" {
		return PilotDecisionReject, fmt.Errorf("%w: actor is required", ErrPilotDecisionInvalid)
	}
	if reasonLen := len([]rune(decision.Reason)); reasonLen < 16 || reasonLen > 2000 {
		return PilotDecisionReject, fmt.Errorf("%w: reason must be 16-2000 characters", ErrPilotDecisionInvalid)
	}
	if decision.GrayPercent < 0 || decision.GrayPercent > 100 {
		return PilotDecisionReject, fmt.Errorf("%w: gray_percent must be 0-100", ErrPilotDecisionInvalid)
	}
	if decision.Stage != PilotStageGray && decision.GrayPercent != 0 {
		return PilotDecisionReject, fmt.Errorf("%w: gray_percent only applies to the gray stage", ErrPilotDecisionInvalid)
	}

	if !current.Exists {
		// First recorded state: register inert or start the pilot.
		// gray/full/rolled_back cannot be the opening move — shadow
		// comes first (pilot acceptance §4).
		if decision.Stage == PilotStageOff || decision.Stage == PilotStageShadow {
			return PilotDecisionApply, nil
		}
		return PilotDecisionReject, fmt.Errorf("%w: a new flag starts at off or shadow", ErrPilotTransitionInvalid)
	}

	if decision.Stage == current.Stage &&
		(decision.Stage != PilotStageGray || decision.GrayPercent == current.GrayPercent) {
		return PilotDecisionNoop, nil
	}
	if decision.Stage == current.Stage && decision.Stage == PilotStageGray {
		return PilotDecisionApply, nil // percentage adjustment within gray
	}
	if !PilotCanTransition(current.Stage, decision.Stage) {
		return PilotDecisionReject, fmt.Errorf("%w: %s → %s", ErrPilotTransitionInvalid, current.Stage, decision.Stage)
	}
	return PilotDecisionApply, nil
}
