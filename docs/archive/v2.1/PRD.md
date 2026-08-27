# Maestro-MCP 产品需求文档 (PRD)

> **注意:** 本文件为合并前的单体文档，已拆分为 `docs/prd/*.md`（11 个独立文件）。拆分文档为权威版本，本文件仅作参考，可能未同步最新修改。详见 [文档中心](README.md)。
>
> 版本: v2.1 | 2026-04-17 | 状态: 已拆分归档

---

## 1. 项目概述

### 1.1 定位

Maestro-MCP 是一个**轻量级本地 MCP 服务**，为多 AI Agent（Claude Code、OpenClaw 等）并行开发提供任务调度、上下文降噪与验证闭环能力。同时提供 Web 看板，让人类开发者实时监控 Agent 舰队的工作状态。

**一句话定义：** 专为 AI Agent 设计的机器可读任务总线 + 为人类设计的实时监控看板。

### 1.2 核心问题

| 痛点 | 现状 | Maestro 解决方式 |
|---|---|---|
| 上下文噪音 | Agent 读取全量 OpenAPI/计划文档，消耗 Token 且易产生幻觉 | 按任务动态裁剪上下文，只下发必要的 API 契约 |
| 边界失控 | 后端 Agent "顺手"改前端代码，工作区冲突 | 任务绑定允许目录列表，越界拦截 |
| 验证缺失 | Agent 跳过测试就宣称完成 | 强制校验测试输出和覆盖率，无证据不接收 |
| 状态不透明 | 人类只能逐个终端查看 Agent 进度 | Web 看板实时展示所有任务状态和 Agent 活动 |

### 1.3 设计原则

- **真正的单二进制**: 零外部运行时依赖，编译后一个文件分发
- **零信任 Agent**: 不信任 Agent 汇报的任何执行结果，服务端主动从物理世界取证
- **物理隔离优先**: 同项目多 Agent 并行时，通过 Git Worktree 提供独立工作目录，从文件系统层面杜绝代码冲突
- **双模部署**: 支持 Docker 容器一键启动，也支持本地单进程常驻
- **人类可观测**: Agent 不需要看板，但人类需要——Web UI 是一等公民

---

## 2. 用户角色与场景

### 2.1 角色

| 角色 | 职责 | 典型代表 |
|---|---|---|
| **协调者 (Coordinator)** | 需求分析、任务拆分、阻塞解除、跨项目协调 | Claude Code（协调者模式） |
| **后端执行者 (Backend Worker)** | 执行后端任务，修改服务端代码 | Claude Code（后端角色） |
| **前端执行者 (Frontend Worker)** | 执行前端任务，修改客户端代码 | Claude Code（前端角色） |
| **验证者 (Verifier)** | 审核已提交任务的代码质量，决定合并或打回 | Claude Code（验证者角色） |
| **人类开发者 (Human Developer)** | 通过 Web 看板监控全局进度，无直接操作任务权限 | 浏览器 |

### 2.2 典型场景

#### 场景 A: 单项目多模块并行

同一项目中，后端 Agent 与前端 Agent 分别处理不同模块的任务，各自拥有独立的工作目录边界，互不干扰。

```
Project: user-service
├── Agent backend-01   → T-001: 用户注册 API (allowed: ["src/api/user/"])
├── Agent backend-02   → T-002: 订单查询 API (allowed: ["src/api/order/"])
└── Agent frontend-01  → T-003: 用户列表页   (allowed: ["web/src/pages/user/"])
```

#### 场景 B: 同模块多实例并行

同一项目中，多个相同角色的 Agent 并行处理不同的后端任务，通过原子认领机制避免冲突。

```
Project: user-service
├── Agent backend-01  → T-005: 支付接口
└── Agent backend-02  → T-008: 订单查询
```

#### 场景 C: 单实例子 Agent 并行

单个 Claude Code 终端中，父 Agent 派出多个子 Agent 并行领取不同任务，通过同一 MCP 连接协调工作。

```
Session: cc-backend-01 (capacity=5)
├── Worker: default → T-005: 支付接口
├── Worker: sub-1   → T-008: 订单查询
└── Worker: sub-2   → T-009: 退款接口
```

#### 场景 D: 跨项目协调

一个协调者 Agent 管理多个相关微服务项目，查看全局进度，建立跨项目依赖关系。

