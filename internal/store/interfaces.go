package store

import (
	"context"
	"errors"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
)

// ---------------------------------------------------------------------------
// 错误变量 — 与 api-spec.md 4.7 错误码规范对齐
// ---------------------------------------------------------------------------

var (
	ErrProjectNotFound       = errors.New("project not found")
	ErrProjectArchived       = errors.New("project archived")
	ErrProjectNotBound       = errors.New("project not bound")
	ErrProjectAmbiguous      = errors.New("ambiguous project match")
	ErrFeatureNotFound       = errors.New("feature not found")
	ErrTaskNotFound          = errors.New("task not found")
	ErrTaskNotOwned          = errors.New("task not owned by session")
	ErrTaskStateInvalid      = errors.New("task state invalid for this operation")
	ErrTaskAlreadyCancelled  = errors.New("task already cancelled")
	ErrTaskDependencyUnmet   = errors.New("task dependency unmet")
	ErrSessionNotFound       = errors.New("session not found")
	ErrSessionCapacityFull   = errors.New("session capacity full")
	ErrWorktreeCreateFailed  = errors.New("worktree create failed")
	ErrWorktreeCleanFailed   = errors.New("worktree clean failed")
	ErrConcurrentConflict    = errors.New("concurrent conflict, retry exhausted")
	ErrNoAvailableTask       = errors.New("no available task")
	ErrInvalidParameter      = errors.New("invalid parameter")
	ErrCircularDependency    = errors.New("circular dependency detected")
	ErrWorktreeNotFound      = errors.New("worktree not found")
	ErrWorkerNotFound        = errors.New("worker not found")
	ErrTestExecutionFailed   = errors.New("test execution failed")
	ErrBoundaryViolation     = errors.New("file boundary violation")
	ErrCoverageBelowMin      = errors.New("coverage below minimum threshold")
	ErrMergeConflict         = errors.New("merge conflict detected")
	ErrValidationFailed      = errors.New("validation failed")
	ErrDependencyNotReady    = errors.New("dependency not ready")
	ErrFeatureStatusInvalid  = errors.New("feature status invalid for this operation")
	ErrProjectAlreadyExists  = errors.New("project already exists")
	ErrContractNotFound      = errors.New("contract not found")
	ErrProjectScopeViolation = errors.New("resource does not belong to the authorized project")
	ErrLeaseNotFound         = errors.New("active lease not found")
	ErrLeaseExpired          = errors.New("lease expired")
	ErrLeaseVersionMismatch  = errors.New("lease version mismatch")
	ErrIdempotencyConflict   = errors.New("idempotency key reused with different request")
	ErrOperationDisabled     = errors.New("operation disabled by platform policy")
	ErrRecoveryIntegrity     = errors.New("startup recovery could not prove state integrity")
)

// M1 identity, runner registry and event pipeline errors. The PostgreSQL
// implementations (M1-DATA-001) must return these exact sentinels so REST,
// MCP and background surfaces map one stable error code per condition.
var (
	ErrUserNotFound          = errors.New("user not found")
	ErrMembershipNotFound    = errors.New("membership not found")
	ErrRunnerNotFound        = errors.New("runner not found")
	ErrRunnerRevoked         = errors.New("runner revoked")
	ErrRunnerStatusInvalid   = errors.New("runner status invalid for this operation")
	ErrRunnerNotBound        = errors.New("runner not bound to project")
	ErrRunnerGenerationStale = errors.New("runner connection generation stale")
	ErrEnrollmentInvalid     = errors.New("enrollment code invalid")
	ErrEnrollmentExpired     = errors.New("enrollment code expired")
	ErrEnrollmentConsumed    = errors.New("enrollment code already consumed")
	ErrDuplicateEvent        = errors.New("event already recorded")
	ErrOutboxClaimMismatch   = errors.New("outbox claim no longer owned by this dispatcher")
	ErrMigrationLocked       = errors.New("schema migration held by another owner")
)

// ---------------------------------------------------------------------------
// 过滤条件类型
// ---------------------------------------------------------------------------

