package app

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
)

// W6-5 (S2C-A5/O-1) PG-gated: the domain-event sink settles outbox
// channels without a dedicated subscriber. The W1 shadow observation
// counted 187 asset.*/workgraph.* events spinning because the only
// consumer re-armed foreign claims instantly; after W6 the sink
// delivers them and the webhook channel stays with its own consumer.

func TestDomainSinkSettlesUnownedChannels(t *testing.T) {
	if os.Getenv("MAESTRO_TEST_POSTGRES_DSN") == "" {
		t.Skip("MAESTRO_TEST_POSTGRES_DSN not set; run against the m1 compose postgres to include this test")
	}
	admin, err := store.OpenPostgres(context.Background(), os.Getenv("MAESTRO_TEST_POSTGRES_DSN"))
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_domain_sink_test WITH (FORCE)`)
	require.NoError(t, err)
	_, err = admin.ExecContext(context.Background(), `CREATE DATABASE maestro_domain_sink_test`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS maestro_domain_sink_test WITH (FORCE)`)
		_ = admin.Close()
	})

	dsn := os.Getenv("MAESTRO_TEST_POSTGRES_DSN")
	db, err := store.OpenPostgres(context.Background(),
		dsn[:strings.LastIndex(dsn, "/")+1]+"maestro_domain_sink_test")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = store.ApplyPostgresMigrations(context.Background(), db)
	require.NoError(t, err)
	pg, err := store.NewPostgresStore(db)
	require.NoError(t, err)

	ctx := context.Background()
	outbox := pg.Outbox()
	now := time.Now().UTC().Format(time.RFC3339)
	domain := []string{"asset.registered", "asset.approved", "workgraph.plan.sealed", "work_item.state.changed"}
	for i, eventType := range domain {
		require.NoError(t, outbox.Enqueue(ctx, &model.OutboxEvent{
			EventEnvelope: model.EventEnvelope{
				EventType: eventType, EventVersion: 1, Source: "control-plane",
				ProjectID: "018f7e00-0000-7000-8000-00000000e0" + string(rune('1'+i)),
				Subject:   "subj-" + eventType, PayloadDigest: "sha256:" + strings.Repeat("0", 63) + string(rune('1'+i)),
				OccurredAt: now, Sensitivity: "internal", Payload: []byte(`{}`),
			},
		}))
	}
	// The webhook channel stays with its dedicated consumer.
	require.NoError(t, outbox.Enqueue(ctx, &model.OutboxEvent{
		EventEnvelope: model.EventEnvelope{
			EventType: webhook.EventTypeWebhookReceived, EventVersion: 1, Source: "webhook-inbox",
			PayloadDigest: "sha256:" + strings.Repeat("0", 64), OccurredAt: now, Sensitivity: "internal", Payload: []byte(`{}`),
		},
	}))

	sink := &DomainEventSink{
		Source:             outbox,
		ExcludedEventTypes: []string{webhook.EventTypeWebhookReceived},
		BatchSize:          8,
		RetryDelay:         time.Second,
	}
	delivered, err := sink.ProcessBatch(ctx, "sink-owner")
	require.NoError(t, err)
	assert.Equal(t, len(domain), delivered, "every unowned channel event settles delivered")

	var pending int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox_events WHERE status IN ('pending','retry_wait','sending')`).Scan(&pending))
	assert.Equal(t, 1, pending, "only the webhook envelope waits for its dedicated consumer")

	var webhookDelivered int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM outbox_events WHERE event_type = $1 AND status = 'delivered'`,
		webhook.EventTypeWebhookReceived).Scan(&webhookDelivered))
	assert.Zero(t, webhookDelivered, "the sink never touches the webhook channel")

	// Idempotence: a second batch finds nothing new.
	delivered, err = sink.ProcessBatch(ctx, "sink-owner")
	require.NoError(t, err)
	assert.Zero(t, delivered)
}
