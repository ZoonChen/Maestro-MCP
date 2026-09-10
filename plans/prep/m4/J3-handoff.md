# J3 交接物：Jira 连接器（过程数据通路）

> 工作层交接文档（`plans/prep/m4/`），非权威真源。分支 `j3/jira-connector`，基于 main `b9d07e9`。同步语义权威：`plans/prep/pilot/SOLUTION-BLUEPRINT.md` §1.1/§1.2（[待评审] 块状态见蓝图 §6）。

## 1. implemented 候选声明

| 切片 | 交付物 | Evidence |
|---|---|---|
| J3-1 锚定存储 | 迁移 0018（`jira_anchors` + `jira_reconcile_items` + pushed_* 记忆列）；store 面 `pgJiraStore`（internal/store/postgres_jira.go） | `TestJiraAnchorStorage`、`TestJiraReconcileDivergenceLifecycle`（PG 门控） |
| J3-2 Jira 客户端 | `internal/jira/client.go`：REST v2、PAT Bearer、JQL search、issue 读、镜像字段写（summary/assignee/labels 三字段，**结构性无 transitions/状态写路径**）；`ErrJiraUnavailable`/`ErrJiraAuthFailed`/`ErrJiraIssueNotFound` 分类 | `internal/jira/client_test.go`（沙箱单测：PAT 头、JQL 解析、写面形状负测试、不可达分类、构造纪律） |
| J3-3 镜像 worker | `internal/jira/worker.go` `MirrorPhase`：SoR→Jira 推标题/指派/两标签槽（迭代标签 + `maestro:<status>`）；issue 反向只读快照（snapshot 列唯一写者 `UpdateJiraSnapshot`，**永不触碰 work_items**）；pushed_* 记忆区分"SoR 变了推"与"Jira 侧漂移不推" | `TestJiraWorkerBidirectionalSemantics`（含双向负测试：Jira 状态永不写 work_items.status；冻结期不回推） |
| J3-4 对账 | `ReconcilePhase`：JQL 批拉（50/批）→ 快照 → 四字段分歧检测 → 清单 upsert（周期计数）→ **两周期未裁决升级**（`jira.reconcile.escalated` 审计同事务）；裁决 `ResolveJiraReconcileItem`（accept_sor 清 pushed 重推 / accept_mirror 重写 SoR 侧——仅 title/assignee，投影字段拒绝）；自愈 `HealJiraDivergence` | 同上 + `TestJiraWorkerAdjudicateAcceptMirror`、`TestJiraWorkerFailClosedOnOutage`（断电 fail-closed：不检测、不升级、不谎报成功） |
| J3-5 控制台 | `GET /api/v3/projects/:pid/jira-anchors`、`GET .../jira-reconcile-items`（权限 `project.read`，冻结矩阵不变）；`#/jira` 视图（锚点两栏 + 对账清单 + 已裁决历史，只读边界文案） | `TestJiraConnectorEndpoints`（PG 门控 handler）+ console-governance e2e `jira connector renders anchors…`（真实端点/真实权限矩阵 DOM） |

装配：config `jira:` 段（`base_url`/`pat_secret_ref`/`sync_interval_seconds`，schema 冻结于 config.schema.json；env 覆盖 `MAESTRO_JIRA_*`）；`cmd/maestro/main.go` 单例 worker（缺段不启动=诚实降级）。

## 2. mcp-atlassian 部署约定（Agent 侧工具面，不在本任务书实现）

协议参考仓库：`sooperset/mcp-atlassian`（MIT）。本地克隆 `~/Works/yuandong/projects/mcp-atlassian`，检出位于 `v0.23.0` 后 49 个提交（`74bdaa8`）；仓库最新 release tag 为 `v0.23.1`。