// TaskFilter 任务列表过滤条件（对应 REST API ?status=&role=&feature_id=）。
type TaskFilter struct {
	Status    string // pending/in_progress/submitted/verifying/ready_to_merge/done/blocked/cancelled/merge_conflicted
	Role      string // backend/frontend/devops/verifier
	FeatureID string // 按 Feature 过滤
}

// AuditFilter 审计日志过滤条件。
type AuditFilter struct {
	BoundProject string // 按绑定项目过滤（对应 audit_log.bound_project）
	SessionID    string // 按 Session 过滤
	Result       string // ALLOWED / DENIED / WARNED
	Limit        int    // 返回条数上限
	Since        string // ISO8601 时间戳，仅返回此时间之后的记录
}

// ---------------------------------------------------------------------------
// ProjectStore — 项目管理
// L4 隔离说明：Project 是顶层实体，自身即为隔离边界，不额外需要 projectID 参数。
// ---------------------------------------------------------------------------

type ProjectStore interface {
	// Create 注册新项目。
	Create(ctx context.Context, p *model.Project) error

	// GetByID 按 ID 获取项目（全局端点，不需要 projectID 前置参数）。
	GetByID(ctx context.Context, id string) (*model.Project, error)

	// Update 更新项目信息（name/description/config 等）。
	Update(ctx context.Context, p *model.Project) error

	// List 列出所有项目。includeArchived 控制是否包含已归档项目。
	List(ctx context.Context, includeArchived bool) ([]*model.Project, error)

	// Archive 归档项目（status → archived）。
	Archive(ctx context.Context, id string) error

	// Restore 恢复已归档项目（status → active）。
	Restore(ctx context.Context, id string) error

	// FindByPath 按工作区路径查找项目。返回匹配列表（可能多个，触发 ErrProjectAmbiguous）。
	FindByPath(ctx context.Context, workspacePath string) ([]*model.Project, error)
}

// ---------------------------------------------------------------------------
// FeatureStore — Feature 管理（项目级）
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type FeatureStore interface {
	Create(ctx context.Context, projectID string, f *model.Feature) error
	GetByID(ctx context.Context, projectID, id string) (*model.Feature, error)
	List(ctx context.Context, projectID string) ([]*model.Feature, error)
	Update(ctx context.Context, projectID string, f *model.Feature) error
	CountByProject(ctx context.Context, projectID string) (int, error)
}

// ---------------------------------------------------------------------------
// TaskStore — Task 管理（核心，方法最多）
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type TaskStore interface {
	// Create 创建新任务。t.ID 由调用方（Service 层）按 T-{5位序号} 格式生成。
	Create(ctx context.Context, projectID string, t *model.Task) error

	// GetByID 按 ID 获取任务。不存在的 ID 或跨项目 ID 均返回 ErrTaskNotFound。
	GetByID(ctx context.Context, projectID, id string) (*model.Task, error)

	// List 按过滤条件列出任务。空字符串字段表示不过滤。
	List(ctx context.Context, projectID string, filter TaskFilter) ([]*model.Task, error)

	// UpdateStatus 原子更新任务状态。实现层应使用 WHERE status = ?oldStatus 做乐观锁。
	UpdateStatus(ctx context.Context, projectID, taskID, newStatus string) error

	// UpdateStatusFrom 条件更新任务状态：仅当当前状态为 expectedOldStatus 时才更新。
	// 用于防止并发修改导致状态被意外覆盖。
	UpdateStatusFrom(ctx context.Context, projectID, taskID, expectedOldStatus, newStatus string) error

	// UpdateStatusFromVersion additionally guards the aggregate version and
	// increments it on success. M0 write use cases should prefer this method.
	UpdateStatusFromVersion(ctx context.Context, projectID, taskID, expectedOldStatus string, expectedVersion int64, newStatus string) error

	// Update 更新任务的可修改字段（title/description/allowed_directories 等）。
	Update(ctx context.Context, projectID string, t *model.Task) error

	// FindNextAvailable 查找下一个可认领的 pending 任务（含动态依赖检查）。
	// role 用于过滤匹配角色的任务。按 priority + created_at 排序。
	FindNextAvailable(ctx context.Context, projectID, role string) (*model.Task, error)

	// FindNextSubmitted 查找下一个 submitted 状态的任务（供验证者认领）。
	// 不按 role 过滤，按 created_at 排序。
	FindNextSubmitted(ctx context.Context, projectID string) (*model.Task, error)

	// Claim 原子认领任务（pending → in_progress）。设置 assigned_session_id、assigned_worker_id、assigned_at。
	// Store-level claiming is disabled; Application Service owns Task+Lease+Worker CAS.
	Claim(ctx context.Context, projectID, taskID, sessionID, workerID string) error

	// ClaimVerification 原子认领验证任务（submitted → verifying）。
	// 不修改 assigned_session_id/assigned_worker_id（保持指向原执行者）。
	ClaimVerification(ctx context.Context, projectID, taskID, verifierSessionID, verifierWorkerID string) error

	// CountByStatus 按状态统计任务数量，返回 map[status]count。
	CountByStatus(ctx context.Context, projectID string) (map[string]int, error)

	// CheckDependencies 检查给定依赖列表是否全部满足。
	// 取消的依赖目标视为已满足（不阻塞下游）。
	CheckDependencies(ctx context.Context, projectID string, deps []model.Dependency) (bool, error)

	// CheckCircular 检查为 taskID 添加 deps 后是否会形成循环依赖。
	CheckCircular(ctx context.Context, projectID, taskID string, deps []model.Dependency) (bool, error)
}

