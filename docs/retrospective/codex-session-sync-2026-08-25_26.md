# Codex 会话记录同步：Maestro-MCP 相同任务（2026-08-25 ～ 08-26）

> 过程记录：本文是对 Codex Desktop 中同一任务会话记录的同步与蒸馏，仅供跨智能体（Codex ↔ ZCode）上下文接续使用。它不是 v3.0 权威需求、状态机、接口或实现依据；权威文档仍从 [`docs/README.md`](../README.md) 进入。

> **同步时间:** 2026-08-27
> **记录来源:** `~/.codex/sessions/2026/08/{25,26}/rollout-*.jsonl`（按 `session_meta.cwd == 本项目根目录` 甄别）
> **会话规模:** 42 个 rollout 文件 ≈ 1 条主线会话 + 41 个分叉/重试副本，对应 11 轮用户指令
> **时间跨度:** 2026-08-25 10:47 ～ 2026-08-26 23:30（文件最后写入 23:49）

---

## 一、同步方法

- Codex Desktop 会话以 JSONL rollout 落盘，首行 `session_meta.payload.cwd` 标识工作目录；扫描全部 352 个本地会话后，42 个以本项目为工作目录。
- 每行指令在网络中断、重试或分叉时会产生多个内容相近的 rollout 副本（常见同一时刻 2～3 个文件）；本同步按"用户指令簇"去重，结论取每簇中规模最大、消息最完整的会话。
- 复现方式：解析每个 rollout 的 `response_item.payload.type == "message"`（`role` 为 user/assistant）与 `compacted` 标记，过滤 `<environment_context>`、`<recommended_plugins>`、`<in-app-browser-context>`、`<codex_internal_context>` 等包装块。

## 二、任务主线时间线

| # | 时间 | 用户指令（摘要） | 产出 |
|---|---|---|---|
| 1 | 08-25 10:47 | 全面检查项目文档与代码：为公司内部构建协调 AI 智能体开发协作的 MCP 服务（前后端联调、BUG 修复、测试下发、质量管控、GitLab 分支基线），做差距分析 | 现状差距分析（对应 `docs/governance/current-state-baseline.md`） |
| 2 | 08-25 11:23 | 将 M0–M4 各阶段任务细化到具体文档（需求、交互、规则、校验、门禁、质量管控、权限） | M0–M4 阶段文档细化方案 |
| 3 | 08-25 13:39 | 执行 "Maestro MCP M0–M4 文档落地计划" | v3.0 文档体系落地：v2.1 归档至 `docs/archive/v2.1/`，新建 governance/delivery/prd/technical/testing 等结构（与当前 git 状态中的大量重命名一致） |
| 4 | 08-25 15:10、15:41 | 网络中断，继续 | 文档落地收尾 |
| 5 | 08-25（主线内） | 下一步的最佳建议 | M0 实施建议 |
| 6 | 08-26 13:10～21:05 | 执行 M0 任务书（17 个会话副本，最大 16MB/387 工具调用） | M0 代码实现 + 最终本地候选审计闭环（见 §三-C） |
| 7 | 08-26 22:11 | 排查 IDEA 运行配置 `$PROJECT_DIR$` 未替换、无法创建输出文件 | 启动/运维指引 + `.run/` 共享配置建议（见 §三-D） |
| 8 | 08-26 22:40 | 启动前端服务进行全面功能验证 | Vite 开发代理打通 HTTP/WS 鉴权，只读 Dashboard 实测通过；遗留风险清单（见 §三-E） |
| 9 | 08-26 23:06 | 评估"通过 Codex 推进任务、下发到 ZCode"验证服务可用性还缺哪些能力（并行下发、评价判断、父子任务协调、上下文隔离、会话-任务绑定） | 10 项阻断清单 + M0.5 协作切片建议（见 §三-F） |
| 10 | 08-26 23:26 | 任务树/父子关系/能力域拆分的唯一标识与建模方法评估 | Work Graph 模型建议 + ADR-009 提案（见 §三-G） |

会话簇统计：

