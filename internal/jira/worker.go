package jira

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// The W4.5 J3 sync worker: one cycle runs the mirror phase (SoR→Jira
// push + reverse read-only snapshot) then the reconcile phase (JQL
// batch pull → snapshot → divergence detection). Sequential phases in
// one cycle keep the snapshot monotonic: the reconcile search never
// races a mirror push into a phantom divergence.
//
// Fail-closed semantics (task brief J3-2): a provider outage aborts
// the whole cycle — anchors keep their last bookkeeping, no detection
// happens, no escalation clock advances — and the task flow is never
// blocked (the worker is a background loop, never on the request
// path).

// StatusLabelPrefix is the one status coupling: Maestro state reaches
// Jira only as a `maestro:<status>` label. The Jira issue status
// field is never written; the Maestro work-item status never takes a
// Jira value (blueprint section 1.2 rule 1).
const StatusLabelPrefix = "maestro:"

// StatusLabelOf maps one Maestro work-item status to its Jira label.
func StatusLabelOf(status string) string {
	return StatusLabelPrefix + status
}

// AnchorSource is the store surface the mirror phase consumes.
type AnchorSource interface {
	ListAllJiraAnchors(ctx context.Context) ([]store.JiraAnchorView, error)
	JiraFrozenFields(ctx context.Context, projectID, anchorID string) ([]string, error)
	UpdateJiraSnapshot(ctx context.Context, projectID, anchorID string, snapshot store.JiraIssueSnapshot) error
	RecordJiraMirrorOutcome(ctx context.Context, projectID, anchorID string, ok bool, errorMessage string) error
	JiraMarkPushed(ctx context.Context, projectID, anchorID string, marks store.JiraPushedMarks) error
}

// ReconcileSource is the store surface the reconcile phase consumes.
type ReconcileSource interface {
	ListAllJiraAnchors(ctx context.Context) ([]store.JiraAnchorView, error)
	UpdateJiraSnapshot(ctx context.Context, projectID, anchorID string, snapshot store.JiraIssueSnapshot) error
	UpsertJiraDivergence(ctx context.Context, projectID, anchorID, field, sorValue, mirrorValue string) (store.JiraReconcileItem, bool, error)
	HealJiraDivergence(ctx context.Context, projectID, anchorID, field string) error
}

// ClientFactory builds the provider client; injectable for the stub
// sandbox, pinned to NewClient in production.
type ClientFactory func() (*Client, error)

// Worker drives the mirror+reconcile cycle.
type Worker struct {
	Anchors    AnchorSource
	Reconcile  ReconcileSource
	NewClient  ClientFactory
	Now        func() time.Time
	OnCycleErr func(error)
}

// MirrorPhase pushes SoR values to every anchored issue and refreshes
// the read-only snapshot. Per-anchor failures (a deleted issue, a
// rejected field) are recorded on the anchor and never abort the
// batch; provider-level failures (unreachable, bad credential) abort
// fail-closed.
func (w *Worker) MirrorPhase(ctx context.Context) error {
	anchors, err := w.Anchors.ListAllJiraAnchors(ctx)
	if err != nil {
		return fmt.Errorf("jira mirror: list anchors: %w", err)
	}
	if len(anchors) == 0 {
		return nil
	}
	client, err := w.NewClient()
	if err != nil {
		return fmt.Errorf("jira mirror: client: %w", err)
	}

	for _, view := range anchors {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := w.mirrorOne(ctx, client, view); err != nil {
			if errors.Is(err, ErrJiraUnavailable) || errors.Is(err, ErrJiraAuthFailed) {
				return err
			}
			// Per-anchor condition (missing issue, rejected update):
			// honest bookkeeping, batch continues.
			if recordErr := w.Anchors.RecordJiraMirrorOutcome(ctx, view.Anchor.ProjectID, view.Anchor.ID, false, err.Error()); recordErr != nil {
				return fmt.Errorf("jira mirror: record failure: %w", recordErr)
			}
			continue
		}
		if recordErr := w.Anchors.RecordJiraMirrorOutcome(ctx, view.Anchor.ProjectID, view.Anchor.ID, true, ""); recordErr != nil {
			return fmt.Errorf("jira mirror: record success: %w", recordErr)
		}
	}
	return nil
}

