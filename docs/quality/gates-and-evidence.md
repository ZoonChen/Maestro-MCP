---
doc_id: QUAL-GATES-EVIDENCE
spec_version: 3.0
spec_status: approved
implementation_status: partial
verification_status: unverified
owner_role: qa_owner
approver_roles: [qa_owner, technical_lead, security_owner]
introduced_in: M2
authority_for: [evidence_model, evidence_authority, gate_aggregation, stale_evidence_semantics]
related_adrs: [ADR-005, ADR-006]
related_specs: [../specs/schemas/quality-policy.schema.json, ../specs/schemas/evidence.schema.json, ../specs/asyncapi/events.yaml, ../specs/openapi/control-plane.yaml]
related_tests: [../testing/integration-test-plan.md, ../testing/gitlab-sandbox-plan.md, ../testing/pilot-acceptance.md]
last_verified_commit: null
---

# Gate、Evidence 与 GitLab 事实源

> 本文规定 v3 目标 Evidence 语义，不表示当前仓库已具备 GitLab Pipeline、Evidence Store 或 Gate Engine。

## 1. 目标与非目标

`EVIDENCE-REQ-001`：每个质量结论必须能追溯到不可覆盖的原始 Evidence、精确代码版本、生产者和解析器。`EVIDENCE-REQ-002`：GitLab CI 是合并质量的权威执行源；Runner 本地结果只用于反馈速度和诊断。本文不允许用人工文本“已测试”替代机器 Evidence。

## 2. 参与者、角色、权限和信任边界

GitLab Pipeline/Job 产生权威构建、测试和扫描结果；Webhook Receiver/Connector 验证并摄取元数据；Artifact Store 保存原始报告；Parser 生成规范化 Evidence；Quality Engine 聚合 Gate；Verifier/用户查看理由。CI Job 内容也可能恶意，因此必须验证项目、Pipeline、Job、SHA、Artifact digest 和受保护配置来源。

## 3. 触发条件、输入和前置条件

Pipeline/job webhook、周期对账、Artifact 完成、source/target/policy 变化和人工重评触发聚合。前置条件：GitLab project mapping verified、event authenticated、Pipeline 对应 MR、source/target SHA 精确匹配、Job 名称与 producer allowlist 匹配、Artifact digest 可校验。

## 4. 正常交互及时序图

```mermaid
sequenceDiagram
  participant G as GitLab
  participant I as Webhook Inbox
  participant A as Artifact Store
  participant P as Parser
  participant Q as Quality Engine
  G->>I: verified pipeline/job event
  I->>G: reconcile pipeline, jobs and exact SHAs
  G-->>A: report artifact
  A->>P: immutable bytes + digest
  P->>Q: normalized append-only Evidence
  Q->>Q: evaluate effective policy
  Q-->>G: named status for exact source SHA
```

Maestro 可发布与 source SHA 绑定的命名 commit status；若 GitLab 版本/套餐支持，可增加 External Status Check。通用阻断方式是 required GitLab Pipeline/CI quality-gate job，不能依赖仅 UI 展示的状态。

## 5. 失败、取消、超时、重试、恢复和用户提示

Pipeline `failed/cancelled/skipped/manual`、Job 缺失、Artifact 404、digest 不符、解析错误或超时均形成阻断 Evidence，不转换为 passed。Webhook 遗漏由 Reconciler 拉取远端状态；GitLab 不可用期间只显示带 last_sync 的旧 snapshot，禁止 ready。用户提示必须链接 Pipeline/Job/Artifact，并区分代码失败、基础设施失败、策略失败和 stale。

## 6. 状态机、规则和不可变式

Evidence：`observed → verified → parsed → accepted/rejected → stale`；GateSnapshot：`collecting → blocked/ready → stale`。

- `EVIDENCE-RULE-001`：Evidence 只追加，不修改或覆盖；更正通过新 Evidence 和 supersedes 引用。
- `EVIDENCE-RULE-002`：每项 Evidence 绑定 project、MR、source SHA、target SHA、pipeline/job、policy digest、producer/parser version 和 content digest。
- `EVIDENCE-RULE-003`：相同 Gate 多个 Required producer 必须全部满足；禁止“最后一个成功覆盖先前失败”。
- `EVIDENCE-RULE-004`：source/target SHA 或策略变化使全部不匹配 Evidence stale。
- `EVIDENCE-RULE-005`：WorkItem done 只由 GitLab merged 事实确认，Gate ready 不等于 merged。

## 7. 字段、配置和格式校验

Evidence 必填 `evidence_id/kind/authority/project_id/source_sha/target_sha/pipeline_id/job_id/status/producer/version/content_digest/observed_at/parsed_at/policy_digest`；本地结果必须 `authority=diagnostic`。状态只接受 `passed/failed/error/cancelled/skipped`，不得省略。覆盖率、JUnit、SARIF、依赖/License 报告按固定 parser version 解析；未知 schema/version 一律 error。

## 8. 并发、幂等和一致性

Webhook event ID、GitLab object ID 与 Artifact digest 分层去重。Pipeline 重试产生新 pipeline/job ID 和新 Evidence，不覆盖旧失败；Gate Engine 按事务快照读取完整集合并以 snapshot version compare-and-swap。乱序事件以远端对象版本和对账结果收敛，不能只相信接收顺序。

## 9. 安全、Secret、隐私和审计

Artifact 使用服务端下载和短期授权链接，禁止把 GitLab Token 下发浏览器。报告入库前扫描 Secret 并按最小保留保存；原始日志不进入 Gate reason。审计事件摄取、验证、解析、拒绝、stale、重算、状态发布和人工查看敏感 Artifact，记录 digest 而非内容。

## 10. 质量门禁、证据与 fail-closed 规则

Baseline、boundary、policy integrity、build、unit、lint/typecheck、coverage 与策略要求的 integration/contract/security Gate 必须各有权威 Evidence。Job 允许失败、条件规则未创建 Job、Pipeline 来源不符合 MR 规则、状态发布到错误 SHA 或 status API 冲突未解决均阻断。External Status Check 若启用，只作为同一 Evidence 的附加执行点，不形成第二事实源。

## 11. 指标、SLO、告警和运维动作

监控 ingest/parse/evaluate 延迟、missing/error/stale Evidence、Artifact 下载/校验错误、Webhook 与 Reconcile 差异、commit status 冲突和 Gate 翻转。事件正常到达后 60 秒内收敛，Evidence 到齐后 30 秒内计算。DLQ、解析错误突增或 ready 后变 stale 触发告警。

## 12. 验收测试和需求追踪

- `TC-EVIDENCE-001`：真实 MR Pipeline 报告可追踪到 SHA、Job、Artifact digest 和 parser。
- `TC-EVIDENCE-002`：重复、乱序、重试 Pipeline 不覆盖失败且最终一致。
- `TC-EVIDENCE-003`：Artifact 缺失/篡改、schema 未知和 source/target 漂移均阻断。
- `TC-EVIDENCE-004`：Runner diagnostic Evidence 无法使 Required Gate 通过。
- `TC-EVIDENCE-005`：Webhook 丢失后 Reconcile 恢复且无重复业务效果。

## 13. 数据迁移、兼容、发布与回滚

旧 `validation_result` 只能导入为 `authority=legacy, status=unverified`。先并行摄取 GitLab 元数据与 Artifact，比较 Gate 但不发布 status；稳定后按项目切换。Parser 升级双跑并保留两版结果。回滚保留所有 Evidence、Inbox 水位与 stale 关系，不重新信任 legacy 或 local Evidence。
