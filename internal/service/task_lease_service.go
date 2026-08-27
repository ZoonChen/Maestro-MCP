package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
	"github.com/google/uuid"
)

const taskLeaseDuration = 60 * time.Second

var taskHeartbeatIdempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$`)

// HeartbeatTask renews the exact active execution Lease. Session and Worker
// identifiers are compatibility inputs in M0; they are still resolved against
// the durable assignment and never establish authority by themselves. M1
// supplies those values from the authenticated Runner context.
//
// A retry with the same idempotency key and request returns the original Lease
// result. A stale lease version, expired deadline, changed epoch, or changed
// owner fails closed without extending any deadline.
func (s *TaskService) HeartbeatTask(
	ctx context.Context,
	projectID, taskID, sessionID, workerID, leaseID string,
	leaseVersion int64,
	idempotencyKey string,
) (_ *model.TaskLease, retErr error) {
	projectID = strings.TrimSpace(projectID)
	taskID = strings.TrimSpace(taskID)
	sessionID = strings.TrimSpace(sessionID)
	workerID = strings.TrimSpace(workerID)
	leaseID = strings.TrimSpace(leaseID)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if projectID == "" || taskID == "" || sessionID == "" || workerID == "" {
		return nil, fmt.Errorf("HeartbeatTask: %w: project, task, session, and worker are required", store.ErrInvalidParameter)
	}
	if _, err := uuid.Parse(leaseID); err != nil {
		return nil, fmt.Errorf("HeartbeatTask: %w: lease_id must be a UUID", store.ErrInvalidParameter)
	}
	if leaseVersion < 1 {
		return nil, fmt.Errorf("HeartbeatTask: %w: lease_version must be positive", store.ErrInvalidParameter)
	}
	if !taskHeartbeatIdempotencyKeyPattern.MatchString(idempotencyKey) {
		return nil, fmt.Errorf("HeartbeatTask: %w: invalid idempotency key", store.ErrInvalidParameter)
	}
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("HeartbeatTask: %w: database unavailable", store.ErrRecoveryIntegrity)
	}

	// Denied heartbeat decisions are useful security evidence. The best-effort
	// append happens after the transaction rollback and never hides the original
	// stable error (database outages may make even that append impossible).
	defer func() {
		if retErr != nil {
			s.recordTaskHeartbeatDenied(ctx, projectID, taskID, sessionID, leaseID, retErr)
		}
	}()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("HeartbeatTask: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var physicalSessionID, sessionStatus string
	var sessionVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT id, status, version FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		projectID, sessionID,
	).Scan(&physicalSessionID, &sessionStatus, &sessionVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrSessionNotFound
		}
		return nil, fmt.Errorf("HeartbeatTask: session lookup: %w", err)
	}
	if sessionStatus != model.SessionStatusOnline {
		return nil, fmt.Errorf("HeartbeatTask: session is not online: %w", store.ErrTaskNotOwned)
	}

	requestHashBytes := sha256.Sum256([]byte(strings.Join([]string{
		taskID, sessionID, workerID, leaseID, fmt.Sprint(leaseVersion),
	}, "\x00")))
	requestHash := hex.EncodeToString(requestHashBytes[:])
	idempotencyScope := sessionID + "/" + workerID + "/" + leaseID
	var priorHash, priorResult string
	err = tx.QueryRowContext(ctx, `SELECT request_hash, result_ref FROM idempotency_records
		WHERE project_id = ? AND scope = ? AND operation = 'task.heartbeat' AND key = ?`,
		projectID, idempotencyScope, idempotencyKey,
	).Scan(&priorHash, &priorResult)
	if err == nil {
		if priorHash != requestHash {
			return nil, store.ErrIdempotencyConflict
		}
		var original model.TaskLease
		if err := json.Unmarshal([]byte(priorResult), &original); err != nil {
			return nil, fmt.Errorf("HeartbeatTask: corrupt idempotency result: %w", errors.Join(store.ErrRecoveryIntegrity, err))
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("HeartbeatTask: commit idempotency hit: %w", err)
		}
		return &original, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("HeartbeatTask: idempotency lookup: %w", err)
	}

	var (
		taskStatus, assignedSessionID, assignedWorkerID string
		taskVersion, taskLeaseEpoch                     int64
		lease                                           model.TaskLease
		leaseIsLive                                     int
	)
	err = tx.QueryRowContext(ctx, `SELECT
		t.status, t.version, COALESCE(t.assigned_session_id, ''),
		COALESCE(t.assigned_worker_id, ''), t.lease_epoch,
		l.id, l.project_id, l.task_id, l.session_id, l.worker_id,
		l.epoch, l.status, l.version, l.expires_at, l.created_at, l.updated_at,
		CASE WHEN julianday(l.expires_at) > julianday('now') THEN 1 ELSE 0 END
		FROM tasks AS t
		JOIN task_leases AS l
		  ON l.project_id = t.project_id AND l.task_id = t.id AND l.id = ?
		WHERE t.project_id = ? AND t.id = ?`,
		leaseID, projectID, taskID,
	).Scan(
		&taskStatus, &taskVersion, &assignedSessionID, &assignedWorkerID, &taskLeaseEpoch,
		&lease.ID, &lease.ProjectID, &lease.TaskID, &lease.SessionID, &lease.WorkerID,
		&lease.Epoch, &lease.Status, &lease.Version, &lease.ExpiresAt, &lease.CreatedAt, &lease.UpdatedAt,
		&leaseIsLive,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			var exists int
			if lookupErr := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM tasks WHERE project_id = ? AND id = ?`, projectID, taskID,
			).Scan(&exists); lookupErr != nil {
				return nil, fmt.Errorf("HeartbeatTask: task existence lookup: %w", lookupErr)
			}
			if exists == 0 {
				return nil, store.ErrTaskNotFound
			}
			return nil, store.ErrLeaseNotFound
		}
		return nil, fmt.Errorf("HeartbeatTask: authority lookup: %w", err)
	}
	if taskStatus != model.TaskStatusExecuting {
		return nil, fmt.Errorf("HeartbeatTask: task is %s: %w", taskStatus, store.ErrTaskStateInvalid)
	}
	if assignedSessionID != physicalSessionID || assignedWorkerID != workerID ||
		lease.SessionID != physicalSessionID || lease.WorkerID != workerID {
		return nil, store.ErrTaskNotOwned
	}
	if lease.Status != model.LeaseStatusActive {
		return nil, store.ErrLeaseNotFound
	}
	if lease.Epoch != taskLeaseEpoch {
		return nil, fmt.Errorf("HeartbeatTask: lease epoch changed: %w", store.ErrLeaseVersionMismatch)
	}
	if lease.Version != leaseVersion {
		return nil, store.ErrLeaseVersionMismatch
	}
	if leaseIsLive != 1 {
		return nil, store.ErrLeaseExpired
	}

	var workerStatus, workerCurrentTask string
	var workerVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT status, COALESCE(current_task_id, ''), version
		FROM agent_workers WHERE project_id = ? AND session_id = ? AND id = ?`,
		projectID, physicalSessionID, workerID,
	).Scan(&workerStatus, &workerCurrentTask, &workerVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrWorkerNotFound
		}
		return nil, fmt.Errorf("HeartbeatTask: worker lookup: %w", err)
	}
	if workerStatus != model.WorkerStatusBusy || workerCurrentTask != taskID {
		return nil, fmt.Errorf("HeartbeatTask: worker authority changed: %w", store.ErrTaskNotOwned)
	}

	expiresAt := time.Now().UTC().Add(taskLeaseDuration).Format("2006-01-02 15:04:05")
	updated, err := tx.ExecContext(ctx, `UPDATE task_leases
		SET expires_at = ?, version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND task_id = ? AND id = ? AND epoch = ?
		  AND status = 'active' AND version = ?
		  AND julianday(expires_at) > julianday('now')`,
		expiresAt, projectID, taskID, leaseID, taskLeaseEpoch, leaseVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("HeartbeatTask: renew lease: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return nil, fmt.Errorf("HeartbeatTask: lease renewal CAS: %w", errors.Join(store.ErrLeaseVersionMismatch, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "lease", leaseID,
		model.LeaseStatusActive, model.LeaseStatusActive, leaseVersion, leaseVersion+1,
		sessionID, "execution lease heartbeat renewed", leaseID); err != nil {
		return nil, err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE tasks SET lease_expires_at = ?,
		version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND id = ? AND status = 'executing' AND version = ?
		  AND assigned_session_id = ? AND assigned_worker_id = ?
		  AND active_lease_id = ? AND lease_epoch = ?`,
		expiresAt, projectID, taskID, taskVersion, physicalSessionID, workerID, leaseID, taskLeaseEpoch,
	)
	if err != nil {
		return nil, fmt.Errorf("HeartbeatTask: bind task deadline: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return nil, fmt.Errorf("HeartbeatTask: task authority CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID,
		model.TaskStatusExecuting, model.TaskStatusExecuting, taskVersion, taskVersion+1,
		sessionID, "execution lease heartbeat renewed", leaseID); err != nil {
		return nil, err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE agent_workers
		SET version = version + 1, last_active = datetime('now')
		WHERE project_id = ? AND session_id = ? AND id = ? AND status = 'busy'
		  AND current_task_id = ? AND version = ?`,
		projectID, physicalSessionID, workerID, taskID, workerVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("HeartbeatTask: worker CAS: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return nil, fmt.Errorf("HeartbeatTask: worker changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "worker", physicalSessionID+"/"+workerID,
		model.WorkerStatusBusy, model.WorkerStatusBusy, workerVersion, workerVersion+1,
		sessionID, "execution worker heartbeat renewed", leaseID); err != nil {
		return nil, err
	}
	updated, err = tx.ExecContext(ctx, `UPDATE agent_sessions
		SET last_heartbeat = datetime('now'), version = version + 1
		WHERE project_id = ? AND id = ? AND status = 'online' AND version = ?`,
		projectID, physicalSessionID, sessionVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("HeartbeatTask: session CAS: %w", err)
	}
	if rows, err := updated.RowsAffected(); err != nil || rows != 1 {
		return nil, fmt.Errorf("HeartbeatTask: session changed: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := appendStateHistory(ctx, tx, projectID, "session", physicalSessionID,
		model.SessionStatusOnline, model.SessionStatusOnline, sessionVersion, sessionVersion+1,
		sessionID, "execution session heartbeat renewed", leaseID); err != nil {
		return nil, err
	}

	lease.Version++
	lease.ExpiresAt = expiresAt
	lease.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	lease.SessionID = sessionID
	encodedResult, err := json.Marshal(&lease)
	if err != nil {
		return nil, fmt.Errorf("HeartbeatTask: encode result: %w", err)
	}
	detailBytes, err := json.Marshal(map[string]any{
		"lease_id": leaseID, "lease_version": lease.Version, "lease_epoch": lease.Epoch,
		"expires_at": expiresAt, "worker_id": workerID,
	})
	if err != nil {
		return nil, fmt.Errorf("HeartbeatTask: encode audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO activity_log
		(project_id, session_id, task_id, action, detail, created_at)
		VALUES (?, ?, ?, 'task_heartbeat', ?, datetime('now'))`,
		projectID, sessionID, taskID, string(detailBytes),
	); err != nil {
		return nil, fmt.Errorf("HeartbeatTask: activity: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log
		(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, ?, 'task.heartbeat', 'ALLOWED', ?, datetime('now'))`,
		sessionID, projectID, projectID, taskID, string(detailBytes),
	); err != nil {
		return nil, fmt.Errorf("HeartbeatTask: audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_records
		(project_id, scope, operation, key, request_hash, result_ref, expires_at)
		VALUES (?, ?, 'task.heartbeat', ?, ?, ?, datetime('now', '+1 day'))`,
		projectID, idempotencyScope, idempotencyKey, requestHash, string(encodedResult),
	); err != nil {
		return nil, fmt.Errorf("HeartbeatTask: idempotency append: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("HeartbeatTask: commit: %w", err)
	}
	safeEmit(s.eventEmitter, "task.heartbeat", projectID, map[string]any{
		"task_id": taskID, "lease_id": leaseID, "lease_version": lease.Version,
	})
	return &lease, nil
}

func (s *TaskService) recordTaskHeartbeatDenied(
	ctx context.Context, projectID, taskID, sessionID, leaseID string, heartbeatErr error,
) {
	if s == nil || s.db == nil {
		return
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 500*time.Millisecond)
	defer cancel()
	leaseHash := sha256.Sum256([]byte(leaseID))
	detail, err := json.Marshal(map[string]string{
		"error_code": heartbeatErrorCode(heartbeatErr),
		"lease_hash": "sha256:" + hex.EncodeToString(leaseHash[:]),
	})
	if err != nil {
		return
	}
	_, _ = s.db.ExecContext(auditCtx, `INSERT INTO audit_log
		(session_id, bound_project, target_project, target_task, action, result, detail, created_at)
		VALUES (?, ?, ?, ?, 'task.heartbeat', 'DENIED', ?, datetime('now'))`,
		sessionID, projectID, projectID, taskID, string(detail),
	)
}

func heartbeatErrorCode(err error) string {
	switch {
	case errors.Is(err, store.ErrLeaseExpired):
		return "LEASE_EXPIRED"
	case errors.Is(err, store.ErrLeaseVersionMismatch):
		return "LEASE_VERSION_MISMATCH"
	case errors.Is(err, store.ErrLeaseNotFound):
		return "LEASE_NOT_FOUND"
	case errors.Is(err, store.ErrTaskNotOwned):
		return "TASK_NOT_OWNED"
	case errors.Is(err, store.ErrIdempotencyConflict):
		return "IDEMPOTENCY_CONFLICT"
	case errors.Is(err, store.ErrInvalidParameter):
		return "INVALID_PARAMETER"
	default:
		return "INTERNAL_ERROR"
	}
}

// GetNextTask is the compatibility entrypoint. It derives the current queue
// version server-side; v3 transports call GetNextTaskWithVersion with their
// explicit CAS token and Idempotency-Key.
func (s *TaskService) GetNextTask(ctx context.Context, projectID, sessionID, role, workerID string) (*model.Task, error) {
	var queueVersion int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT version FROM project_queue_versions WHERE project_id = ?), 0)`,
		projectID,
	).Scan(&queueVersion); err != nil {
		return nil, fmt.Errorf("GetNextTask: read queue version: %w", err)
	}
	return s.claimNextTask(ctx, projectID, sessionID, role, workerID, "", queueVersion, "", "", nil)
}

// GetNextTaskForContext returns whether this call created the claim. The MCP
// context-delivery path uses that fact to decide whether an undelivered fresh
// Worktree can be discarded or an older execution must be quarantined.
func (s *TaskService) GetNextTaskForContext(
	ctx context.Context,
	projectID, sessionID, role, workerID string,
) (*model.Task, bool, error) {
	var queueVersion int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT version FROM project_queue_versions WHERE project_id = ?), 0)`,
		projectID,
	).Scan(&queueVersion); err != nil {
		return nil, false, fmt.Errorf("GetNextTask: read queue version: %w", err)
	}
	var claimCreated bool
	task, err := s.claimNextTask(
		ctx, projectID, sessionID, role, workerID, "", queueVersion, "", "", &claimCreated,
	)
	return task, claimCreated, err
}

