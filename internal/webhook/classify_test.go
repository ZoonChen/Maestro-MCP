package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		header     string
		kind       Kind
		contracted bool
	}{
		{"Push Hook", KindPush, true},
		{"Merge Request Hook", KindMergeRequest, true},
		{"Pipeline Hook", KindPipeline, true},
		{"Job Hook", KindJob, true},
		// Every other GitLab event type is outside the contract and must
		// archive without business effect.
		{"Tag Push Hook", "", false},
		{"Note Hook", "", false},
		{"Issue Hook", "", false},
		{"Wiki Page Hook", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		kind, contracted := Classify(tc.header)
		assert.Equal(t, tc.contracted, contracted, "header %q", tc.header)
		assert.Equal(t, tc.kind, kind, "header %q", tc.header)
	}
}

func TestProjectOf(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		project int64
		known   bool
	}{
		{"top-level project_id (push/job shape)", `{"project_id": 42, "ref": "refs/heads/main"}`, 42, true},
		{"project object (MR/pipeline shape)", `{"project": {"id": 77, "path": "g/legacy"}, "object_kind": "merge_request"}`, 77, true},
		{"object id wins over zero top-level", `{"project_id": 0, "project": {"id": 9}}`, 9, true},
		{"missing project entirely", `{"object_kind": "push", "ref": "refs/heads/x"}`, 0, false},
		{"non-numeric project", `{"project_id": "abc"}`, 0, false},
		{"invalid JSON", `{"project_id": `, 0, false},
		{"empty body", ``, 0, false},
		{"zero ids only", `{"project_id": 0, "project": {"id": 0}}`, 0, false},
	}
	for _, tc := range cases {
		project, known := ProjectOf([]byte(tc.body))
		assert.Equal(t, tc.known, known, tc.name)
		assert.Equal(t, tc.project, project, tc.name)
	}
}

func TestDeliveryKey(t *testing.T) {
	key, compat := DeliveryKey("018f6000-0000-7000-8000-00000000000a", 7,
		"Push Hook", "sha256:bb", "evt-uuid-1")
	assert.False(t, compat, "a present event UUID is the key")
	assert.Equal(t, "evt-uuid-1", key)

	key, compat = DeliveryKey("018f6000-0000-7000-8000-00000000000a", 7,
		"Push Hook", "sha256:bb", "")
	assert.True(t, compat, "a missing UUID raises compatibility mode")
	assert.Equal(t,
		"compat:018f6000-0000-7000-8000-00000000000a:7:Push Hook:sha256:bb", key)

	// Identical payloads with the same header collapse; a different event
	// type or digest does not.
	other, _ := DeliveryKey("018f6000-0000-7000-8000-00000000000a", 7,
		"Job Hook", "sha256:bb", "")
	assert.NotEqual(t, key, other)
	otherDigest, _ := DeliveryKey("018f6000-0000-7000-8000-00000000000a", 7,
		"Push Hook", "sha256:cc", "")
	assert.NotEqual(t, key, otherDigest)
}

func TestBodyDigest(t *testing.T) {
	digest := BodyDigest([]byte("hello"))
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, digest)
	assert.Equal(t, "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", digest)
}
