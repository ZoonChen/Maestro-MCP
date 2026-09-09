package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// GET /api/v1/metrics platform section (G2): the sampled gauges
// project additively; unknown lag percentiles stay null instead of a
// misleading zero.
func TestPlatformDepthsPayload(t *testing.T) {
	lag := 41.5
	payload := platformDepthsPayload(store.PlatformDepths{
		InboxBacklog: 3, DLQDepth: 2, OutboxPending: 7,
		InboxLagP95Seconds: &lag,
	})
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"webhook_inbox_depth": 3,
		"webhook_dlq_depth": 2,
		"webhook_outbox_pending": 7,
		"inbox_lag_p95_seconds": 41.5,
		"outbox_lag_p95_seconds": null
	}`, string(encoded))
}