// GetNextTaskWithVersion provides the M0 CAS/idempotency contract used by the
// canonical MCP command shape.
func (s *TaskService) GetNextTaskWithVersion(
	ctx context.Context,
	projectID, sessionID, role, workerID, idempotencyKey string,
	expectedQueueVersion int64,
) (*model.Task, error) {
	if len(idempotencyKey) < 16 || len(idempotencyKey) > 128 {
		return nil, fmt.Errorf("GetNextTask: %w: idempotency key length", store.ErrInvalidParameter)
	}
	requestHash := sha256.Sum256([]byte(sessionID + "\x00" + role + "\x00" + workerID + "\x00" + fmt.Sprint(expectedQueueVersion)))
	return s.claimNextTask(ctx, projectID, sessionID, role, workerID, idempotencyKey, expectedQueueVersion, hex.EncodeToString(requestHash[:]), "", nil)
}

func (s *TaskService) claimNextTask(
	ctx context.Context,
	projectID, sessionID, role, workerID, idempotencyKey string,
	expectedQueueVersion int64,
	requestHash, preferredTaskID string,
	claimCreated *bool,
) (*model.Task, error) {
	if claimCreated != nil {
		*claimCreated = false
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("GetNextTask: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO project_queue_versions(project_id, version) VALUES (?, 0)`,
		projectID,
	); err != nil {
		return nil, fmt.Errorf("GetNextTask: initialize queue version: %w", err)
	}

	if idempotencyKey != "" {
		var priorHash, taskID string
		err := tx.QueryRowContext(ctx,
			`SELECT request_hash, result_ref FROM idempotency_records
			 WHERE project_id = ? AND scope = ? AND operation = 'task.claim' AND key = ?`,
			projectID, sessionID+"/"+workerID, idempotencyKey,
		).Scan(&priorHash, &taskID)
		if err == nil {
			if priorHash != requestHash {
				return nil, store.ErrIdempotencyConflict
			}
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("GetNextTask: commit idempotency hit: %w", err)
			}
			return s.taskStore.GetByID(ctx, projectID, taskID)
		}
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("GetNextTask: idempotency lookup: %w", err)
		}
	}

	var queueVersion int64
	if err := tx.QueryRowContext(ctx,
		`SELECT version FROM project_queue_versions WHERE project_id = ?`, projectID,
	).Scan(&queueVersion); err != nil {
		return nil, fmt.Errorf("GetNextTask: queue version: %w", err)
	}
	if queueVersion != expectedQueueVersion {
		return nil, fmt.Errorf("GetNextTask: queue version %d != %d: %w", queueVersion, expectedQueueVersion, store.ErrConcurrentConflict)
	}

	var sessionKey, sessionRole, sessionStatus string
	var capacity int
	if err := tx.QueryRowContext(ctx,
		`SELECT id, role, status, capacity FROM agent_sessions
		 WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		projectID, sessionID,
	).Scan(&sessionKey, &sessionRole, &sessionStatus, &capacity); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrSessionNotFound
		}
		return nil, fmt.Errorf("GetNextTask: session lookup: %w", err)
	}
	if sessionStatus != model.SessionStatusOnline || sessionRole != role {
		return nil, fmt.Errorf("GetNextTask: session role/status mismatch: %w", store.ErrTaskNotOwned)
	}

	var currentTask sql.NullString
	var workerStatus string
	var workerVersion int64
	err = tx.QueryRowContext(ctx,
		`SELECT current_task_id, status, version FROM agent_workers
		 WHERE project_id = ? AND session_id = ? AND id = ?`,
		projectID, sessionKey, workerID,
	).Scan(&currentTask, &workerStatus, &workerVersion)
	if err == sql.ErrNoRows {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM agent_workers WHERE project_id = ? AND session_id = ?`,
			projectID, sessionKey,
		).Scan(&count); err != nil {
			return nil, fmt.Errorf("GetNextTask: worker capacity count: %w", err)
		}
		if capacity <= 0 {
			capacity = 1
		}
		if capacity > 5 {
			capacity = 5
		}
		if count >= capacity {
			return nil, store.ErrSessionCapacityFull
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO agent_workers
			 (id, session_id, project_id, current_task_id, status, tasks_completed, version, last_active)
			 VALUES (?, ?, ?, NULL, 'idle', 0, 0, datetime('now'))`,
			workerID, sessionKey, projectID,
		); err != nil {
			return nil, fmt.Errorf("GetNextTask: create worker: %w", err)
		}
		workerStatus = model.WorkerStatusIdle
		workerVersion = 0
	} else if err != nil {
		return nil, fmt.Errorf("GetNextTask: worker lookup: %w", err)
	} else if currentTask.Valid {
		// A worker has at most one active Execution. Returning the existing task is
		// deterministic and prevents claim_batch from creating hidden double work.
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("GetNextTask: commit existing assignment: %w", err)
		}
		return s.taskStore.GetByID(ctx, projectID, currentTask.String)
	}
	if workerStatus != model.WorkerStatusIdle {
		return nil, fmt.Errorf("GetNextTask: worker is %s: %w", workerStatus, store.ErrConcurrentConflict)
	}

	var taskID string
	var taskVersion, priorEpoch int64
	err = tx.QueryRowContext(ctx, `
		SELECT t.id, t.version, t.lease_epoch FROM tasks AS t
		WHERE t.project_id = ? AND t.role = ? AND t.status = 'queued'
		  AND (? = '' OR t.id = ?)
		  AND NOT EXISTS (
		      SELECT 1 FROM json_each(t.dependencies) AS dep
		      LEFT JOIN tasks AS dep_task
		        ON dep_task.project_id = t.project_id
		       AND dep_task.id = json_extract(dep.value, '$.task_id')
		      WHERE dep_task.id IS NULL
		         OR (COALESCE(json_extract(dep.value, '$.require_state'), 'done') = 'validating'
		             AND dep_task.status NOT IN ('validating','ready_for_human_merge','done','cancelled'))
		         OR (COALESCE(json_extract(dep.value, '$.require_state'), 'done') <> 'validating'
		             AND dep_task.status NOT IN ('done','cancelled'))
		  )
		ORDER BY CASE t.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
		         t.created_at ASC
		LIMIT 1`, projectID, role, preferredTaskID, preferredTaskID).Scan(&taskID, &taskVersion, &priorEpoch)
	if err == sql.ErrNoRows {
		return nil, store.ErrNoAvailableTask
	}
	if err != nil {
		return nil, fmt.Errorf("GetNextTask: select queued task: %w", err)
	}

	leaseID := uuid.NewString()
	leaseEpoch := priorEpoch + 1
	expiresAt := time.Now().UTC().Add(taskLeaseDuration).Format("2006-01-02 15:04:05")

	result, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'leased', version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = 'queued' AND version = ?`,
		taskID, projectID, taskVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("GetNextTask: queued to leased: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID, model.TaskStatusQueued, model.TaskStatusLeased, taskVersion, taskVersion+1, sessionID, "lease offered", leaseID); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO task_leases
		 (id, project_id, task_id, session_id, worker_id, epoch, status, version, expires_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'active', 1, ?, datetime('now'), datetime('now'))`,
		leaseID, projectID, taskID, sessionKey, workerID, leaseEpoch, expiresAt,
	); err != nil {
		return nil, fmt.Errorf("GetNextTask: create lease: %w", err)
	}

	result, err = tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'executing', assigned_session_id = ?, assigned_worker_id = ?,
		     assigned_at = datetime('now'), lease_epoch = ?, active_lease_id = ?, lease_expires_at = ?,
		     version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = 'leased' AND version = ?`,
		sessionKey, workerID, leaseEpoch, leaseID, expiresAt,
		taskID, projectID, taskVersion+1,
	)
	if err != nil {
		return nil, fmt.Errorf("GetNextTask: leased to executing: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, projectID, "worker", sessionKey+"/"+workerID,
		model.WorkerStatusIdle, model.WorkerStatusBusy, workerVersion, workerVersion+1,
		sessionID, "execution lease accepted", leaseID); err != nil {
		return nil, err
	}
	if err := appendStateHistory(ctx, tx, projectID, "task", taskID, model.TaskStatusLeased, model.TaskStatusExecuting, taskVersion+1, taskVersion+2, sessionID, "lease accepted", leaseID); err != nil {
		return nil, err
	}

	result, err = tx.ExecContext(ctx,
		`UPDATE agent_workers SET current_task_id = ?, status = 'busy', version = version + 1,
		     last_active = datetime('now')
		 WHERE project_id = ? AND session_id = ? AND id = ? AND status = 'idle'
		   AND current_task_id IS NULL AND version = ?`,
		taskID, projectID, sessionKey, workerID, workerVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("GetNextTask: reserve worker: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, store.ErrConcurrentConflict
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO activity_log(project_id, session_id, task_id, action, detail, created_at)
		 VALUES (?, ?, ?, ?, ?, datetime('now'))`,
		projectID, sessionID, taskID, model.ActionClaimed,
		fmt.Sprintf(`{"worker_id":%q,"role":%q,"lease_id":%q,"epoch":%d}`, workerID, role, leaseID, leaseEpoch),
	); err != nil {
		return nil, fmt.Errorf("GetNextTask: activity: %w", err)
	}
	if idempotencyKey != "" {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO idempotency_records(project_id, scope, operation, key, request_hash, result_ref)
			 VALUES (?, ?, 'task.claim', ?, ?, ?)`,
			projectID, sessionID+"/"+workerID, idempotencyKey, requestHash, taskID,
		); err != nil {
			return nil, fmt.Errorf("GetNextTask: idempotency append: %w", err)
		}
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE project_queue_versions SET version = version + 1 WHERE project_id = ? AND version = ?`,
		projectID, queueVersion,
	)
	if err != nil {
		return nil, fmt.Errorf("GetNextTask: queue version CAS: %w", err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return nil, fmt.Errorf("GetNextTask: queue version changed: %w", store.ErrConcurrentConflict)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("GetNextTask: commit: %w", err)
	}

	task, err := s.taskStore.GetByID(ctx, projectID, taskID)
	if err != nil {
		return nil, fmt.Errorf("GetNextTask: read committed task: %w", err)
	}
	if err := s.ensureWorktreeForClaim(ctx, task, sessionID); err != nil {
		compErr := s.compensateFailedClaim(ctx, task, sessionID, workerID, idempotencyKey, err)
		if compErr != nil {
			return nil, errors.Join(fmt.Errorf("GetNextTask: workspace allocation: %w", err), compErr)
		}
		return nil, fmt.Errorf("GetNextTask: workspace allocation: %w", err)
	}
	if claimCreated != nil {
		*claimCreated = true
	}
	safeEmit(s.eventEmitter, "task.claimed", projectID, map[string]string{"task_id": taskID, "lease_id": leaseID})
	return task, nil
}

func appendStateHistory(ctx context.Context, tx *sql.Tx, projectID, aggregateType, aggregateID, from, to string, fromVersion, toVersion int64, actor, reason, causationID string) error {
	if tx == nil || strings.TrimSpace(projectID) == "" || strings.TrimSpace(aggregateID) == "" ||
		fromVersion < 0 || toVersion != fromVersion+1 || strings.TrimSpace(actor) == "" ||
		strings.TrimSpace(reason) == "" || strings.TrimSpace(causationID) == "" {
		return fmt.Errorf("append state history has incomplete authority or version chain: %w", store.ErrInvalidParameter)
	}
	validTransition := false
	switch aggregateType {
	case "task":
		validTransition = model.CanTaskTransition(from, to)
	case "session":
		validTransition = model.CanSessionTransition(from, to)
	case "worker":
		validTransition = model.CanWorkerTransition(from, to)
	case "worktree":
		validTransition = model.CanWorktreeTransition(from, to)
	case "lease":
		validTransition = isLeaseStatus(from) && isLeaseStatus(to) &&
			(from == to || (from == model.LeaseStatusActive && to != model.LeaseStatusActive))
	}
	if !validTransition {
		return fmt.Errorf("append state history rejects %s transition %s -> %s: %w",
			aggregateType, from, to, store.ErrTaskStateInvalid)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO state_history
		 (project_id, aggregate_type, aggregate_id, from_status, to_status, from_version, to_version,
		  actor_id, reason, causation_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		projectID, aggregateType, aggregateID, from, to, fromVersion, toVersion, actor, reason, causationID,
	); err != nil {
		return fmt.Errorf("append state history %s %s -> %s: %w", aggregateID, from, to, err)
	}
	return nil
}

func isLeaseStatus(status string) bool {
	switch status {
	case model.LeaseStatusActive, model.LeaseStatusCompleted, model.LeaseStatusReleased,
		model.LeaseStatusExpired, model.LeaseStatusCancelled:
		return true
	default:
		return false
	}
}

func (s *TaskService) ensureWorktreeForClaim(ctx context.Context, task *model.Task, sessionID string) error {
	if task.ActiveLeaseID == nil || task.LeaseEpoch <= 0 {
		return store.ErrLeaseNotFound
	}
	if existing, err := s.worktreeStore.GetByTaskID(ctx, task.ProjectID, task.ID); err == nil {
		if existing.Generation == task.LeaseEpoch && existing.Status == model.WorktreeStatusActive &&
			existing.SessionID != nil && *existing.SessionID == sessionID {
			project, err := s.projectStore.GetByID(ctx, task.ProjectID)
			if err != nil {
				return err
			}
			_, _, err = verifyWorktreeRepository(ctx, project.WorkspacePath, existing.WorktreePath, existing.BaseCommit)
			if err != nil {
				return fmt.Errorf("existing workspace identity validation failed: %w", err)
			}
			return nil
		}
		if existing.Generation < task.LeaseEpoch && existing.Status == model.WorktreeStatusActive {
			var sessionKey string
			if err := s.db.QueryRowContext(ctx,
				`SELECT id FROM agent_sessions WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
				task.ProjectID, sessionID,
			).Scan(&sessionKey); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return store.ErrSessionNotFound
				}
				return fmt.Errorf("resolve workspace session: %w", err)
			}
			project, err := s.projectStore.GetByID(ctx, task.ProjectID)
			if err != nil {
				return err
			}
			if _, _, err := verifyWorktreeRepository(
				ctx, project.WorkspacePath, existing.WorktreePath, existing.BaseCommit,
			); err != nil {
				return fmt.Errorf("reusable workspace identity validation failed: %w", err)
			}
			return s.rebindActiveWorktreeForClaim(ctx, task, sessionID, sessionKey, existing)
		}
		return fmt.Errorf("existing workspace owner/generation/status conflicts with lease: %w", store.ErrConcurrentConflict)
	} else if !errors.Is(err, store.ErrWorktreeNotFound) {
		return err
	}

	project, err := s.projectStore.GetByID(ctx, task.ProjectID)
	if err != nil {
		return err
	}
	baseCommit, err := getBaseCommit(ctx, project.WorkspacePath)
	if err != nil {
		return err
	}
	worktreePath, err := createWorktree(ctx, project.WorkspacePath, task.ID)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	wt := &model.Worktree{
		TaskID:       task.ID,
		ProjectID:    task.ProjectID,
		SessionID:    &sessionID,
		WorktreePath: worktreePath,
		BranchName:   fmt.Sprintf("task/%s", task.ID),
		BaseCommit:   baseCommit,
		Status:       model.WorktreeStatusActive,
		Generation:   task.LeaseEpoch,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if _, err := s.worktreeStore.Create(ctx, task.ProjectID, wt); err != nil {
		_ = removeWorktree(ctx, project.WorkspacePath, worktreePath)
		_ = deleteBranch(ctx, project.WorkspacePath, wt.BranchName)
		return err
	}
	return nil
}

