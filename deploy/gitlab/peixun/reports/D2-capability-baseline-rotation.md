# D2 交接物：capability-gated 基线 3.1.0 轮换记录

> 会话 D2（F14 结构解·其二），2026-09-15。任务书：`plans/prep/m4/brief-D2-capability-gates.md`。
> 本文是工作层交接物（非权威真源）；权威语义见 `docs/quality/quality-policy.md` §7、`docs/quality/gates-and-evidence.md` §3.2 与 `docs/specs/schemas/quality-policy.schema.json`。

## 1. 基线 3.1.0 轮换记录

| 项 | 3.0.0（旧） | 3.1.0（新） |
| --- | --- | --- |
| 形态 | `required_gates` 12 门无条件必达（其中 8 门全系统无 CI 生产者） | 两档：core 6 门无条件 + `capability_gates` 8 门声明才必达 |
| core 6 | — | `build` `unit` `secret_scan`（CI 生产）+ `policy_integrity` `baseline_freshness` `boundary`（D1 控制面自证生产） |
| integration/contract | 文档特例（"涉及服务集成时增加"） | 收编进 `capability_gates` 统一机制 |
| 项目 overlay 加门 | 直接在 `required_gates` 追加（无需生产者声明——F14 病根） | 仅经 `capabilities` 声明（必带 producer 锚），声明与门列表双向一致 |
| effective 公式 | company 12 门 ∪ overlay 追加 | `core ∪ {g ∈ capability_gates : g.capability ∈ project.capabilities}` |
| 未声明 capability 门 | 永久 pending 噪音 | 不进 gate_snapshots；同名 CI 证据仅存为未评估记录 |
| 棘轮 | core 不可被 overlay 移除 | 不变（QG-RULE-001）；能力声明只增不减 |
| digest | （3.0.0 文档形状） | `sha256:c37d1b2efc095a7ca8c10dbbe455e12b64af63155ce5f54b42074793b1948266` |

**兼容性（fail-closed 预期行为）**：3.0.0 形状的存储 overlay（12 门、无 capabilities）在新二进制下 `ParsePolicy` 拒绝——"未声明生产者的必达门"正是要消灭的形态。存量 overlay 需按 3.1.0 重发（core 6 + 选择性声明）。peixun 试点零声明（无 overlay 行），不受影响。

**验证**：`docs 四检` 全绿（schema-check 13 schemas / 8 examples）；全量 Go（含 PG `-p 1`）全绿；lint 隔离缓存 0 issues；e2e 18 passed + 15 skipped（skips 为 P3 角色对调后的既有环境状态，见 §5）。

## 2. capability 声明与生产者锚对照表（冻结目录）

| gate_id | capability_key | producer_kind | 生产者语义 |
| --- | --- | --- | --- |
| coverage | `quality.coverage` | pipeline_job | repo 内名为 `coverage` 的 CI 作业 |
| lint_typecheck | `quality.lint` | pipeline_job | 名为 `lint_typecheck` 的 CI 作业 |
| license | `supply-chain.license` | pipeline_job | 名为 `license` 的 CI 作业 |
| sast | `security.sast` | pipeline_job | 名为 `sast` 的 CI 作业 |
| dependency | `supply-chain.dependency` | pipeline_job | 名为 `dependency` 的 CI 作业 |
| image | `supply-chain.image` | pipeline_job | 名为 `image` 的 CI 作业 |
| integration | `integration.enabled` | pipeline_job | 名为 `integration` 的 CI 作业（原文档特例收编） |
| contract | `contract.openapi` | pipeline_job | 名为 `contract` 的 CI 作业（原文档特例收编） |

声明样例（overlay）：

```json
"required_gates": ["build","unit","secret_scan","policy_integrity","baseline_freshness","boundary","coverage"],
"capabilities": [
  {"capability": "quality.coverage", "producer": {"repo": "peixun/backend", "job": "coverage"}}
]
```

强制点：schema（`$defs.capability_producer` 必填 repo/job；8 条 if/then 前向蕴含）+ 引擎（`validateCapabilityTiers` 双向一致）双重拒绝无锚声明。

## 3. A1-4/A1-6 → done 的 Evidence 指针

- **仓库内 PG 门控（CI 可复现）**：
  - `TestCapabilityBaselinePilotShape`（`internal/store/postgres_controlplane_test.go`）——零声明项目 → 快照恰 6 门全 passed（3 CI + 3 自证）→ `ready_for_human_merge`；声明对照：+1 门恰 7 快照、coverage 诚实 pending。
  - `TestControlPlaneA14TupleReplay`（同文件）——A1-4 形态历史元组重跑：三门自证 + build/unit CI → 补 secret_scan 后 Ready → ready writer 触发（3.1.0 下该回环自动收敛为 6 门版）。
  - `TestResolveEffectiveCoreOnlyForUndeclaredCapabilities` / `TestResolveEffectiveCapabilityDeclarationsAddGates`（`internal/evidence/effective_test.go`）——并集正确性与"未声明门不出现"。
- **常驻栈实测（D2-5，合并后窗口）**：见 §4 执行记录（重建 + 迁移 0022 + A1-4 重跑六门翻绿查询结果）。

## 4. 常驻栈重建窗口（D2-5）

- 配方：`deploy/gitlab/peixun/resident-server.sh`（build|migrate|up），窗口纪律按 ART-incident-002（登记 + 前后 pg_dump）。
- 顺序红线（J5）：先 `migrate`（D1 迁移 0022 落库）后换二进制。
- 验收：常驻库 A1-4 重跑评估 → 6/6 门 passed → `ready_for_human_merge`。
- 状态：待本 PR 合入后执行（结果回填本节）。

## 5. 偏离项与新登记缺口

1. **e2e governance 15 skips（既有，非 D2 回归）**：`tests/e2e/specs-m0/console-governance.spec.ts` 以 `maestro` 用户在 5434 常驻库 `CREATE DATABASE maestro_ui_e2e`——P3 角色对调（`maestro` NOCREATEDB）后本地 setup 失败优雅跳过；CI 无 5434 亦跳过。修复需裁决（dba 凭据接入 e2e 或专用 e2e 库），建议排独立小切片。
2. **常驻 PG 被停事件**：本会话开工时发现 `maestro-resident-postgres` 已被干净停止（exit 0，时间≈会话派发），常驻 server 处于 DB 失联状态。已恢复 `docker start`（无重建动作，库 110 项完整）。若为人为维护动作请知会调度板。
3. **基线 digest 轮换的存量影响**：评估元组 policy_version 将翻至 3.1.0；3.0.0 钉住的旧快照按 QG-RULE-003 自然 stale（StaleGateIDs 已按版本漂移处理，无需清洗）。

## 6. 校准回路建议（周报增栏）

`scripts/pilot/shadow-report.sh` 周报建议每门增列**拦截率**（该门 failed/error 快照数 ÷ 该门评估总数）与**噪音率**（pending 中"missing"占比）。两档制后预期：core 门拦截率非零（真实拦截），capability 门在未声明项目恒为零快照（零噪音）——若某已声明能力门拦截率长期为 0，按 RDOS 门禁校准纪律（"没拦住过=删"）复查该声明是否过强。数据源：`gate_snapshots` 按 check 聚合。
