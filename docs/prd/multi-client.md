# 3.8 M8: 多客户端支持

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 功能需求 > 多客户端支持
> **相关文档:** [MCP 协议层](mcp-protocol.md) | [并发模型](../technical/concurrency-model.md) | [角色与场景](roles-and-scenarios.md)

---

## 客户端类型

| 客户端 | 传输方式 | 配置方式 |
|---|---|---|
| **Claude Code** | stdio | 在 `.claude/settings.json` 中配置命令 |
| **OpenClaw** | SSE | 配置 MCP Server URL |
| **自定义 MCP 客户端** | SSE | 配置 MCP Server URL |

## Session + Worker 两层模型

Maestro 采用 **Session（会话）+ Worker（工作者）** 两层架构管理 Agent 并行：

- **Session**: 一个 MCP 连接 = 一个 Session，对应一个终端中启动的 Agent 客户端
- **Worker**: Session 内的执行单元。主 Agent 自身为 `default` Worker，每个子 Agent 为独立的 Worker

**三层并行级别：**

| 并行级别 | 模型 | 说明 |
|---|---|---|
| **跨模块并行** | 多个独立 Session | 不同终端、不同角色，各自拥有独立 MCP 连接 |
| **同模块多实例** | 多个独立 Session | 同角色多终端，不同 MCP 连接，各自领取不同任务 |
| **单实例子 Agent** | 一个 Session，多个 Worker | 单个终端内父 Agent 派出子 Agent，共享 MCP 连接，各自绑定不同任务 |

**Worker 数量上限由 Session 的 `capacity` 控制**（默认为 1，上限 5。Claude Code 子 Agent 场景可设为 N，但不超过 5）。

## Worker 标识唯一性

| 标识 | 唯一性范围 | 格式 | 示例 |
|---|---|---|---|
| `session_id` | 全局唯一 | 用户指定或 `sess_{8位hex}` | `cc-backend-01`, `sess_a3f8b2c1` |
| `worker_id` | **Session 内唯一** | `default` / `sub-{N}` | `default`, `sub-1` |

**全局标识规范:**
- 在 API、日志、WebSocket 事件中，Worker 的全局唯一标识使用复合键: `{session_id}/{worker_id}`
- Web UI 展示 Worker 时显示为: `cc-backend-01/default`, `cc-backend-01/sub-1`
- WebSocket 事件中同时返回 `session_id` + `worker_id` + `task_id`，便于前端聚合

**冲突处理:**
- 同一 Session 内若出现重复 worker_id，后注册的覆盖先注册的（仅影响 REST API 显式注册场景）
- MCP 隐式注册的 worker_id 由系统分配，不会冲突

## 隐式 Worker 注册

Agent 调用 `get_next_task` 时，若当前 Session 中尚无对应 Worker，系统自动隐式注册一个 Worker，无需显式调用注册接口。

**隐式注册规则：**
- `get_next_task` 不传 `worker_id` 参数时：自动注册为 `default` Worker
- `get_next_task` 传入 `worker_id` 参数时：以传入值注册（如 `sub-1`），前提是 Session capacity 未满
- 隐式注册的 Worker 自动绑定当前 Session 的 project_id 和 role
- 如果 Session capacity 已满，返回 `SESSION_CAPACITY_FULL` 错误

## Verifier 的 Session 与资源策略

### Session 类型

Verifier 可以是以下两种形式之一：
- **独立 Session**: 单独的 MCP 连接，角色为 `verifier`，专门用于代码审查
- **兼任模式**: 协调者 Session（role=coordinator）同时承担验证职责，无需独立连接

### 资源策略

Verifier 审查任务时**不需要创建 Worktree**，因为 Verifier 只做代码审查（只读），不修改代码。

| 场景 | Worktree 归属 | 说明 |
|---|---|---|
| Verifier 领取 submitted 任务 | 保留原 Worker 的 Worktree | Worktree 仍指向原执行者，Verifier 通过 `git diff` 或 Worktree 路径查看代码 |
| Verifier approve | Worktree 保持不变，等待 merge | merge 完成后才清理 |
| Verifier reject | Worktree 保留给原 Worker | Task 回 in_progress，原 Worker 继续修改 |

`get_verification_task` 的项目绑定通过 Session 的连接绑定自动推断（与 `get_next_task` 相同的 L1 绑定规则）。传入 `{}` 空参数时，自动使用当前 Session 绑定的 project_id。

## Session 生命周期

**注意:** 下图中的 `timed_out`/`reconnect`/`disconnected` 为逻辑状态（运行时行为），不映射到数据库字段。数据库仅存储 `online`/`offline`（见 [数据模型](../technical/data-model.md)）。`timed_out` 和 `reconnect` 由服务端内存管理，`disconnected` 对应 DB 中 `status='offline'`。

```
                  连接建立
┌──────────┐ ──────────────► ┌───────────┐
│  未连接   │                 │ connected │
└──────────┘                 └─────┬─────┘
      ▲                            │
      │            心跳超时          │ 正常工作
      │            ┌───────────────┘
      │            │
      │      ┌─────▼──────┐
      │      │ timed_out   │ 释放所有 Worker 的任务
      │      └─────┬──────┘ tasks → pending
      │            │
      │      ┌─────▼──────┐
      │      │ reconnect  │ 同一 session_id 重连
      │      └─────┬──────┘ 恢复之前的 Worker 状态
      │            │
      │      ┌─────▼──────┐
      └──────│disconnected │ 彻底断开，清理资源
             └────────────┘
```

**超时恢复规则：**
- Session 心跳超时（默认 300 秒） → 标记离线，按任务状态分级处理：
  - **in_progress 任务**（执行者 Session） → 回到 `pending`，清空 session/worker 绑定，Worktree 标记 stale
  - **verifying 任务**（验证者 Session） → 回到 `submitted`，清空 Worker 的 `current_task_id`，其他验证者可重新领取
  - **ready_to_merge 任务** → 不受影响（验证已通过，任何验证者可执行 merge）
  - **submitted 任务** → 不受影响（已进入验证队列，assigned_session_id 保留用于归属追踪，不影响验证者领取）
  - **blocked 任务** → 保持 blocked，清空 assigned_session_id/assigned_worker_id，Worktree 标记 stale（等待协调者处理）
  - **merge_conflicted 任务** → 保持 merge_conflicted，清空 assigned_session_id/assigned_worker_id，Worktree 标记 stale（等待协调者处理）
- 同一 `session_id` 重连 → 恢复之前的 Worker 状态（遵循"不抢夺"原则，见重连冲突处理）
- 彻底断开 → 清理所有资源

**重连冲突处理:**

Session 重连时，若原 Worker 的任务已被其他 Session 认领：
1. 检查原 Worker 的 `current_task_id` 对应的 Task 当前 `assigned_session_id` / `assigned_worker_id`
2. 若仍指向原 Worker → 恢复绑定，Task 继续 `in_progress`
3. 若已被其他 Worker 认领 → 原 Worker 恢复为 `idle` 状态（**不抢夺任务**），原任务由当前持有者继续
4. 若任务已不在 `in_progress`（已提交/完成/取消） → 原 Worker 恢复为 `idle`

**原则:** 重连恢复不破坏已建立的新工作分配。宁可 Worker 空闲重新领取任务，也不强制夺回。