1. **版本钉子**：部署用官方 release `v0.23.1`（写本文时最新 tag）；升级走蓝图 §2.3 外部组件评审口径（MIT 宽松 License 已核，无传染）。
2. **认证（Server/DC PAT）**：环境变量 `JIRA_PERSONAL_TOKEN`（与 Maestro 控制面 `MAESTRO_JIRA_PAT` 各自独立持有、独立轮换，**不共享 PAT**）。PAT 只授只读范围（见下）；凭据仅 env 注入，不入任何台账/审计正文（蓝图 §2.2 `secret` 级）。
3. **工具范围**：Jira 侧启用只读工具面（search/get_issue/get_comments/get_transitions 等读路径）；**禁用** `create_issue`/`update_issue`/`transition_issue` 等写工具（Agent 侧不直接改 Jira——写路径唯一走 Maestro 镜像 worker，保持 SoR 单向语义）。工具集开关用其 `--toolsets`/read-only 配置（Server 部署时以 disabled-tools 清单固化，配置进部署仓库而非本文）。
4. **传输**：Streamable HTTP + 内网 TLS；实例 URL 与 Maestro `jira.base_url` 指向同一 Jira Server。
5. **运维责任**：Jira Server 由企业既有运维（蓝图 §3.2 [待评审]）；mcp-atlassian 进程属平台组沙箱栈。

## 3. 契约变更请求登记（待集成会话裁决）

| 编号 | 请求 | 动机 | 建议 |
|---|---|---|---|
| J3-CR-1 | 锚点创建写端点（`POST /api/v3/projects/:pid/jira-anchors`）与裁决写端点（`POST .../jira-reconcile-items/:id/adjudicate`） | 生产上锚定与人工裁决目前只有 store 面（PG 门控测试覆盖），无 HTTP/MCP 面；控制台按任务书保持只读 | 新冻结权限 `jira.reconcile`（project_admin/platform_admin/背景 worker）；写操作计数钉子 31→33 需同步 spec-consistency-check.rb；或并入 J2c 的 MCP 工具面波次 |
| J3-CR-2 | OpenAPI tags 列表补 `Pilot` 声明 | 既有缺口（pilot 端点用 `[Pilot]` tag 但 tags 列表未声明），本次已顺手补上 | 无需动作（本次已修） |

## 4. 偏离项清单

1. **迁移编号 0018**：与 J1（0016）/J2a（0017）并行协调预留；若实际合入顺序不同，先合者定号、后合者 renumber（迁移 README 已登记语义）。
2. **迭代/状态标签为纯投影通道**：`accept_mirror` 裁决仅对 title/assignee 有意义（会重写 SoR 侧）；iteration_label/status_label 只接受 `accept_sor`（store 层拒绝并给稳定错误）。理由：两者是 Maestro 状态的投影，蓝图 §1.2 的"Jira→Maestro 镜像字段"对这两者的体现是快照展示（issue_labels/issue_status 列），不是重写期望值。
3. **镜像 worker 的诚实竞态**：Jira 侧人工编辑与 SoR 变更落在同一同步窗口内时，SoR 值直接推送、Jira 中间编辑不进清单（SoR 移动即 SoR 胜出）。已注释在 `mirrorOne`。
4. **标签槽语义**：镜像只"拥有"两个标签槽（迭代标签 + `maestro:*` 状态标签）；其余 Jira 人工标签原样保留、永不清理、不产生分歧噪声（分歧检测是槽位在场检查，非集合相等）。
5. **escalation 周期计数**：一个周期 = 一次镜像+对账同步循环（单 ticker 两阶段串行，避免快照竞态）；升级线=2 个周期（`JiraReconcileEscalateAfterCycles`）。试点 `sync_interval_seconds` 建议 ≥300s（人味裁决窗口）。
6. **沙箱交付**：内网 Jira Server 可用性实测按任务书留待 P5a；本任务书全部以 httptest 模拟沙箱（loopback + stub transport，与 gitlab 客户端测试同一纪律）验证。

## 5. P5a 实测项（移交）

- 内网 Jira Server 可达性与 TLS（不可达→蓝图回退：Jira Cloud 或手工锚定）。
- PAT 实际权限范围核验（只读 + issue 编辑两档：镜像 worker 需要写 summary/assignee/labels，Agent 侧工具面只要读——**两个 PAT 分开发**）。
- `jira:` 段生产配置落地与 `sync_interval_seconds` 定值。
