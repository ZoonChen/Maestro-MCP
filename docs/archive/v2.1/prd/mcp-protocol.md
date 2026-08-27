# 3.5 M5: MCP 协议层

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 功能需求 > MCP 协议层
> **相关文档:** [多客户端支持](multi-client.md) | [接口规范](../technical/api-spec.md)

---

## Session 建立

Agent 通过 MCP 连接建立 Session。Session 的关键参数通过以下方式传递：

| 参数 | stdio (Claude Code) | SSE (OpenClaw / 自定义) | REST API |
|---|---|---|---|
| `session_id` | 自动生成 (`sess_{8位hex}`) 或配置文件指定 | URL 参数 `?session_id=xxx` | POST body |
| `role` | 启动 Prompt (`start-coordinator` / `start-worker` / `start-verifier`) | URL 参数 `?role=xxx` | POST body |
| `capacity` | 默认 1（上限 5），通过配置文件覆盖 | URL 参数 `?capacity=N` | POST body |
| `client_type` | 自动识别为 `claude-code` | 自动识别为 `openclaw` / 手动指定 | POST body |
| `project_id` | cwd 自动匹配或 `--project` flag | URL 参数 `?project_id=xxx` | 路径 `:pid` |

**参数缺失时的默认行为：**
- `role` 缺失 → 拒绝连接，返回 `INVALID_PARAMETER`
- `session_id` 缺失 → 系统自动生成 `sess_{8位hex}`
- `capacity` 缺失 → 默认 1
- `project_id` 缺失 → 通过 cwd 自动匹配（匹配失败返回 `PROJECT_NOT_BOUND`）

**Session ID 全局唯一约束：** 同一 `session_id` 不可跨项目（如已绑定 project-a 的 session_id 不能用于 project-b）。重复 session_id 且同一项目 → 视为重连。

## Tools（工具）

### 项目管理

| Tool 名称 | 说明 |
|---|---|
| `register_project` | 注册新项目 |
| `list_projects` | 列出所有项目及状态（协调者跨项目视图） |

### 协调者工具

| Tool 名称 | 说明 |
|---|---|
| `create_feature` | 创建 Feature，含标题、描述、可选的关联文档 URL 列表（reference_urls）。项目由连接绑定推断 |
| `split_task` | 拆分子任务（含角色、边界、禁止模式、依赖、测试要求、优先级） |
| `update_task` | 修改任务参数（修复拆分错误、解锁阻塞任务） |
| `cancel_task` | 取消任务，释放 Worker |
| `resolve_blocker` | 解除 blocked 状态。默认回到 `pending` 等待重新认领。若 `reassign=true` 且原 Worker 仍在线，则回到 `in_progress` 并保持原 session/worker/worktree 绑定。`resolution` 为结构化字符串，描述解除方式和注意事项 |

**resolve_blocker 结果语义：**

| 条件 | Task 结果 | Worker/Worktree |
|---|---|---|
| 默认（不传 reassign） | `blocked → pending` | 清空 session/worker 绑定，Worktree 标记 stale |
| `reassign=true` + 原 session 在线 + Worker 空闲 + Worktree 可用 | `blocked → in_progress` | 保留原 session/worker/worktree 绑定 |
| `reassign=true` + 原 session 离线 | `blocked → pending` | 同默认行为 |
| `reassign=true` + Worker 已有其他任务 | `blocked → pending` | 同默认行为 |
| `reassign=true` + Worktree 不可用 | `blocked → pending` | 标记旧 Worktree 为 abandoned，需重新领取时创建新 Worktree |

| `resolve_merge_conflict` | 处理 merge_conflicted 任务。action 可选: `reopen`（回到 in_progress 保持原 Worker/Worktree）、`cancel`（取消任务）、`followup`（创建后续任务） |

**resolve_merge_conflict 结果语义：**

| action | 条件 | Task 结果 | Worker/Worktree |
|---|---|---|---|
| `reopen` | 原 session 在线 + Worker 空闲 + Worktree 可用 | `merge_conflicted → in_progress` | 保留原 session/worker/worktree，通知 Worker 继续修改 |
| `reopen` | 原 session 离线 | `merge_conflicted → pending` | 清空 session/worker 绑定，Worktree 标记 stale，等待重新认领 |
| `reopen` | Worker 已有其他任务 | `merge_conflicted → pending` | 同上 |
| `reopen` | Worktree 不可用 | `merge_conflicted → pending` | 旧 Worktree 标记 abandoned，等待重新认领时创建新 Worktree |
| `cancel` | — | `merge_conflicted → cancelled` | Worker 回 idle，Worktree 标记 abandoned |
| `followup` | — | 当前 Task 保持 `merge_conflicted`（标记为不可继续，等待人工关闭） | 原 Worktree 标记 stale，自动创建新 Task（`parent_task_id` 指向当前 Task，`relation_type=conflict_resolution`，继承原 Task 的 `feature_id`、`role`、`allowed_directories`、`forbidden_patterns`、`test_requirements`） |

