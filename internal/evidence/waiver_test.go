package evidence

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewWaiverValidation(t *testing.T) {
	resolved, err := ResolveEffective(testCompanyPolicy(), nil)
	require.NoError(t, err)

	valid := WaiverRequestInput{
		GateID:          StableGateID(evalTuple(), GateUnit),
		Check:           GateUnit,
		SourceSHA:       sha(40),
		MergeRequestIID: 7,
		Requester:       "req-1",
		Reason:          "infra flake documented in ticket-123",
		ExpiresAt:       evalNow.Add(48 * time.Hour),
	}

	waiver, err := NewWaiver(resolved, valid, evalNow)
	require.NoError(t, err)
	assert.Equal(t, WaiverRequested, waiver.State)

	cases := []struct {
		name   string
		mutate func(*WaiverRequestInput)
	}{
		{"non-waivable principle", func(w *WaiverRequestInput) { w.Check = "sha_integrity" }},
		{"non-waivable gate kind", func(w *WaiverRequestInput) { w.Check = GatePolicyIntegrity }},
		{"longer than seven days", func(w *WaiverRequestInput) { w.ExpiresAt = evalNow.Add(8 * 24 * time.Hour) }},
		{"already expired", func(w *WaiverRequestInput) { w.ExpiresAt = evalNow.Add(-time.Hour) }},
		{"missing expiry", func(w *WaiverRequestInput) { w.ExpiresAt = time.Time{} }},
		{"short reason", func(w *WaiverRequestInput) { w.Reason = "too short" }},
		{"missing requester", func(w *WaiverRequestInput) { w.Requester = "" }},
		{"bad source SHA", func(w *WaiverRequestInput) { w.SourceSHA = "not-a-sha" }},
		{"missing merge request", func(w *WaiverRequestInput) { w.MergeRequestIID = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.mutate(&input)
			_, err := NewWaiver(resolved, input, evalNow)
			assert.Error(t, err)
		})
	}
}

func TestWaiverLifecycle(t *testing.T) {
	resolved, _ := ResolveEffective(testCompanyPolicy(), nil)
	base := WaiverRequestInput{
		GateID:          StableGateID(evalTuple(), GateUnit),
		Check:           GateUnit,
		SourceSHA:       sha(40),
		MergeRequestIID: 7,
		Requester:       "req-1",
		Reason:          "infra flake documented in ticket-456",
		ExpiresAt:       evalNow.Add(24 * time.Hour),
	}

	t.Run("approve requires a distinct approver", func(t *testing.T) {
		waiver, err := NewWaiver(resolved, base, evalNow)
		require.NoError(t, err)
		assert.ErrorIs(t, waiver.Approve("req-1"), ErrWaiverSelfApprove)
		require.NoError(t, waiver.Approve("app-1"))
		assert.Equal(t, WaiverApproved, waiver.State)
		assert.True(t, waiver.IsValid(evalNow))
	})

	t.Run("double approval is rejected", func(t *testing.T) {
		waiver, _ := NewWaiver(resolved, base, evalNow)
		require.NoError(t, waiver.Approve("app-1"))
		assert.ErrorIs(t, waiver.Approve("app-1"), ErrWaiverState)
	})

	t.Run("reject and revoke are terminal", func(t *testing.T) {
		rejected, _ := NewWaiver(resolved, base, evalNow)
		require.NoError(t, rejected.Reject("app-2"))
		assert.Equal(t, WaiverRejected, rejected.State)
		assert.False(t, rejected.IsValid(evalNow))
		assert.ErrorIs(t, rejected.Approve("app-3"), ErrWaiverState)

		revoked, _ := NewWaiver(resolved, base, evalNow)
		require.NoError(t, revoked.Approve("app-2"))
		require.NoError(t, revoked.Revoke())
		assert.Equal(t, WaiverRevoked, revoked.State)
		assert.False(t, revoked.IsValid(evalNow))
	})

	t.Run("expiry invalidates lazily", func(t *testing.T) {
		waiver, _ := NewWaiver(resolved, base, evalNow)
		require.NoError(t, waiver.Approve("app-2"))
		later := evalNow.Add(25 * time.Hour)
		assert.True(t, waiver.Expire(later))
		assert.Equal(t, WaiverExpired, waiver.State)
		assert.False(t, waiver.IsValid(later))
		assert.False(t, waiver.Expire(later.Add(time.Hour)), "expiry is idempotent")
	})
}
