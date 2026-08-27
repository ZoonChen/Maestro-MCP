# 4. 接口规范

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 接口规范
> **相关文档:** [MCP 协议层 PRD](../prd/mcp-protocol.md) | [Web 看板 PRD](../prd/web-dashboard.md) | [数据模型](data-model.md) | [任务管理 PRD](../prd/task-management.md)

---

## 4.1 REST API 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| **全局端点** |||
| GET | `/api/v1/projects` | 列出所有项目 |
| POST | `/api/v1/projects` | 注册新项目 |
| GET | `/api/v1/projects/:id` | 项目详情 |
| PATCH | `/api/v1/projects/:id` | 更新项目 |
| POST | `/api/v1/projects/:id/archive` | 归档项目 |
| POST | `/api/v1/projects/:id/restore` | 恢复项目 |
| GET | `/api/v1/overview` | 跨项目总览 |
| **Feature (项目级)** |||
| POST | `/api/v1/projects/:pid/features` | 创建 Feature |
| GET | `/api/v1/projects/:pid/features` | 列出 Feature |
| GET | `/api/v1/projects/:pid/features/:id` | Feature 详情 |
| PATCH | `/api/v1/projects/:pid/features/:id` | 更新 Feature |
| **Task (项目级)** |||
| POST | `/api/v1/projects/:pid/tasks` | 创建 Task |
| GET | `/api/v1/projects/:pid/tasks` | 列出 Task (支持 `?status=&role=&feature_id=`) |
| GET | `/api/v1/projects/:pid/tasks/next?role=backend&worker_id=default` | 获取下一个可执行任务（`worker_id` 可选，用于隐式 Worker 注册） |
| GET | `/api/v1/projects/:pid/tasks/:id` | Task 详情 |
| PATCH | `/api/v1/projects/:pid/tasks/:id` | 更新 Task（对应 MCP `update_task`） |
| POST | `/api/v1/projects/:pid/tasks/:id/claim` | 认领任务 |
| POST | `/api/v1/projects/:pid/tasks/:id/submit` | 提交结果 |
| POST | `/api/v1/projects/:pid/tasks/:id/block` | 上报阻塞 |
| POST | `/api/v1/projects/:pid/tasks/:id/resolve` | 解除阻塞 |
| POST | `/api/v1/projects/:pid/tasks/:id/verify` | 提交验证 |
| POST | `/api/v1/projects/:pid/tasks/:id/merge` | 执行 merge |
| POST | `/api/v1/projects/:pid/tasks/:id/resolve-merge-conflict` | 解决 merge 冲突 |
| POST | `/api/v1/projects/:pid/tasks/:id/cancel` | 取消任务 |
| **Session (项目级)** |||
| POST | `/api/v1/projects/:pid/sessions` | 注册 Session |
| PUT | `/api/v1/projects/:pid/sessions/:sid/heartbeat` | 心跳 |
| GET | `/api/v1/projects/:pid/sessions` | 列出 Session |
| GET | `/api/v1/projects/:pid/sessions/:sid` | Session 详情 |
| DELETE | `/api/v1/projects/:pid/sessions/:sid` | 断开 Session |
| **Worker (Session 级)** |||
| POST | `/api/v1/projects/:pid/sessions/:sid/workers` | 注册 Worker |
| GET | `/api/v1/projects/:pid/sessions/:sid/workers` | 列出 Workers |
| DELETE | `/api/v1/projects/:pid/sessions/:sid/workers/:wid` | 释放 Worker |
| **Board (项目级)** |||
| GET | `/api/v1/projects/:pid/board` | 看板数据聚合 |
| GET | `/api/v1/projects/:pid/board/activity` | 活动日志 (`?limit=&since=`) |
| **WebSocket** |||
| WS | `/ws` | 实时事件推送 (`?project_id=` 过滤) |

## 4.2 MCP Tools

