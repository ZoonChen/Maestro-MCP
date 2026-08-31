package webhook

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// Dispatch outcomes reported to the worker loop.
const (
	DispatchEmpty    = "empty"
	DispatchApplied  = "applied"
	DispatchRetry    = "retry"
	DispatchDead     = "dead_letter"
	DispatchInternal = "internal_error"
)

// Dispatcher drains the webhook inbox: each claimed row is decrypted,
// resolved to a Maestro project, and applied exactly once as the frozen
// gitlab.webhook.received outbox envelope. Terminal data defects
// (undecryptable body, unresolvable project, unmapped project) go to the
// DLQ; transient defects retry with exponential backoff and jitter until
// the attempt budget is spent (M2-WHK-001).
type Dispatcher struct {
	Store       Store
	Cipher      *PayloadCipher
	MaxAttempts int           // attempts beyond which a retrying row dead-letters
	BaseBackoff time.Duration // first retry delay
	MaxBackoff  time.Duration // backoff ceiling
	randFloat   func() float64
}

// DispatchOne claims and settles at most one inbox row. A nil error with
// outcome DispatchEmpty means the inbox is currently drained; every other
// outcome has already been durably settled (or the error explains why the
// row is stranded for the stale-claim reclaim).
func (d *Dispatcher) DispatchOne(ctx context.Context, owner string) (string, error) {
	row, err := d.Store.ClaimInbox(ctx, owner)
	if err != nil {
		return DispatchInternal, err
	}
	if row == nil {
		return DispatchEmpty, nil
	}

	plain, err := d.Cipher.Open(row.RawBodyEncrypted)
	if err != nil {
		return d.settle(ctx, func(unit ApplyUnit) error {
			return unit.MarkDeadLetter(ctx, row.ID, owner, ReasonDecryptFailed)
		}, DispatchDead)
	}
	gitlabProject, known := ProjectOf(plain)
	if !known {
		return d.settle(ctx, func(unit ApplyUnit) error {
			return unit.MarkDeadLetter(ctx, row.ID, owner, ReasonPayloadInvalid)
		}, DispatchDead)
	}

	unit, err := d.Store.BeginApply(ctx)
	if err != nil {
		return DispatchInternal, err
	}
	defer func() { _ = unit.Rollback() }() // no-op after a successful commit

	projectID, err := unit.ProjectForMapping(ctx, row.InstanceID, gitlabProject)
	if err != nil {
		return DispatchInternal, err
	}
	if projectID == "" {
		// Events for projects this deployment never mapped are recorded
		// then dead-lettered: a visible signal to map or unregister the
		// hook, never silently discarded and never auto-mapped.
		if err := unit.MarkDeadLetter(ctx, row.ID, owner, ReasonUnmappedProject); err != nil {
			return DispatchInternal, err
		}
		if err := unit.Commit(); err != nil {
			return DispatchInternal, err
		}
		return DispatchDead, nil
	}

	envelope, err := ReceivedEnvelope(row, projectID)
	if err != nil {
		return DispatchInternal, err
	}
	if err := unit.EnqueueOutbox(ctx, envelope); err != nil && !errors.Is(err, ErrEnvelopeDuplicate) {
		// The envelope identity equals the inbox identity, so a duplicate
		// is the crash-replay idempotence path, not a defect. Both retry
		// and exhaustion are SETTLED outcomes (the row is durably parked),
		// so they report nil error; the transient cause is logged here
		// because the delivery audit trail has no retry outcome.
		slog.WarnContext(ctx, "webhook dispatch: transient failure settled as retry",
			"inbox_id", row.ID, "attempts", row.Attempts, "error", err.Error())
		if row.Attempts >= d.MaxAttempts {
			if deadErr := unit.MarkDeadLetter(ctx, row.ID, owner, ReasonRetryExhausted); deadErr != nil {
				return DispatchInternal, deadErr
			}
			if err := unit.Commit(); err != nil {
				return DispatchInternal, err
			}
			return DispatchDead, nil
		}
		if err := unit.MarkRetry(ctx, row.ID, owner, d.backoffFor(row.Attempts)); err != nil {
			return DispatchInternal, err
		}
		if err := unit.Commit(); err != nil {
			return DispatchInternal, err
		}
		return DispatchRetry, nil
	}
	if err := unit.MarkProcessed(ctx, row.ID, owner); err != nil {
		return DispatchInternal, err
	}
	if err := unit.Commit(); err != nil {
		return DispatchInternal, err
	}
	return DispatchApplied, nil
}

// settle runs one terminal settlement in its own transaction.
func (d *Dispatcher) settle(ctx context.Context, mark func(ApplyUnit) error, outcome string) (string, error) {
	unit, err := d.Store.BeginApply(ctx)
	if err != nil {
		return DispatchInternal, err
	}
	defer func() { _ = unit.Rollback() }() // no-op after a successful commit
	if err := mark(unit); err != nil {
		return DispatchInternal, err
	}
	if err := unit.Commit(); err != nil {
		return DispatchInternal, err
	}
	return outcome, nil
}

// backoffFor computes the retry delay for the Nth attempt with jitter in
// [1.0x, 1.3x) so concurrent retry storms decorrelate.
func (d *Dispatcher) backoffFor(attempts int) time.Duration {
	backoff := d.BaseBackoff
	for i := 1; i < attempts && backoff < d.MaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > d.MaxBackoff {
		backoff = d.MaxBackoff
	}
	jitter := 0.3
	if d.randFloat != nil {
		jitter = d.randFloat() * 0.3
	}
	return time.Duration(float64(backoff) * (1 + jitter))
}

// Run drains the inbox until the context is cancelled. It is the worker
// loop body: one row per iteration keeps settle transactions small and
// lets shutdown land between rows.
func (d *Dispatcher) Run(ctx context.Context, owner string, interval time.Duration, onError func(err error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for {
			outcome, err := d.DispatchOne(ctx, owner)
			if err != nil && onError != nil {
				onError(err)
			}
			if outcome == DispatchEmpty || ctx.Err() != nil {
				break
			}
		}
	}
}