// ---------------------------------------------------------------------------
// TaskResultStore — Task 提交结果（服务端取证后写入）
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type TaskResultStore interface {
	// Upsert 插入或更新任务结果。每次 submit_task_result 调用后由服务端写入。
	Upsert(ctx context.Context, projectID string, r *model.TaskResult) error

	// GetByTaskID 按 taskID 获取当前（最新）任务结果。
	GetByTaskID(ctx context.Context, projectID, taskID string) (*model.TaskResult, error)
}

// ---------------------------------------------------------------------------
// ValidationRunStore — 验证历史（append-only）
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type ValidationRunStore interface {
	// Create appends M0 local diagnostic Evidence. Authority/producer are
	// server-owned and fixed to diagnostic/maestro-local; merge_gate ingestion
	// requires a separate authenticated CI port in M2.
	Create(ctx context.Context, projectID string, r *model.ValidationRun) (int64, error)

	// ListByTask 列出某任务的全部验证历史（按 attempt 升序）。
	ListByTask(ctx context.Context, projectID, taskID string) ([]*model.ValidationRun, error)

	// LatestByTask 获取某任务的最新一次验证记录。
	LatestByTask(ctx context.Context, projectID, taskID string) (*model.ValidationRun, error)
}

// ---------------------------------------------------------------------------
// WorktreeStore — Worktree 资源管理
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type WorktreeStore interface {
	// Create 创建 Worktree 记录。返回自增 ID。
	Create(ctx context.Context, projectID string, w *model.Worktree) (int64, error)

	// GetByTaskID 按 taskID 获取 Worktree（一个任务最多一个 Worktree）。
	GetByTaskID(ctx context.Context, projectID, taskID string) (*model.Worktree, error)

	// UpdateStatus 更新 Worktree 状态（allocated/active/submitted/stale/merged/abandoned）。
	UpdateStatus(ctx context.Context, projectID string, id int64, status string) error

	// ListByStatus 按状态列出 Worktree（用于 GC 扫描 stale/abandoned 等）。
	ListByStatus(ctx context.Context, projectID, status string) ([]*model.Worktree, error)

	// Delete 删除 Worktree 记录。
	Delete(ctx context.Context, projectID string, id int64) error

	// ListByProject 列出项目下所有 Worktree。
	ListByProject(ctx context.Context, projectID string) ([]*model.Worktree, error)
}

// ---------------------------------------------------------------------------
// SessionStore — Agent Session
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// FindStale 除外——它跨项目扫描超时 Session，由后台 goroutine 调用。
// ---------------------------------------------------------------------------

