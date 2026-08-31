package webhook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeApplyUnit settles claims in memory.
type fakeApplyUnit struct {
	store     *fakeDispatchStore
	committed bool
}

func (a *fakeApplyUnit) ProjectForMapping(_ context.Context, _ string, gitlabProjectID int64) (string, error) {
	if a.store.mappingErr != nil {
		return "", a.store.mappingErr
	}
	project, ok := a.store.mappingProjects[gitlabProjectID]
	if !ok {
		return "", nil
	}
	return project, nil
}

func (a *fakeApplyUnit) EnqueueOutbox(_ context.Context, event *model.OutboxEvent) error {
	if a.store.enqueueErr != nil {
		return a.store.enqueueErr
	}
	for _, seen := range a.store.envelopes {
		if seen.EventID == event.EventID {
			return ErrEnvelopeDuplicate
		}
	}
	a.store.envelopes = append(a.store.envelopes, event)
	return nil
}

func (a *fakeApplyUnit) MarkProcessed(_ context.Context, inboxID, owner string) error {
	return a.store.settle(inboxID, owner, StatusProcessed, "")
}

func (a *fakeApplyUnit) MarkRetry(_ context.Context, inboxID, owner string, availableIn time.Duration) error {
	a.store.lastRetryDelay = availableIn
	return a.store.settle(inboxID, owner, StatusRetryWait, "")
}

func (a *fakeApplyUnit) MarkDeadLetter(_ context.Context, inboxID, owner, reason string) error {
	return a.store.settle(inboxID, owner, StatusDeadLetter, reason)
}

func (a *fakeApplyUnit) Commit() error {
	a.committed = true
	return nil
}

func (a *fakeApplyUnit) Rollback() error { return nil }

type claimedRow struct {
	row    InboxRow
	status string
	reason string
}

type fakeDispatchStore struct {
	rows            map[string]*claimedRow
	order           []string
	next            int
	mappingProjects map[int64]string
	mappingErr      error
	enqueueErr      error
	envelopes       []*model.OutboxEvent
	lastRetryDelay  time.Duration
	denials         []string // dead-letter reasons recorded via settle
}

func (f *fakeDispatchStore) settle(inboxID, owner, status, reason string) error {
	row, ok := f.rows[inboxID]
	if !ok {
		return errors.New("unknown row")
	}
	if row.row.Attempts < 1 {
		return errors.New("row not claimed")
	}
	row.status = status
	row.reason = reason
	if reason != "" {
		f.denials = append(f.denials, reason)
	}
	return nil
}

func (f *fakeDispatchStore) InstanceByID(context.Context, string) (Instance, bool, error) {
	return Instance{}, false, nil
}
func (f *fakeDispatchStore) MappingWebhookUUID(context.Context, string, int64) (string, bool, error) {
	return "", false, nil
}
func (f *fakeDispatchStore) RecordDenial(context.Context, AuditRow) error { return nil }
func (f *fakeDispatchStore) IngestDelivery(context.Context, IngestRecord) (IngestResult, error) {
	return IngestResult{}, nil
}

func (f *fakeDispatchStore) ClaimInbox(_ context.Context, _ string) (*InboxRow, error) {
	for f.next < len(f.order) {
		id := f.order[f.next]
		f.next++
		row := f.rows[id]
		row.row.Attempts++
		return &row.row, nil
	}
	return nil, nil
}

func (f *fakeDispatchStore) BeginApply(context.Context) (ApplyUnit, error) {
	return &fakeApplyUnit{store: f}, nil
}

func (f *fakeDispatchStore) ReplayDeadLetter(_ context.Context, inboxID string) (bool, error) {
	row, ok := f.rows[inboxID]
	if !ok || row.status != StatusDeadLetter {
		return false, nil
	}
	row.status = StatusReceived
	return true, nil
}

func newTestDispatcher(store Store) *Dispatcher {
	cipher, err := NewPayloadCipher("dispatch-test-key")
	if err != nil {
		panic(err)
	}
	return &Dispatcher{
		Store: store, Cipher: cipher,
		MaxAttempts: 3, BaseBackoff: time.Second, MaxBackoff: 30 * time.Second,
		randFloat: func() float64 { return 0 },
	}
}