```
协调者 Agent (全局视角)
├── 查看 user-service 的进度
├── 查看 order-service 的进度
└── 发现 order-service T-010 依赖 user-service T-001，建立依赖
```

#### 场景 E: Monorepo 子项目

单一 Monorepo 注册为一个 Project，通过 Task 的目录边界实现子模块隔离。

```
Project: my-monorepo
├── T-001 (backend, allowed: ["services/user/"])
├── T-002 (frontend, allowed: ["apps/web/src/user/"])
├── T-003 (backend, allowed: ["services/order/"])
└── T-004 (frontend, allowed: ["apps/web/src/order/"])
```

---

## 3. 功能需求清单

### 3.1 M1: 多项目管理

#### 项目注册方式

| 方式 | 适用角色 | 说明 |
|---|---|---|
| CLI 注册 | 人类开发者 | 在项目根目录执行命令，自动扫描配置或交互式引导 |
| MCP Tool 注册 | 协调者 Agent | 通过 `register_project` 工具创建项目 |
| REST API 注册 | 外部集成 | 通过 HTTP 接口创建项目 |

#### 项目绑定机制

Agent 连接 Maestro 时，通过以下方式确定其所属项目：

1. **显式指定优先**: 通过连接参数或命令行 flag 指定 `project_id`
2. **cwd 自动匹配**: 从 Agent 当前工作目录匹配已注册项目的 `workspace_path`（最长路径匹配，精确匹配优先）
3. **绑定不可更改**: 连接建立后，`project_id` 锁定，后续操作均限定在该项目范围内
4. **匹配失败则拒绝**: 无法匹配任何项目时，拒绝连接并提示

#### 项目生命周期

```
┌───────────┐     register      ┌──────────┐
│  未注册    │ ───────────────► │  active   │
└───────────┘                   └────┬─────┘
                                     │ archive
                                ┌────▼─────┐
                                │ archived  │
                                └────┬─────┘
                                     │ restore
                                ┌────▼─────┐
                                │  active   │
                                └──────────┘
```

**归档规则：**
- archived 项目的 Agent 无法连接（自动拒绝）
- archived 项目的数据保留但不在默认看板显示
- 可随时 restore 恢复
- 不提供删除，只能归档（数据安全）

#### 项目隔离规则（防"窜台"）

"窜台"指 Agent A（绑定 project-a）意外或恶意访问/修改 project-b 的数据。Maestro 通过以下业务规则实现纵深防御：

| 隔离层级 | 规则 |
|---|---|
| **L1 连接绑定层** | 连接建立时锁定 `project_id`，一旦绑定不可更改。Agent 无法在 Tool 参数中伪造 `project_id` |
| **L2 请求校验层** | 每个请求校验目标资源是否属于当前绑定的项目。不匹配则拒绝并记录审计日志 |
| **L3 业务逻辑层** | 所有业务操作（领取任务、提交结果等）自动限定在当前项目范围内。`task_id` 归属校验——不属于当前项目则拒绝 |
| **L4 数据存储层** | 所有数据查询强制携带 `project_id` 条件，不存在无项目范围的查询方法 |

**审计告警规则：**
- 单次越权访问 → 记录日志
- 同一 Agent 5 分钟内 3 次越权 → 看板弹窗告警
- 多个 Agent 同时越权 → 疑似配置错误，建议检查项目注册

**协调者豁免：** 协调者角色可获准跨项目只读访问，但写操作仍需在授权项目范围内。

---

### 3.2 M2: Feature & Task 管理

#### Feature 管理

| 操作 | 说明 |
|---|---|
| 创建 Feature | 协调者录入史诗级需求，含标题、描述、关联文档路径 |
| 查看 Feature | 列出所有 Feature 及其进度百分比 |
| 更新 Feature | 修改需求描述或状态 |
| 关闭 Feature | 所有子 Task 完成后自动关闭，或手动关闭 |

#### Task 状态机

