# M1 PostgreSQL 迁移与 SQLite 导入

> P3 数据模型建设产物（S1 主导）。权威契约在 `docs/technical/data-model.md`（TECH-DATA-001）与 ADR-002；本目录是执行层：迁移 DDL、导入映射表与回滚 runbook。DDL 变更走单 owner 串行合入（DISCIPLINE-PHASES P3）。

## 迁移机制

- 文件命名 `NNNN_name.up.sql` / `NNNN_name.down.sql`，嵌入二进制（`internal/store/postgres_migrations.go`）。
- `schema_migrations` 版本表记录 version/name/digest（sha256），启动与应用时复核 digest，漂移即 `SCHEMA_INTEGRITY_FAILED`（与 SQLite schema catalog 同一纪律）。
- 迁移在 `pg_advisory_lock(hashtext(current_database() || ':maestro_schema_migration'))` 单连接锁内执行；每个迁移单事务提交。
- 命令：`maestro migrate up`（driver=postgres 时走 PG）；`maestro migrate revert [--steps N]` 仅限 PG（cutover 前回滚演练用）。

## M1 基线表清单（0001）

锚定卡 M1-DATA-001 表清单全覆盖，另含两类操作表：

| 类别 | 表 | 追溯 |
| --- | --- | --- |
| 身份 | `users` `teams` `memberships` | ADR-003、SEC-IDENTITY-RBAC |
| 项目/工作 | `projects` `features` `work_items` | DATA-REQ-001/002、锚定卡 |
| Runner | `runners` `runner_bindings` `runner_enrollments` | ADR-001、SEC-RUNNER-SECURITY |
| 租约/执行 | `leases` `executions` | DATA-INV-002、runner-security §6 |
| 工作区 | `worktrees` | TECH-WT-001（导入保真） |
| 证据 | `validation_runs`（diagnostic） | DATA-REQ-003 不可变 |
| 审计/事件 | `audit_events` `outbox_events` `inbox_events` | ADR-002、SEC-IDENTITY-RBAC §9 |
| 幂等 | `api_idempotency` | TECH-DATA-001 §8 |
| 导入映射 | `legacy_id_map` | architecture §13 新旧 ID 映射表 |
| 迁移目录 | `schema_migrations` | 锚定卡"迁移锁与版本表" |

**锚定卡之外的偏差记录**（schema 评审项）：

1. `features`、`worktrees`、`validation_runs` 不在锚定卡最小清单中，但为 SQLite 导入保真与 TECH-WT-001/Evidence 不可变要求所必需。
2. `audit_events`/`validation_runs` 由触发器完全禁止 UPDATE/DELETE；`outbox_events`/`inbox_events` 仅封套列不可变（dispatch 簿记列可迁移状态）——DATA-GATE-002 的"更新必须失败"对事件表按"封套不可变"实现，因为 ADR-002 §6 的状态机要求 status 可迁移。
3. `work_items`/`leases` 保留 `legacy_session_id`/`legacy_worker_id` 文本列承载 M0 会话引用（M1 会话-任务绑定落地后由 connection_generation 取代）。

## M2 GitLab 集成表清单（0004/0005）

锚定卡 M2-GL/WHK/GIT/MR/QG 表清单全覆盖：`gitlab_instances`、`gitlab_project_mappings`、`webhook_inbox`、`webhook_deliveries`、`merge_requests`、`pipelines`、`pipeline_jobs`、`evidence`（append-only + supersedes 链）、`gate_snapshots`、`waivers`。`evidence` 与 `webhook_deliveries` 挂 `maestro_raise_immutable` 触发器（DATA-REQ-003 / WEBHOOK 审计不可变）。

**偏差记录**（schema 评审项）：

