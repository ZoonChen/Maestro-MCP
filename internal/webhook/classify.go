package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// headerKinds maps X-Gitlab-Event values onto the contracted kinds. Tag
// hooks and every other GitLab event type are deliberately absent: the
// hook registration only asks for the four contracted kinds, and anything
// else arriving is archived without business effect (secrets-webhooks
// section 7: 未知事件只归档不执行业务).
var headerKinds = map[string]Kind{
	"Push Hook":          KindPush,
	"Merge Request Hook": KindMergeRequest,
	"Pipeline Hook":      KindPipeline,
	"Job Hook":           KindJob,
}

// Classify maps the X-Gitlab-Event header to its contracted kind. The
// second return is false for uncontracted event types.
func Classify(eventHeader string) (Kind, bool) {
	kind, ok := headerKinds[eventHeader]
	return kind, ok
}

// payloadProbe reads just the project identity from any of the four
// contracted payload shapes: push and job hooks carry a top-level
// project_id, merge-request and pipeline hooks a project object.
type payloadProbe struct {
	ProjectID int64 `json:"project_id"`
	Project   struct {
		ID int64 `json:"id"`
	} `json:"project"`
}

// ProjectOf extracts the numeric GitLab project identity from a verified
// raw body. False means the payload does not resolve a project: quarantined,
// never auto-mapped.
func ProjectOf(body []byte) (int64, bool) {
	var probe payloadProbe
	if err := json.Unmarshal(body, &probe); err != nil {
		return 0, false
	}
	if probe.ProjectID >= 1 {
		return probe.ProjectID, true
	}
	if probe.Project.ID >= 1 {
		return probe.Project.ID, true
	}
	return 0, false
}

// DeliveryKey derives the inbox dedup key (WEBHOOK-RULE-002). A present
// X-Gitlab-Event-UUID is the key; without one the compatibility key
// composes instance, project, event type and payload digest, and the
// caller must surface the compatibility-mode alert.
func DeliveryKey(instanceID string, gitlabProjectID int64, eventHeader, payloadDigest, eventUUID string) (key string, compatibilityMode bool) {
	if eventUUID != "" {
		return eventUUID, false
	}
	return fmt.Sprintf("compat:%s:%d:%s:%s", instanceID, gitlabProjectID, clampText(eventHeader), payloadDigest), true
}

// BodyDigest computes the canonical sha256:<hex64> digest of the raw bytes.
func BodyDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}
