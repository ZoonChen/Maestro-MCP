package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
)

// OutboxSource is the durable-event claim surface (ADR-002).
type OutboxSource interface {
	ClaimPending(ctx context.Context, batchSize int, owner, now string) ([]*model.OutboxEvent, error)
	MarkDelivered(ctx context.Context, eventID, owner string) error
	MarkRetry(ctx context.Context, eventID, owner string, attempts int, availableAt string) error
}

// RawDeliverySource reads the sealed original body for one inbox
// delivery by its dedup key.
type RawDeliverySource interface {
	InboxEncryptedBody(ctx context.Context, instanceID, deliveryKey string) (sealed []byte, kind string, err error)
}

// Consumer drains gitlab.webhook.received envelopes from the outbox and
// applies their verified originals to the projections. Events of other
// types are requeued untouched: this consumer owns exactly one channel
// of the durable event stream.
type Consumer struct {
	Outbox     OutboxSource
	Deliveries RawDeliverySource
	Syncer     *Syncer
	Cipher     *webhook.PayloadCipher
	BatchSize  int
	// RetryDelay defers events whose dependencies have not landed yet
	// (for example a job event arriving before its pipeline event):
	// out-of-order deliveries converge (GLINT-002 section 8).
	RetryDelay time.Duration
}

// envelopePayload reads the frozen GitLabWebhookPayload fields.
type envelopePayload struct {
	InstanceID  string `json:"instance_id"`
	EventKind   string `json:"event_kind"`
	DeliveryKey string `json:"delivery_key"`
}

// ProcessBatch claims and applies up to BatchSize events, returning the
// number applied. Failures mark the event for retry; a withheld done
// transition (work item outside ready_for_human_merge) is a RECORDED
// correct outcome, not a failure.
func (c *Consumer) ProcessBatch(ctx context.Context, owner string) (int, error) {
	batch := c.BatchSize
	if batch < 1 {
		batch = 16
	}
	events, err := c.Outbox.ClaimPending(ctx, batch, owner, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("gitlab consumer: claim: %w", err)
	}
	applied := 0
	for _, event := range events {
		if event.EventType != webhook.EventTypeWebhookReceived {
			// Not ours: hand it straight back to the stream.
			if err := c.Outbox.MarkRetry(ctx, event.EventID, owner, event.Attempts, time.Now().UTC().Format(time.RFC3339)); err != nil {
				slog.ErrorContext(ctx, "gitlab consumer: requeue foreign event failed", "error", err.Error())
			}
			continue
		}
		if err := c.apply(ctx, event); err != nil {
			retryDelay := c.RetryDelay
			if retryDelay <= 0 {
				retryDelay = time.Minute
			}
			slog.WarnContext(ctx, "gitlab consumer: event deferred", "event_id", event.EventID, "error", err.Error())
			if err := c.Outbox.MarkRetry(ctx, event.EventID, owner, event.Attempts+1,
				time.Now().UTC().Add(retryDelay).Format(time.RFC3339)); err != nil {
				slog.ErrorContext(ctx, "gitlab consumer: retry mark failed", "error", err.Error())
			}
			continue
		}
		if err := c.Outbox.MarkDelivered(ctx, event.EventID, owner); err != nil {
			slog.ErrorContext(ctx, "gitlab consumer: delivered mark failed", "error", err.Error())
		}
		applied++
	}
	return applied, nil
}

func (c *Consumer) apply(ctx context.Context, event *model.OutboxEvent) error {
	var payload envelopePayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("envelope payload: %w", err)
	}
	if payload.InstanceID == "" || payload.DeliveryKey == "" || payload.EventKind == "" {
		return errors.New("envelope payload lacks instance/kind/delivery key")
	}
	sealed, kind, err := c.Deliveries.InboxEncryptedBody(ctx, payload.InstanceID, payload.DeliveryKey)
	if err != nil {
		return fmt.Errorf("inbox lookup: %w", err)
	}
	if len(sealed) == 0 {
		return errors.New("inbox delivery has no sealed body (retention or DLQ cleanup)")
	}
	plain, err := c.Cipher.Open(sealed)
	if err != nil {
		return fmt.Errorf("inbox decrypt: %w", err)
	}
	outcome, err := c.Syncer.ApplyBody(ctx, payload.InstanceID, kind, plain)
	if err != nil {
		return err
	}
	switch {
	case outcome.Transitioned:
		slog.InfoContext(ctx, "gitlab sync: merged fact drove work item to done",
			"event_id", event.EventID, "kind", outcome.Kind)
	case outcome.Withheld:
		slog.WarnContext(ctx, "gitlab sync: merged fact withheld (work item outside ready_for_human_merge)",
			"event_id", event.EventID)
	}
	return nil
}

// Run polls until the context is cancelled.
func (c *Consumer) Run(ctx context.Context, owner string, interval time.Duration) {
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
			if _, err := c.ProcessBatch(ctx, owner); err != nil {
				slog.Error("gitlab consumer: batch failed", "error", err.Error())
			}
		}
	}
}