type SessionStore interface {
	// Create 注册新 Session。
	Create(ctx context.Context, projectID string, s *model.AgentSession) error

	// CreateIfNotExists inserts session only if id does not exist. Returns true if created.
	CreateIfNotExists(ctx context.Context, projectID string, s *model.AgentSession) (bool, error)

	// GetByID 按 ID 获取 Session。
	GetByID(ctx context.Context, projectID, id string) (*model.AgentSession, error)

	// List 列出项目下所有 Session。
	List(ctx context.Context, projectID string) ([]*model.AgentSession, error)

	// UpdateHeartbeat 更新 last_heartbeat 为当前时间。
	UpdateHeartbeat(ctx context.Context, projectID, id string) error

	// UpdateStatus 更新 Session 状态（online/offline）。
	UpdateStatus(ctx context.Context, projectID, id, status string) error

	// FindStale 查找超时未心跳的在线 Session（跨项目扫描，不需要 projectID）。
	// timeoutSec 为超时阈值（秒）。后台 goroutine 每 30s 调用一次。
	FindStale(ctx context.Context, timeoutSec int) ([]*model.AgentSession, error)
}

// ---------------------------------------------------------------------------
// WorkerStore — Agent Worker
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type WorkerStore interface {
	// Create 注册新 Worker（隐式注册或显式 REST API 注册）。
	Create(ctx context.Context, projectID, sessionID string, w *model.AgentWorker) error

	// GetByID 按 workerID 获取 Worker。
	GetByID(ctx context.Context, projectID, sessionID, workerID string) (*model.AgentWorker, error)
	// GetByIdle retrieves an idle worker from the given session.
	GetByIdle(ctx context.Context, projectID, sessionID string) (*model.AgentWorker, error)

	// ListBySession 列出某 Session 下所有 Worker。
	ListBySession(ctx context.Context, projectID, sessionID string) ([]*model.AgentWorker, error)

	// UpdateCurrentTask 更新 Worker 当前执行的任务 ID。传空字符串表示清空（任务完成/释放）。
	UpdateCurrentTask(ctx context.Context, projectID, sessionID, workerID, taskID string) error
	// Update updates a worker's status and tasks_completed count.
	Update(ctx context.Context, projectID, sessionID string, w *model.AgentWorker) error

	// Delete 删除 Worker 记录（释放子 Worker 时调用）。
	Delete(ctx context.Context, projectID, sessionID, workerID string) error

	// CountBySession 统计某 Session 下的 Worker 数量（用于容量检查）。
	CountBySession(ctx context.Context, projectID, sessionID string) (int, error)
}

// ---------------------------------------------------------------------------
// ContractStore — API 契约
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type ContractStore interface {
	// Upsert 插入或更新 API 契约（按 project_id + method + path 唯一键）。
	Upsert(ctx context.Context, projectID string, c *model.APIContract) error

	// GetByMethodPath 按 method + path 查询契约（供边界校验使用）。
	GetByMethodPath(ctx context.Context, projectID, method, path string) (*model.APIContract, error)

	// List 列出项目下所有契约。
	List(ctx context.Context, projectID string) ([]*model.APIContract, error)

	// DeleteByProject 删除项目下所有契约（重新解析前清空）。
	DeleteByProject(ctx context.Context, projectID string) error
}

// ---------------------------------------------------------------------------
// ActivityLogStore — 业务活动日志（append-only）
// 所有查询方法的第一个参数必须是 projectID（L4 隔离）。
// ---------------------------------------------------------------------------

type ActivityLogStore interface {
	// Create 追加一条活动日志。
	Create(ctx context.Context, projectID string, log *model.ActivityLog) error

	// List 按时间倒序列出活动日志。limit 控制返回条数，since 为 ISO8601 时间戳过滤。
	List(ctx context.Context, projectID string, limit int, since string) ([]*model.ActivityLog, error)
}

// ---------------------------------------------------------------------------
// AuditLogStore — 安全审计日志（append-only，跨项目）
// Create 不需要 projectID——审计日志由 API 中间件写入，bound_project 在 log 结构体内。
// List 按 AuditFilter 过滤（filter.BoundProject 提供 L4 隔离）。
// CountDenied 不需要 projectID——用于窜台检测，按 sessionID 统计跨项目拒绝次数。
// ---------------------------------------------------------------------------

type AuditLogStore interface {
	// Create 追加一条安全审计记录。
	Create(ctx context.Context, log *model.AuditLog) error

	// List 按过滤条件查询审计日志。
	List(ctx context.Context, filter AuditFilter) ([]*model.AuditLog, error)

	// CountDenied 统计指定 Session 在给定时间之后的 DENIED 记录数（窜台检测用）。
	CountDenied(ctx context.Context, sessionID string, since string) (int, error)
}