| 日期 | 指令簇 | 会话数 | 最大规模 |
|---|---|---|---|
| 08-25 | 全面检查与差距分析 | 4 | 51.7MB / 1572 tools（主线） |
| 08-25 | M0–M4 细化到文档 | 4 | 1.6MB / 24 tools |
| 08-25 | 执行 v3.0 文档落地计划 | 5 | 2.1MB / 98 tools |
| 08-25 | 网络中断继续 | 2 | 0.5MB / 16 tools |
| 08-26 | 执行 M0 任务书 | 17 | 16.2MB / 387 tools |
| 08-26 | 自动续跑/目标驱动（无独立指令，含前端验证会话） | 4 | 3.1MB / 86 tools |
| 08-26 | IDEA 运行配置排障 | 1 | 2.4MB / 32 tools |
| 08-26 | Codex→ZCode 下发能力评估 | 3 | 4.3MB / 37 tools |
| 08-26 | 任务树建模评估 | 2 | 0.8MB / 11 tools |

## 三、各阶段关键结论（蒸馏自各会话最终回复）

### A. 差距分析与 v3.0 文档体系（08-25）

- 结论：当前文档与代码不足以直接支撑"团队级 AI 智能体协作 MCP 服务"目标，需按 M0–M4 交付阶段重建文档基线。
- v3.0 文档治理规则：M0–M4 仅代表交付阶段，领域文档不再使用旧 M1–M8 模块编号；`archive/v2.1/` 属不受信任的历史上下文，不得作为 v3 实现依据；文档状态、代码实现状态、验证状态三者独立，只有 `approved + implemented + passed` 才代表可交付。
- 该轮产出的文档结构即当前仓库 `docs/` 的治理/交付/决策/运维/质量/安全/规范等目录。

### B. M0 实现（08-26，"执行 M0 任务书"）

最终本地候选审计闭环，未发现剩余代码阻断项。本轮关闭的关键问题：

- 配置、REST、MCP、ProjectGuard、Recovery、NoRoute 全部使用稳定脱敏错误与 `correlation_id`；禁止信任 XFF。
- Runner 非 maintenance owner 只能校验 Schema；Schema manifest 可识别字符串篡改。
- 禁用直接删除 Worktree 的状态机旁路；拒绝 SQLite `file:` URI 等危险 DB path。
- `BeginDrain` 立即撤销写权限；stdio Runner 严格保持 stdout 仅 JSON-RPC。

验证结果（当轮）：`go test -count=1 ./...`、`go vet`、`go test -race ./...` 通过；golangci-lint v2.12.2 `0 issues`；状态机覆盖率 100.0%，零信任验证核心覆盖率 86.0%。

**注意：M0 文档状态仍应维持 `partial / unverified`** —— 尚缺 clean-clone Docker 构建、镜像/依赖安全扫描、远程 CI、目标提交绑定及角色审批证据。

### C. IDEA 启动与运维指引（08-26 22:11）

- 全新数据库由 `server` 自行初始化；旧库需停 server 后 `migrate up --db $PROJECT_DIR$/data/maestro-idea.db`；损坏库不自动修复。
- 同一 SQLite DB 只允许一个 maintenance server；`runner` 是 stdio MCP 进程，应由 MCP 客户端拉起，不能当普通 Web 服务配置启动。
- 配置优先级：CLI `--db/--http` > `MAESTRO_*` 环境变量 > YAML > 默认值；Token 只能经 `MAESTRO_AUTH_TOKEN` 注入，不得写入 YAML。
- 建议新增并提交 `.run/` 共享运行配置：`Maestro Web Build`、`Maestro Server M0`、`Maestro Doctor M0`、`Maestro Migrate M0`（不保存 Token）。

### D. 前端全面验证（08-26 22:40）

实测通过：`npm --prefix web run build`；Vite `/dashboard/` 200；代理注入鉴权后 REST POST 200；WebSocket 经同一代理鉴权成功并收到精确 `feature.created` 事件。推荐启动方式：

```bash
MAESTRO_DEV_BACKEND_URL=http://127.0.0.1:28080 \
MAESTRO_DEV_AUTH_TOKEN='<与后端一致的新Token>' \
npm --prefix web run dev   # 访问 http://127.0.0.1:5173/dashboard/
```

仍需关闭的风险（摘要）：