1. 计划中的独立 `dlq` 表由 `webhook_inbox.status='dead_letter'` 承载（S4 偏差 1 已被冻结事件目录吸收）：隔离与重试耗尽共用一行，DLQ 审计走 append-only 送达表。
2. `evidence.pipeline_id` 是普通 FK 而非 SHA 元组外键：`pipelines` 以 uuid 键控而 evidence 携带 SHA 元组，应用层负责校验 pipeline.sha 与 evidence.source_sha 一致（README 0004 注记补登）。
3. `0005` 补齐收件箱调度簿记：`webhook_inbox.next_attempt_at`（指数退避调度）、`lease_owner`/`claimed_at`（dispatcher 崩溃后的有界 stale 重认领，镜像 outbox 租约纪律）、`webhook_deliveries.inbox_id` 可空（验签拒绝/归档路径没有收件箱行，deny 审计仍落表）。
4. `0006` 项目质量策略存储 `quality_policies`（单行/项目 + CAS row_version，对应 putProjectQualityPolicy 的 If-Match/If-None-Match；公司基线走二进制内嵌，不入库），并为 0004 未建模的冻结 wire 必填列补齐：`evidence.attempt`（flaky 重试簿记）、`evidence.status`（质量结论枚举）、`evidence.sensitivity`（数据分类，缺省 confidential）、`gate_snapshots.version` 与 `waivers.version`（ResourceVersion / If-Match）、`waivers.merge_request_iid`（豁免的 MR 绑定）。merge_gate 证据的数字 provider ID 自 0009 起独立成列；uuid 外键回填留给对账期富化。
5. `0007` 为 `work_items` 补 `merged_fact_id`（GL-INV-003：done 由 merged webhook 或对账确认并记录来源事件；SQLite 侧 task_store 已有该列，0001 建 PG 基线时未带）。
6. `0008` 实例/映射 ResourceVersion（wire 必需）：`gitlab_instances.version`、`gitlab_project_mappings.version`、映射行 `id`（0004 为复合外键键控，无行标识）、`UNIQUE (project_id)`（putGitLabProjectMapping 的"每项目单条当前映射"语义）。
8. `0010` M3 九表（api_contracts / integration_runs / findings / defects / defect_occurrences / defect_task_links / budget_ledgers / budget_entries / agent_runs）：defects/findings 增加 `UNIQUE (project_id, id)` 以支撑 occurrence/link 的复合外键（投影列跨表一致性）；`one_active_fix_per_defect` 部分唯一索引承载"同 defect 单活跃修复"；SQLite 不新增表（M3 实体仅 PG）。
10. `0012` M4 治理三表（telemetry_aggregates 窗口桶唯一 / evaluation_records (run,case,layer) 唯一 + security 层必带 risk 表级不变量 / pilot_flags 每项目每 flag 唯一 + gray-only 百分比）。偏差：evaluation 的 risk 必填由 schema 的条件要求转为表 CHECK；telemetry 只存脱敏聚合（redaction_version 随行）。
9. `0011` `agent_runs` 增 `(project_id, defect_id, attempt)` 唯一索引：崩溃恢复按持久状态续跑（find-or-resume），副作用调用永不重放（agent-remediation §8）。
8. `0010` M3 九表（`gitlab_pipeline_id`/`gitlab_job_id` bigint）：冻结 wire 的 pipeline_id/job_id 是整数，0004 建成 uuid 外键。数字列满足 wire 往返；uuid 外键回填仍留给对账期富化。
11. `0013` M4 备份/WAL 恢复元数据 `backup_runs`（矩阵 M4-REL-001 规划项）：kind full/wal、状态机 running→completed→verified/failed；REL-RULE-004 表级化——verified 强制携带异机 restore 主机、checksum 匹配与业务 smoke 三项证据；`storage_ref` 仅允许 `env:MAESTRO_*` 引用（与 Webhook Secret 同纪律）。偏差：备份是控制面整库对象，不挂 project_id（遥测/审计按项目，备份按平台）；状态机方向由存储层守卫 UPDATE 承载，CHECK 只锁每状态形状。
12. `0014` `evaluation_records` 补 `trajectory_constraints` jsonb 列：冻结 evaluation-record.schema.json 的 wire 字段在 0012 投影中缺列，不补则写入会静默丢字段；默认 `[]` 保持既有行兼容（P4 期间发现的投影缺口，随 S1 评测存储切片落地）。
13. `0015` M4 浏览器 BFF 会话两表（任务书 E / M4-UI-001 UI-AUTH，权威：SEC-IDENTITY-RBAC §2/§7/§9）：`auth_login_requests`（OIDC 授权码握手的服务端 state——state 令牌只存 sha256 哈希、`consumed_at` 单次消费即 CSRF/重放边界、10 分钟过期）与 `auth_sessions`（不透明会话——cookie 令牌只存 sha256 哈希、`revoked_at` 终态撤销、有效期逐请求重查以传播撤销）。偏差：会话/登录请求生命周期未列入任何锚定卡表清单（M4-P4 补切片）；会话创建与撤销各在写事务内同步落 `audit_events`（`auth.session.created`/`auth.session.revoked`），满足"状态变更+审计原子"而非事后补记；绝对有效期 8 小时（安全文档未冻结 BFF 会话时长，登记为任务书 E 交接物决策项）。
14. `0016` W4.5 职能主体表 `functional_principals`（任务书 J1 / G-α / UI-4，权威：冻结 permissions.yaml `functional_approvers` + 试点蓝图 §1.3/§4.2）：一行 = 一次"用户×职能"授权（五职能枚举 security_owner/qa_owner/operations_owner/product_owner/technical_lead；有效期窗口 + 终态 `revoked_at` + 授权来源引用文本 `source_ref`）。部分唯一索引 `(user_id, function) WHERE revoked_at IS NULL` 承载"同职能单活跃授权"；与 `memberships` 并列不混用——职能 grant 只授予冻结 functional_approvers 权限，不叠加项目权限，冻结条件 `project_membership_required` 仍由 memberships 派生。偏差登记：(a) 授权书追溯（绑定必须指向一份可解析的有效授权书资产）依赖 J2a 台账，本切片只强制 `source_ref` 非空文本，登记为 J2a 合入后的补强切片；(b) 五职能中仅 security_owner/qa_owner 在冻结矩阵有 grant，其余三者可绑定但零授权，等契约 PR 扩展矩阵；(c) 无管理端点（与 memberships 同现状，SQL/store 层管理），控制台管理面登记为后续切片。J2a 迁移编号从 0017 顺延。
15. `0017` W4.5 Work Graph + 资产台账九表（任务书 J2a；权威：ADR-009（2026-09-10 四方批准）+ technical/work-graph-model.md 定稿版 + ADR-009 评审记录载入的 ARTIFACT-STANDARDS 台账语义）：`work_plans`/`work_nodes`（contains 为邻接列 parent_node_id + root/depth 冗余；结构列触发器不可变；root 由部分唯一索引锁单根）、`plan_revisions`（封板后触发器禁 UPDATE/DELETE）/`work_node_revisions`（全程仅插入）、`work_dependencies`（requires；复合外键 (id, plan_id) 把边钉死在同 plan 内）、`node_lineage`（followup_of/replacement_of 血缘）、`node_artifact_flows`（consumes/produces 边指向台账资产@版本）、`assets`（台账：(asset_id, version) 主键、状态流转触发器、内容列不可变触发器、supersede 单后继部分唯一索引、15 类类型目录 CHECK）、`asset_gate_bindings`（平面 work_items 的 locked_gate 消费桥）。偏差与决策（详见 ADR-009 评审记录）：(a) 编号与 J1 的 0016 并行协调、先合者定号，J2a 取 0017；(b) Intent 层表（business_problems/outcome_contracts/capabilities）与 ExecutionAttempt 五元组绑定延期至 J2b 拆解协议；(c) 资产类型目录 15 值冻结在表 CHECK，扩目录=新迁移（有意摩擦，防止目录静默漂移）；(d) requires 无环为应用层检测（PG 约束不可表达 DAG 无环，已知边界）；(e) 台账不存内容 blob——内容文件存试点仓 `assets/<asset_id>/`，台账只存 digest+指针+摘要；(f) expand 第一步纯增量，不动任何既有表与列；影子构图/双读/切换写入是后续契约仪式决策。
16. `0018` W4.5 J3 Jira 连接器两表（任务书 J3，权威：plans/prep/pilot/SOLUTION-BLUEPRINT.md §1.1/§1.2——工作层设计件，V4 前需正式评审）：`jira_anchors`（WorkItem↔issue 双向锚定，§1.2 规则 3：锚定在创建时建立、未锚定 issue 不产生 Maestro 副作用；行内并列 SoR 侧期望镜像值与 Jira 侧只读快照，快照永不回写 `work_items.status`）与 `jira_reconcile_items`（字段分歧清单，§1.2 规则 2：SoR 胜出、人工裁决后才更新镜像；部分唯一索引承载"每锚点每字段至多一条 live 分歧"；两个对账周期未裁决升级 owner + 审计事件）。偏差：编号与 J1（0016）/J2a（0017）并行协调预留；标题/状态标签的 SoR 值不入锚点行（镜像时从 `work_items` 实时读取），指派人/迭代标签的 SoR 值持在锚点行（Maestro 无同名系统字段，锚点行即 SoR 载体）；升级审计动作 `jira.reconcile.escalated` 与裁决审计动作 `jira.reconcile.resolved` 均与状态变更同事务落 `audit_events`。
17. `0019` W4.5 J2b 拆解协议与执行绑定七表（任务书 J2b；权威：ADR-009 写实决策 #2 + technical/work-graph-scheduler.md 定稿版 + work-graph-model.md §6 映射）：`business_problems`/`outcome_contracts`（Intent 层，契约版本行 insert-only）/`capabilities`+`problem_capability_links`/`work_plan_intents`（plan 主绑定唯一）、`work_patterns`（版本化拆解模板，body 不可变 + draft→active→retired 流转触发器）、`decomposition_proposals`（协议台账：payload 原文 + 稳定错误码 violations JSON + 幂等键唯一 + 决策后行不可变触发器）、`execution_attempts`（五元组绑定 principal/role/session/worker/worktree + node_revision+spec_digest+context_digest；绑定列不可变触发器、单活跃 Attempt 部分唯一索引、retry_of 自引用、Lease 围栏列 lease_token/epoch/version/generation、状态机 running→终态单向）。偏差与决策：(a) `budget_ledgers.scope_kind` CHECK 扩 'work_node'（唯一既有表变更，纯扩枚举可完整回滚）——图节点预算台账按"spec 预算=节点跨 attempt 累计上限"语义，重试预约剩余额度，余额为零 fail-closed 不可领取；(b) 平面 leases 表不动（work_items 外键专用），图路径 Lease 围栏内嵌于 execution_attempts，同一 CAS/代际围栏纪律；(c) 编号顺延 J3（0018）；(d) outbox 事件类型（workgraph.proposal.*/node.claimed/attempt.* 等）沿 J2a 先例暂不登记 events.yaml，登记为后续契约仪式项；(e) 拆解协议四类校验（作用域/环/预算/资源）的域逻辑在 `internal/workgraph`（纯函数、零内部依赖），store 层只做装配与持久化。
18. `0020` P5 J5 平台授权表 `platform_grants`（任务书 J5 / CR-P5a-1，权威：冻结 permissions.yaml `roles.platform_admin` + 试点蓝图 §1.3/§4.2）：一行 = 一次"用户×平台角色"授权（枚举暂只 platform_admin，扩枚举=新迁移；有效期窗口 + 终态 `revoked_at` + 授权来源引用文本 `source_ref`——与 0016 职能主体同一授权书纪律）。部分唯一索引 `(user_id, role) WHERE revoked_at IS NULL` 承载"同角色单活跃授权"；与 `memberships`（0001 CHECK 有意禁 platform_admin 入项目）及 `functional_principals`（只覆盖 functional_approvers 权限串）三方并列——平台 grant 只授予冻结 platform_admin 角色权限（pilot.write/gitlab_instance.configure 等），不需要项目 membership，不叠加职能权限。偏差登记：(a) 平台角色目录单值冻结在表 CHECK，扩目录=新迁移（沿 0017 资产目录的有意摩擦先例）；(b) 无管理端点（与 memberships/functional_principals 同现状，SQL/store 层管理），控制台管理面登记为后续切片；(c) 授权书追溯与 0016 同口径——只强制 `source_ref` 非空文本，绑定指向可解析授权书资产的校验随 J2a 补强切片统一处理。

