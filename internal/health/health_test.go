package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubDependency struct {
	name  string
	check func(ctx context.Context) error
}

func (s stubDependency) Name() string { return s.name }

func (s stubDependency) Check(ctx context.Context) error { return s.check(ctx) }

func TestEmptyRegistryIsReady(t *testing.T) {
	registry := &Registry{}
	statuses, ready := registry.Check(context.Background())
	assert.Empty(t, statuses)
	assert.True(t, ready, "the M0 baseline registers no dependencies and stays ready")
}

func TestFailingDependencyFailsReadinessClosed(t *testing.T) {
	registry := &Registry{}
	require.NoError(t, registry.Register(stubDependency{
		name: "postgres",
		check: func(ctx context.Context) error {
			return errors.New("dial tcp 10.0.0.5:5432: password auth failed for dsn postgres://admin:hunter2@db")
		},
	}))
	require.NoError(t, registry.Register(stubDependency{name: "oidc", check: func(ctx context.Context) error { return nil }}))

	statuses, ready := registry.Check(context.Background())
	require.False(t, ready)
	require.Len(t, statuses, 2)

	var postgresStatus DependencyStatus
	for _, status := range statuses {
		if status.Name == "postgres" {
			postgresStatus = status
		}
	}
	assert.False(t, postgresStatus.Ready)
	assert.Equal(t, "dependency check failed", postgresStatus.Reason)
	assert.NotContains(t, postgresStatus.Reason, "hunter2", "probe reasons must not leak credentials or addresses")
}

func TestRegisterRejectsNilAndDuplicates(t *testing.T) {
	registry := &Registry{}
	require.Error(t, registry.Register(nil))

	require.NoError(t, registry.Register(stubDependency{name: "runner-pool", check: func(ctx context.Context) error { return nil }}))
	err := registry.Register(stubDependency{name: "runner-pool", check: func(ctx context.Context) error { return nil }})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already registered")

	require.Error(t, registry.Register(stubDependency{name: "", check: func(ctx context.Context) error { return nil }}))
}

func TestExpiredProbeContextFailsClosed(t *testing.T) {
	registry := &Registry{}
	require.NoError(t, registry.Register(stubDependency{
		name: "slow-oidc",
		check: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	statuses, ready := registry.Check(ctx)
	require.False(t, ready)
	require.Len(t, statuses, 1)
	assert.Equal(t, "probe context expired", statuses[0].Reason)
}
