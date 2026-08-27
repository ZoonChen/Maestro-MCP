# 3.1 项目访问隔离

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 核心机制 > 项目访问隔离
> **相关文档:** [Service 层边界](service-boundary.md) | [并发模型](concurrency-model.md) | [项目管理 PRD](../prd/project-management.md)

---

## 四层防御

```
                    Agent 请求
                        │
    ┌───────────────────▼────────────────────┐
    │  L1: 连接绑定层 (Connection Binding)    │  连接建立时锁定 project_id
    └───────────────────┬────────────────────┘
                        │ project_ctx (不可变)
    ┌───────────────────▼────────────────────┐
    │  L2: API 中间件层 (Request Guard)       │  每个请求校验 :pid
    └───────────────────┬────────────────────┘
                        │ 强制携带 project_id
    ┌───────────────────▼────────────────────┐
    │  L3: 业务逻辑层 (Service Enforcement)   │  task_id 归属校验
    └───────────────────┬────────────────────┘
                        │ WHERE project_id = ?
    ┌───────────────────▼────────────────────┐
    │  L4: 数据存储层 (Store Scoping)         │  SQL 强制隔离
    └────────────────────────────────────────┘
```

### L1: 连接绑定层

连接建立时锁定 project_id，后续不可更改。Agent 无法在 Tool 参数中指定或覆盖。

```go
type ProjectContext struct {
    ProjectID, ProjectName, WorkspacePath string
    BoundAt    time.Time
    BindMethod string // "cwd_match" | "flag" | "handshake"
}

func BindProject(connInfo ConnectionInfo) (*ProjectContext, error) {
    // 1. 显式指定优先 (flag/handshake)
    if connInfo.ExplicitProject != "" {
        proj, err := store.GetProject(connInfo.ExplicitProject)
        if err != nil || proj.Status != "active" { return nil, ErrProjectNotFound }
        return &ProjectContext{ProjectID: proj.ID, BindMethod: "flag"}, nil
    }
    // 2. cwd 匹配（精确匹配或包含关系匹配；多个匹配时返回 ErrAmbiguousProject）
    matches := store.FindProjectByPath(connInfo.WorkingDir)
    switch len(matches) {
    case 1:  return &ProjectContext{ProjectID: matches[0].ID, BindMethod: "cwd_match"}, nil
    case 0:  return nil, ErrProjectNotBound
    default: return nil, ErrAmbiguousProject{Candidates: matches}
    }
}
```

### L2: API 中间件层

URL 路径中的 `:pid` 必须与绑定的 project_id 一致，不匹配返回 403。

```go
func ProjectGuard() gin.HandlerFunc {
    return func(c *gin.Context) {
        boundPID := c.GetString("project_id")
        pathPID := c.Param("pid")
        if pathPID == "" { c.Next(); return }
        if pathPID != boundPID {
            audit.Log(c, AuditEvent{Action: "cross_project_access_denied", BoundPID: boundPID, TargetPID: pathPID})
            c.AbortWithStatusJSON(403, gin.H{"error": "access denied: project mismatch"})
            return
        }
        proj, err := store.GetProject(pathPID)
        if err != nil || proj.Status == "archived" {
            c.AbortWithStatusJSON(403, gin.H{"error": "project not found or archived"}); return
        }
        c.Set("project", proj); c.Next()
    }
}
```

### L3: 业务逻辑层

所有 Service 方法第一参数均为 `projectID`。`get_next_task` 自动追加项目过滤；`submit_task_result` 校验 task 归属，不区分 "不存在" 和 "不属于本项目"。

```go
type TaskService struct { store TaskStore }
func (s *TaskService) GetNextTask(projectID, sessionID, role, workerID string) (*Task, error) {
    // 简化示意：完整实现包含原子认领、session/worker 绑定、Worktree 创建，见 concurrency-model.md
    return s.store.FindNextAvailable(projectID, role)
}
func (s *TaskService) SubmitTaskResult(projectID, taskID string, result TaskResult) error {
    task, err := s.store.GetByID(projectID, taskID) // store 层已含 project_id
    if err != nil { return ErrTaskNotFound }        // 不泄漏跨项目信息
    if task.Status != "in_progress" { return ErrTaskStateInvalid }
    // ... 验证逻辑
}
```

### L4: 数据存储层

Store 接口中**不存在**不带 `projectID` 参数的查询方法。所有 SQL 自动注入 `WHERE project_id = ?`。

```go
type TaskStore interface {
    Create(projectID string, task *Task) error
    GetByID(projectID, taskID string) (*Task, error)
    FindNextAvailable(projectID, role string) (*Task, error)
    List(projectID string, filter TaskFilter) ([]*Task, error)
    UpdateStatus(projectID, taskID, newStatus string) error
    // 禁止：GetByID(taskID) 或 List(filter) 不带 projectID
}

func (s *SQLiteTaskStore) GetByID(projectID, taskID string) (*Task, error) {
    // WHERE project_id = ? AND id = ? — 统一返回 ErrTaskNotFound，不泄漏跨项目信息
}
```

## 审计日志

每次请求记录至 `audit_log` 表，字段对应：`session_id`, `bound_project`, `target_project?`, `action`, `target_task?`, `result: ALLOWED|DENIED|WARNED`。同一 Agent 5 分钟内 3 次 DENIED 触发看板告警。

## 协调者跨项目访问

```go
type CoordinatorAccess struct {
    BoundProjectID string
    GlobalRead     bool     // 全局只读
    WriteProjects  []string // 可写项目列表
}
```