func claimedBody(t *testing.T, cipher *PayloadCipher, body string) []byte {
	t.Helper()
	sealed, err := cipher.Seal([]byte(body))
	require.NoError(t, err)
	return sealed
}

func TestDispatchAppliesEnvelope(t *testing.T) {
	cipher, _ := NewPayloadCipher("dispatch-test-key")
	store := &fakeDispatchStore{
		rows: map[string]*claimedRow{
			"018f6400-0000-7000-8000-000000000001": {row: InboxRow{
				ID: "018f6400-0000-7000-8000-000000000001", InstanceID: testInstance,
				ExternalEventID: "evt-1", EventKind: "push",
				PayloadDigest: "sha256:aa", RawBodyEncrypted: claimedBody(t, cipher, `{"project_id":42}`),
				ReceivedAt: "2026-08-31T10:00:00Z",
			}, status: StatusReceived},
		},
		order:           []string{"018f6400-0000-7000-8000-000000000001"},
		mappingProjects: map[int64]string{42: "018f6400-0000-7000-8000-0000000000aa"},
	}

	outcome, err := newTestDispatcher(store).DispatchOne(t.Context(), "worker-1")
	require.NoError(t, err)
	assert.Equal(t, DispatchApplied, outcome)
	require.Len(t, store.envelopes, 1)
	assert.Equal(t, "018f6400-0000-7000-8000-000000000001", store.envelopes[0].EventID)
	assert.Equal(t, StatusProcessed, store.rows["018f6400-0000-7000-8000-000000000001"].status)
}

func TestDispatchTerminalDefects(t *testing.T) {
	cipher, _ := NewPayloadCipher("dispatch-test-key")

	t.Run("undecryptable body dead-letters", func(t *testing.T) {
		store := &fakeDispatchStore{
			rows: map[string]*claimedRow{
				"r1": {row: InboxRow{ID: "r1", RawBodyEncrypted: []byte("garbage")}, status: StatusReceived},
			},
			order: []string{"r1"},
		}
		outcome, err := newTestDispatcher(store).DispatchOne(t.Context(), "w")
		require.NoError(t, err)
		assert.Equal(t, DispatchDead, outcome)
		assert.Equal(t, StatusDeadLetter, store.rows["r1"].status)
		assert.Equal(t, ReasonDecryptFailed, store.rows["r1"].reason)
	})

	t.Run("payload without project dead-letters", func(t *testing.T) {
		store := &fakeDispatchStore{
			rows: map[string]*claimedRow{
				"r2": {row: InboxRow{ID: "r2", RawBodyEncrypted: claimedBody(t, cipher, `{"ref":"x"}`)}, status: StatusReceived},
			},
			order: []string{"r2"},
		}
		outcome, err := newTestDispatcher(store).DispatchOne(t.Context(), "w")
		require.NoError(t, err)
		assert.Equal(t, DispatchDead, outcome)
		assert.Equal(t, ReasonPayloadInvalid, store.rows["r2"].reason)
	})

	t.Run("unmapped project dead-letters visibly", func(t *testing.T) {
		store := &fakeDispatchStore{
			rows: map[string]*claimedRow{
				"r3": {row: InboxRow{ID: "r3", RawBodyEncrypted: claimedBody(t, cipher, `{"project_id":99}`)}, status: StatusReceived},
			},
			order:           []string{"r3"},
			mappingProjects: map[int64]string{42: "p"},
		}
		outcome, err := newTestDispatcher(store).DispatchOne(t.Context(), "w")
		require.NoError(t, err)
		assert.Equal(t, DispatchDead, outcome)
		assert.Equal(t, ReasonUnmappedProject, store.rows["r3"].reason)
		assert.Empty(t, store.envelopes, "no envelope for unmapped projects")
	})
}

