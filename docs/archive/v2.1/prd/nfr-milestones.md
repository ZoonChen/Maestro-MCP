# 4-7. 非功能需求、里程碑、ID 规范与术语表

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 非功能需求与附录
> **相关文档:** [配置与部署](deployment.md) | [项目结构](../technical/project-structure.md) | [接口规范](../technical/api-spec.md)

---

## 4. 非功能需求

| 维度 | 要求 |
|---|---|
| **启动速度** | 冷启动 < 500ms |
| **内存占用** | 单进程 < 80MB（含 Web 静态资源 + 数据库） |
| **二进制体积** | < 30MB（压缩后 < 10MB） |
| **外部依赖** | 零。单二进制分发，无需任何运行时 |
| **并发能力** | 支持 >= 10 个 MCP 连接，每个连接内支持 <= 5 个并行 Worker。最大 MCP 连接数硬限制可通过 `max_connections` 配置（默认 50），超出后新连接返回 `CONNECTION_LIMIT_REACHED` 错误 |
| **数据持久化** | 文件存储，进程重启数据不丢失 |
| **可观测性** | 结构化日志 (JSON)，可通过 MCP Resource 查看 |
| **安全性** | 本地部署场景暂不做鉴权，预留中间件接口 |
| **兼容性** | MCP 协议遵循最新规范，兼容 Claude Code 和 OpenClaw |
| **数据生命周期** | activity_log 保留最近 90 天；audit_log 保留最近 180 天；done/cancelled 状态的 Task 数据永久保留；归档项目数据保留但不在默认视图展示 |
| **实体数量限制** | 单 Project 建议 Feature 上限 50 个、Task 上限 500 个。超过时 `split_task` 返回软限制警告（不阻止，但记录 audit_log） |

### 日志职责划分

| 日志类型 | 存储位置 | 用途 | 保留期限 |
|---|---|---|---|
| `activity_log` | SQLite 表 | Web 看板展示、任务流转追踪、Agent 活动记录 | 90 天 |
| `audit_log` | SQLite 表 | 越权检测、软限制告警、安全事件追踪、合规审计 | 180 天 |
| 测试日志 | `.maestro/logs/tests/{task_id}/attempt-{N}.log` | 服务端测试执行完整输出，供人工和 Verifier 查看 | 30 天 |
| 结构化系统日志 | stdout / 日志文件 | 进程级别日志（启动、配置加载、错误），JSON 格式 | 跟随部署配置 |

### 日志消费路径

| 日志类型 | 消费者 | 展示位置 | 过期后行为 |
|---|---|---|---|
| `activity_log` | 人类开发者 | 项目时间线、Task 详情页、Feature 历史视图 | 原始记录清理，统计口径（完成率、耗时）永久保留 |
| `audit_log` | 人类开发者、运维 | 项目告警面板、设置页、管理员导出 | 记录清理，累计告警计数保留 |
| 测试日志 | 人类开发者、Verifier | Task 验证详情页、reject 时附日志路径引用 | 日志文件删除，Task/ValidationRun 状态不受影响 |
| 结构化系统日志 | 运维 | 部署侧观测工具，不暴露给 Agent | 跟随日志轮转策略 |

### 关键时间字段定义

| 时间点 | 所属表 | 含义 | 统计用途 |
|---|---|---|---|
| `created_at` | 所有表 | 记录创建时间 | 通用 |
| `updated_at` | tasks, features | 最后更新时间 | 变更追踪 |
| `assigned_at` | tasks | 首次认领时间 | 排队耗时计算 |
| `submitted_at` | task_results | 任务提交时间 | 执行耗时计算 |
| `validated_at` | task_results | 验证完成时间 | 验证耗时计算 |
| `verified_at` | tasks | Verifier 确认时间 | 验证→merge 耗时 |
| `last_heartbeat` | agent_sessions | 最后心跳时间 | 在线判定 |
| `last_active` | agent_workers | Worker 最后活动时间 | Worker 活跃度 |
| `cleaned_at` | (已移除) | GC 清理时直接删除 worktree 行，不再保留 cleaned_at 字段 | 统计改由 GC 执行时写入 activity_log |

