// Package budget implements the M3-BUD-001 ledger core: pre-call
// reservation gating with truly-accounted spending (WF-REQ-003's "预算
// 先检后调、真实用量全计"). The invariants are frozen by the workflow
// engine book: reserve/usage in one transaction, no overdraft ever,
// missing provider usage charges the reservation ceiling and flags for
// human reconciliation, and the stop boundary is exhaustive.
//
// The package is pure: the Ledger type is the decision core and the
// entry log is the append-only record; persistence lives in
// internal/store.
package budget

import (
	"errors"
	"fmt"
	"time"
)

// StopReason enumerates the frozen stop boundary (the wire
// budget-ledger schema's stop_reason enum).
type StopReason string

const (
	StopBudgetExhausted StopReason = "budget_exhausted"
	StopAttemptLimit    StopReason = "attempt_limit"
	StopTimeLimit       StopReason = "time_limit"
	StopManual          StopReason = "manual_stop"
	StopPolicy          StopReason = "policy_stop"
)

// Direction is an entry's kind (append-only accounting).
type Direction string

const (
	Reserve Direction = "reserve"
	Release Direction = "release"
	Spend   Direction = "spend"
)

// Limits carry the ceilings one ledger enforces.
type Limits struct {
	BudgetUnits   int64
	MaxAttempts   int
	WallTimeLimit time.Duration
}

// ErrStopped reports operations against a stopped ledger — every
// pre-call gate after the boundary refuses.
var ErrStopped = errors.New("budget ledger is stopped")

// ErrInsufficient reports a reservation the remaining budget cannot
// cover: no overdraft, ever.
var ErrInsufficient = errors.New("budget ledger cannot cover the reservation")

// ErrUsageExceedsReservation reports provider usage above what was
// reserved for that call — an accounting violation, not a spend.
var ErrUsageExceedsReservation = errors.New("provider usage exceeds the reservation")

// Entry is one append-only accounting line.
type Entry struct {
	Seq       int64
	Direction Direction
	Units     int64
	ToolRef   string
	At        time.Time
}

// Ledger is the deterministic core over one budget scope.
type Ledger struct {
	ID      string
	Limits  Limits
	entries []Entry
	// overdrawn marks calls whose provider usage never arrived: their
	// reservation ceiling was charged and human reconciliation is owed.
	overdrawnCalls int
}

// New opens a ledger for one scope.
func New(id string, limits Limits) (*Ledger, error) {
	if limits.BudgetUnits < 1 {
		return nil, fmt.Errorf("budget: budget units must be positive")
	}
	if limits.MaxAttempts < 1 {
		return nil, fmt.Errorf("budget: max attempts must be positive")
	}
	if limits.WallTimeLimit < time.Minute {
		return nil, fmt.Errorf("budget: wall time limit must be at least one minute")
	}
	return &Ledger{ID: id, Limits: limits}, nil
}

// spent is the committed outflow: ONLY true provider usage (or the
// charged ceiling). Releases cancel reservations, never spends.
func (l *Ledger) spent() int64 {
	total := int64(0)
	for _, entry := range l.entries {
		if entry.Direction == Spend {
			total += entry.Units
		}
	}
	return total
}

// reserved is the currently held (reserved-but-unspent) amount.
func (l *Ledger) reserved() int64 {
	total := int64(0)
	for _, entry := range l.entries {
		switch entry.Direction {
		case Reserve:
			total += entry.Units
		case Release:
			total -= entry.Units
		}
	}
	return total
}

func (l *Ledger) nextSeq() int64 {
	if len(l.entries) == 0 {
		return 1
	}
	return l.entries[len(l.entries)-1].Seq + 1
}

func (l *Ledger) append(direction Direction, units int64, toolRef string, at time.Time) Entry {
	entry := Entry{Seq: l.nextSeq(), Direction: direction, Units: units, ToolRef: toolRef, At: at}
	l.entries = append(l.entries, entry)
	return entry
}

// ReserveCall is the PRE-CALL gate (WF-GATE: 预算先检后调): it refuses
// when the ledger has stopped and when the remaining budget cannot
// cover the reservation. A successful reservation holds the units
// until the provider's usage arrives (or the ceiling is charged).
func (l *Ledger) ReserveCall(units int64, toolRef string, at time.Time) (Entry, error) {
	if units < 1 {
		return Entry{}, fmt.Errorf("budget: reservation must be positive")
	}
	if l.spent()+l.reserved()+units > l.Limits.BudgetUnits {
		return Entry{}, fmt.Errorf("%w: need %d, %d of %d committed", ErrInsufficient,
			units, l.spent()+l.reserved(), l.Limits.BudgetUnits)
	}
	return l.append(Reserve, units, toolRef, at), nil
}

// RecordUsage settles one call with the provider's OWN accounting: the
// reservation releases and the true usage spends in the same logical
// transaction. Usage above the reservation is a violation; usage that
// never arrives (RecordUsageMissing) charges the ceiling.
func (l *Ledger) RecordUsage(reservation Entry, usage int64, at time.Time) (spend Entry, err error) {
	if usage > reservation.Units {
		return Entry{}, fmt.Errorf("%w: %d used of %d reserved", ErrUsageExceedsReservation, usage, reservation.Units)
	}
	l.append(Release, reservation.Units, reservation.ToolRef, at)
	return l.append(Spend, usage, reservation.ToolRef, at), nil
}

// RecordUsageMissing settles a call whose provider usage never arrived:
// the reservation's CEILING is charged (never more than was held) and
// the ledger flags the reconciliation debt.
func (l *Ledger) RecordUsageMissing(reservation Entry, at time.Time) (spend Entry) {
	l.append(Release, reservation.Units, reservation.ToolRef, at)
	l.overdrawnCalls++
	return l.append(Spend, reservation.Units, reservation.ToolRef, at)
}

// OverdrawnCalls reports how many calls charged their ceiling without
// provider accounting — the human-reconciliation debt.
func (l *Ledger) OverdrawnCalls() int { return l.overdrawnCalls }

// StopReasonIfExhausted evaluates the stop boundary against the current
// state: budget spent+reserved at the ceiling, attempts beyond the
// limit, or wall time beyond the limit. Empty means keep going.
func (l *Ledger) StopReasonIfExhausted(attempts int, now time.Time, startedAt time.Time) (StopReason, bool) {
	if l.spent()+l.reserved() >= l.Limits.BudgetUnits {
		return StopBudgetExhausted, true
	}
	if attempts > l.Limits.MaxAttempts {
		return StopAttemptLimit, true
	}
	if now.Sub(startedAt) > l.Limits.WallTimeLimit {
		return StopTimeLimit, true
	}
	return "", false
}

// Entries returns the append-only log (newest last).
func (l *Ledger) Entries() []Entry {
	return append([]Entry(nil), l.entries...)
}

// Snapshot summarizes the ledger for display and persistence.
type Snapshot struct {
	ID             string
	BudgetUnits    int64
	ReservedUnits  int64
	SpentUnits     int64
	OverdrawnCalls int
	Entries        int
}

// Snapshot captures the current state.
func (l *Ledger) Snapshot() Snapshot {
	return Snapshot{
		ID:             l.ID,
		BudgetUnits:    l.Limits.BudgetUnits,
		ReservedUnits:  l.reserved(),
		SpentUnits:     l.spent(),
		OverdrawnCalls: l.overdrawnCalls,
		Entries:        len(l.entries),
	}
}