// rebindActiveWorktreeForClaim preserves the edits from a blocked execution
// while atomically binding that exact workspace to a newly issued Lease
// generation. A prior Lease must be durably non-active and all Task, Lease and
// Worktree versions must still match the snapshots verified by the caller.
func (s *TaskService) rebindActiveWorktreeForClaim(
	ctx context.Context,
	task *model.Task,
	actorSessionID, physicalSessionID string,
	existing *model.Worktree,
) error {
	if task == nil || existing == nil || task.ActiveLeaseID == nil ||
		task.Status != model.TaskStatusExecuting || task.LeaseEpoch <= existing.Generation ||
		existing.Status != model.WorktreeStatusActive || existing.SessionID == nil {
		return fmt.Errorf("workspace rebind snapshot is invalid: %w", store.ErrRecoveryIntegrity)
	}
	if task.AssignedSessionID == nil || *task.AssignedSessionID != actorSessionID ||
		task.AssignedWorkerID == nil {
		return fmt.Errorf("workspace rebind owner mismatch: %w", store.ErrTaskNotOwned)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("workspace rebind begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		currentStatus, currentLeaseID, currentSessionID, currentWorkerID string
		currentVersion, currentEpoch                                     int64
	)
	if err := tx.QueryRowContext(ctx, `SELECT status, version, lease_epoch,
		COALESCE(active_lease_id, ''), COALESCE(assigned_session_id, ''),
		COALESCE(assigned_worker_id, '')
		FROM tasks WHERE project_id = ? AND id = ?`, task.ProjectID, task.ID).Scan(
		&currentStatus, &currentVersion, &currentEpoch, &currentLeaseID,
		&currentSessionID, &currentWorkerID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrTaskNotFound
		}
		return fmt.Errorf("workspace rebind task authority: %w", err)
	}
	if currentStatus != model.TaskStatusExecuting || currentVersion != task.Version ||
		currentEpoch != task.LeaseEpoch || currentLeaseID != *task.ActiveLeaseID ||
		currentSessionID != physicalSessionID || currentWorkerID != *task.AssignedWorkerID {
		return fmt.Errorf("workspace rebind task authority changed: %w", store.ErrConcurrentConflict)
	}

	var newLeaseStatus string
	var newLeaseEpoch int64
	if err := tx.QueryRowContext(ctx, `SELECT status, epoch FROM task_leases
		WHERE project_id = ? AND task_id = ? AND id = ? AND session_id = ? AND worker_id = ?`,
		task.ProjectID, task.ID, *task.ActiveLeaseID, physicalSessionID, *task.AssignedWorkerID,
	).Scan(&newLeaseStatus, &newLeaseEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrLeaseNotFound
		}
		return fmt.Errorf("workspace rebind current lease: %w", err)
	}
	if newLeaseStatus != model.LeaseStatusActive || newLeaseEpoch != task.LeaseEpoch {
		return fmt.Errorf("workspace rebind current lease is not authoritative: %w", store.ErrLeaseVersionMismatch)
	}

	var priorLeaseStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM task_leases
		WHERE project_id = ? AND task_id = ? AND epoch = ?`,
		task.ProjectID, task.ID, existing.Generation,
	).Scan(&priorLeaseStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("workspace generation has no prior lease: %w", store.ErrRecoveryIntegrity)
		}
		return fmt.Errorf("workspace rebind prior lease: %w", err)
	}
	if priorLeaseStatus == model.LeaseStatusActive {
		return fmt.Errorf("workspace prior lease remains active: %w", store.ErrRecoveryIntegrity)
	}

	var (
		worktreeStatus, worktreeSession string
		worktreeVersion, generation     int64
	)
	var priorPhysicalSessionID string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM agent_sessions
		WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		task.ProjectID, *existing.SessionID).Scan(&priorPhysicalSessionID); err != nil {
		return fmt.Errorf("workspace rebind prior session: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT status, version, generation, COALESCE(session_id, '')
		FROM worktrees WHERE project_id = ? AND task_id = ? AND id = ?`,
		task.ProjectID, task.ID, existing.ID,
	).Scan(&worktreeStatus, &worktreeVersion, &generation, &worktreeSession); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrWorktreeNotFound
		}
		return fmt.Errorf("workspace rebind snapshot: %w", err)
	}
	if worktreeStatus != model.WorktreeStatusActive || worktreeVersion != existing.Version ||
		generation != existing.Generation || worktreeSession != priorPhysicalSessionID {
		return fmt.Errorf("workspace rebind snapshot changed: %w", store.ErrConcurrentConflict)
	}

	result, err := tx.ExecContext(ctx, `UPDATE worktrees
		SET session_id = ?, generation = ?, version = version + 1, updated_at = datetime('now')
		WHERE project_id = ? AND task_id = ? AND id = ? AND status = 'active'
		  AND session_id = ? AND generation = ? AND version = ?`,
		physicalSessionID, task.LeaseEpoch, task.ProjectID, task.ID, existing.ID,
		priorPhysicalSessionID, existing.Generation, existing.Version,
	)
	if err != nil {
		return fmt.Errorf("workspace rebind update: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return fmt.Errorf("workspace rebind CAS: %w", errors.Join(store.ErrConcurrentConflict, err))
	}
	if err := appendStateHistory(ctx, tx, task.ProjectID, "worktree", fmt.Sprint(existing.ID),
		model.WorktreeStatusActive, model.WorktreeStatusActive,
		existing.Version, existing.Version+1, actorSessionID,
		"workspace rebound to fresh lease generation", *task.ActiveLeaseID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("workspace rebind commit: %w", err)
	}
	return nil
}