```
                    ┌──────────┐
                    │ pending  │ ← 初始状态
                    └────┬─────┘
                         │ get_next_task()
                    ┌────▼─────┐
              ┌─────│in_progress│
              │     └────┬─────┘
              │          │ submit_task_result() (服务端验证通过)
              │     ┌────▼──────────┐
              │     │   submitted   │
              │     └────┬──────────┘
              │          │ verifier 领取
              │     ┌────▼──────────┐     验证不通过
              │     │   verifying   │──────────────┐
              │     └────┬──────────┘              │
              │          │ 验证通过                  ▼
              │     ┌────▼──────────┐     ┌──────────────┐
              │     │ready_to_merge │     │   rejected    │
              │     └────┬──────────┘     │ → in_progress│
              │          │                 │ (修改后重新提交)
              │     ┌────▼──────────┐     └──────────────┘
              │     │ merge 执行    │
              │     └──┬────────┬──┘
              │   成功 │        │ 冲突
              │  ┌─────▼──┐  ┌──▼────────────┐
              │  │ merged │  │merge_conflicted│
              │  └───┬────┘  └──┬─────────────┘
              │      │          │ 协调者介入解决
              │  ┌───▼────┐     │ 或分配新任务
              │  │  done  │     │
              │  └────────┘     │
              │                 │
              │ report_blocker()│
              │     ┌───────────▼──┐
              └────►│   blocked    │ → 协调者解除后回 pending
                    └──────────────┘
```

**状态定义表：**

| 状态 | 含义 | 可由谁触发 | 允许的操作 |
|---|---|---|---|
| pending | 等待认领 | 系统 | get_next_task → in_progress |
| in_progress | 执行中 | Worker | submit → submitted, report_blocker → blocked |
| submitted | 服务端验证通过，等待 Verifier | 服务端自动 | get_verification_task → verifying |
| verifying | Verifier 审查中 | Verifier | approve → ready_to_merge, reject → in_progress |
| ready_to_merge | 验证通过，待合并 | Verifier | merge → merged/merge_conflicted |
| merge_conflicted | 合并冲突 | 系统自动 | 协调者介入 → 重新分配或新建任务 |
| merged | 已合并到主分支 | 系统自动 | 自动 → done |
| done | 完成 | 系统 | 只读 |
| blocked | 阻塞 | Worker | resolve → pending |

**关键澄清：**

- **"done" 的定义**: 合并完成且 Worktree 已清理
- **谁执行 merge**: 由 Verifier 触发，Maestro 服务端执行 `git merge`
- **冲突处理**: 标记 merge_conflicted，通知协调者，由协调者决定解决方式

#### Task 字段定义

| 字段 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `feature_id` | string | 是 | 所属 Feature |
| `role` | enum | 是 | `backend` / `frontend` / `devops` / `verifier` |
| `title` | string | 是 | 任务标题 |
| `description` | string | 是 | 详细描述 |
| `allowed_directories` | string[] | 是 | 允许修改的目录列表 |
| `forbidden_patterns` | string[] | 否 | 禁止修改的文件模式 |
| `required_apis` | object[] | 否 | 依赖的 API 契约片段 |
| `dependencies` | string[] | 否 | 前置任务 ID 列表 |
| `test_requirements` | object | 否 | 测试要求（命令、覆盖率阈值） |
| `priority` | enum | 否 | `low` / `normal` (默认) / `high` / `urgent` |

**任务认领顺序:** priority DESC → dependencies satisfied → created_at ASC

#### 依赖满足条件

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

#### 角色权限矩阵

| 角色 | 创建 Feature | 拆分 Task | 领取 Task | 提交结果 | 上报阻塞 | 验证 |
|---|:---:|:---:|:---:|:---:|:---:|:---:|
| `coordinator` | Y | Y | - | - | - | - |
| `backend` | - | - | Y(后端) | Y | Y | - |
| `frontend` | - | - | Y(前端) | Y | Y | - |
| `verifier` | - | - | Y(验证) | Y | Y | Y |

---

### 3.3 M3: 动态上下文降噪

#### 降噪策略

Agent 领取任务时，Maestro 返回**最小必要上下文**，而非全量文档：

| 上下文类型 | 降噪规则 |
|---|---|
| **API 契约** | 仅含 `required_apis` 指定的接口（方法、路径、请求/响应 Schema），其余全部丢弃 |
| **文件树** | 仅列出 Worktree 中 `allowed_directories` 内的文件，跳过 node_modules、.git 等 |
| **依赖摘要** | 前置任务的输出仅返回 `summary` 字段，不返回全量变更文件列表 |

#### 契约源 Provider

| Provider | 格式 | 支持阶段 |
|---|---|---|
| `openapi` | OpenAPI 3.x YAML/JSON | v0.2（首版） |
| `manual_json` | 手动录入的 JSON 契约 | v0.2 |
| `graphql` | GraphQL Schema | 规划中 |
| `protobuf` | Proto 文件 | 规划中 |

