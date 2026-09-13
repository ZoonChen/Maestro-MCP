package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// DomainEventSink (W6-5, S2C-A5/O-1): the terminal consumer for outbox
// channels without a dedicated subscriber. The W1 shadow observation
// found 187 asset.*/workgraph.* events spinning in retry because the
// only outbox consumer owns exactly the gitlab.webhook.received
// channel; every other registered event had no sink, and the webhook
// consumer re-armed each foreign claim instantly — an idle retry loop
// with no ceiling.
//
// Ownership model: this sink claims every dispatchable event EXCEPT the
// exclusion list (the channels dedicated consumers own — today the
// webhook envelope channel) and settles them DELIVERED. The durable
// record of every domain event already lives in the append-only audit
// chain (written atomically with the outbox row, WGM-INV-012); the
// outbox row's delivery closes the fan-out ledger for channels nobody
// subscribes to in this deployment. When a real subscriber lands for a
// channel, wire it and add the channel to the exclusion list.
type DomainEventSink struct {
	// Source claims and settles outbox rows.
	Source DomainSinkSource
	// ExcludedEventTypes are the channels owned by dedicated consumers;
	// the sink never claims them.
	ExcludedEventTypes []string
	// BatchSize bounds one claim (default 32).
	BatchSize int
	// RetryDelay re-arms a claim the sink could not settle.
	RetryDelay time.Duration
}

// DomainSinkSource is the sink's persistence surface.
type DomainSinkSource interface {
	ClaimPendingExcluding(ctx context.Context, batchSize int, owner string, excludedEventTypes []string) ([]*model.OutboxEvent, error)
	MarkDelivered(ctx context.Context, eventID, owner string) error
	MarkRetry(ctx context.Context, eventID, owner string, attempts int, availableAt string) error
}

// Run polls until the context is cancelled.
func (s *DomainEventSink) Run(ctx context.Context, owner string, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.ProcessBatch(ctx, owner); err != nil {
				slog.Error("domain sink: batch failed", "error", err.Error())
			}
		}
	}
}

// ProcessBatch claims and settles up to BatchSize events, returning the
// number delivered.
func (s *DomainEventSink) ProcessBatch(ctx context.Context, owner string) (int, error) {
	batch := s.BatchSize
	if batch < 1 {
		batch = 32
	}
	events, err := s.Source.ClaimPendingExcluding(ctx, batch, owner, s.ExcludedEventTypes)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, event := range events {
		// The durable audit row already carries the event body; delivery
		// here only closes the fan-out ledger.
		if err := s.Source.MarkDelivered(ctx, event.EventID, owner); err != nil {
			retry := s.RetryDelay
			if retry <= 0 {
				retry = time.Minute
			}
			slog.WarnContext(ctx, "domain sink: settle deferred", "event_id", event.EventID, "error", err.Error())
			if retryErr := s.Source.MarkRetry(ctx, event.EventID, owner, event.Attempts,
				time.Now().UTC().Add(retry).Format(time.RFC3339)); retryErr != nil {
				slog.ErrorContext(ctx, "domain sink: retry mark failed", "event_id", event.EventID, "error", retryErr.Error())
			}
			continue
		}
		delivered++
	}
	return delivered, nil
}