func (s *TaskService) compensateFailedClaim(
	ctx context.Context,
	task *model.Task,
	sessionID, workerID, idempotencyKey string,
	allocationErr error,
) error {
	if task.ActiveLeaseID == nil {
		return store.ErrLeaseNotFound
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var sessionKey string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM agent_sessions WHERE project_id = ? AND COALESCE(external_id, id) = ?`,
		task.ProjectID, sessionID,
	).Scan(&sessionKey); err != nil {
		return err
	}
	var (
		leaseStatus, leaseSession, leaseWorker string
		leaseVersion, leaseEpoch               int64
	)
	if err := tx.QueryRowContext(ctx, `SELECT status, version, epoch, session_id, worker_id
		FROM task_leases WHERE project_id = ? AND task_id = ? AND id = ?`,
		task.ProjectID, task.ID, *task.ActiveLeaseID,
	).Scan(&leaseStatus, &leaseVersion, &leaseEpoch, &leaseSession, &leaseWorker); err != nil {
		return err
	}
	if leaseStatus != model.LeaseStatusActive || leaseEpoch != task.LeaseEpoch ||
		leaseSession != sessionKey || leaseWorker != workerID {
		return fmt.Errorf("claim compensation lease authority changed: %w", store.ErrConcurrentConflict)
	}
	var workerStatus string
	var workerVersion int64
	var currentTask sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, current_task_id, version FROM agent_workers
		WHERE project_id = ? AND session_id = ? AND id = ?`,
		task.ProjectID, sessionKey, workerID,
	).Scan(&workerStatus, &currentTask, &workerVersion); err != nil {
		return err
	}
	if workerStatus != model.WorkerStatusBusy || !currentTask.Valid || currentTask.String != task.ID {
		return fmt.Errorf("claim compensation worker authority changed: %w", store.ErrConcurrentConflict)
	}

	var (
		worktreeID, worktreeVersion, worktreeGeneration int64
		worktreeStatus                                  string
	)
	worktreeErr := tx.QueryRowContext(ctx, `SELECT id, status, version, generation FROM worktrees
		WHERE project_id = ? AND task_id = ?`, task.ProjectID, task.ID).Scan(
		&worktreeID, &worktreeStatus, &worktreeVersion, &worktreeGeneration)
	if worktreeErr != nil && !errors.Is(worktreeErr, sql.ErrNoRows) {
		return worktreeErr
	}
	targetTaskStatus := model.TaskStatusQueued
	reason := "workspace allocation failed before durable workspace creation"
	if worktreeErr == nil {
		targetTaskStatus = model.TaskStatusNeedsHuman
		reason = "workspace allocation or generation binding failed; manual recovery required"
	}
	if allocationErr == nil {
		return fmt.Errorf("claim compensation requires an allocation failure: %w", store.ErrInvalidParameter)
	}

	result, err := tx.ExecContext(ctx,
		`UPDATE task_leases SET status = 'released', version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND task_id = ? AND session_id = ? AND worker_id = ?
		   AND status = 'active' AND epoch = ? AND version = ?`,
		*task.ActiveLeaseID, task.ProjectID, task.ID, sessionKey, workerID,
		task.LeaseEpoch, leaseVersion,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return errors.Join(store.ErrConcurrentConflict, err)
	}
	if err := appendStateHistory(ctx, tx, task.ProjectID, "lease", *task.ActiveLeaseID,
		model.LeaseStatusActive, model.LeaseStatusReleased, leaseVersion, leaseVersion+1,
		"system", reason, *task.ActiveLeaseID); err != nil {
		return err
	}

	result, err = tx.ExecContext(ctx,
		`UPDATE tasks SET status = ?, assigned_session_id = NULL, assigned_worker_id = NULL,
		     assigned_at = NULL, active_lease_id = NULL, lease_expires_at = NULL,
		     blocker_reason = CASE WHEN ? = 'needs_human' THEN ? ELSE blocker_reason END,
		     version = version + 1, updated_at = datetime('now')
		 WHERE id = ? AND project_id = ? AND status = 'executing' AND version = ? AND active_lease_id = ?`,
		targetTaskStatus, targetTaskStatus, reason,
		task.ID, task.ProjectID, task.Version, *task.ActiveLeaseID,
	)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return store.ErrConcurrentConflict
	}
	if err := appendStateHistory(ctx, tx, task.ProjectID, "task", task.ID,
		model.TaskStatusExecuting, targetTaskStatus, task.Version, task.Version+1,
		"system", reason, *task.ActiveLeaseID); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx,
		`UPDATE agent_workers SET current_task_id = NULL, status = 'idle', version = version + 1,
		     last_active = datetime('now')
		 WHERE project_id = ? AND session_id = ? AND id = ? AND current_task_id = ?
		   AND status = 'busy' AND version = ?`,
		task.ProjectID, sessionKey, workerID, task.ID, workerVersion,
	)
	if err != nil {
		return err
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		return errors.Join(store.ErrConcurrentConflict, err)
	}
	if err := appendStateHistory(ctx, tx, task.ProjectID, "worker", sessionKey+"/"+workerID,
		model.WorkerStatusBusy, model.WorkerStatusIdle, workerVersion, workerVersion+1,
		"system", reason, *task.ActiveLeaseID); err != nil {
		return err
	}

	if worktreeErr == nil && worktreeStatus != model.WorktreeStatusQuarantined {
		if !model.CanWorktreeTransition(worktreeStatus, model.WorktreeStatusQuarantined) {
			return fmt.Errorf("failed claim workspace %s cannot be quarantined: %w", worktreeStatus, store.ErrRecoveryIntegrity)
		}
		result, err = tx.ExecContext(ctx, `UPDATE worktrees SET status = 'quarantined',
			version = version + 1, updated_at = datetime('now')
			WHERE project_id = ? AND task_id = ? AND id = ? AND status = ?
			  AND version = ? AND generation = ?`,
			task.ProjectID, task.ID, worktreeID, worktreeStatus, worktreeVersion, worktreeGeneration)
		if err != nil {
			return err
		}
		if rows, err := result.RowsAffected(); err != nil || rows != 1 {
			return errors.Join(store.ErrConcurrentConflict, err)
		}
		if err := appendStateHistory(ctx, tx, task.ProjectID, "worktree", fmt.Sprint(worktreeID),
			worktreeStatus, model.WorktreeStatusQuarantined, worktreeVersion, worktreeVersion+1,
			"system", reason, *task.ActiveLeaseID); err != nil {
			return err
		}
	}
	if idempotencyKey != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM idempotency_records
			 WHERE project_id = ? AND scope = ? AND operation = 'task.claim' AND key = ?`,
			task.ProjectID, sessionID+"/"+workerID, idempotencyKey,
		); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE project_queue_versions SET version = version + 1 WHERE project_id = ?`, task.ProjectID,
	); err != nil {
		return err
	}
	return tx.Commit()
}
