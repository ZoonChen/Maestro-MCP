// Package health provides the Control Plane dependency-health skeleton
// (M1-ARCH-001): readiness is the aggregate of named dependency probes, and
// an unreachable dependency fails readiness closed instead of degrading to
// anonymous or read-only behavior (TECH-ARCH-001 ARCH-INV-003).
//
// M0 registers no dependencies, which keeps readiness identical to the
// single-process baseline. M1 streams register their probes with the
// composition root: PostgreSQL connectivity (S1), the OIDC provider/JWKS
// cache (S2) and the runner lease pool capacity signal (S3).
package health

import (
	"context"
	"fmt"
	"sync"
)

// Dependency is one named readiness dependency of the Control Plane. Check
// must be cheap, bounded by the caller's context and side-effect free;
// implementations never repair state, they only report it.
type Dependency interface {
	Name() string
	Check(ctx context.Context) error
}

// Registry aggregates dependency probes for the readiness endpoint. The
// zero value is ready to use and reports ready when empty, which preserves
// the M0 local baseline.
type Registry struct {
	mu           sync.RWMutex
	dependencies []Dependency
}

// Register adds a dependency probe. Duplicate names are rejected so a
// miswired composition root fails fast at startup instead of double
// counting one dependency.
func (r *Registry) Register(dependency Dependency) error {
	if dependency == nil {
		return fmt.Errorf("health dependency must not be nil")
	}
	if dependency.Name() == "" {
		return fmt.Errorf("health dependency name must not be empty")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.dependencies {
		if existing.Name() == dependency.Name() {
			return fmt.Errorf("health dependency %q already registered", dependency.Name())
		}
	}
	r.dependencies = append(r.dependencies, dependency)
	return nil
}

// DependencyStatus is the probe outcome for one dependency.
type DependencyStatus struct {
	Name   string `json:"name"`
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

// Check probes every registered dependency concurrently under ctx and
// returns their statuses plus the aggregate readiness. A cancelled or
// expired context marks unprobed dependencies not ready; a dependency that
// blocks longer than the context budget fails closed.
func (r *Registry) Check(ctx context.Context) ([]DependencyStatus, bool) {
	r.mu.RLock()
	dependencies := append([]Dependency(nil), r.dependencies...)
	r.mu.RUnlock()

	statuses := make([]DependencyStatus, len(dependencies))
	if len(dependencies) == 0 {
		return statuses, true
	}

	var wg sync.WaitGroup
	for index, dependency := range dependencies {
		wg.Add(1)
		go func(index int, dependency Dependency) {
			defer wg.Done()
			status := DependencyStatus{Name: dependency.Name(), Ready: false}
			switch err := dependency.Check(ctx); {
			case err == nil:
				status.Ready = true
			case ctx.Err() != nil:
				status.Reason = "probe context expired"
			default:
				// Probe errors are operator diagnostics; they must not leak
				// credentials, DSNs or internal addresses.
				status.Reason = "dependency check failed"
			}
			statuses[index] = status
		}(index, dependency)
	}
	wg.Wait()

	ready := true
	for _, status := range statuses {
		if !status.Ready {
			ready = false
		}
	}
	return statuses, ready
}