| Tool 名称 | 参数 | 说明 |
|---|---|---|
| **项目管理** |||
| `register_project` | `{ name, workspace_path, description?, config? }` | 注册新项目 |
| `list_projects` | `{ include_archived? }` | 列出所有项目 |
| **协调者** |||
| `create_feature` | `{ title, description, reference_urls? }` | 创建 Feature |
| `split_task` | `{ feature_id, role, title, description, allowed_directories, forbidden_patterns?, required_apis?, dependencies?, test_requirements?, priority? }` | 拆分子任务 |
| `update_task` | `{ task_id, title?, description?, summary?, allowed_directories?, forbidden_patterns?, required_apis?, test_requirements? }` | 修改任务参数（`pending` 状态可修改全部字段；`in_progress` 仅可修改 `description`/`summary`；其余状态不可修改） |
| `cancel_task` | `{ task_id, reason }` | 取消任务 |
| `resolve_blocker` | `{ task_id, resolution, reassign? }` | 解除 blocked 状态 |
| `resolve_merge_conflict` | `{ task_id, action, reason? }` | 处理 merge_conflicted (action: reopen/cancel/followup) |
| **执行者** |||
| `get_next_task` | `{ role, worker_id? }` | 领取下一个任务（含降噪上下文 + Worktree 路径）。`role` 必须匹配 Session 角色，不匹配返回 `TASK_STATE_INVALID` |
| `submit_task_result` | `{ task_id, summary? }` | 声明完成（服务端自动取证） |
| `report_blocker` | `{ task_id, reason }` | 上报阻塞 |
| `claim_batch` | `{ role, count }` | 批量认领任务 |
| `release_worker` | `{ worker_id }` | 释放子 Worker |

**claim_batch 语义：**
- 批量认领多个任务的便利接口。Session 下的空闲 Worker（含隐式注册的 `default`）按顺序领取，每个认领的 Task 走与 `get_next_task` 相同的分配逻辑（设置 assigned_session_id、assigned_worker_id、assigned_at，创建 Worktree）
- **执行顺序:** 先创建 Worktree，再执行数据库事务（与单次 `get_next_task` 的"先事务后 Worktree"顺序不同，以便在 Worktree 创建失败时避免数据库回滚）
- 部分成功语义: 返回 `{ claimed: [...], failed: [...] }`
- 不承诺全有或全无事务性 (worktree 创建非数据库事务)
- 如果某个 worktree 创建失败, 已认领的任务保留, 失败的任务留在 pending
- 如果 Worktree 创建成功但数据库事务失败（并发冲突），已创建的 Worktree 标记 abandoned 等待 GC

| **验证者** |||
| `get_verification_task` | `{}` | 领取 submitted 状态任务 |
| `submit_verification` | `{ task_id, passed, notes }` | 提交验证结果 |
| `merge_task` | `{ task_id }` | 对 ready_to_merge 任务执行 git merge |

## 4.3 MCP Resources

| URI | 返回内容 |
|---|---|
| `project://list` | 所有已注册项目列表及状态概览 |
| `project://{project_id}` | 单项目详情：配置、进度统计、Agent 列表 |
| `board://active` | 当前项目看板摘要 |
| `board://all` | 跨项目全局看板 |
| `task://{task_id}/context` | 任务纯净上下文（动态组装） |
| `feature://{feature_id}/summary` | Feature 级进度 |

## 4.4 MCP Prompts

| Prompt 名称 | 说明 |
|---|---|
| `start-coordinator` | 注入协调者角色：需求分析、任务拆分、定期检查 Blocked 队列 |
| `start-worker` | 注入执行者角色：绑定 role，专注执行，Worktree 内操作 |
| `start-verifier` | 注入验证者角色：领取 submitted 任务，检查代码质量，决定 merge 或打回 |

## 4.5 MCP 传输模式

| 传输模式 | 端口/方式 | 适用场景 | 配置示例 |
|---|---|---|---|
| **stdio** | 标准输入输出 | Claude Code | `"command": "maestro", "args": ["mcp", "--transport", "stdio"]` |
| **SSE** | `:3000/sse` | OpenClaw / 远程 | `"url": "http://localhost:3000/sse"` |

## 4.6 WebSocket 事件类型

```typescript
type WSEvent =
  | { type: "project.registered"; project_id: string; project: Project }
  | { type: "project.archived"; project_id: string }
  | { type: "task.created"; project_id: string; task: Task }
  | { type: "task.claimed"; project_id: string; task_id: string; session_id: string; worker_id: string }
  | { type: "task.submitted"; project_id: string; task_id: string; result: TaskResult }
  | { type: "task.blocked"; project_id: string; task_id: string; reason: string }
  | { type: "task.unblocked"; project_id: string; task_id: string; resolution: string }  // resolve_blocker 解除阻塞
  | { type: "task.verifying"; project_id: string; task_id: string; session_id: string; worker_id: string }    // Verifier 领取 submitted 任务（session_id/worker_id 为验证者，Task.assigned_session_id 不变）
  | { type: "task.rejected"; project_id: string; task_id: string; notes: string }
  | { type: "task.ready_to_merge"; project_id: string; task_id: string }    // 对应 activity_log action = "approved"
  | { type: "task.merge_requested"; project_id: string; task_id: string; session_id: string; worker_id: string }
  | { type: "task.merge_conflicted"; project_id: string; task_id: string; conflicts: string[] }
  | { type: "task.reopened"; project_id: string; task_id: string; reason: string }       // merge_conflicted → in_progress (reopen)
  | { type: "task.merged"; project_id: string; task_id: string; commit: string }    // 系统触发，Web 看板展示为 "system merged"
  | { type: "task.done"; project_id: string; task_id: string }    // 系统触发，Web 看板展示为 "system done"
  | { type: "task.cancelled"; project_id: string; task_id: string; reason: string }
  | { type: "task.followup_created"; project_id: string; task_id: string; new_task_id: string }
  | { type: "agent.online"; project_id: string; session_id: string; role: string }
  | { type: "agent.offline"; project_id: string; session_id: string }
```