1. 未配置后端地址时 Vite 默认向 `127.0.0.1:8080` 发 Token（本机 8080 是 Java 进程）——应强制显式 `MAESTRO_DEV_BACKEND_URL`。
2. `ActivityLog`/`FeatureView` 不随 WS 事件刷新；`TaskDetail` 的 `Promise.all` 会因 queued 任务 diff 404 丢弃 validation 结果（应 `allSettled`）。
3. 未知 Task 状态会从看板静默消失，应进入 `Needs Attention/Unsupported`。
4. 键盘可操作性、Dialog 语义、live region、44px 控件高度、窄屏布局等 a11y 缺口。
5. 前端无写操作界面，`MAESTRO_REMOTE_WRITE=false` 拒绝所有 REST 写——只能"只读 Dashboard 验证"，完整生命周期验证需临时隔离环境 + `MAESTRO_REMOTE_WRITE=true`（不得指向真实团队仓库）。
6. Playwright M0 套件仅用 `APIRequestContext`，没有真实浏览器 DOM/交互/视觉用例；建议新建真实浏览器 UI 套件（隔离数据库、临时 Git 仓、测试端口、确定性 seed）。

### E. Codex→ZCode 任务下发能力评估（08-26 23:06）

阻断当前 Codex→ZCode 全链路验证的事项（按优先级）：

1. 服务以 `MAESTRO_REMOTE_WRITE=false` 启动，远程 HTTP MCP 无法创建/更新任务。
2. 没有 Zcode Adapter/推送能力，只能由 Zcode 配置 MCP 后主动拉取。
3. MCP 没有 Session/Worker 注册、恢复协议。
4. 领取结果不返回精确 worktree 路径。
5. 实际 MCP 领取接口缺少幂等键和队列版本（与权威 Schema 漂移：`get_next_task` 缺 `idempotency_key + queue_version + capabilities`，且允许调用方自报 `project_id/role/session_id`）。
6. 没有可信的 Codex/Zcode 会话与任务绑定。
7. 父子任务无法通过现有公开接口建立，也没有聚合协调规则。
8. 提交验证要求配置批准的 Command Profile、Policy 并在隔离环境执行。
9. M0 无法产生真实 GitLab CI merge-gate Evidence。
10. 静态 Bearer Token + 参数自报身份，不适合多成员并行试用。

其他能力缺口：上下文隔离（无 manifest/digest、文件白名单、token 预算、checkpoint、强隔离边界）；并行调度只有基础容量没有协调策略（无能力匹配、fan-out/join、背压、亲和性）；评价只有确定性门禁没有 Agent 能力评价（rubric/轨迹/多评审/预算）；重启安全但不能恢复 Agent 思考上下文。

建议先实现的 **M0.5 协作切片**：`ConversationSession/ExecutionAttempt` 实体 → MCP Session 注册/心跳/恢复 → `GetNextTaskWithVersion` 强制幂等键 → 统一 `ExecutionEnvelope`（Task、Lease、精确 worktree、generation、base SHA、上下文 digest、预算）→ 项目内父子层级（project-scoped FK、root/depth、状态聚合、fan-out/join、失败取消传播）→ capability routing/最大并发/资源并发键 → typed Artifact 父子传递（子任务只收最小上下文）→ `EvaluationRecord`（rubric、Evidence、score、verdict、reviewer、retry、needs_human）→ 最后接 Zcode Adapter、OIDC/RBAC、PostgreSQL/Outbox、真实 Runner。

在补齐前只可做受控演示：预建 Project/Session/Worker，创建互相独立的平面任务，多个 Zcode Worker 各自拉取、操作独立 worktree、发心跳、验证阻塞与 Lease 恢复；不得表述为已支持父子协调、上下文续接或完整自动评价。

### F. 任务树建模评估（08-26 23:26）

核心建议一句话：**用树回答"属于谁"，用 DAG 回答"先做什么"，用 Artifact Contract 回答"传递什么"，用 Attempt 回答"谁在哪个会话执行过"，用 Evidence 回答"为什么可以通过"。**