// ---------------------------------------------------------------------------
// DB — 数据库初始化接口
// ---------------------------------------------------------------------------

type DB interface {
	// Init 创建所有表和索引（幂等，使用 IF NOT EXISTS）。
	Init(ctx context.Context) error

	// Close 关闭数据库连接。
	Close() error
}

// ---------------------------------------------------------------------------
// M1 PostgreSQL-era contracts (M1-DATA-001 / ADR-002 / ADR-003).
//
// These interfaces are the frozen persistence contract for the PostgreSQL
// source of truth. The SQLite implementation keeps serving the M0 tables
// unchanged until cutover; S1 lands the PG adapters in M1-P3/P4. Every
// project-scoped method keeps projectID as the leading scope parameter so
// one signature shape serves both drivers.
// ---------------------------------------------------------------------------

// ProjectMembershipView is the derived per-project role used to build the
// server-side PrincipalContext; it is computed from team ownership plus
// active memberships and never accepted from a request payload.
type ProjectMembershipView struct {
	ProjectID string
	Role      string
}

// IdentityStore persists OIDC-backed users and their memberships
// (ADR-003). Only hashes and non-sensitive claims are stored.
type IdentityStore interface {
	// GetOrCreateUser maps a verified issuer+subject pair to a user row.
	// Repeated logins of the same identity return the same user idempotently.
	GetOrCreateUser(ctx context.Context, issuer, subject, displayName string) (*model.User, error)

	// GetUser returns the user by server-side ID.
	GetUser(ctx context.Context, id string) (*model.User, error)

	// UpdateUserStatus transitions invited/active/suspended/removed.
	UpdateUserStatus(ctx context.Context, id, expectedStatus, newStatus string) error

	// CreateMembership adds a team membership; (team_id, user_id) is unique.
	CreateMembership(ctx context.Context, m *model.TeamMembership) error

	// ListMembershipsByUser returns all memberships valid at the given time.
	ListMembershipsByUser(ctx context.Context, userID, at string) ([]*model.TeamMembership, error)

	// ListProjectMemberships derives the project_id -> role map used to
	// construct PrincipalContext.ProjectMemberships.
	ListProjectMemberships(ctx context.Context, userID string) ([]ProjectMembershipView, error)
}

// RunnerRegistryStore persists the runner device registry, one-time
// enrollment codes and project bindings (ADR-001 / runner-security).
type RunnerRegistryStore interface {
	// CreateEnrollment stores a hashed one-time, project-bound code with the
	// fixed enrollment TTL; the plaintext code is never persisted.
	CreateEnrollment(ctx context.Context, e *model.RunnerEnrollment) error

	// ConsumeEnrollment atomically consumes an unconsumed, unexpired code
	// (compare-and-swap). Expired, unknown or reused codes return
	// ErrEnrollmentExpired / ErrEnrollmentInvalid / ErrEnrollmentConsumed.
	ConsumeEnrollment(ctx context.Context, enrollmentID, codeHash string) error

	// EnrollmentByCodeHash resolves an unconsumed, unexpired code to its
	// enrollment and project binding; the enrollment endpoint uses this
	// to authenticate the presented code before consuming it.
	EnrollmentByCodeHash(ctx context.Context, codeHash string) (*model.RunnerEnrollment, string, error)

	// ProjectOfRunner resolves the single project a runner is bound to;
	// unbound runners return ErrRunnerNotBound.
	ProjectOfRunner(ctx context.Context, runnerID string) (string, error)

	// CreateRunner registers a pending_approval device and its initial
	// project binding in one transaction.
	CreateRunner(ctx context.Context, runner *model.RunnerDevice, binding *model.RunnerBinding) error

	// GetRunner returns the device by ID.
	GetRunner(ctx context.Context, id string) (*model.RunnerDevice, error)

	// UpdateRunnerStatus performs a guarded status transition
	// (expectedStatus compare-and-swap); revoked is terminal.
	UpdateRunnerStatus(ctx context.Context, id, expectedStatus, newStatus string) error

	// BumpRunnerGeneration rotates the fencing generation on reconnect; all
	// messages carrying an older generation must be rejected afterwards.
	BumpRunnerGeneration(ctx context.Context, id string) (int64, error)

	// UpdateRunnerHeartbeat records bounded liveness for an approved device.
	UpdateRunnerHeartbeat(ctx context.Context, id string) error

	// ListRunnersByProject lists devices bound to the project scope.
	ListRunnersByProject(ctx context.Context, projectID string) ([]*model.RunnerDevice, error)

	// RevokeRunner marks the device revoked (terminal) and stamps RevokedAt.
	RevokeRunner(ctx context.Context, id string) error
}

