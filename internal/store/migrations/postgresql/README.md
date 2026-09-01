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
5. `0007` 为 `work_items` 补 `merged_fact_id`（GL-INV-003：done 由 merged webhook 或对账确认并记录来源事件；SQLite 侧 task_store 已有该列，0001 建 PG 基线时未带）。
4. `0006` 项目质量策略存储 `quality_policies`（单行/项目 + CAS row_version，对应 putProjectQualityPolicy 的 If-Match/If-None-Match；公司基线走二进制内嵌，不入库），并为 0004 未建模的冻结 wire 必填列补齐：`evidence.attempt`（flaky 重试簿记）、`evidence.status`（质量结论枚举）、`evidence.sensitivity`（数据分类，缺省 confidential）、`gate_snapshots.version` 与 `waivers.version`（ResourceVersion / If-Match）、`waivers.merge_request_iid`（豁免的 MR 绑定）。merge_gate 证据的 `pipeline_id`/`job_id` 外键暂为 NULL，数值 GitLab ID 记录在 producer 载荷内——投影行随 S4a 连接器落地后回填（偏差注记）。

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