未配置契约源的项目，上下文降级为纯 description + allowed_directories + 文件列表。

#### 数据源与降级

- 项目可配置 API 契约文件路径（如 OpenAPI 文档）
- Maestro 启动时解析契约文件，构建索引，领取任务时按需提取
- 契约文件变更时自动重新解析
- **降级策略**: 未配置契约文件时，`required_apis` 字段失效，上下文仅包含任务描述 + 目录边界 + 文件列表。不影响边界控制和测试验证

#### 配置继承

优先级：`Task.test_requirements` > `Project.config` > `全局默认配置`

---

### 3.4 M4: 边界控制与验证闭环

#### 零信任原则

**Agent 是不可信的。** 大模型可能产生幻觉或偷懒——伪造测试输出、隐瞒越界修改。因此，Agent 提交任务结果时，**不接受** Agent 汇报的任何数据。所有校验均由 Maestro 服务端从物理世界主动取证。

#### 服务端验证流程

```
Agent 调用 submit_task_result(task_id, summary?)
         │
         ▼
   ┌──────────────────┐
   │ 1. Git Diff 取证  │     获取真实变更文件列表
   └────────┬──────────┘
            │
   ┌────────▼──────────┐    失败    ┌──────────────────┐
   │ 2. 文件边界校验    │──────────►│ 拒绝：返回越界文件 │
   │  真实 diff vs     │           │ 列表              │
   │  allowed_dirs     │           └──────────────────┘
   └────────┬──────────┘
            │ 通过
   ┌────────▼──────────┐
   │ 3. 执行测试       │     执行任务配置的测试命令
   │                   │     捕获真实输出和退出码
   └────────┬──────────┘
            │
   ┌────────▼──────────┐    失败    ┌──────────────────┐
   │ 4. 测试结果校验    │──────────►│ 拒绝：返回测试    │
   │  退出码 = 0?      │           │ 错误信息摘要      │
   └────────┬──────────┘           └──────────────────┘
            │ 通过
   ┌────────▼──────────┐    失败    ┌──────────────────┐
   │ 5. 覆盖率校验      │──────────►│ 拒绝：实际覆盖率  │
   │  读取结构化覆盖率  │           │ < 要求阈值        │
   │  报告文件          │           └──────────────────┘
   └────────┬──────────┘
            │ 通过
   ┌────────▼──────────┐
   │ 6. 状态 → submitted│    保存变更文件、测试结果、覆盖率
   └───────────────────┘
```

#### 校验规则

| 校验项 | 取证方式 | 规则 | 失败行为 |
|---|---|---|---|
| 文件变更 | 服务端执行 git diff | 每个 diff 文件路径均在 `allowed_directories` 内 | 拒绝，返回越界文件列表 |
| 测试通过 | 服务端执行测试命令 | 退出码 = 0 | 拒绝，返回错误输出摘要 |
| 覆盖率 | 读取结构化覆盖率文件 | 覆盖率 >= `min_coverage` | 拒绝，返回实际覆盖率 |

#### 覆盖率文件格式

| 语言 | 覆盖率文件格式 | 路径示例 |
|---|---|---|
| Go | `cover.out` / `coverage.txt` | `coverage/cover.out` |
| TypeScript/JS | Cobertura XML / Istanbul JSON | `coverage/cobertura-coverage.xml` |
| Python | coverage.xml (Cobertura) | `coverage.xml` |
| Java | JaCoCo XML | `target/site/jacoco/jacoco.xml` |

> Maestro 不解析测试命令的 stdout，而是直接读取标准结构化覆盖率文件。

#### Git Worktree 物理隔离

当同一项目中多个 Agent 并行工作时，为每个任务的 Agent 创建独立的 Git Worktree（独立工作目录），从文件系统层面杜绝代码冲突。

**Worktree 生命周期：**

1. Agent 领取任务 → Maestro 创建独立 Worktree
2. Agent 获取的路径全部指向 Worktree 目录
3. Agent 在独立 Worktree 中修改代码，互不干扰
4. Agent 提交结果 → Maestro 在 Worktree 中执行测试和校验
5. 校验通过 → 验证者负责将 Worktree 分支合并回主分支
6. 合并完成 → 清理 Worktree

**冲突处理策略：**

