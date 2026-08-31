package webhook

import (
	"encoding/json"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Frozen gitlab.webhook.received envelope constants (events.yaml 3.0.0).
const (
	EventTypeWebhookReceived = "gitlab.webhook.received"
	EventVersionWebhook      = 1
	SourceWebhookIngest      = "maestro.webhook.ingest"
	SensitivityWebhook       = "confidential"
)

// GitLabWebhookPayload is the frozen payload body: it references the
// verified delivery by identity and digest and carries nothing the
// receiver derived from unvalidated payload semantics.
type GitLabWebhookPayload struct {
	InstanceID    string `json:"instance_id"`
	EventKind     string `json:"event_kind"`
	DeliveryKey   string `json:"delivery_key"`
	PayloadDigest string `json:"payload_digest"`
}

// ReceivedEnvelope builds the outbox event for one inbox row. The event
// identity IS the inbox row identity, so a dispatch replay after a crash
// re-emits the same event_id and collapses onto the outbox unique key
// instead of duplicating the notification (at-least-once, applied once).
func ReceivedEnvelope(row *InboxRow, projectID string) (*model.OutboxEvent, error) {
	payload, err := json.Marshal(GitLabWebhookPayload{
		InstanceID:    row.InstanceID,
		EventKind:     row.EventKind,
		DeliveryKey:   row.ExternalEventID,
		PayloadDigest: row.PayloadDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("webhook envelope: %w", err)
	}
	return &model.OutboxEvent{
		EventEnvelope: model.EventEnvelope{
			EventID:       row.ID,
			EventType:     EventTypeWebhookReceived,
			EventVersion:  EventVersionWebhook,
			Source:        SourceWebhookIngest,
			ProjectID:     projectID,
			Subject:       fmt.Sprintf("gitlab/instances/%s/events/%s", row.InstanceID, row.ExternalEventID),
			OccurredAt:    row.ReceivedAt,
			CorrelationID: row.ID,
			CausationID:   "webhook:" + row.ExternalEventID,
			// The envelope digest binds the event to the exact delivery
			// bytes; the inner payload repeats it per the frozen schema.
			PayloadDigest: row.PayloadDigest,
			Sensitivity:   SensitivityWebhook,
			Payload:       payload,
		},
	}, nil
}