// mirrorOne runs the anchor's mirror cycle. The pushed-* memory is
// the direction discriminator (blueprint section 1.2):
//
//   - desired value differs from what Maestro last established → the
//     SoR changed → push (and remember);
//   - desired equals the pushed memory but the Jira side shows
//     something else → a Jira-side drift → DO NOT push; the reconcile
//     phase opens the divergence and only a human adjudication may
//     overwrite it (accept_sor re-arms the push).
//
// The one honest race: a Jira-side edit and a SoR change landing in
// the same window push the SoR value and the intermediate Jira edit
// never reaches the list — SoR wins by definition when it moved.
func (w *Worker) mirrorOne(ctx context.Context, client *Client, view store.JiraAnchorView) error {
	anchor := view.Anchor
	remote, err := client.Issue(ctx, anchor.IssueKey)
	if err != nil {
		return err
	}
	if err := w.Anchors.UpdateJiraSnapshot(ctx, anchor.ProjectID, anchor.ID, snapshotOf(remote, w.now())); err != nil {
		return fmt.Errorf("jira mirror: snapshot: %w", err)
	}

	frozen := map[string]bool{}
	fields, err := w.Anchors.JiraFrozenFields(ctx, anchor.ProjectID, anchor.ID)
	if err != nil {
		return fmt.Errorf("jira mirror: frozen fields: %w", err)
	}
	for _, field := range fields {
		frozen[field] = true
	}

	update := MirrorFieldUpdate{}
	marks := store.JiraPushedMarks{}

	// Title channel.
	if !frozen[store.JiraFieldTitle] && pushedChanged(anchor.PushedTitle, view.WorkItemTitle) {
		if remote.Title == view.WorkItemTitle {
			marks.Title = &view.WorkItemTitle // confirmed without a write
		} else {
			title := view.WorkItemTitle
			update.Summary = &title
			marks.Title = &title
		}
	}
	// Assignee channel.
	if !frozen[store.JiraFieldAssignee] && pushedChanged(anchor.PushedAssignee, anchor.Assignee) {
		if remote.Assignee == anchor.Assignee {
			marks.Assignee = &anchor.Assignee
		} else {
			assignee := anchor.Assignee
			update.Assignee = &assignee
			marks.Assignee = &assignee
		}
	}
	// Label channels: the mirror owns exactly two slots — the
	// iteration label and the maestro status label. Human labels are
	// preserved verbatim. Either slot needing a push rewrites the
	// whole label set (one wire field), unless a slot is frozen — a
	// frozen slot holds the Jira-side state, so the labels push waits
	// for the adjudication.
	iterationStatus := StatusLabelOf(view.WorkItemStatus)
	iterationNeedsPush := anchor.IterationLabel != "" &&
		!frozen[store.JiraFieldIteration] && pushedChanged(anchor.PushedIteration, anchor.IterationLabel)
	statusNeedsPush := !frozen[store.JiraFieldStatus] &&
		pushedChanged(anchor.PushedStatusLabel, iterationStatus)
	if iterationNeedsPush || statusNeedsPush {
		iterationPresent := containsLabel(remote.Labels, anchor.IterationLabel)
		statusPresent := containsLabel(remote.Labels, iterationStatus)
		slotsAbsent := (iterationNeedsPush && !iterationPresent) || (statusNeedsPush && !statusPresent)
		labelsPushed := false
		if slotsAbsent && !frozen[store.JiraFieldIteration] && !frozen[store.JiraFieldStatus] {
			update.Labels = desiredLabelsOf(remote.Labels, anchor.IterationLabel, iterationStatus)
			labelsPushed = true
		}
		// A mark is honest only when the slot really holds the SoR
		// value on the Jira side (confirmed, or established by the
		// push above).
		if iterationNeedsPush && (iterationPresent || labelsPushed) {
			marks.Iteration = &anchor.IterationLabel
		}
		if statusNeedsPush && (statusPresent || labelsPushed) {
			marks.StatusLabel = &iterationStatus
		}
	}

	if !update.IsEmpty() {
		if err := client.UpdateMirrorFields(ctx, anchor.IssueKey, update); err != nil {
			return err
		}
		// Record the established state: what was pushed is the new
		// observed truth for the pushed fields; unpushed fields keep
		// the GET observation.
		established := *remote
		if update.Summary != nil {
			established.Title = *update.Summary
		}
		if update.Assignee != nil {
			established.Assignee = *update.Assignee
		}
		if update.Labels != nil {
			established.Labels = update.Labels
		}
		if err := w.Anchors.UpdateJiraSnapshot(ctx, anchor.ProjectID, anchor.ID, snapshotOf(&established, w.now())); err != nil {
			return fmt.Errorf("jira mirror: post-push snapshot: %w", err)
		}
	}
	if marks != (store.JiraPushedMarks{}) {
		if err := w.Anchors.JiraMarkPushed(ctx, anchor.ProjectID, anchor.ID, marks); err != nil {
			return fmt.Errorf("jira mirror: mark pushed: %w", err)
		}
	}
	return nil
}

