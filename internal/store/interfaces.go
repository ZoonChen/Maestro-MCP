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
	ErrProjectNotFound      = errors.New("project not found")
	ErrProjectArchived      = errors.New("project archived")
	ErrProjectNotBound      = errors.New("project not bound")
	ErrProjectAmbiguous     = errors.New("ambiguous project match")
	ErrFeatureNotFound      = errors.New("feature not found")
	ErrTaskNotFound         = errors.New("task not found")
	ErrTaskNotOwned         = errors.New("task not owned by session")
	ErrTaskStateInvalid     = errors.New("task state invalid for this operation")
	ErrTaskAlreadyCancelled = errors.New("task already cancelled")
	ErrTaskDependencyUnmet  = errors.New("task dependency unmet")
	ErrSessionNotFound      = errors.New("session not found")
	ErrSessionCapacityFull  = errors.New("session capacity full")
	ErrWorktreeCreateFailed = errors.New("worktree create failed")
	ErrWorktreeCleanFailed  = errors.New("worktree clean failed")
	ErrConcurrentConflict   = errors.New("concurrent conflict, retry exhausted")
	ErrNoAvailableTask      = errors.New("no available task")
	ErrInvalidParameter     = errors.New("invalid parameter")
	ErrCircularDependency   = errors.New("circular dependency detected")
	ErrWorktreeNotFound     = errors.New("worktree not found")
	ErrWorkerNotFound       = errors.New("worker not found")
	ErrTestExecutionFailed  = errors.New("test execution failed")
	ErrBoundaryViolation    = errors.New("file boundary violation")
	ErrCoverageBelowMin     = errors.New("coverage below minimum threshold")
	ErrMergeConflict        = errors.New("merge conflict detected")
	ErrValidationFailed     = errors.New("validation failed")
	ErrDependencyNotReady   = errors.New("dependency not ready")
	ErrFeatureStatusInvalid = errors.New("feature status invalid for this operation")
	ErrProjectAlreadyExists = errors.New("project already exists")
	ErrContractNotFound     = errors.New("contract not found")
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

	// Update 更新任务的可修改字段（title/description/allowed_directories 等）。
	Update(ctx context.Context, projectID string, t *model.Task) error

	// FindNextAvailable 查找下一个可认领的 pending 任务（含动态依赖检查）。
	// role 用于过滤匹配角色的任务。按 priority + created_at 排序。
	FindNextAvailable(ctx context.Context, projectID, role string) (*model.Task, error)

	// FindNextSubmitted 查找下一个 submitted 状态的任务（供验证者认领）。
	// 不按 role 过滤，按 created_at 排序。
	FindNextSubmitted(ctx context.Context, projectID string) (*model.Task, error)

	// Claim 原子认领任务（pending → in_progress）。设置 assigned_session_id、assigned_worker_id、assigned_at。
	// 实现层应使用事务 + WHERE status='pending' 做乐观锁，并发冲突时返回 ErrConcurrentConflict。
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
	// Create 追加一条验证记录。返回自增 ID。
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