## SQLite → PostgreSQL 导入映射表

命令：`maestro pg-import --sqlite PATH [--dry-run] [--reconcile] [--report FILE]`（目标 DSN 来自 `MAESTRO_DATABASE_DSN`/配置）。

### 导入（幂等，按 source row identity）

| SQLite 源表 | PG 目标表 | 变换 |
| --- | --- | --- |
| `projects` | `teams` + `projects` | 每项目自动建 `legacy-<slug>` team；`workspace_path` 不入库（记入 `legacy_id_map.metadata`）；`status` 值域映射（active/archived 直传）；**不建 membership——owner 未知进人工清单**（SEC-IDENTITY-RBAC §13） |
| `features` | `features` | 直传；`reference_urls` 校验 JSON 后转 jsonb |
| `tasks` | `work_items` | 状态经 `LegacyTaskStatusToCanonical` 归一；`feature_id` 经映射表换 UUID；`role/assigned_session_id/assigned_worker_id` → `role/legacy_session_id/legacy_worker_id`；`dependencies/test_requirements/forbidden_patterns` 校验 JSON 后转 jsonb；`allowed_directories/required_apis` 不迁移（记入 metadata） |
| `task_leases` | `leases` | 状态枚举一致（active/completed/released/expired/cancelled）；`session_id/worker_id` → legacy 文本列；`attempt=1` |
| `worktrees` | `worktrees` | 状态枚举为 TECH-WT-001 规范集的子集直传；`worktree_path` → `workspace_path`（runner 侧元数据） |
| `validation_runs` | `validation_runs` | `authority='diagnostic'`、`producer='maestro-local'` 固定；`test_command` 不迁移（命令字符串不入库，CLAUDE.md 红线）；attempt 唯一索引冲突 → quarantine |