**任务耗时计算口径：**

| 指标 | 公式 | 含义 |
|---|---|---|
| 排队耗时 | `assigned_at − created_at` | 从创建到被认领的等待时间 |
| 执行耗时 | `submitted_at − assigned_at` | 从认领到提交的开发时间 |
| 验证耗时 | `validated_at − submitted_at` | 从提交到服务端验证完成 |
| 总耗时 | `done 时间 − created_at` | 从创建到最终完成的全周期 |

---

## 错误语义表

以下错误码在 MCP Tool、REST API、WebSocket 中统一使用。技术实现细节见 [接口规范](../technical/api-spec.md)。

| 错误码 | HTTP Status | 含义 | 典型触发场景 | 可重试 |
|---|---|---|---|---|
| `PROJECT_NOT_FOUND` | 404 | 项目不存在 | 访问未注册的项目 ID | N |
| `PROJECT_ARCHIVED` | 403 | 项目已归档 | 向已归档项目发起操作 | N |
| `PROJECT_NOT_BOUND` | 400 | Agent 未绑定项目 | cwd 无法匹配任何项目 | N |
| `PROJECT_AMBIGUOUS` | 400 | cwd 匹配到多个项目 | workspace_path 存在包含关系 | N |
| `FEATURE_NOT_FOUND` | 404 | Feature 不存在 | 访问不存在的 Feature ID | N |
| `TASK_NOT_FOUND` | 404 | Task 不存在 | Task ID 不存在或跨项目越权 | N |
| `TASK_NOT_OWNED` | 403 | Task 不属于当前 Session | 提交非自己认领的任务 | N |
| `TASK_STATE_INVALID` | 409 | 当前状态不允许该操作 | 对 submitted/done/verifying/ready_to_merge 状态执行不允许的操作（如 cancel submitted 任务） | N |
| `TASK_ALREADY_CANCELLED` | 409 | 任务已取消（`TASK_STATE_INVALID` 的特化错误码） | 对已取消的任务重复调用 cancel_task | N |
| `TASK_DEPENDENCY_UNMET` | 412 | 前置依赖未满足 | 领取前置任务未完成的任务 | Y |
| `SESSION_NOT_FOUND` | 404 | Session 不存在 | 心跳上报不存在的 Session | N |
| `SESSION_CAPACITY_FULL` | 429 | Session Worker 数已达上限 | 超过 capacity 注册 Worker | Y |
| `WORKTREE_CREATE_FAILED` | 500 | Worktree 创建失败 | Git 工作区不干净（需 Agent 提交或暂存变更）；或磁盘/权限问题。返回 500 因触发原因混合了环境状态和基础设施错误 | Y |
| `WORKTREE_CLEAN_FAILED` | 500 | Worktree 清理失败 | 磁盘 I/O 错误 | Y |
| `TEST_EXECUTION_FAILED` | 422 | 测试执行失败 | 测试退出码非 0 | Conditional |
| `TEST_EXECUTION_TIMEOUT` | 408 | 测试执行超时 | 超过 test_timeout | Y |
| `COVERAGE_BELOW_THRESHOLD` | 422 | 覆盖率低于阈值 | 实际覆盖率 < min_coverage | Conditional |
| `COVERAGE_FILE_NOT_FOUND` | 422 | 覆盖率文件不存在 | 覆盖率路径配置错误 | Conditional |
| `BOUNDARY_VIOLATION` | 422 | 文件变更越界或命中禁止模式 | 修改了 allowed_directories 外的文件，或文件匹配 forbidden_patterns。`detail.sub_type` 区分: `out_of_bounds` / `forbidden_pattern` | Conditional |
| `CROSS_PROJECT_ACCESS_DENIED` | 403 | 跨项目访问被拒绝 | Agent 访问非绑定项目的资源 | N |
| `VALIDATION_REJECTED` | 422 | Verifier 驳回任务 | Verifier 调用 `submit_verification(passed=false)` 驳回任务。该错误码记录在 `task.rejected` WS 事件中通知原 Worker，不作为 HTTP 响应错误返回给 Verifier。注：此为 Verifier 人工驳回，区别于 `validation_runs.result=rejected` 的服务端自动校验拒绝（边界/测试/覆盖率不通过） | Conditional |
| `MERGE_CONFLICT` | 409 | 合并冲突 | merge 时发现代码冲突 | N |
| `INVALID_PARAMETER` | 400 | 参数校验失败 | split_task 参数不合法 | N |
| `CIRCULAR_DEPENDENCY` | 422 | 循环依赖 | 创建任务后依赖图出现环路 | N |
| `CONNECTION_LIMIT_REACHED` | 429 | MCP 连接数达到上限 | 超过 max_connections 配置 | Y |
| `NO_AVAILABLE_TASK` | 404 | 当前无符合条件的可执行任务 | `get_next_task` / `get_verification_task` 无匹配 | Y |
| `CONCURRENT_CONFLICT` | 409 | 并发冲突，乐观锁重试耗尽 | SQLite SERIALIZABLE 事务竞争 | Y |

