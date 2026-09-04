package budget

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var epoch = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

func newLedger(t *testing.T) *Ledger {
	t.Helper()
	ledger, err := New("ledger-1", Limits{BudgetUnits: 1000, MaxAttempts: 3, WallTimeLimit: time.Hour})
	require.NoError(t, err)
	return ledger
}

func TestReserveCallGatesBeforeTheCall(t *testing.T) {
	ledger := newLedger(t)

	// The pre-call gate refuses what the budget cannot cover — no
	// overdraft, ever.
	_, err := ledger.ReserveCall(1001, "tool", epoch)
	require.ErrorIs(t, err, ErrInsufficient)

	entry, err := ledger.ReserveCall(600, "model-a", epoch)
	require.NoError(t, err)
	assert.Equal(t, int64(1), entry.Seq, "entries are sequentially numbered")

	// Held reservations count against further reservations.
	_, err = ledger.ReserveCall(500, "tool", epoch)
	require.ErrorIs(t, err, ErrInsufficient, "reserved units are committed")
	_, err = ledger.ReserveCall(400, "tool", epoch)
	require.NoError(t, err)
}

func TestUsageAccountingIsTrueAndBounded(t *testing.T) {
	ledger := newLedger(t)
	reservation, err := ledger.ReserveCall(600, "model-a", epoch)
	require.NoError(t, err)

	// The provider's own numbers spend; the rest releases back.
	spend, err := ledger.RecordUsage(reservation, 420, epoch.Add(time.Second))
	require.NoError(t, err)
	assert.Equal(t, Spend, spend.Direction)
	assert.Equal(t, int64(420), spend.Units)

	snap := ledger.Snapshot()
	assert.Equal(t, int64(420), snap.SpentUnits)
	assert.Equal(t, int64(0), snap.ReservedUnits, "settled reservations release fully")

	// Usage above the reservation is an accounting violation.
	res2, err := ledger.ReserveCall(100, "model-a", epoch)
	require.NoError(t, err)
	_, err = ledger.RecordUsage(res2, 101, epoch)
	require.ErrorIs(t, err, ErrUsageExceedsReservation)
}

func TestMissingUsageChargesTheCeilingAndFlags(t *testing.T) {
	ledger := newLedger(t)
	reservation, err := ledger.ReserveCall(300, "model-a", epoch)
	require.NoError(t, err)

	spend := ledger.RecordUsageMissing(reservation, epoch)
	assert.Equal(t, int64(300), spend.Units, "the ceiling is charged")
	assert.Equal(t, 1, ledger.OverdrawnCalls(), "the reconciliation debt is flagged")

	snap := ledger.Snapshot()
	assert.Equal(t, int64(300), snap.SpentUnits)
	assert.Equal(t, int64(0), snap.ReservedUnits)
}

func TestStopBoundary(t *testing.T) {
	t.Run("budget exhaustion at the ceiling", func(t *testing.T) {
		ledger := newLedger(t)
		reservation, err := ledger.ReserveCall(1000, "model", epoch)
		require.NoError(t, err)
		reason, stop := ledger.StopReasonIfExhausted(1, epoch, epoch)
		require.True(t, stop)
		assert.Equal(t, StopBudgetExhausted, reason)
		// The next pre-call gate refuses.
		_, err = ledger.ReserveCall(1, "tool", epoch)
		require.ErrorIs(t, err, ErrInsufficient)
		_ = reservation
	})

	t.Run("attempt limit", func(t *testing.T) {
		ledger := newLedger(t)
		reason, stop := ledger.StopReasonIfExhausted(4, epoch, epoch)
		require.True(t, stop)
		assert.Equal(t, StopAttemptLimit, reason)
		_, _, noStop := reason, stop, false
		_ = noStop
	})

	t.Run("wall time", func(t *testing.T) {
		ledger := newLedger(t)
		reason, stop := ledger.StopReasonIfExhausted(1, epoch.Add(2*time.Hour), epoch)
		require.True(t, stop)
		assert.Equal(t, StopTimeLimit, reason)
	})

	t.Run("healthy ledger keeps going", func(t *testing.T) {
		ledger := newLedger(t)
		_, stop := ledger.StopReasonIfExhausted(1, epoch.Add(time.Minute), epoch)
		assert.False(t, stop)
	})
}

func TestLedgerConstructionFailsClosed(t *testing.T) {
	_, err := New("x", Limits{BudgetUnits: 0, MaxAttempts: 3, WallTimeLimit: time.Hour})
	require.Error(t, err)
	_, err = New("x", Limits{BudgetUnits: 10, MaxAttempts: 0, WallTimeLimit: time.Hour})
	require.Error(t, err)
	_, err = New("x", Limits{BudgetUnits: 10, MaxAttempts: 3, WallTimeLimit: 30 * time.Second})
	require.Error(t, err)
}

func TestEntriesAreAppendOnlyAndSequenced(t *testing.T) {
	ledger := newLedger(t)
	res, _ := ledger.ReserveCall(100, "t", epoch)
	_, _ = ledger.RecordUsage(res, 80, epoch)
	res2, _ := ledger.ReserveCall(50, "t", epoch)
	_ = ledger.RecordUsageMissing(res2, epoch)

	entries := ledger.Entries()
	// reserve(1) release(2) spend(3) reserve(4) release(5) spend(6)
	require.Len(t, entries, 6)
	for index, entry := range entries {
		assert.Equal(t, int64(index+1), entry.Seq)
	}
}
