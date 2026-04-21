# 3.2 M2: Feature & Task 管理

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 功能需求 > Feature & Task 管理
> **相关文档:** [多项目管理](project-management.md) | [边界控制与验证](validation.md) | [数据模型](../technical/data-model.md)

---

## Feature 管理

#### Feature 状态

| 状态 | 含义 | 触发条件 |
|---|---|---|
| `planning` | 规划中 | 创建时的默认状态，协调者正在拆分任务 |
| `active` | 执行中 | 第一个子 Task 被创建后自动进入 |
| `completed` | 已完成 | 所有非 cancelled 子 Task 均为 done 时自动进入 |
| `closed` | 已关闭 | 协调者手动关闭（可强制关闭仍有 pending 任务的 Feature） |

Feature 状态流转: `planning → active → completed`，以及任意状态可手动 `→ closed`。`closed` 为手工终态，当前版本不支持 reopen。如误关闭，建议创建新 Feature 并在描述中引用原 Feature。

| 操作 | 说明 |
|---|---|
| 创建 Feature | 协调者录入史诗级需求，含标题、描述、关联文档 URL（reference_urls） |
| 查看 Feature | 列出所有 Feature 及其进度百分比 |
| 更新 Feature | 修改需求描述或状态 |
| 关闭 Feature | 所有非 cancelled 子 Task 完成后自动进入 `completed`；协调者也可随时手动关闭为 `closed` |

#### Feature 进度计算

Feature 进度百分比 = `done 任务数 / (总任务数 - cancelled 任务数) × 100%`

各状态在进度计算中的归类：
- **计为完成**: `done`
- **从总数剔除**: `cancelled`
- **计为未完成**: `pending`, `in_progress`, `submitted`, `verifying`, `ready_to_merge`, `merge_conflicted`, `blocked`

**边界条件:**
- 总任务数为 0（空 Feature）: 进度显示为 N/A，不触发自动关闭，协调者可手动关闭
- 所有任务均为 cancelled: 进度显示为 N/A，Feature 不自动关闭（保持在 `active` 状态，协调者需手动 `closed` 或创建新任务继续推进）
- 看板中同时展示完成率和取消率两个指标

**Feature 自动关闭规则:** 所有非 cancelled 子 Task 均为 `done` 时，Feature 自动进入 `completed`（不是 `closed`）。`closed` 仅由协调者手动触发。

**followup 场景的 Feature 完成判定:** `resolve_merge_conflict(action=followup)` 会使原 Task 保持在 `merge_conflicted` 状态。由于 `merge_conflicted` 计为"未完成"，原 Task 会阻止 Feature 自动完成。协调者应在 followup 子任务完成后，手动将原 `merge_conflicted` 任务 `cancel`（通过 `resolve_merge_conflict(action=cancel)`），使其从进度计算中剔除，从而解除 Feature 完成阻塞。

## Task 状态机

```
                    ┌──────────┐
                    │ pending  │ ← 初始状态
                    └──┬───┬───┘
                       │   │ cancel_task()
          get_next_task()  │
                    ┌──▼───▼───┐
              ┌─────│in_progress│
              │     └──┬───┬───┘
              │        │   │ cancel_task()
              │        │ submit_task_result() (服务端验证通过)
              │     ┌──▼──────────┐
              │     │   submitted   │
              │     └──┬──────────┘
              │        │ verifier 领取
              │     ┌──▼──────────┐     验证不通过
              │     │   verifying   │──────────────┐
              │     └──┬──────────┘              │
              │        │ 验证通过                  ▼
              │     ┌──▼──────────┐     ┌──────────────┐
              │     │ready_to_merge │     │   rejected    │
              │     └──┬──────────┘     │ → in_progress│
              │        │                 │ (修改后重新提交)
              │     ┌──▼──────────┐     └──────────────┘
              │     │ merge 执行    │
              │     └──┬────────┬──┘
              │   成功 │        │ 冲突
              │  ┌─────▼──┐  ┌──▼────────────┐
              │  │  done  │  │merge_conflicted│
              │  └────────┘  └──┬─────────────┘
              │                  │ resolve_merge_conflict:
              │                  │  reopen → in_progress
              │                  │  cancel → cancelled
              │                  │  followup → 新 Task
              │                 │
              │ report_blocker()│
              │     ┌───────────▼──┐
              └────►│   blocked    │ → resolve → pending（默认）或 in_progress（reassign=true）
                    └──────┬───────┘
                           │ cancel_task()
                           └──────────────► cancelled

         ┌─────────────┐
         │  cancelled  │ ← pending/in_progress/blocked 通过 cancel_task()，merge_conflicted 通过 resolve_merge_conflict(action=cancel) 进入
         └─────────────┘   只读终态
```

