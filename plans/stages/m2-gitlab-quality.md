# M2 执行计划（波次 W2 → 收敛点 V2）

> 目标：自建 GitLab 安全接入、远端 baseline、任务分支/MR/Pipeline 同步、exact-SHA 权威质量闭环。非目标：平台自动合并、推保护分支、本地诊断授权合并。
> 权威依据：`docs/delivery/m2-gitlab-quality-loop.md`、ADR-005/006。本地 GitLab CE 容器先行，公司 GitLab 接入为可选升级。

## P1 文档规划（I2 开局；W1 后半已由 S4 预备启动）

- 文档推进：m2 任务书 review→approved；`technical/gitlab-integration.md`、`security/secrets-webhooks-supply-chain.md`、`quality/gates-and-evidence.md`、`quality/quality-policy.md` not_started→approved（质量两文档是本阶段规则核心）；ADR-005（GitLab 人审合并）、ADR-006（CI Evidence 权威）→ approved。
- 需求锚定卡：六任务（GL/WHK/GIT/MR/QG/SEC），Test ID 以 m2 书第 12 章清单为准，P1 时逐卡提取（不得臆造 ID）。
- 关键验收锚点（摘自 Exit Gate）：无效签名无业务效果；重复/乱序事件恰好一次；SHA 漂移立即阻断 ready、旧 Evidence stale；缺任一 Required Gate 不得 ready；Maestro 无法推送/合并保护分支且未调用 merge API；GitLab 不可用时只读缓存、不新授权、恢复对账通过。

## P2 实现方案（I2 契约冻结）

- 契约 PR：Webhook Inbox 事件信封与签名验证接口（`docs/specs/asyncapi/events.yaml` 扩展 GitLab 事件目录）、MR/Pipeline/Job 同步模型、Evidence/Gate/Waiver 状态机字段（`docs/specs/schemas/evidence*.schema.json` 定稿）、**状态机咽喉点变更：`ready_for_human_merge → done` 入边开启**（仅由签名验证的 merged Webhook 或对账确认，`internal/model/state_machine.go`）。
- 保护分支防护设计：Runner host Git broker 独占 `maestro/*` 推送（成员凭据 OS Keychain）；中央 GitLab Bot 无源码推送能力。
- S4 内部拆分点：S4a（GL 接入 + GIT 分支 + MR 同步）/ S4b（WHK 收件箱 + QG 质量引擎）。
- 出口 Gate：契约 PR 合入；`ruby scripts/spec-consistency-check.rb`、`node scripts/asyncapi-check.mjs`、`node scripts/mermaid-check.mjs` 通过。

## P3 数据模型建设（S1 支持，S4 主导）

- 新表：webhook_inbox（去重键/签名/原始载荷）、dlq、merge_request/pipeline/job 映射、gate_snapshot、evidence（append-only + supersedes 链）、waiver（审批/到期/撤销）。
- 迁移：`done` 入边开启的 schema 配套；对账游标（last seen event id）；expand/contract。
- 出口 Gate：本地 GitLab CE + PG 上迁移与回滚演练；`ruby scripts/schema-check.rb` 通过。

## P4 代码工程建设

| 承担 | 任务 | 落点 |
|---|---|---|
| S4a | M2-GL-001、M2-MR-001 | `internal/gitlab/`（新：connector/onboarding）、MR/Pipeline 同步、merged 真相 → `done` |
| S4b | M2-WHK-001、M2-QG-001 | `internal/webhook/`（新：verify/inbox/DLQ/replay）、`internal/evidence/`（新：policy/gate/waiver 引擎） |
| S3 | M2-GIT-001 | `internal/runner/` git broker：远端 SHA、命名分支、保护分支防护、Keychain 凭据 |
| S4 全程 | M2-SEC-001 | Token/Secret/Webhook 防护、供应链（签名/provenance 落地到构建） |

出口 Gate：每流 `make build test vet lint` + test-hygiene 全绿；本地 diagnostic Evidence 与 GitLab `merge_gate` 权威分离实现正确。

## P5 测试验证（convergence/v2 剧本）

签名无效/重放/乱序/重复事件；SHA 漂移与 Evidence stale；Required Gate 缺失阻断 ready；不可豁免项（身份隔离/SHA 完整性/策略完整性/Webhook 真实性）无绕过路径；保护分支推送与 merge API 调用双重防护；GitLab 容器停机 → 只读降级 → 恢复对账；M0/M1 回归全量。

## P6 质量工程（V2 收敛仪式）

含阶段特定审计：Evidence 权威性审计（diagnostic vs merge_gate 无混淆）、豁免流程审计（审批人隔离/期限/SHA 绑定）、Webhook Secret 管理审计；m2 书 + 矩阵 M2 六行翻转。

## 时序估算

P1–P2（含 W1 预备承接）3–5 天 → P3 2–3 天（并行）→ P4 1–1.5 周 → P5 3–5 天 → P6 1–2 天。W2 总粗估 2–3 周。
