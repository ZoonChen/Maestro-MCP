package model

import "testing"

func TestCanonicalTaskStateMachineExhaustive(t *testing.T) {
	statuses := []string{
		TaskStatusDraft, TaskStatusQueued, TaskStatusLeased, TaskStatusExecuting,
		TaskStatusValidating, TaskStatusReadyForHumanMerge, TaskStatusDone,
		TaskStatusBlocked, TaskStatusCancelling, TaskStatusCancelled,
		TaskStatusFailed, TaskStatusNeedsHuman,
	}
	for _, from := range statuses {
		if !IsTaskStatus(from) || !CanTaskTransition(from, from) {
			t.Fatalf("canonical status %q must be recognized and idempotent", from)
		}
		for _, to := range statuses {
			_, expected := taskTransitions[from][to]
			if from == to {
				expected = true
			}
			if got := CanTaskTransition(from, to); got != expected {
				t.Errorf("transition %s -> %s = %v, want %v", from, to, got, expected)
			}
		}
	}
	if IsTaskStatus("pending") || CanTaskTransition("unknown", "unknown") {
		t.Fatal("legacy/unknown values must not become new domain states")
	}
	// M2 contract freeze: the merge-fact edge EXISTS for the verified
	// webhook/reconciliation authority to drive — but it is the ONLY way
	// into done. No other state may reach it.
	if !CanTaskTransition(TaskStatusReadyForHumanMerge, TaskStatusDone) {
		t.Fatal("ready_for_human_merge -> done must exist for the verified merge fact")
	}
	for from := range taskTransitions {
		if from == TaskStatusReadyForHumanMerge || from == TaskStatusDone {
			continue
		}
		if CanTaskTransition(from, TaskStatusDone) {
			t.Fatalf("state %q must not reach done directly", from)
		}
	}
}

func TestSecondaryStateMachinesExhaustive(t *testing.T) {
	tests := []struct {
		name       string
		registry   map[string]map[string]struct{}
		isStatus   func(string) bool
		transition func(string, string) bool
	}{
		{"session", sessionTransitions, IsSessionStatus, CanSessionTransition},
		{"worker", workerTransitions, IsWorkerStatus, CanWorkerTransition},
		{"worktree", worktreeTransitions, IsWorktreeStatus, CanWorktreeTransition},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for from, next := range tc.registry {
				if !tc.isStatus(from) || !tc.transition(from, from) {
					t.Fatalf("status %q not recognized", from)
				}
				for to := range tc.registry {
					_, expected := next[to]
					if from == to {
						expected = true
					}
					if got := tc.transition(from, to); got != expected {
						t.Errorf("%s -> %s = %v, want %v", from, to, got, expected)
					}
				}
			}
			if tc.isStatus("unknown") || tc.transition("unknown", "unknown") {
				t.Fatal("unknown status accepted")
			}
		})
	}
}

func TestLegacyTaskStatusMappingIsImportOnly(t *testing.T) {
	cases := map[string]string{
		"pending":           TaskStatusQueued,
		"in_progress":       TaskStatusExecuting,
		"submitted":         TaskStatusValidating,
		"verifying":         TaskStatusValidating,
		"ready_to_merge":    TaskStatusReadyForHumanMerge,
		"merge_conflicted":  TaskStatusNeedsHuman,
		TaskStatusCancelled: TaskStatusCancelled,
	}
	for input, expected := range cases {
		got, ok := LegacyTaskStatusToCanonical(input)
		if !ok || got != expected {
			t.Errorf("map %q = %q,%v want %q,true", input, got, ok, expected)
		}
	}
	if _, ok := LegacyTaskStatusToCanonical("not-a-state"); ok {
		t.Fatal("unknown legacy state accepted")
	}
}