// OutboxStore implements the transactional outbox (ADR-002): rows are
// enqueued in the same transaction as the business state change and
// dispatched at least once with FOR UPDATE SKIP LOCKED semantics.
type OutboxStore interface {
	// Enqueue appends a pending event bound to the caller's transaction.
	Enqueue(ctx context.Context, e *model.OutboxEvent) error

	// ClaimPending leases up to batchSize pending/retry_wait events to one
	// dispatcher owner; competing dispatchers never claim the same row.
	ClaimPending(ctx context.Context, batchSize int, owner, now string) ([]*model.OutboxEvent, error)

	// MarkDelivered finalizes a successful dispatch for the claiming owner.
	MarkDelivered(ctx context.Context, eventID, owner string) error

	// MarkRetry schedules the next attempt with exponential backoff plus
	// jitter; attempts exceeding the limit move the row to dead_letter.
	MarkRetry(ctx context.Context, eventID, owner string, attempts int, availableAt string) error

	// MarkDeadLetter parks an exhausted event for audited, privileged replay.
	MarkDeadLetter(ctx context.Context, eventID, owner string) error
}

// InboxStore implements the durable inbox: verified external events are
// persisted exactly once before any business effect is produced.
type InboxStore interface {
	// Record inserts a received event; it reports false when the event
	// identity already exists so consumers stay idempotent.
	Record(ctx context.Context, e *model.InboxEvent) (bool, error)

	// ClaimProcessing guards a consumer's transition to processing.
	ClaimProcessing(ctx context.Context, eventID string) error

	// MarkProcessed records the exactly-once business completion.
	MarkProcessed(ctx context.Context, eventID string) error
}

// IdempotencyRecord is the stored outcome of a prior write request, keyed by
// (principal_id, project_id, operation, key) per TECH-DATA-001 section 8.
type IdempotencyRecord struct {
	PrincipalID     string
	ProjectID       string
	Operation       string
	Key             string
	RequestHash     string
	ResponseStatus  int
	ResponseSummary string
	CreatedAt       string
}

// APIIdempotencyStore backs the mandatory write-path idempotency contract:
// the same key with the same request hash replays the original result; the
// same key with a different body returns ErrIdempotencyConflict.
type APIIdempotencyStore interface {
	LookupOrCreate(ctx context.Context, record *IdempotencyRecord) (replayed bool, existing *IdempotencyRecord, err error)
}

// Repositories is the transaction-scoped aggregate exposing every store a
// use case may touch. Implementations MUST bind every returned store to the
// single *sql.Tx owned by the surrounding UnitOfWork; re-entering the base
// pool inside a transaction is a contract violation (SVC-INV / SVC-GATE-003).
type Repositories interface {
	Projects() ProjectStore
	Features() FeatureStore
	Tasks() TaskStore
	TaskResults() TaskResultStore
	ValidationRuns() ValidationRunStore
	Worktrees() WorktreeStore
	Sessions() SessionStore
	Workers() WorkerStore
	Contracts() ContractStore
	ActivityLogs() ActivityLogStore
	AuditLogs() AuditLogStore
	Identities() IdentityStore
	RunnerRegistry() RunnerRegistryStore
	Outbox() OutboxStore
	Inbox() InboxStore
	APIIdempotency() APIIdempotencyStore
}

// UnitOfWork owns the single transaction boundary for application services
// (TECH-SVC-001 section 4): authorization happens before Begin, and the
// callback receives tx-scoped repositories plus the inherited context.
type UnitOfWork interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, r Repositories) error) error
}