| 场景 | 策略 |
|---|---|
| Worktree 创建失败（有未提交修改） | 返回错误，要求先提交或暂存 |
| 合并时产生冲突 | 通知验证者/协调者处理 |
| Worktree 磁盘空间不足 | 配置最大数量限制（默认 10） |
| 非 Git 仓库的项目 | 回退到"目录隔离"模式，要求 `allowed_directories` 之间无交集 |

#### 测试执行安全边界

Maestro 在 Worktree 中执行测试命令，本质上是本地命令执行器。必须设定明确的安全边界：

**命令来源策略:**
- 测试命令只能来自 Task 配置中的 `test_requirements.command`
- 不允许 Agent 动态传入或修改测试命令
- 项目可配置 `allowed_test_commands` 白名单模板

**执行约束:**

| 约束 | 规则 |
|---|---|
| 工作目录 | 必须在 Worktree 内，禁止 `../` 逃逸 |
| 超时 | 可配置 `test_timeout`（默认 120s），超时强杀进程树 |
| 输出截断 | stdout/stderr 各最大 100KB，超出截断并标记 |
| 环境变量 | 白名单继承（PATH, HOME, GOPATH 等），禁止传递敏感变量 |
| 交互式命令 | 默认禁用，强制非交互模式执行 |
| 联网 | 默认允许（本地开发场景），Docker 模式下可配置网络隔离 |
| 进程管理 | 超时后 SIGTERM → 5s → SIGKILL，清理整个进程树 |

**风险声明（本地模式）:**
测试命令拥有当前用户权限。Maestro 不提供沙箱隔离。建议在 Docker 模式下运行生产环境。

**白名单模板示例:**
- Go: `go test ./... -coverprofile={{.CoveragePath}}`
- Node: `npm test -- --coverageDirectory={{.CoveragePath}}`
- Python: `pytest --cov={{.Package}} --cov-report=xml:{{.CoveragePath}}`

---

### 3.5 M5: MCP 协议层

#### Tools（工具）

**项目管理：**

| Tool 名称 | 说明 |
|---|---|
| `register_project` | 注册新项目 |
| `list_projects` | 列出所有项目及状态（协调者跨项目视图） |

**协调者工具：**

| Tool 名称 | 说明 |
|---|---|
| `create_feature` | 创建 Feature（项目由连接绑定推断） |
| `split_task` | 拆分子任务（含角色、边界、依赖、测试要求） |
| `update_task` | 修改任务参数（修复拆分错误、解锁阻塞任务） |
| `cancel_task` | 取消任务，释放 Worker |
| `resolve_blocker` | 解除 blocked 状态 |

**任务变更规则:**

| 任务状态 | update_task | cancel_task | 备注 |
|---|---|---|---|
| pending | 允许修改全部字段 | 允许 | 正常操作 |
| in_progress | 仅允许修改 description/summary | 允许，但需通知当前 Worker | 修改 allowed_directories/test_requirements 需回退到 pending 再重新认领 |
| submitted 及之后 | 不允许 | 不允许 | 由验证流程控制 |

**执行者工具：**

| Tool 名称 | 说明 |
|---|---|
| `get_next_task` | 领取下一个可执行任务，返回降噪上下文和 Worktree 路径。若 Worker 未注册则自动隐式注册 |
| `submit_task_result` | 声明任务完成。服务端自动取证（git diff + 执行测试 + 读取覆盖率文件）。Agent 提交时可附带结构化摘要： |
| `report_blocker` | 上报阻塞，通知协调者 |
| `claim_batch` | 批量认领任务，自动分配给空闲 Worker |
| `release_worker` | 主动释放子 Worker，其未完成任务回退 pending |

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

**验证者工具：**

| Tool 名称 | 说明 |
|---|---|
| `get_verification_task` | 领取 submitted 状态的任务 |
| `submit_verification` | 提交验证结果（通过/不通过 + 备注） |

#### Resources（资源）

| URI | 返回内容 |
|---|---|
| `project://list` | 所有已注册项目的列表及状态概览 |
| `project://{project_id}` | 单项目详情：配置、进度统计、Agent 列表 |
| `board://active` | 当前项目看板摘要（由连接绑定的项目决定） |
| `board://all` | 跨项目全局看板（协调者用） |
| `task://{task_id}/context` | 任务纯净上下文（动态组装，项目隐含在任务中） |
| `feature://{feature_id}/summary` | Feature 级进度：子任务列表及各自状态 |

#### Prompts（提示词模板）

