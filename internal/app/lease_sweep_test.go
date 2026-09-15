package app

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spySweep records the lease-sweep lifecycle the application hosts
// (S2B2-F12): started exactly once, stopped by Close's context
// cancellation, never restarted.
type spySweep struct {
	mu      sync.Mutex
	started int
	stopped int
	release chan struct{}
	RunErr  error
}

func (s *spySweep) Run(ctx context.Context) error {
	s.mu.Lock()
	s.started++
	release := s.release
	s.mu.Unlock()
	if release != nil {
		// Gate the loop so the test can observe "running" precisely.
		<-release
	}
	<-ctx.Done()
	s.mu.Lock()
	s.stopped++
	s.mu.Unlock()
	return s.RunErr
}

func TestLeaseSweepRunsUntilClose(t *testing.T) {
	gin.SetMode(gin.TestMode)
	spy := &spySweep{release: make(chan struct{})}
	a, err := New(context.Background(), Options{
		DBPath:           filepath.Join(t.TempDir(), "maestro.db"),
		MaintenanceOwner: true,
		LeaseSweep:       &LeaseSweepOptions{Monitor: spy},
	})
	require.NoError(t, err)

	// The monitor must be running by the time New returns.
	assert.Eventually(t, func() bool {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		return spy.started == 1
	}, time.Second, 5*time.Millisecond, "the sweep monitor starts with the application")

	close(spy.release)

	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not stop the lease sweep monitor")
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	assert.Equal(t, 1, spy.started, "exactly one sweep owner per process")
	assert.Equal(t, 1, spy.stopped, "the sweep stops with the application context")
}

func TestLeaseSweepAbsentStaysUnstarted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a, err := New(context.Background(), Options{
		DBPath:           filepath.Join(t.TempDir(), "maestro.db"),
		MaintenanceOwner: true,
	})
	require.NoError(t, err)
	require.NoError(t, a.Close())
	// No panic, no goroutine leak assertion beyond Close completing —
	// nil options leave the sweep unstarted by construction.
}