幂等性：目标行 UUID 记入 `legacy_id_map(source_table, source_id)`；重跑先查映射，已存在则跳过（imported=0）。整个导入单事务，任何错误整体回滚。

### 不导入（报告记录原因，非错误）

| SQLite 源表 | 处置 | 原因 |
| --- | --- | --- |
| `agent_sessions` `agent_workers` | skip | 运行时状态非持久事实；引用经 legacy 文本列保留 |
| `task_results` | skip | 被 validation evidence 取代（ARCH：本地结果仅 diagnostic） |
| `activity_log` | skip | M0 UI feed，可由事件流重建 |
| `audit_log` | skip | M0 token 时代审计与 v3 AuditEvent 语义不同，留在 SQLite 归档 |
| `api_contracts` | skip | M2 连接器范围 |
| `idempotency_records` `project_queue_versions` `state_history` `runtime_state` | skip | 短 TTL 运行时状态 |

### quarantine（不得静默修复）

非法状态、无法解析的时间戳、非法 JSON、跨项目外键引用、attempt 冲突 → 逐行记录 source identity + 原因，导入其余行，人工清单随报告输出。

## 回滚 runbook（cutover 前）

1. `maestro migrate up`（前向，记录版本）。
2. `maestro pg-import --dry-run` → 报告人工复核。
3. `maestro pg-import`（单事务）→ `--reconcile` 对账（行数 + 逐表 checksum + 不变量：active lease 唯一、状态枚举合法、外键完整）。
4. read-only compare / shadow read（P4 提供双读开关后启用）。
5. 回滚演练：`maestro migrate revert --steps 1` → 验证 schema 清空 → 再次 `migrate up` + 重复导入验证幂等（第二次 imported=0）。
6. cutover 后：只允许 PITR/forward-fix，禁止回 SQLite 双写（ADR-002 §13）。

## 本地钻孔

`scripts/m1-pg-drill.sh` 一键执行上述 1–5 步（Compose PG + 样例 SQLite），是 P3 出口 Gate 的 Evidence 生成器。