---

## 5. 里程碑规划

### Phase 1: 核心骨架 + 多项目基础 (v0.1)

**目标：** 服务可启动，支持多项目注册，Task 状态机可运转，MCP 可连通

- 项目注册（CLI + API + MCP Tool）
- Feature/Task CRUD
- Task 状态机 + 任务领取逻辑
- Agent 注册 + workspace_path 自动绑定
- 核心 MCP Tools: 注册项目、创建 Feature、拆分 Task、领取 Task、提交结果

**验收标准：** 注册两个项目，两个终端分别在两个项目目录下启动 Agent，各自领取不同项目的任务

### Phase 2: 验证闭环 + 上下文降噪 (v0.2)

**目标：** 提交必须过测试，上下文按需裁剪

- 文件边界越界检测
- 测试输出校验 + 覆盖率解析
- API 契约动态裁剪
- MCP Resources + Prompts
- 阻塞上报与解除
- SSE 传输支持

**验收标准：** Agent 提交任务后，系统自动校验边界/测试/覆盖率，不通过则打回

### Phase 3: Web 看板 (v0.3)

**目标：** 人类可观测，实时看到多项目 Agent 工作状态

- WebSocket 实时事件推送
- 项目侧边栏 + 跨项目总览
- Kanban 看板 + Agent 监控面板
- 活动日志 + Task 详情
- Agent 心跳 + 离线检测

**验收标准：** 浏览器打开看板，实时看到多项目的 Agent 活动和任务流转

### Phase 4: 生产就绪 (v0.4)

**目标：** 可一键部署，配置完善

- Docker + docker-compose 部署
- 配置文件 + 项目级配置覆盖
- 验证者角色完整流程
- 结构化日志体系
- 进程重启后 Agent 重连与任务状态回滚
- 项目归档/恢复
- 数据生命周期管理（日志归档、过期清理）
- 人工兜底运维能力
  - 强制释放卡死的 Session
  - 强制回退 in_progress/blocked 任务到 pending
  - 强制清理 stale/abandoned Worktree
  - 查看服务端执行的测试日志全文
  - 导出 Task 的代码变更 diff/patch

**验收标准：** Docker 一键启动，配置项目级覆盖，Agent 异常断连后任务自动回退

### Phase 5: 增强特性 (v1.0)

- 跨项目 Task 依赖关系
- 任务模板 / 预设（常见项目结构的默认拆分策略）
- Web 看板手动干预（紧急阻塞、重新分配）
- 项目配置热重载
- 性能指标暴露
- 性能优化与压测

**验收标准：** 跨项目依赖可用，任务模板可复用，看板支持人工干预

---

## 6. ID 与命名规范