| Prompt 名称 | 说明 |
|---|---|
| `start-coordinator` | 注入协调者角色：引导需求分析、任务拆分。定期检查看板中的阻塞队列，主动修复错误拆分或取消不可能完成的任务 |
| `start-worker` | 注入执行者角色：绑定角色，专注执行，不发散。只在 Worktree 目录内操作，完成后触发服务端验证 |
| `start-verifier` | 注入验证者角色：领取已提交任务，检查代码质量，决定合并或打回 |

---

### 3.6 M6: Web 看板

#### 页面布局

```
┌───────────────────────────────────────────────────────────────┐
│  Maestro MCP                    [user-svc ▼]       [Settings] │
├──────────┬────────────────────────────────────────────────────┤
│          │                                                     │
│ PROJECTS │  ┌─ Summary ────────────────────────────────────┐  │
│          │  │  Features: 3  │ Tasks: 12 │ Done: 7 │ Active │  │
│ ● user   │  │  ████████████████████░░░░░░  58%             │  │
│   service│  └──────────────────────────────────────────────┘  │
│          │                                                     │
│          │  ┌─ Active Sessions ─────────────────────────────┐  │
│ ● order  │  │  cc-backend-01 (backend)  3/5 workers        │  │
│   service│  │  ├── default  │ T-005: 支付API      23min    │  │
│ ● admin  │  │  ├── sub-1    │ T-008: 订单查询     15min    │  │
│   web    │  │  ├── sub-2    │ T-009: 退款接口     12min    │  │
│ ○ shared │  │  ├── sub-3    │ idle                        │  │
│   libs   │  │  └── sub-4    │ idle                        │  │
│          │  └──────────────────────────────────────────────┘  │
│          │                                                     │
│          │  ┌─ Task Board ─────────────────────────────────┐  │
│          │  │  Pending (3)    In Progress (2)    Done (7)   │  │
│          │  │  ┌─────────┐   ┌──────────────┐  ┌────────┐ │  │
│          │  │  │ T-008   │   │ T-005        │  │ T-001  │ │  │
│          │  │  │ 实现支付│   │ 订单查询API  │  │ 用户模型│ │  │
│          │  │  │ backend │   │ backend-01   │  │ backend │ │  │
│          │  │  └─────────┘   └──────────────┘  └────────┘ │  │
│          │  └──────────────────────────────────────────────┘  │
│          │                                                     │
│          │  ┌─ Activity Log ───────────────────────────────┐  │
│          │  │  14:32  backend-01  submitted T-005 (92%)    │  │
│          │  │  14:28  frontend-01 claimed T-007            │  │
│          │  └──────────────────────────────────────────────┘  │
└──────────┴────────────────────────────────────────────────────┘
```

#### 功能清单

| 功能 | 说明 |
|---|---|
| **项目侧边栏** | 左侧列出所有已注册项目，点击切换看板视图。实心圆=活跃，空心圆=已归档 |
| **项目下拉选择器** | 顶部下拉框快速切换项目，支持键盘快捷跳转 |
| **跨项目总览页** | 首页展示所有项目的进度概览（缩略卡片），无需逐个点进去 |
| **总览面板** | Feature/Task 统计、整体进度条、Agent 在线状态 |
| **看板视图** | Kanban 风格，按状态列排布 Task 卡片 |
| **Agent 监控** | 实时显示各 Agent 当前任务、已耗时、Worker 分配、历史完成数 |
| **活动日志** | WebSocket 推送的实时操作流：任务创建/认领/提交/阻塞等事件 |
| **Task 详情** | 点击卡片弹出详情：描述、边界、API 契约、测试结果、变更文件列表 |
| **Feature 视图** | 按 Feature 聚合查看，展示各 Feature 下所有 Task 的进度 |
| **暗色/亮色主题** | 跟随系统或手动切换 |

#### 人工运维能力 (Phase 4 规划)

初期 Web 看板为只读。Phase 4 起逐步增加以下人工干预能力：

| 操作 | 说明 | Phase |
|---|---|---|
| 强制释放 Session | 将卡死的 Session 标记离线，释放其所有任务 | Phase 4 |
| 强制回退 Task | 将 in_progress/block 任务回退到 pending | Phase 4 |
| 强制清理 Worktree | 删除 stale 状态的 Worktree | Phase 4 |
| 查看测试日志 | 展示服务端执行测试的完整输出 | Phase 3 |
| 下载 Diff/Patch | 下载任务的代码变更 | Phase 4 |