- 新模型：`WorkPlan`（含 BusinessProblem/Outcome）+ 原子 `WorkItem`；归属树用邻接表（`parent_node_id`），依赖用规范化 edge table（DAG，无环）；`work_lineage` 承载 retry/followup/replacement 血缘，不与结构父子混用。
- ID 体系区分：`work_node_id`（逻辑身份，跨修订稳定）/ `node_revision_id`（不可变规格）/ `execution_attempt_id` / `idempotency_key`（创建意图）/ `semantic_fingerprint`（仅重复告警，不自动合并）。
- 父任务状态是纯函数投影（子 outcome + JoinPolicy + Evidence 计算），汇聚规则拆为 `success_threshold: all|any|quorum(k)` × `failure_policy: fail_fast|collect_all|needs_human` × `cancel_policy`；子任务全部结束 ≠ 父任务通过，还需独立 Integration/Evaluation 证据。
- 调度：当前规模用确定性贪心（priority DESC, deadline ASC, critical_path_length DESC…）最易解释测试；图规模明显增长后再考虑 min-cost max-flow。
- 存储：继续以 PostgreSQL 为权威写库（复合 project FK、部分唯一索引、CAS、事务、Audit、Outbox 原子性），`ltree` 仅作查询投影；不引入图数据库作写主库。证据血缘可借鉴 W3C PROV（Entity/Activity/Agent/used/generated/derived-from）。
- 迁移：`Feature→WorkPlan`、`Task→WorkItem`、`Dependencies JSON→WorkDependency`、`Role→ExecutionRequirement`（不是 Capability）、`AssignedSessionID/WorkerID→ExecutionAttempt/SessionBinding`、`TaskResult→ResultCapsule/Artifact`、`ValidationRun→Evidence/EvaluationRun`；采用 expand/contract（新增表→影子构图→双读对账→切换写入→删旧字段）；语义不明的 `ParentTaskID` 历史数据进 `needs_reconcile`，不得猜测。
- 落地动作建议：新增 `ADR-009: Versioned Typed Work Graph and WorkPattern`，以及三份权威设计 `prd/work-planning-and-orchestration.md`、`technical/work-graph-model.md`、`technical/work-graph-scheduler.md`。**本轮仅评估，未修改文件。**

## 四、与当前仓库状态的对应

- git 状态中的大量 `R`（`docs/*` → `docs/archive/v2.1/*`）即第 3 轮 v3.0 文档落地计划的执行结果；`docs/` 新结构、`internal/`、`web/`、`Makefile`、`Dockerfile` 等修改对应 M0 实现与前端验证轮次。
- 最近提交 `f24bdf7 "Merge dev: full implementation with all code, tests, and docs"` 之前的主体实现即上述 Codex 会话产出；其后的工作区修改尚未提交。
- 截至同步时点（08-26 深夜），**ADR-009 / Work Graph 三份设计文档尚未创建**；M0.5 协作切片尚未实现；前端真实浏览器 UI 测试套件尚未建立。
- 后续进展：2026-08-27 的 ZCode 会话按上轮建议创建了 ADR-009 与三份权威设计文档（均为 `draft`、`not_started`）：[ADR-009-versioned-typed-work-graph.md](../decisions/ADR-009-versioned-typed-work-graph.md)、[work-planning-and-orchestration.md](../prd/work-planning-and-orchestration.md)、[work-graph-model.md](../technical/work-graph-model.md)、[work-graph-scheduler.md](../technical/work-graph-scheduler.md)，并同步了术语表词条；docs-check / mermaid-check / spec 一致性检查通过。M0.5 协作切片与前端浏览器测试套件仍未实现。

## 五、原始会话文件索引（关键文件）

| 角色 | 文件 |
|---|---|
| 主线会话（含全部 11 轮指令） | `~/.codex/sessions/2026/08/25/rollout-2026-08-25T10-47-45-01a036d1-0ced-7681-84b9-6499d585188f.jsonl` |
| M0 实现最大会话 | `~/.codex/sessions/2026/08/26/rollout-2026-08-26T17-03-04-01a03d4f-05e2-7810-8f24-81b767fa3ab9.jsonl` |
| IDEA 排障 + 前端验证 | `~/.codex/sessions/2026/08/26/rollout-2026-08-26T22-11-25-01a03e69-53e8-7330-9e8f-f95e93e66630.jsonl` |
| 前端功能验证（浏览器） | `~/.codex/sessions/2026/08/26/rollout-2026-08-26T22-40-26-01a03e83-e4f9-7121-8353-6cc4b2ce20a9.jsonl` |
| Codex→Zcode 下发评估 | `~/.codex/sessions/2026/08/26/rollout-2026-08-26T23-06-04-01a03e9b-5c31-7d21-af43-7009f409b3cc.jsonl` |
| 任务树建模评估 | `~/.codex/sessions/2026/08/26/rollout-2026-08-26T23-26-07-01a03ead-b90a-7652-958a-0a1833964aad.jsonl` |

其余 36 个为同指令的分叉/重试副本，清单可按 §一 方法重新扫描获得。
