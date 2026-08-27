# 3.5 Session + Worker 并发模型

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 核心机制 > 并发模型
> **相关文档:** [项目隔离](project-isolation.md) | [Worktree 模型](worktree-model.md) | [多客户端 PRD](../prd/multi-client.md)

---

## 数据模型

```typescript
interface AgentSession {
    session_id: string;
    project_id: string;
    role: Role;
    client_type: "claude-code" | "openclaw" | "other";
    capacity: number;         // 最大并发 Worker 数
    status: "online" | "offline";
    last_heartbeat: string;
    workers: AgentWorker[];
}

interface AgentWorker {
    worker_id: string;        // "default", "sub-1", ...
    session_id: string;
    project_id: string;
    current_task_id: string | null;
    status: "idle" | "busy";
    tasks_completed: number;
    last_active: string;
}
```

## 三种并行场景映射

```
场景 1: 跨模块并行 (不同角色，不同连接)
├─ Session A: cc-backend-01  (role=backend, capacity=1)
│  └─ Worker: default → T-005
└─ Session B: cc-frontend-01 (role=frontend, capacity=1)
   └─ Worker: default → T-007

场景 2: 同模块多实例并行 (同角色，不同连接)
├─ Session A: cc-backend-01  (capacity=1)
│  └─ Worker: default → T-005
└─ Session C: cc-backend-02  (capacity=1)
   └─ Worker: default → T-008

场景 3: 单实例子 Agent 并行 (一个连接，多个 Worker)
└─ Session A: cc-backend-01  (capacity=5)
   ├─ Worker: default → T-005  (主 Agent)
   ├─ Worker: sub-1    → T-008  (子 Agent 1)
   ├─ Worker: sub-2    → T-009  (子 Agent 2)
   ├─ Worker: sub-3    → null   (空闲)
   └─ Worker: sub-4    → null   (空闲)
```

## get_next_task 原子认领 (SQLite 事务)

```go
const maxClaimRetry = 3

func (s *TaskService) GetNextTask(ctx context.Context, projectID, sessionID, role, workerID string) (*Task, error) {
    return s.getNextTaskWithRetry(ctx, projectID, sessionID, role, workerID, 0)
}

func (s *TaskService) getNextTaskWithRetry(ctx context.Context, projectID, sessionID, role, workerID string, attempt int) (*Task, error) {
    if attempt >= maxClaimRetry { return nil, ErrConcurrentConflict }
    tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
    if err != nil { return nil, fmt.Errorf("begin tx: %w", err) }
    // 1. 查找下一个 pending 任务 (动态依赖检查)
    var taskID string
    err = tx.QueryRowContext(ctx, `
        SELECT id FROM tasks
        WHERE project_id = ?
          AND role = ?
          AND status = 'pending'
          AND NOT EXISTS (
              SELECT 1 FROM json_each(tasks.dependencies) AS dep
              LEFT JOIN tasks AS dep_task ON dep_task.id = json_extract(dep.value, '$.task_id')
              WHERE dep_task.id IS NULL
                 OR (
                     COALESCE(json_extract(dep.value, '$.require_state'), 'done') = 'submitted'
                 AND dep_task.status NOT IN ('submitted','verifying','ready_to_merge','done','cancelled')
                 )
                 OR (
                     COALESCE(json_extract(dep.value, '$.require_state'), 'done') != 'submitted'
                 AND dep_task.status NOT IN ('done','cancelled')
                 )
          )
        ORDER BY
          CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
          created_at ASC
        LIMIT 1`, projectID, role).Scan(&taskID)
    if err == sql.ErrNoRows { tx.Rollback(); return nil, ErrNoAvailableTask }
    if err != nil { tx.Rollback(); return nil, fmt.Errorf("query next task: %w", err) }
    // 2. 原子更新：仅 status='pending' 时更新
    result, err := tx.ExecContext(ctx, `UPDATE tasks SET status='in_progress',
        assigned_session_id=?, assigned_worker_id=?, assigned_at=datetime('now'), updated_at=datetime('now')
        WHERE id=? AND project_id=? AND status='pending'`, sessionID, workerID, taskID, projectID)
    if err != nil { tx.Rollback(); return nil, fmt.Errorf("update task: %w", err) }
    affected, err := result.RowsAffected()
    if err != nil { tx.Rollback(); return nil, fmt.Errorf("check affected: %w", err) }
    if affected == 0 {
        tx.Rollback(); return s.getNextTaskWithRetry(ctx, projectID, sessionID, role, workerID, attempt+1)
    }
    tx.Commit()
    s.workerStore.UpdateCurrentTask(projectID, sessionID, workerID, taskID)
    // Worktree 创建在事务成功后执行。若创建失败，补偿回退 Task 状态为 pending，Worker current_task 清空
    return s.buildTaskContext(projectID, taskID)
}
```

## 隐式 Worker 注册

`get_next_task` 调用时若未注册 Worker，自动以 `worker_id="default"` 注册。

Worker 注册方式说明:
- MCP Tool: 隐式注册 (`get_next_task` 带新 worker_id 自动注册)
- REST API: 显式注册 (`POST /sessions/{sid}/workers`) — 供运维/调试使用

## Session 超时与任务释放

后台 goroutine 每 30s 扫描 `status='online' AND last_heartbeat < now - timeout` 的 session，标记 offline，按任务状态分级处理（遵循 [多客户端支持 PRD](../prd/multi-client.md) 超时恢复规则）:
- **执行者 Session**: `in_progress` 任务 → `pending`，清空 session/worker 绑定，Worktree 标记 stale
- **验证者 Session**: `verifying` 任务 → `submitted`，清空 Worker 的 `current_task_id`
- **ready_to_merge 及之后**: 不受影响
- 清空 Worker 状态，审计日志 + WebSocket 广播 `agent.offline`

