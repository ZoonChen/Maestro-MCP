package store

import (
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// Shared fixtures for the PostgreSQL store integration tests.

type runnerBindingFixture struct{ ProjectID string }

func bindingFixture(b *runnerBindingFixture) *model.RunnerBinding {
	return &model.RunnerBinding{ProjectID: b.ProjectID}
}

func testMembership(teamID, userID, role string) *model.TeamMembership {
	return &model.TeamMembership{
		TeamID:    teamID,
		UserID:    userID,
		Role:      role,
		ValidFrom: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	}
}

func testEnrollment(projectID, createdBy string, expires time.Time) *model.RunnerEnrollment {
	return &model.RunnerEnrollment{
		ID:        pgNewUUID(),
		ProjectID: projectID,
		CodeHash:  "sha256:" + pgNewUUID(),
		ExpiresAt: expires.Format(time.RFC3339),
		CreatedBy: createdBy,
	}
}

func testRunner(name, keyHash string) *model.RunnerDevice {
	return &model.RunnerDevice{
		ID:            pgNewUUID(),
		DisplayName:   name,
		DeviceKeyHash: keyHash,
		Status:        model.RunnerStatusPendingApproval,
		Capabilities:  []byte(`["rootless_oci","no_new_privileges"]`),
	}
}

func testOutboxEvent(name string) *model.OutboxEvent {
	return &model.OutboxEvent{
		EventEnvelope: model.EventEnvelope{
			EventID:       pgNewUUID(),
			EventType:     "work_item.state.changed",
			EventVersion:  1,
			Source:        "control-plane",
			ProjectID:     "018f1f4d-8f50-7b65-b4d1-43f8a49870d2",
			Subject:       name,
			OccurredAt:    time.Now().UTC().Format(time.RFC3339),
			CorrelationID: "corr-" + name,
			CausationID:   "cause-" + name,
			PayloadDigest: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000",
			Sensitivity:   model.EventSensitivityInternal,
			Payload:       []byte(`{"fixture":true}`),
		},
	}
}

func testInboxEvent(name string) *model.InboxEvent {
	return &model.InboxEvent{
		EventEnvelope: model.EventEnvelope{
			EventID:       pgNewUUID(),
			EventType:     "webhook.received",
			EventVersion:  1,
			Source:        "gitlab",
			ProjectID:     "018f1f4d-8f50-7b65-b4d1-43f8a49870d2",
			Subject:       name,
			OccurredAt:    time.Now().UTC().Format(time.RFC3339),
			CorrelationID: "corr-" + name,
			CausationID:   "cause-" + name,
			PayloadDigest: "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111",
			Sensitivity:   model.EventSensitivityInternal,
			Payload:       []byte(`{"fixture":true}`),
		},
	}
}