func TestDispatchRetryAndExhaustion(t *testing.T) {
	cipher, _ := NewPayloadCipher("dispatch-test-key")
	store := &fakeDispatchStore{
		rows: map[string]*claimedRow{
			"r1": {row: InboxRow{ID: "r1", RawBodyEncrypted: claimedBody(t, cipher, `{"project_id":42}`), Attempts: 0}, status: StatusReceived},
		},
		order:           []string{"r1"},
		mappingProjects: map[int64]string{42: "p"},
		enqueueErr:      errors.New("outbox unavailable"),
	}

	// Attempt 1 (attempts becomes 1 < MaxAttempts 3): settled as retry
	// with backoff — a settled outcome reports nil error.
	outcome, err := newTestDispatcher(store).DispatchOne(t.Context(), "w")
	require.NoError(t, err, "a settled retry is not an error")
	assert.Equal(t, DispatchRetry, outcome)
	assert.Equal(t, StatusRetryWait, store.rows["r1"].status)
	assert.Equal(t, time.Second, store.lastRetryDelay)

	// Attempt 3 (attempts becomes 3 >= MaxAttempts 3): dead-letter.
	store.rows["r1"].row.Attempts = 2
	store.rows["r1"].status = StatusRetryWait
	store.next = 0
	outcome, err = newTestDispatcher(store).DispatchOne(t.Context(), "w")
	require.NoError(t, err)
	assert.Equal(t, DispatchDead, outcome)
	assert.Equal(t, StatusDeadLetter, store.rows["r1"].status)
	assert.Equal(t, ReasonRetryExhausted, store.rows["r1"].reason)
}

func TestDispatchBackoffGrowthAndJitter(t *testing.T) {
	d := &Dispatcher{BaseBackoff: time.Second, MaxBackoff: 30 * time.Second, randFloat: func() float64 { return 0 }}
	assert.Equal(t, time.Second, d.backoffFor(1))
	assert.Equal(t, 2*time.Second, d.backoffFor(2))
	assert.Equal(t, 4*time.Second, d.backoffFor(3))
	assert.Equal(t, 30*time.Second, d.backoffFor(30), "backoff is capped")

	d.randFloat = func() float64 { return 1 }
	assert.Equal(t, time.Duration(float64(time.Second)*1.3), d.backoffFor(1), "jitter stays within 30%")
}

func TestDispatchDuplicateEnvelopeIsIdempotent(t *testing.T) {
	cipher, _ := NewPayloadCipher("dispatch-test-key")
	const dupRowID = "018f6400-0000-7000-8000-0000000000id"
	store := &fakeDispatchStore{
		rows: map[string]*claimedRow{
			dupRowID: {row: InboxRow{ID: dupRowID,
				InstanceID: testInstance, ExternalEventID: "evt-1", EventKind: "push",
				PayloadDigest: "sha256:aa", RawBodyEncrypted: claimedBody(t, cipher, `{"project_id":42}`),
				ReceivedAt: "2026-08-31T10:00:00Z"}, status: StatusReceived},
		},
		order:           []string{dupRowID},
		mappingProjects: map[int64]string{42: "p"},
	}
	// Pre-existing envelope with the same identity: the crash-replay path.
	store.envelopes = append(store.envelopes, &model.OutboxEvent{EventEnvelope: model.EventEnvelope{
		EventID: "018f6400-0000-7000-8000-0000000000id"}})

	outcome, err := newTestDispatcher(store).DispatchOne(t.Context(), "w")
	require.NoError(t, err, "a duplicate envelope is not a defect")
	assert.Equal(t, DispatchApplied, outcome)
	assert.Equal(t, StatusProcessed, store.rows[dupRowID].status)
}

func TestDispatchReplayKeepsIdentity(t *testing.T) {
	cipher, _ := NewPayloadCipher("dispatch-test-key")
	store := &fakeDispatchStore{
		rows: map[string]*claimedRow{
			"r1": {row: InboxRow{ID: "r1", RawBodyEncrypted: claimedBody(t, cipher, `{"project_id":42}`)}, status: StatusDeadLetter, reason: ReasonUnmappedProject},
		},
	}
	replayed, err := store.ReplayDeadLetter(t.Context(), "r1")
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, StatusReceived, store.rows["r1"].status)
	assert.Equal(t, "r1", store.rows["r1"].row.ID, "replay keeps the original event identity")

	replayed, err = store.ReplayDeadLetter(t.Context(), "r1")
	require.NoError(t, err)
	assert.False(t, replayed, "only dead-letter rows are replayable")
}
