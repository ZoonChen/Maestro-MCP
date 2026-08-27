# S4 GitLab 与质量引擎流任务书（会话输入）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/PIPELINE.md` → 本文件 → 当前任务对应的 `plans/stages/m2-gitlab-quality.md` 与其权威文档。做 brief 之外的事之前先登记偏离项。

## 1. 使命与所有权

端到端拥有**GitLab 集成与质量证据引擎**：host/repository onboarding、Webhook 收件箱（验证/去重/DLQ/replay/对账）、MR/Pipeline/Job 同步与 merged 真相、质量策略/Gate/Evidence/Waiver 引擎、Token/Secret/供应链防护。红线：本地 Runner Evidence 永远只是 `diagnostic`，只有 GitLab CI `merge_gate` 是合并权威；Maestro 不推不合并保护分支。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | Evidence 权威、保护分支红线 |
| 2 | `docs/README.md`、`plans/PIPELINE.md`、`plans/DISCIPLINE-PHASES.md` | 治理与管线位置 |
| 3 | `docs/technical/gitlab-integration.md` | 集成权威 |
| 4 | `docs/quality/gates-and-evidence.md`、`docs/quality/quality-policy.md` | Gate/Evidence/Waiver 规则核心 |
| 5 | `docs/security/secrets-webhooks-supply-chain.md` | M2-SEC-001 权威 |
| 6 | `docs/specs/schemas/evidence*.schema.json`、`docs/specs/asyncapi/events.yaml`、`docs/specs/examples/evidence.gitlab-ci.json` | wire shape 权威 |
| 7 | ADR-005（人审合并）、ADR-006（CI Evidence 权威） | 架构决策 |
| 8 | `docs/delivery/m2-gitlab-quality-loop.md` 第 6/7 章 | 任务范围 |

## 3. 任务序列

| 波次 | 任务 | 主轴位置 | 收敛点 |
|---|---|---|---|
| W1 后半 | 预备：连本地 GitLab CE（S1 Compose 提供）验证连接器/权限模型/webhook 配置 | M2-P1..P3 提前量 | — |
| W2 | M2-GL-001（onboarding + 最小 scope） | M2-P4 | V2 |
| W2 | M2-WHK-001（raw verify、Inbox、去重、DLQ/replay/对账） | M2-P4 | V2 |
| W2 | M2-MR-001（MR/Pipeline 状态、merged 真相 → `done`） | M2-P4 | V2 |
| W2 | M2-QG-001（策略继承、Gate、Evidence、豁免） | M2-P4 | V2 |
| W2 全程 | M2-SEC-001（Token/Secret/Webhook/供应链） | M2-P4 横切 | V2 |
| W3 | 豁免机制加固、Gate 聚合性能（SLO：事件 60s 收敛、Evidence 30s 出结论） | M2 收尾 | V3 协同 |

## 4. 文件边界

- **可改**：`internal/gitlab/`（新：connector、onboarding、MR/pipeline 同步）、`internal/webhook/`（新：verify、inbox、DLQ、replay）、`internal/evidence/`（新：policy、gate、evidence、waiver 引擎）、`internal/service/validation_policy.go`、`internal/service/validation_service.go` 的 Evidence 权威分离改造
- **需协调**：`internal/model/state_machine.go`（`done` 入边开启——**必须走契约 PR**，本流只提变更请求）、`internal/service/task_service.go`（ConfirmMergedFact 从 disabled 改为 webhook 驱动）
- **禁改**（只随契约 PR）：`docs/specs/**`、`internal/handler/router.go`、`internal/store/interfaces.go`

## 5. DoD 与本地验收命令

- 流内门禁：`make build && make test && make vet && make lint && ruby scripts/test-hygiene-check.rb`
- Evidence 专项：`ruby scripts/schema-check.rb`（evidence schema 与 examples）；append-only + supersedes 语义有 DB 触发器级测试
- 负测试：无效签名、重放、乱序、重复事件；SHA 漂移 stale；豁免越权；GitLab 不可用降级

## 6. 交接物契约（向集成会话）

1. implemented 候选 Task ID 与 Evidence 指针
2. Webhook 恰好一次语义实测记录（重复/乱序注入用例）
3. Gate 聚合与豁免链路演示脚本（供 V2 联调）
4. Secret 管理清单（哪些句柄、存哪、轮换策略）供 M2-SEC 审计

## 7. 与其他流的接口

- **S3**：git broker 推送 `maestro/*`（本流只管 MR 元数据与保护分支配置）
- **S1**：Inbox/Outbox/gate_snapshot/waiver 表的存储语义
- **S2**：连接器服务端身份与 webhook 端点授权
- **S5**：pipeline 失败事件 → Finding 摄取；merge_gate 结果供 Defect 关闭判定
- **S6**：MR/pipeline 视图数据 API

## 8. 内部拆分点

W2 可拆两会话：**S4a**（GL + MR 同步 + 与 S3 的分支边界）/ **S4b**（WHK + QG）。M2-SEC-001 横切两线，由 S4b 牵头评审。
