package runner

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Daemon drives the member-side Runner lifecycle over the frozen outbound
// protocol (M1-RUN-001, SEC-RUNNER-SECURITY section 5): long-poll claim,
// heartbeats every 15 seconds while a lease is held, terminal protocol
// refusals stop the process, transport trouble retries with bounded
// backoff. Every message carries the daemon's connection generation so the
// server can fence stale authority.

// Frozen liveness windows (config section `runner`, tools of the same
// numbers govern the server side).
const (
	HeartbeatInterval = 15 * time.Second
	ClaimBackoffBase  = 2 * time.Second
	ClaimBackoffCap   = 30 * time.Second
)

// Executor performs the leased work. The rootless OCI sandbox (S3b)
// implements it; tests use fakes. The daemon never inspects work content —
// it transports the server's digest-pinned profile references and the
// executor's outcomes.
type Executor interface {
	// Execute runs one lease to a terminal outcome. The heartbeat context
	// is cancelled when the lease deadline passes; executors must stop.
	Execute(ctx context.Context, lease *Lease, heartbeat func() error) (ExecutionCompletion, error)
}

// DaemonConfig controls the loop timing.
type DaemonConfig struct {
	// ClaimWaitSeconds is the long-poll wait (schema bound 1..30).
	ClaimWaitSeconds int
	// Capabilities advertised on every claim (schema minimum three).
	Capabilities []string
	// Heartbeat cadence; defaults to the frozen 15s.
	HeartbeatInterval time.Duration
	// Now/AfterSleep are injection points for tests.
	NowFunc   func() time.Time
	SleepFunc func(ctx context.Context, d time.Duration) error
}

func (c *DaemonConfig) normalize() error {
	if c.ClaimWaitSeconds < 1 || c.ClaimWaitSeconds > MaxWaitSeconds {
		return fmt.Errorf("runner: claim wait must be 1..%d seconds", MaxWaitSeconds)
	}
	if len(c.Capabilities) < 3 {
		return fmt.Errorf("runner: at least three capabilities are required by the frozen schema")
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = HeartbeatInterval
	}
	if c.NowFunc == nil {
		c.NowFunc = time.Now
	}
	if c.SleepFunc == nil {
		c.SleepFunc = func(ctx context.Context, d time.Duration) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	return nil
}

// Daemon is one Runner connection lifecycle.
type Daemon struct {
	client   *Client
	executor Executor
	config   DaemonConfig

	generation string
	// claims is the monotonically increasing idempotency sequence; keys are
	// unique per connection so retries replay safely.
	sequence int
}

// NewDaemon validates the wiring and mints a fresh connection generation.
func NewDaemon(client *Client, executor Executor, config DaemonConfig) (*Daemon, error) {
	if client == nil || executor == nil {
		return nil, fmt.Errorf("runner: client and executor are required")
	}
	if err := config.normalize(); err != nil {
		return nil, err
	}
	generation, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 only fails when the crypto source is dead.
		if _, cryptoErr := rand.Read(make([]byte, 1)); cryptoErr != nil {
			return nil, fmt.Errorf("runner: connection generation: %w", err)
		}
		generation = uuid.Must(uuid.NewRandom())
	}
	return &Daemon{
		client:     client,
		executor:   executor,
		config:     config,
		generation: generation.String(),
	}, nil
}

// Generation exposes the connection generation for diagnostics.
func (d *Daemon) Generation() string { return d.generation }

// Run loops until the context is cancelled or the Control Plane returns a
// terminal refusal (revocation, fencing, bad request). A nil error return
// means the context ended the session; a non-nil error is terminal and the
// process must not silently continue.
func (d *Daemon) Run(ctx context.Context) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			// A cancelled context ends the session; that is a clean stop.
			return nil //nolint:nilerr // cancellation is the intended shutdown path
		}
		lease, noWork, err := d.claim(ctx)
		if err != nil {
			if isTerminal(err) {
				return err
			}
			attempt++
			if sleepErr := d.config.SleepFunc(ctx, Backoff(attempt, ClaimBackoffBase, ClaimBackoffCap)); sleepErr != nil {
				return nil //nolint:nilerr // interrupted sleep means shutdown was requested
			}
			continue
		}
		attempt = 0
		if noWork != nil {
			wait := time.Duration(noWork.RetryAfterSeconds) * time.Second
			if wait <= 0 {
				wait = ClaimBackoffBase
			}
			if sleepErr := d.config.SleepFunc(ctx, wait); sleepErr != nil {
				return nil //nolint:nilerr // interrupted sleep means shutdown was requested
			}
			continue
		}
		if err := d.executeLease(ctx, lease); err != nil {
			if isTerminal(err) {
				return err
			}
			// Transport trouble during reporting: the lease will expire or
			// be fenced server-side; back off and re-poll.
			if sleepErr := d.config.SleepFunc(ctx, Backoff(1, ClaimBackoffBase, ClaimBackoffCap)); sleepErr != nil {
				return nil //nolint:nilerr // interrupted sleep means shutdown was requested
			}
		}
	}
}

func (d *Daemon) claim(ctx context.Context) (*Lease, *NoWork, error) {
	d.sequence++
	return d.client.ClaimLease(ctx, ClaimRequest{
		ProtocolVersion:      ProtocolVersion,
		ConnectionGeneration: d.generation,
		Capabilities:         d.config.Capabilities,
		WaitSeconds:          d.config.ClaimWaitSeconds,
	}, fmt.Sprintf("daemon-%s-claim-%06d", d.generation, d.sequence))
}

func (d *Daemon) executeLease(ctx context.Context, lease *Lease) error {
	leaseCtx, cancel := context.WithDeadline(ctx, lease.ExpiresAt)
	defer cancel()

	heartbeat := func() error {
		if err := d.client.HeartbeatLease(leaseCtx, lease.ID, HeartbeatRequest{
			LeaseVersion:         lease.Version,
			ConnectionGeneration: d.generation,
			ObservedAt:           d.config.NowFunc().UTC(),
		}, d.idempotencyKey("heartbeat")); err != nil {
			return fmt.Errorf("heartbeat rejected: %w", err)
		}
		return nil
	}

	completion, err := d.executor.Execute(leaseCtx, lease, heartbeat)
	if err != nil {
		// The executor failed to reach an outcome: report the failure
		// honestly; the server decides the consequence.
		completion = ExecutionCompletion{
			LeaseID:              lease.ID,
			LeaseVersion:         lease.Version,
			ConnectionGeneration: d.generation,
			WorkspaceGeneration:  lease.WorkspaceGeneration,
			Outcome:              OutcomeFailed,
			Summary:              truncate(err.Error(), 8000),
		}
	}
	if completion.ConnectionGeneration == "" {
		completion.ConnectionGeneration = d.generation
	}
	return d.client.CompleteExecution(ctx, lease.ExecutionID, completion, d.idempotencyKey("complete"))
}

func (d *Daemon) idempotencyKey(purpose string) string {
	d.sequence++
	return fmt.Sprintf("daemon-%s-%s-%06d", d.generation, purpose, d.sequence)
}

func isTerminal(err error) bool {
	protocolErr, ok := err.(*ProtocolError)
	return ok && protocolErr.Terminal()
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