**状态定义表：**

| 状态 | 含义 | 可由谁触发 | 允许的操作 |
|---|---|---|---|
| pending | 等待认领 | 系统 | get_next_task → in_progress（同时设置 `assigned_at` 为首次认领时间） |
| in_progress | 执行中 | Worker | submit → submitted, report_blocker → blocked |
| submitted | 服务端验证通过，等待 Verifier | 服务端自动 | get_verification_task → verifying |
| verifying | Verifier 审查中 | Verifier | approve → ready_to_merge, reject → in_progress |
| rejected | **瞬时伪状态**（非稳定状态，立即回到 in_progress） | Verifier | reject 后保持原 session/worker/worktree |
| cancelled | 已取消 | 协调者（cancel_task）/ 协调者（resolve_merge_conflict action=cancel） | 只读。cancel 后自动释放：Worker 回 idle，Worktree 标记 abandoned 等待 GC 清理。pending/in_progress/blocked 通过 cancel_task 进入；merge_conflicted 通过 resolve_merge_conflict(action=cancel) 进入 |
| ready_to_merge | 验证通过，待合并 | Verifier | merge → done/merge_conflicted |
| merge_conflicted | 合并冲突 | 系统自动 | resolve_merge_conflict: reopen → in_progress, cancel → cancelled, followup → 新 Task |
| done | 完成 | 系统 | 只读 |
| blocked | 阻塞 | Worker | resolve → pending（默认）, resolve(reassign=true) → in_progress |

#### 任务归属与资源绑定语义

`assigned_session_id` / `assigned_worker_id` 始终指向**执行者**（拥有 Worktree 的 Worker），不随验证流程变更：

| Task 状态 | assigned_session_id | assigned_worker_id | Worktree 状态 |
|---|---|---|---|
| pending | NULL | NULL | 无 |
| in_progress | 执行者 Session | 执行者 Worker | allocated / active |
| submitted | 保持原执行者 | 保持原执行者 | submitted |
| verifying | 保持原执行者（**不切换给 Verifier**） | 保持原执行者 | submitted |
| ready_to_merge | 保持原执行者 | 保持原执行者 | submitted |
| done | 清空 | 清空 | merged（待 GC） |
| blocked | 保持原执行者 | 保持原执行者 | active / stale |
| cancelled | 清空 | 清空 | abandoned（待 GC；注：从 `pending` 直接 cancel 的任务无 Worktree） |
| merge_conflicted | 保持原执行者 | 保持原执行者 | active（reopen 时）/ stale（followup 后原 Worktree 不再活跃） |

**崩溃恢复特殊行为：** 上表描述正常运行时的归属语义。进程崩溃重启时，`blocked` 和 `merge_conflicted` 任务保持状态但清空 `assigned_session_id`/`assigned_worker_id`（原 Session 已不存在），Worktree 标记 stale。详见 [恢复与灾难处理](../technical/recovery.md)。

**Verifier 归属追踪：** Verifier 领取任务时，通过 Verifier Worker 的 `current_task_id` 字段追踪验证归属，不修改 Task 的 `assigned_session_id`。当 Verifier 批准任务（`verifying → ready_to_merge`）时，设置 `verified_by`（Verifier Session ID）和 `verified_at`（确认时间戳）。这确保了：
- Reject 后可直接回到原执行者（无需额外查询恢复归属）
- Verifier Session 超时时，通过 Workers 表查找其正在验证的任务，回退至 `submitted`（见 [多客户端支持](multi-client.md)）
- `verified_by` / `verified_at` 仅在验证通过时写入，用于验证→merge 耗时统计

**TaskResult 与验证历史：** `task_results` 存储当前最新提交结果（UNIQUE 约束，每次提交覆写），供 Verifier 审查。同时，每次 `submit_task_result` 在 `validation_runs` 表追加一条记录（append-only），保留完整的提交-验证历史（含 attempt 编号、各项校验结果、耗时、日志路径）。多次 reject-resubmit 的完整链路可追溯。