// pushedChanged reports whether the SoR value moved past what Maestro
// last established (nil memory = never established or re-armed).
func pushedChanged(pushed *string, desired string) bool {
	if pushed == nil {
		return true
	}
	return *pushed != desired
}

// desiredLabelsOf rebuilds the label set: every human label survives;
// the iteration slot and the status slot carry the Maestro values.
func desiredLabelsOf(observed []string, iteration, statusLabel string) []string {
	desired := make([]string, 0, len(observed)+2)
	for _, label := range observed {
		if label == iteration || label == statusLabel || hasPrefix(label, StatusLabelPrefix) {
			continue
		}
		desired = append(desired, label)
	}
	if iteration != "" {
		desired = append(desired, iteration)
	}
	return append(desired, statusLabel)
}

func hasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

// ReconcilePhase pulls every anchored issue in JQL batches, refreshes
// the snapshots, then detects the four field divergences. Converged
// fields heal their live items; live divergences advance one cycle
// (the frozen two-cycle line escalates inside the store, atomically
// with its audit row).
func (w *Worker) ReconcilePhase(ctx context.Context) error {
	anchors, err := w.Reconcile.ListAllJiraAnchors(ctx)
	if err != nil {
		return fmt.Errorf("jira reconcile: list anchors: %w", err)
	}
	if len(anchors) == 0 {
		return nil
	}
	client, err := w.NewClient()
	if err != nil {
		return fmt.Errorf("jira reconcile: client: %w", err)
	}

	observed := map[string]RemoteIssue{}
	for start := 0; start < len(anchors); start += reconcileBatchSize {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		end := start + reconcileBatchSize
		if end > len(anchors) {
			end = len(anchors)
		}
		keys := make([]string, 0, end-start)
		for _, view := range anchors[start:end] {
			keys = append(keys, view.Anchor.IssueKey)
		}
		issues, searchErr := client.Search(ctx, issueKeyJQL(keys), end-start)
		if searchErr != nil {
			return searchErr
		}
		for _, issue := range issues {
			observed[issue.Key] = issue
		}
	}

	now := w.now()
	for _, view := range anchors {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		anchor := view.Anchor
		remote, ok := observed[anchor.IssueKey]
		if !ok {
			// The anchored issue vanished from the search results —
			// an honest per-anchor degradation, not a transport
			// failure; detection skips it this cycle.
			slog.Warn("jira reconcile: anchored issue missing from search", "issue_key", anchor.IssueKey)
			continue
		}
		if err := w.Reconcile.UpdateJiraSnapshot(ctx, anchor.ProjectID, anchor.ID, snapshotOf(&remote, now)); err != nil {
			return fmt.Errorf("jira reconcile: snapshot: %w", err)
		}

		statusLabel := StatusLabelOf(view.WorkItemStatus)
		divergences := []struct {
			field       string
			sor, mirror string
			converged   bool
		}{
			{store.JiraFieldTitle, view.WorkItemTitle, remote.Title, view.WorkItemTitle == remote.Title},
			{store.JiraFieldAssignee, anchor.Assignee, remote.Assignee, anchor.Assignee == remote.Assignee},
			{store.JiraFieldIteration, anchor.IterationLabel, iterationMirrorValue(remote.Labels, anchor.IterationLabel),
				anchor.IterationLabel == "" || containsLabel(remote.Labels, anchor.IterationLabel)},
			{store.JiraFieldStatus, statusLabel, labelSetString(remote.Labels),
				containsLabel(remote.Labels, statusLabel)},
		}
		for _, divergence := range divergences {
			if divergence.converged {
				if err := w.Reconcile.HealJiraDivergence(ctx, anchor.ProjectID, anchor.ID, divergence.field); err != nil {
					return fmt.Errorf("jira reconcile: heal %s: %w", divergence.field, err)
				}
				continue
			}
			if _, _, upsertErr := w.Reconcile.UpsertJiraDivergence(ctx, anchor.ProjectID, anchor.ID,
				divergence.field, divergence.sor, divergence.mirror); upsertErr != nil {
				return fmt.Errorf("jira reconcile: upsert %s: %w", divergence.field, upsertErr)
			}
		}
	}
	return nil
}