Phase 1-3 阶段如需人工干预，可直接操作 SQLite 数据库（文档提供应急 SQL）。

---

### 3.7 M7: 配置与部署

#### 部署模式

| 模式 | 说明 |
|---|---|
| **Docker 容器** | 单容器部署，暴露 Web UI + REST API + WebSocket 端口以及 MCP SSE 端口。挂载项目工作区目录供 Worktree 创建，挂载配置文件和 SQLite 数据目录 |
| **本地单进程** | 下载单二进制文件，一条命令启动全部服务（Web + MCP + REST）。也可仅作为 MCP stdio Server 运行（用于 Claude Code 本地接入） |

#### 配置层次

| 层次 | 说明 |
|---|---|
| 全局配置文件 | `maestro.yaml`，定义默认行为（端口、存储路径、校验策略、Agent 超时等） |
| 项目级配置 | 覆盖全局默认值（如覆盖率阈值、是否强制测试、边界违规策略等） |
| Task 级配置 | 覆盖项目级配置（如关键任务要求更高的覆盖率） |

---

### 3.8 M8: 多客户端支持

#### 客户端类型

| 客户端 | 传输方式 | 配置方式 |
|---|---|---|
| **Claude Code** | stdio | 在 `.claude/settings.json` 中配置命令 |
| **OpenClaw** | SSE | 配置 MCP Server URL |
| **自定义 MCP 客户端** | SSE | 配置 MCP Server URL |

#### Session + Worker 两层模型

Maestro 采用 **Session（会话）+ Worker（工作者）** 两层架构管理 Agent 并行：

- **Session**: 一个 MCP 连接 = 一个 Session，对应一个终端中启动的 Agent 客户端
- **Worker**: Session 内的执行单元。主 Agent 自身为 `default` Worker，每个子 Agent 为独立的 Worker

**三层并行级别：**

| 并行级别 | 模型 | 说明 |
|---|---|---|
| **跨模块并行** | 多个独立 Session | 不同终端、不同角色，各自拥有独立 MCP 连接 |
| **同模块多实例** | 多个独立 Session | 同角色多终端，不同 MCP 连接，各自领取不同任务 |
| **单实例子 Agent** | 一个 Session，多个 Worker | 单个终端内父 Agent 派出子 Agent，共享 MCP 连接，各自绑定不同任务 |

**Worker 数量上限由 Session 的 `capacity` 控制**（默认为 1，Claude Code 子 Agent 场景可设为 N）。

#### 隐式 Worker 注册

Agent 调用 `get_next_task` 时，若当前 Session 中尚无对应 Worker，系统自动隐式注册一个 Worker，无需显式调用注册接口。

#### Session 生命周期

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
- Session 心跳超时（默认 300 秒） → 标记离线，释放所有 in_progress 任务回 pending
- 同一 `session_id` 重连 → 恢复之前的 Worker 状态
- 彻底断开 → 清理所有资源

---

## 4. 非功能需求

| 维度 | 要求 |
|---|---|
| **启动速度** | 冷启动 < 500ms |
| **内存占用** | 单进程 < 80MB（含 Web 静态资源 + 数据库） |
| **二进制体积** | < 30MB（压缩后 < 10MB） |
| **外部依赖** | 零。单二进制分发，无需任何运行时 |
| **并发能力** | 支持 >= 10 个 MCP 连接，每个连接内支持 <= 5 个并行 Worker |
| **数据持久化** | 文件存储，进程重启数据不丢失 |
| **可观测性** | 结构化日志 (JSON)，可通过 MCP Resource 查看 |
| **安全性** | 本地部署场景暂不做鉴权，预留中间件接口 |
| **兼容性** | MCP 协议遵循最新规范，兼容 Claude Code 和 OpenClaw |

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

| 实体 | 格式 | 示例 | 生成方 |
|---|---|---|---|
| Project ID | kebab-case slug | `user-service` | 用户指定 |
| Feature ID | `F-{4位序号}` | `F-0001` | 系统自增 |
| Task ID | `T-{5位序号}` | `T-00042` | 系统自增 |
| Session ID | `sess_{8位随机}` 或用户指定 | `sess_a3f8b2c1`, `backend-01` | 用户指定或系统 |
| Worker ID | `default`, `sub-{N}` | `default`, `sub-1` | 系统默认或隐式 |
| Worktree Branch | `task/{task_id}` | `task/T-00042` | 系统生成 |

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