#### 数据一致性原则

Task 提交、验证、状态流转、merge 等关键链路遵循以下一致性约束：

- **主状态原子性：** Task 状态变更与关键留痕（activity_log、validation_runs 写入）必须原子一致。不允许出现"Task 已 done 但 validation_runs 无对应记录"的矛盾状态
- **允许延迟补写：** 非关键日志（如测试日志文件落盘、audit_log 细节）允许异步写入，不阻塞主状态流转
- **中断恢复：** 若关键链路中途失败（如 merge 时服务崩溃），Task 停留在前一个确定状态（如 `ready_to_merge`），不进入不确定的中间状态。恢复后从断点重试
- **配置快照：** Task 创建时固化的字段（`allowed_directories`、`test_requirements`）在其生命周期内不受 Project 级配置变更影响。验证始终基于 Task 自身的字段执行。未在 Task 级指定的配置项（如 `default_test_command`）才回退到 Project → Global 优先级链

#### Reject 后归属规则

`rejected` 是瞬时伪状态（非稳定状态，立即回到 in_progress）——Verifier reject 后：

1. Task 立即回到 `in_progress`
2. 保持 `assigned_session_id` / `assigned_worker_id` 不变
3. 保留原 worktree
4. 给当前 worker 推送 `task.rejected` 事件
5. 如果原 session 已离线：转 `pending`，worktree 标记 `stale`

**关键澄清：**

- **"done" 的定义**: merge 成功后 Task 直接进入 `done`（`merged` 是事件而非稳定状态，表示代码已成功合入目标分支）。`done` 的完整含义：代码已合并 + Worktree 已标记清理 + 结果已归档。首版不做 merge 后再验证
- **谁执行 merge**: 由 Verifier 触发，Maestro 服务端执行 `git merge`
- **冲突处理**: 标记 merge_conflicted，通知协调者，由协调者决定解决方式
- **cancelled 任务的处理**: cancelled 任务不计入 Feature 进度计算的总任务数。已取消的任务为只读状态，不展示在看板默认视图中。
- **cancelled 的资源释放**: 
  - pending 状态 cancel: 无需释放资源（无 Worker/Worktree 绑定）
  - in_progress 状态 cancel: 通知当前 Worker（`task.cancelled` 事件），Worker 状态回 idle，Worktree 标记 abandoned 等待 GC
  - blocked 状态也可以被 cancel（协调者决定放弃阻塞任务）
- **test_requirements 缺失时的行为**: 
  - `test_requirements` 为非必填字段。当 Task 未配置、Project 也无默认、全局也无默认时，`submit_task_result` 跳过测试执行和覆盖率校验步骤，直接进入 `submitted` 状态
  - `min_coverage` 默认值为 0（即不强制覆盖率）。显式设置为 0 等同于跳过覆盖率校验
  - context-filtering 返回结构中，`test_requirements` 为空时返回 `{}`
- **merge_conflicted 处理**: 仅协调者可处理，通过 `resolve_merge_conflict` 工具执行。action 可选：
  - `reopen`: 回到 `in_progress`，保持原 session/worker/worktree，Agent 继续修改
  - `cancel`: 取消任务，释放资源
  - `followup`: 保持当前任务状态不变，自动创建新的冲突解决任务（`parent_task_id` 指向当前任务，`relation_type=conflict_resolution`——action 名与 relation_type 名不同：followup 是操作语义，conflict_resolution 是关系语义，用于区分通用后续补充与冲突解决方案）。新任务继承原任务的 `feature_id`、`role`、`allowed_directories`、`forbidden_patterns`、`test_requirements`
  - 完整结果语义见 [MCP 协议层](mcp-protocol.md) resolve_merge_conflict 结果语义表