#### split_task 参数校验规则

`split_task` 创建任务时执行以下校验，不满足则拒绝并返回对应错误码：

| 校验项 | 规则 | 失败错误码 |
|---|---|---|
| `feature_id` 存在性 | 必须属于当前项目 | `FEATURE_NOT_FOUND` |
| `allowed_directories` 非空 | 至少指定一个目录 | `INVALID_PARAMETER` |
| `allowed_directories` 交集 | 同一 Feature 下不同 Task 的 allowed_directories 允许有交集（由 Worktree 保证物理隔离） | - |
| `dependencies.task_id` 存在性 | 引用的 task_id 必须属于当前项目。跨项目依赖当前不支持（Phase 5 规划） | `TASK_NOT_FOUND` |
| **循环依赖检测** | 创建任务后依赖图不允许出现环路。检测算法：以新 Task 为起点，沿依赖链深度遍历，若回到新 Task 则存在环路 | `CIRCULAR_DEPENDENCY` |
| `role` 合法性 | 必须为 `backend` / `frontend` / `devops` / `verifier` 之一 | `INVALID_PARAMETER` |
| `priority` 合法性 | 如果非空，必须为 `low` / `normal` / `high` / `urgent` 之一（默认 `normal`） | `INVALID_PARAMETER` |
| `test_requirements.command` | 如果非空，不允许包含换行符 `\n`，不允许 `&&`/`||`/`;` 多命令链 | `INVALID_PARAMETER` |
| `test_requirements.coverage_format` | 如果非空，必须为 `go-cover` / `cobertura` / `jacoco` / `istanbul` 之一 | `INVALID_PARAMETER` |

**跨字段校验规则（首版保证）：**

| 校验项 | 规则 |
|---|---|
| `allowed_directories` 不能为空 | 至少指定一个目录 |
| `allowed_directories` 路径安全 | 不允许包含 `..`，必须为相对路径（相对于 project root） |
| `forbidden_patterns` 与 `allowed_directories` | 不校验冲突（允许同时指定） |
| `dependencies` 不能引用自身 | `task_id` 不能是当前正在创建的任务 |
| `dependencies` 循环检测 | 创建后依赖图不允许出现环路（深度遍历检测） |
| `test_requirements.min_coverage` | 如果非空，必须在 `0..100` 范围内 |
| `required_apis` 格式 | 如果非空，每项必须包含 `method`（GET/POST/PUT/DELETE/PATCH）和 `path`（以 `/` 开头） |
| `role=verifier` 的限制 | verifier 角色的 Task 不允许设置 `required_apis`（Verifier 不需要 API 契约上下文） |

**任务变更规则:**

| 任务状态 | update_task | cancel_task | 备注 |
|---|---|---|---|
| pending | 允许修改全部字段（title, description, summary, allowed_directories, forbidden_patterns, required_apis, test_requirements） | 允许 | 正常操作，cancelled 后只读 |
| in_progress | 仅允许修改 description/summary | 允许，需通知当前 Worker | 修改 allowed_directories/test_requirements 需回退到 pending 再重新认领；cancel 需释放 Worker 和 Worktree |
| submitted 及之后 | 不允许 | 不允许 | 由验证流程控制 |
| cancelled | 不允许（返回 `TASK_ALREADY_CANCELLED`） | 不允许（返回 `TASK_ALREADY_CANCELLED`） | 已取消，只读状态。`TASK_ALREADY_CANCELLED` 是 `TASK_STATE_INVALID` 的特化错误码，专门用于 cancelled 状态的任何操作尝试 |
| merge_conflicted | 不允许（使用 resolve_merge_conflict 代替） | 允许（resolve_merge_conflict 的 cancel action） | 通过 resolve_merge_conflict 处理 |

### 执行者工具

| Tool 名称 | 说明 |
|---|---|
| `get_next_task` | 领取下一个可执行任务，返回降噪上下文和 Worktree 路径。若 Worker 未注册则自动隐式注册。**角色校验：** 服务端校验 `role` 参数必须与 Session 的 `role` 一致（协调者 Session 调用返回 `TASK_STATE_INVALID`；后端 Session 不可领取前端任务）。`role` 参数不可覆盖 Session 角色 |
| `submit_task_result` | 声明任务完成。服务端自动取证（git diff + 执行测试 + 读取覆盖率文件）。Agent 提交时可附带结构化摘要： |
| `report_blocker` | 上报阻塞，通知协调者 |
| `claim_batch` | 批量认领任务。Session 下的空闲 Worker（含隐式注册的 `default`）按顺序领取，若 Worker 不足则仅认领等于空闲 Worker 数量的任务。每个认领的 Task 走与 `get_next_task` 相同的分配逻辑（设置 assigned_session_id、assigned_worker_id、assigned_at，创建 Worktree） |
| `release_worker` | 主动释放子 Worker，其未完成任务回退 pending |

