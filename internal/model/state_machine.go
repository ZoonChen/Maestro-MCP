package model

// The registries in this file are the domain source of truth for legal state
// changes in the M0 SQLite runtime. Actor, project, Lease, version, Evidence and
// external-source guards remain mandatory in the application service.

var taskTransitions = map[string]map[string]struct{}{
	TaskStatusDraft: {
		TaskStatusQueued: {},
	},
	TaskStatusQueued: {
		TaskStatusLeased:    {},
		TaskStatusCancelled: {},
	},
	TaskStatusLeased: {
		TaskStatusExecuting:  {},
		TaskStatusQueued:     {}, // lease expires before a side effect
		TaskStatusCancelling: {},
	},
	TaskStatusExecuting: {
		TaskStatusValidating: {},
		TaskStatusBlocked:    {},
		TaskStatusCancelling: {},
		TaskStatusFailed:     {},
		TaskStatusNeedsHuman: {},
		TaskStatusQueued:     {}, // proven-safe lease expiry/recovery compensation
	},
	TaskStatusValidating: {
		TaskStatusReadyForHumanMerge: {},
		TaskStatusFailed:             {},
		TaskStatusNeedsHuman:         {},
	},
	TaskStatusReadyForHumanMerge: {
		TaskStatusValidating: {}, // SHA/evidence/policy changed
		TaskStatusNeedsHuman: {},
		// done is deliberately absent. M0 has no verified GitLab merge-fact
		// authority; M2 must introduce that transition with its durable fact.
	},
	TaskStatusBlocked: {
		TaskStatusQueued:     {},
		TaskStatusCancelling: {},
		TaskStatusNeedsHuman: {},
	},
	TaskStatusCancelling: {
		TaskStatusCancelled:  {},
		TaskStatusNeedsHuman: {},
	},
	TaskStatusFailed: {
		TaskStatusQueued:     {},
		TaskStatusNeedsHuman: {},
	},
	TaskStatusNeedsHuman: {
		TaskStatusQueued:     {},
		TaskStatusCancelling: {},
	},
	TaskStatusDone:      {},
	TaskStatusCancelled: {},
}

var sessionTransitions = map[string]map[string]struct{}{
	SessionStatusOnline:  {SessionStatusOffline: {}},
	SessionStatusOffline: {SessionStatusOnline: {}},
}

var workerTransitions = map[string]map[string]struct{}{
	WorkerStatusIdle: {
		WorkerStatusReserved: {},
		WorkerStatusBusy:     {},
		WorkerStatusLost:     {},
	},
	WorkerStatusReserved: {
		WorkerStatusBusy: {},
		WorkerStatusIdle: {},
		WorkerStatusLost: {},
	},
	WorkerStatusBusy: {
		WorkerStatusIdle: {},
		WorkerStatusLost: {},
	},
	WorkerStatusLost: {
		WorkerStatusIdle: {}, // only after recovery proves the slot is safe
	},
}

var worktreeTransitions = map[string]map[string]struct{}{
	WorktreeStatusAllocated: {
		WorktreeStatusActive:         {},
		WorktreeStatusCleanupPending: {},
		WorktreeStatusQuarantined:    {},
	},
	WorktreeStatusActive: {
		WorktreeStatusSealed:         {},
		WorktreeStatusSubmitted:      {},
		WorktreeStatusStale:          {},
		WorktreeStatusAbandoned:      {},
		WorktreeStatusQuarantined:    {},
		WorktreeStatusCleanupPending: {},
	},
	WorktreeStatusSealed: {
		WorktreeStatusSubmitted:   {},
		WorktreeStatusStale:       {},
		WorktreeStatusQuarantined: {},
	},
	WorktreeStatusSubmitted: {
		WorktreeStatusMerged:      {},
		WorktreeStatusStale:       {},
		WorktreeStatusAbandoned:   {},
		WorktreeStatusQuarantined: {},
	},
	WorktreeStatusCleanupPending: {
		WorktreeStatusAbandoned:   {},
		WorktreeStatusQuarantined: {},
	},
	WorktreeStatusStale:       {WorktreeStatusAbandoned: {}},
	WorktreeStatusMerged:      {},
	WorktreeStatusAbandoned:   {},
	WorktreeStatusQuarantined: {},
}

func canTransition(registry map[string]map[string]struct{}, from, to string) bool {
	next, ok := registry[from]
	if !ok {
		return false
	}
	if from == to {
		return true
	}
	_, ok = next[to]
	return ok
}

func CanTaskTransition(from, to string) bool { return canTransition(taskTransitions, from, to) }

func IsTaskStatus(status string) bool {
	_, ok := taskTransitions[status]
	return ok
}

// LegacyTaskStatusToCanonical is used by migration/import code only.
func LegacyTaskStatusToCanonical(status string) (string, bool) {
	switch status {
	case "pending":
		return TaskStatusQueued, true
	case "in_progress":
		return TaskStatusExecuting, true
	case "submitted", "verifying":
		return TaskStatusValidating, true
	case "ready_to_merge":
		return TaskStatusReadyForHumanMerge, true
	case "merge_conflicted":
		return TaskStatusNeedsHuman, true
	default:
		return status, IsTaskStatus(status)
	}
}

func CanSessionTransition(from, to string) bool { return canTransition(sessionTransitions, from, to) }
func IsSessionStatus(status string) bool {
	_, ok := sessionTransitions[status]
	return ok
}
func CanWorkerTransition(from, to string) bool { return canTransition(workerTransitions, from, to) }
func IsWorkerStatus(status string) bool {
	_, ok := workerTransitions[status]
	return ok
}
func CanWorktreeTransition(from, to string) bool { return canTransition(worktreeTransitions, from, to) }
func IsWorktreeStatus(status string) bool {
	_, ok := worktreeTransitions[status]
	return ok
}