// reconcileBatchSize bounds one JQL IN clause (Jira's practical
// URL/body budget); 50 keys is comfortably inside it.
const reconcileBatchSize = 50

// issueKeyJQL builds the batched issuekey IN (...) query.
func issueKeyJQL(keys []string) string {
	expr := ""
	for index, key := range keys {
		if index > 0 {
			expr += ", "
		}
		expr += key
	}
	return "issuekey IN (" + expr + ")"
}

// iterationMirrorValue describes the Jira side of the iteration slot:
// the matching label when present, otherwise the honest absent/other
// view (display only — the field takes accept_sor adjudications).
func iterationMirrorValue(labels []string, iteration string) string {
	if containsLabel(labels, iteration) {
		return iteration
	}
	human := make([]string, 0, len(labels))
	for _, label := range labels {
		if !hasPrefix(label, StatusLabelPrefix) {
			human = append(human, label)
		}
	}
	if len(human) == 0 {
		return "(absent)"
	}
	return labelSetString(human)
}

func labelSetString(labels []string) string {
	if len(labels) == 0 {
		return "(no labels)"
	}
	result := labels[0]
	for _, label := range labels[1:] {
		result += ", " + label
	}
	return result
}

func containsLabel(labels []string, wanted string) bool {
	for _, label := range labels {
		if label == wanted {
			return true
		}
	}
	return false
}

func snapshotOf(remote *RemoteIssue, at time.Time) store.JiraIssueSnapshot {
	return store.JiraIssueSnapshot{
		IssueKey:   remote.Key,
		Title:      remote.Title,
		Assignee:   remote.Assignee,
		Labels:     remote.Labels,
		Status:     remote.Status,
		SnapshotAt: at,
	}
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// Run drives the cycle loop until the context drains; provider
// failures are surfaced through OnCycleErr (degraded, never fatal).
func (w *Worker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.Cycle(ctx); err != nil && w.OnCycleErr != nil && ctx.Err() == nil {
			w.OnCycleErr(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Cycle is one mirror pass followed by one reconcile pass.
func (w *Worker) Cycle(ctx context.Context) error {
	if err := w.MirrorPhase(ctx); err != nil {
		return err
	}
	return w.ReconcilePhase(ctx)
}