## get_verification_task 原子认领

与 `get_next_task` 类似的原子认领机制，但针对 `submitted` 状态的任务：

```go
func (s *TaskService) GetVerificationTask(ctx context.Context, projectID string, verifierSessionID, verifierWorkerID string) (*Task, error) {
    return s.getVerificationTaskWithRetry(ctx, projectID, verifierSessionID, verifierWorkerID, 0)
}

func (s *TaskService) getVerificationTaskWithRetry(ctx context.Context, projectID string, verifierSessionID, verifierWorkerID string, attempt int) (*Task, error) {
    if attempt >= maxClaimRetry { return nil, ErrConcurrentConflict }
    tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
    if err != nil { return nil, fmt.Errorf("begin tx: %w", err) }
    // 1. 查找下一个 submitted 任务（不区分 role，验证者审查所有角色任务）
    var taskID string
    err = tx.QueryRowContext(ctx, `
        SELECT id FROM tasks
        WHERE project_id = ?
          AND status = 'submitted'
        ORDER BY created_at ASC
        LIMIT 1`, projectID).Scan(&taskID)
    if err == sql.ErrNoRows { tx.Rollback(); return nil, ErrNoAvailableTask }
    if err != nil { tx.Rollback(); return nil, fmt.Errorf("query submitted task: %w", err) }
    // 2. 原子更新：submitted → verifying
    result, err := tx.ExecContext(ctx, `UPDATE tasks SET status='verifying',
        updated_at=datetime('now')
        WHERE id=? AND project_id=? AND status='submitted'`, taskID, projectID)
    if err != nil { tx.Rollback(); return nil, fmt.Errorf("update to verifying: %w", err) }
    affected, err := result.RowsAffected()
    if err != nil { tx.Rollback(); return nil, fmt.Errorf("check affected: %w", err) }
    if affected == 0 {
        tx.Rollback(); return s.getVerificationTaskWithRetry(ctx, projectID, verifierSessionID, verifierWorkerID, attempt+1)
    }
    tx.Commit()
    // 3. 更新 Verifier Worker 的 current_task_id（不修改 assigned_session_id）
    s.workerStore.UpdateCurrentTask(projectID, verifierSessionID, verifierWorkerID, taskID)
    return s.buildVerificationContext(projectID, taskID)
}
```

**与 get_next_task 的关键区别：**
- 查询 `status = 'submitted'`（非 pending）
- 不按 role 过滤（验证者审查所有角色任务）
- 不修改 `assigned_session_id`/`assigned_worker_id`（保持指向原执行者）
- 通过 Verifier Worker 的 `current_task_id` 追踪验证归属
- 不创建 Worktree（Verifier 只读审查）

**submit_verification 流程：**

```go
func (s *TaskService) SubmitVerification(ctx context.Context, projectID, verifierSessionID, verifierWorkerID, taskID string, passed bool, notes string) error {
    tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
    if err != nil { return fmt.Errorf("begin tx: %w", err) }
    // 1. 校验 task 存在且状态为 verifying（防止并发双审）
    task, err := s.store.GetByID(projectID, taskID)
    if err != nil { tx.Rollback(); return ErrTaskNotFound }
    if task.Status != "verifying" { tx.Rollback(); return ErrTaskStateInvalid }

    if passed {
        // 2a. 通过：verifying → ready_to_merge，原子写入 verified_by/verified_at + activity_log
        result, err := tx.ExecContext(ctx, `UPDATE tasks SET status='ready_to_merge', verified_by=?, verified_at=datetime('now'),
            updated_at=datetime('now') WHERE id=? AND project_id=? AND status='verifying'`,
            verifierSessionID, taskID, projectID)
        if err != nil { tx.Rollback(); return fmt.Errorf("approve task: %w", err) }
        affected, _ := result.RowsAffected()
        if affected == 0 { tx.Rollback(); return ErrTaskStateInvalid } // 并发冲突
        tx.ExecContext(ctx, `INSERT INTO activity_log (project_id, session_id, task_id, action, detail) VALUES (?,?,?,?,?)`,
            projectID, verifierSessionID, taskID, "approved", fmt.Sprintf(`{"notes":%q}`, notes))
    } else {
        // 2b. 驳回：verifying → in_progress（rejected 瞬时事件，记录 activity_log 后直接回 in_progress）
        result, err := tx.ExecContext(ctx, `UPDATE tasks SET status='in_progress', updated_at=datetime('now')
            WHERE id=? AND project_id=? AND status='verifying'`, taskID, projectID)
        if err != nil { tx.Rollback(); return fmt.Errorf("reject task: %w", err) }
        affected, _ := result.RowsAffected()
        if affected == 0 { tx.Rollback(); return ErrTaskStateInvalid } // 并发冲突
        tx.ExecContext(ctx, `INSERT INTO activity_log (project_id, session_id, task_id, action, detail) VALUES (?,?,?,?,?)`,
            projectID, verifierSessionID, taskID, "rejected", fmt.Sprintf(`{"notes":%q}`, notes))
    }
    tx.Commit()
    // 3. 清空 Verifier Worker 的 current_task_id（事务外）
    s.workerStore.UpdateCurrentTask(projectID, verifierSessionID, verifierWorkerID, "")
    return nil
}
```

**并发保护要点：** `WHERE status='verifying'` 作为乐观锁，确保只有一个 Verifier 能成功提交结果。若 RowsAffected=0，说明其他 Verifier 已先处理，返回 `ErrTaskStateInvalid`。