## Task 字段定义

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `feature_id` | string | 是 | 所属 Feature |
| `role` | enum | 是 | `backend` / `frontend` / `devops` / `verifier` |
| `title` | string | 是 | 任务标题 |
| `description` | string | 是 | 详细描述 |
| `allowed_directories` | string[] | 是 | 允许修改的目录列表 |
| `forbidden_patterns` | string[] | 否 | 禁止修改的文件模式 |
| `required_apis` | object[] | 否 | 依赖的 API 契约引用列表（每项含 method + path，用于查询 api_contracts 表获取完整 Schema） |
| `dependencies` | object[] | 否 | 前置任务依赖列表，每项含 `task_id` 和可选的 `require_state`（见下方依赖满足条件） |
| `parent_task_id` | string | 否 | 父任务 ID，用于 follow-up / retry / replacement 等派生关系 |
| `relation_type` | enum | 否 | 与父任务的关系类型: `followup` / `retry` / `replacement` / `conflict_resolution` |
| `test_requirements` | object | 否 | 测试要求，子字段: `command`(命令), `coverage_format`(格式), `coverage_path`(覆盖率文件路径), `min_coverage`(阈值 0-100, 浮点数)。注: `test_timeout` 不在此字段内，仅在 Project 级和全局级配置。详见 [边界控制与验证](validation.md) |
| `priority` | enum | 否 | `low` / `normal` (默认) / `high` / `urgent` |
| `summary` | string | 否（系统维护） | 纯文本摘要（从 `submit_task_result` 结构化 JSON 的 summary 字段提取），用于 `dependency_summaries` 传递给下游任务。每次 `submit_task_result` 时覆写。与 `task_results.summary`（完整 JSON）不同 |

> `role` 的可选值为 `backend` / `frontend` / `devops` / `verifier`。其中 `devops` 适用于基础设施变更、CI/CD 配置、部署脚本等运维类任务。

#### 任务调度规则

`get_next_task` 的任务选择遵循以下排序：

1. **状态过滤**: `status = 'pending'`
2. **依赖满足**: 所有前置任务已达到 `require_state` 指定的状态
3. **优先级排序**: `urgent > high > normal > low`
4. **创建时间**: `created_at ASC` (先进先出)

首版 (v0.1) 暂不支持动态优先级调整，所有任务默认 `normal`。

#### 任务关联模型

任务之间存在两种关联关系：

| 关联类型 | 含义 | 典型场景 |
|---|---|---|
| `dependencies` | 执行前置条件（阻塞型） | T-002 依赖 T-001 完成 |
| `parent_task_id` + `relation_type` | 血缘关系（溯源型） | T-015 是 T-010 的冲突解决方案 |

**relation_type 枚举：**

| 类型 | 含义 | 触发场景 |
|---|---|---|
| `followup` | 后续补充任务（relation_type 映射为 conflict_resolution） | merge_conflict 后通过 `resolve_merge_conflict(action=followup)` 创建 |
| `retry` | 重试任务 | 原任务多次失败后重新创建 |
| `replacement` | 替代任务 | 原任务 cancelled 后创建替代方案 |
| `conflict_resolution` | 冲突解决方案（由 action=followup 创建的任务使用此 relation_type） | merge_conflict 后专门处理代码冲突 |

**看板展示:** 任务详情中显示关联链路（如"派生自 T-010（冲突解决）"），Activity Log 追踪关联事件。

## 依赖满足条件

前置任务必须在特定状态下才被视为"已完成依赖"：

| 策略 | 含义 | 适用场景 |
|---|---|---|
| `require_state: "done"` | 合并完成后才算满足 | **默认策略**，适用于代码有实际依赖关系的任务 |
| `require_state: "submitted"` | 服务端验证通过即可 | 适用于仅需 API 契约信息，不需实际代码的任务 |

在 Task 的 dependencies 字段中配置：
```json
"dependencies": [
  { "task_id": "T-001", "require_state": "done" },
  { "task_id": "T-002", "require_state": "submitted" }
]
```

默认不指定 `require_state` 时等同于 `"done"`。

**依赖目标被取消时的处理：** 当前置任务进入 `cancelled` 状态时，**视为依赖已满足**（被取消的任务不会再达到任何目标状态，不应永久阻塞下游任务）。下游任务变为可认领状态后，执行者需自行判断被取消的依赖是否影响本任务的执行方案。协调者也可在取消上游任务时，主动评估是否需要同步取消下游任务。

## 角色权限矩阵

| 角色 | 创建 Feature | 拆分 Task | 领取 Task | 提交结果 | 上报阻塞 | 验证 |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| `coordinator` | Y | Y | - | - | - | - |
| `backend` | - | - | Y(后端) | Y | Y | - |
| `frontend` | - | - | Y(前端) | Y | Y | - |
| `verifier` | - | - | Y(验证) | Y(验证) | - | Y |
| `devops` | - | - | Y(运维) | Y | Y | - |