| 实体 | 格式 | 示例 | 生成方 | 字符集 | 长度限制 |
|---|---|---|---|---|---|
| Project ID | kebab-case slug | `user-service` | 用户指定 | `[a-z0-9-]` | 3-64 字符 |
| Feature ID | `F-{4位序号}` | `F-0001` | 系统自增 | 固定格式 | 固定 6 字符 |
| Task ID | `T-{5位序号}` | `T-00042` | 系统自增 | 固定格式 | 固定 7 字符 |
| Session ID | `sess_{8位hex}` 或用户指定 | `sess_a3f8b2c1`, `backend-01` | 用户指定或系统 | `[a-zA-Z0-9_-]` | 4-64 字符 |
| Worker ID | `default`, `sub-{N}` | `sub-1` | 系统默认或隐式 | 固定格式 | - |
| Worktree Branch | `task/{task_id}` | `task/T-00042` | 系统生成 | 固定格式 | - |

**标识规范补充:**
- 所有 ID 不区分大小写（建议统一使用小写）
- Project ID 不允许用户自定义后修改
- Session ID 若用户指定，不可与全局已有 Session 冲突（Session ID 全局唯一，见 [多客户端支持](multi-client.md)）
- Worker ID 在 Session 内唯一，全局标识使用 `{session_id}/{worker_id}` 复合键

---

## 7. 术语表

| 术语 | 定义 |
|---|---|
| **Project** | 已注册的项目命名空间，绑定 workspace 路径，是 Feature/Task/Agent 的顶层隔离边界 |
| **Feature** | 史诗级需求，属于某个 Project，由协调者创建，包含多个 Task |
| **Task** | 原子执行单元，属于某个 Project，绑定角色、边界和验证要求 |
| **Agent** | AI 编程助手实例（如一个 Claude Code 终端），绑定到特定 Project |
| **Session** | 一个 MCP 连接对应一个 Session，代表一个 Agent 客户端实例 |
| **Worker** | Session 内的执行单元，主 Agent 为 `default` Worker，子 Agent 为独立 Worker |
| **MCP** | Model Context Protocol，AI 助手与外部工具通信的标准协议 |
| **上下文降噪** | 按任务需求裁剪 API 文档和代码路径，减少 Agent 无关信息摄入 |
| **边界控制** | 限制 Agent 只能修改指定目录内的文件 |
| **零信任验证** | 不信任 Agent 汇报的结果，服务端主动取证（git diff、执行测试、读取覆盖率文件） |
| **项目绑定** | Agent 连接时通过 workspace_path 自动匹配或显式指定 Project 的过程 |
| **窜台** | Agent 意外或恶意访问/修改非所属项目的数据 |
| **Worktree** | Git Worktree，为每个并行任务创建的独立工作目录，实现物理隔离 |
| **验证闭环** | 从任务领取到代码提交、测试执行、覆盖率校验、验证者审核的完整质量链路 |
| **cancelled** | Task 的只读终态，由协调者通过 cancel_task 触发，不计入 Feature 进度 |
| **reopen** | merge_conflicted 的处理选项之一，将冲突任务回退给原执行者继续修改 |
| **stale** | Worktree 状态之一，表示对应的 Session 已超时，Worktree 待评估是否可复用 |
| **base_commit** | Worktree 创建时的 Git HEAD commit，用于零信任验证中计算真实变更范围 |
| **capacity** | Session 允许的最大并行 Worker 数量 |
| **claim_batch** | 批量认领任务的便利封装，部分成功语义 |
| **followup** | merge_conflicted 的处理选项之一，创建新的冲突解决任务 |
| **循环依赖** | Task 依赖图中的环路，会导致依赖的任务永远无法被领取，split_task 时检测并拒绝 |
| **parent_task_id** | 任务关联模型的核心字段，指向衍生该任务的父任务。用于 follow-up / retry / replacement / conflict_resolution 等场景 |
| **relation_type** | 任务与父任务的关联类型枚举: followup / retry / replacement / conflict_resolution |
| **manual_json** | 契约源 Provider 类型之一，允许手动录入 JSON 格式的 API 契约，适用于无标准 OpenAPI 文档的项目 |
| **conflict_resolution** | relation_type 枚举值之一，表示该任务是为解决父任务的合并冲突而创建的 |
