package handler

import (
	"errors"
	"io"
	"net/http"

	"github.com/ZoonChen/Maestro-MCP/internal/webhook"
	"github.com/gin-gonic/gin"
)

// Control Plane side of the frozen control-plane.yaml ingestGitLabWebhook
// operation (M2-WHK-001). The route authenticates with the instance's
// GitLab shared token — NOT a bearer principal — so it mounts outside the
// /api/v1 authorization tree and self-gates in the ingestor, exactly like
// the v3 Runner group carries its own device-token scheme. It inherits
// the engine-level body limit (413), rate limit, CORS, drain and
// remote-write guards.

// GitLabWebhookOptions carries the webhook receiver wiring.
type GitLabWebhookOptions struct {
	Ingestor *webhook.Ingestor
}

// RegisterGitLabWebhookIngest mounts the frozen receiver path under the
// control-plane /api/v3 base (contract path alignment).
func RegisterGitLabWebhookIngest(r *gin.Engine, options GitLabWebhookOptions) {
	ingestor := options.Ingestor
	if ingestor == nil {
		return
	}
	r.POST("/api/v3/webhooks/gitlab/:instance_id", func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				staticErrorReply(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Webhook body exceeds the size limit")
				return
			}
			staticErrorReply(c, http.StatusBadRequest, "BODY_UNREADABLE", "Webhook body could not be read")
			return
		}
		result := ingestor.Receive(c.Request.Context(), webhook.ReceiveRequest{
			InstanceID:  c.Param("instance_id"),
			ContentType: c.GetHeader("Content-Type"),
			EventHeader: c.GetHeader("X-Gitlab-Event"),
			EventUUID:   c.GetHeader("X-Gitlab-Event-UUID"),
			WebhookUUID: c.GetHeader("X-Gitlab-Webhook-UUID"),
			Token:       c.GetHeader("X-Gitlab-Token"),
			RawBody:     raw,
		})
		if result.Status == http.StatusAccepted {
			c.JSON(result.Status, gin.H{"code": result.Code})
			return
		}
		staticErrorReply(c, result.Status, result.Code, webhookReplyText(result.Code))
	})
}

func webhookReplyText(code string) string {
	switch code {
	case "INSTANCE_UNKNOWN":
		return "Webhook target is unknown"
	case "INSTANCE_SUSPENDED":
		return "Webhook target is suspended; retry later"
	case "SECRET_UNRESOLVED":
		return "Webhook secret is not resolvable; retry later"
	case "WEBHOOK_TOKEN_INVALID":
		return "Webhook token verification failed"
	case "WEBHOOK_IDENTITY_MISMATCH":
		return "Webhook identity does not match the registered hook"
	case "CONTENT_TYPE_UNSUPPORTED":
		return "Content-Type must be application/json"
	case "EVENT_HEADER_MISSING":
		return "X-Gitlab-Event header is required"
	default:
		return "Webhook delivery was rejected"
	}
}