**claim_batch 失败回退规则:**
- claim_batch 采用"先创建 Worktree，再执行数据库事务"的策略
- 如果 Worktree 创建失败：该 Task 不执行数据库更新，状态保持 `pending`，不会出现"数据库 in_progress 但无 Worktree"的不一致状态
- 返回结果中 `failed` 列表包含每个失败 Task 的 `{ task_id, error }`
- 已成功认领的任务不受影响，不需要回退

**结构化摘要格式（submit_task_result）：**

Agent 提交时可附带结构化摘要：
```json
{
  "summary": "完成订单查询 API",
  "outputs": [
    { "type": "api", "name": "GET /api/v1/orders" },
    { "type": "file", "path": "src/api/orders/controller.go" }
  ],
  "notes": ["依赖用户鉴权中间件"]
}
```
后续任务通过 `dependency_summaries` 获取前置任务的结构化输出，用于更精准的上下文降噪。

**reject 后通知机制:**

Verifier 调用 `submit_verification(task_id, passed=false, notes)` 后：
1. Task 立即回到 `in_progress`
2. 保持原 session_id / worker_id 绑定
3. 保留原 worktree
4. Maestro 通过 MCP 发送通知给原 Worker（如果 session 在线）
5. 如果原 session 已离线：Task 转 `pending`，worktree 标记 `stale`

### 验证者工具

| Tool 名称 | 说明 |
|---|---|
| `get_verification_task` | 领取 submitted 状态的任务 |
| `submit_verification` | 提交验证结果（通过/不通过 + 备注） |
| `merge_task` | 对 ready_to_merge 状态的任务执行 git merge。Maestro 服务端将 Worktree 分支合并到主分支。成功后 Task 直接进入 `done`（merge 成功为事件，不是稳定状态）；冲突则进入 `merge_conflicted` |

#### get_verification_task 返回结构

验证者领取任务时获得以下信息：

```json
{
  "task": {
    "id": "T-00042",
    "title": "实现订单查询 API",
    "description": "...",
    "role": "backend",
    "allowed_directories": ["src/api/orders/"],
    "forbidden_patterns": ["*.md"]
  },
  "result": {
    "base_commit": "abc1234",
    "changed_files": ["src/api/orders/controller.go", "src/api/orders/model.go"],
    "test_command": "go test ./src/api/orders/...",
    "test_output": "ok  github.com/example/orders  0.45s  coverage: 85.2%",
    "coverage": 85.2,
    "submitted_at": "2026-04-17T14:32:00Z"
  },
  "review": {
    "worktree_path": "/path/to/project/.maestro/worktrees/T-00042"
  }
}
```

Verifier 通过 `worktree_path` 可查看代码变更全貌（`git diff base_commit`），`result` 提供服务端取证的摘要。

#### Merge 流程

1. Verifier 调用 `submit_verification(passed=true)` → Task 进入 `ready_to_merge`
2. Verifier 调用 `merge_task(task_id)` → Maestro 在**主仓库目录**中执行 `git merge task/{task_id}`
3. 合并成功 → Task 直接进入 `done`（merge 成功作为事件记录在 Activity Log），Worktree 标记 merged 等待 GC
4. 合并冲突 → Task 进入 `merge_conflicted`，通知协调者处理

**目标分支:** 使用项目主仓库的当前分支（默认 main/master，可通过项目配置 `merge_target_branch` 覆盖）。

## Resources（资源）

| URI | 返回内容 |
|---|---|
| `project://list` | 所有已注册项目的列表及状态概览 |
| `project://{project_id}` | 单项目详情：配置、进度统计、Agent 列表 |
| `board://active` | 当前项目看板摘要（由连接绑定的项目决定） |
| `board://all` | 跨项目全局看板（协调者用） |
| `task://{task_id}/context` | 任务纯净上下文（动态组装，项目隐含在任务中） |
| `feature://{feature_id}/summary` | Feature 级进度：子任务列表及各自状态 |

## Prompts（提示词模板）

| Prompt 名称 | 说明 |
|---|---|
| `start-coordinator` | 注入协调者角色：引导需求分析、任务拆分。定期检查看板中的阻塞队列，主动修复错误拆分或取消不可能完成的任务 |
| `start-worker` | 注入执行者角色：绑定角色，专注执行，不发散。只在 Worktree 目录内操作，完成后触发服务端验证 |
| `start-verifier` | 注入验证者角色：领取已提交任务，检查代码质量，决定合并或打回 |