## 4.7 错误码规范

MCP Tool、REST API、WebSocket 使用统一错误模型:

```go
type MaestroError struct {
    Code    string `json:"code"`              // 机器可读
    Message string `json:"message"`           // 人类可读
    Detail  any    `json:"detail,omitempty"`  // 附加上下文
}
```

**可重试语义:** 错误码表中 `可重试` 列标识该错误是否适合 Agent 自动重试：
- `Y`: 可直接重试（如超时、限流）
- `N`: 不可重试，需修改请求或人工介入
- `Conditional`: 修复根本原因后可重试（如测试失败 → 修复代码后重新 submit）

| 错误码 | HTTP Status | 含义 | 可重试 |
|---|---|---|
| `PROJECT_NOT_FOUND` | 404 | 项目不存在 | N |
| `PROJECT_ARCHIVED` | 403 | 项目已归档 | N |
| `PROJECT_NOT_BOUND` | 400 | Agent 未绑定项目 | N |
| `PROJECT_AMBIGUOUS` | 400 | cwd 匹配到多个项目 | N |
| `FEATURE_NOT_FOUND` | 404 | Feature 不存在 | N |
| `TASK_NOT_FOUND` | 404 | Task 不存在（含不属于当前项目） | N |
| `TASK_NOT_OWNED` | 403 | Task 不属于当前 Session | N |
| `TASK_STATE_INVALID` | 409 | 当前状态不允许此操作 | N |
| `TASK_DEPENDENCY_UNMET` | 412 | 前置依赖未满足 | Y |
| `SESSION_NOT_FOUND` | 404 | Session 不存在 | N |
| `SESSION_CAPACITY_FULL` | 429 | Session Worker 数已达上限 | Y |
| `WORKTREE_CREATE_FAILED` | 500 | Worktree 创建失败（Git 工作区不干净/磁盘/权限） | Y |
| `WORKTREE_CLEAN_FAILED` | 500 | Worktree 清理失败 | Y |
| `TEST_EXECUTION_FAILED` | 422 | 测试执行失败 | Conditional |
| `TEST_EXECUTION_TIMEOUT` | 408 | 测试执行超时 | Y |
| `COVERAGE_BELOW_THRESHOLD` | 422 | 覆盖率低于阈值 | Conditional |
| `COVERAGE_FILE_NOT_FOUND` | 422 | 覆盖率文件不存在 | Conditional |
| `BOUNDARY_VIOLATION` | 422 | 文件变更越界或命中禁止模式（sub_type: out_of_bounds / forbidden_pattern） | Conditional |
| `CROSS_PROJECT_ACCESS_DENIED` | 403 | 跨项目访问被拒绝 | N |
| `VALIDATION_REJECTED` | 422 | Verifier 人工驳回任务（记录在 WS 事件，不作为 HTTP 响应返回。区别于 validation_runs.result=rejected 的服务端自动校验拒绝） | Conditional |
| `MERGE_CONFLICT` | 409 | 合并冲突 | N |
| `TASK_ALREADY_CANCELLED` | 409 | 任务已取消 | N |
| `INVALID_PARAMETER` | 400 | 参数校验失败 | N |
| `CIRCULAR_DEPENDENCY` | 422 | 循环依赖 | N |
| `CONNECTION_LIMIT_REACHED` | 429 | MCP 连接数达到上限 | Y |
| `NO_AVAILABLE_TASK` | 404 | 当前无符合条件的可执行任务（`get_next_task` / `get_verification_task` 无匹配） | Y |
| `CONCURRENT_CONFLICT` | 409 | 并发冲突，乐观锁重试耗尽（SQLite SERIALIZABLE 事务竞争） | Y |
