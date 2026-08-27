---
doc_id: TEST-INTEGRATION-PLAN
spec_version: 3.0
spec_status: review
implementation_status: partial
verification_status: unverified
owner_role: qa_owner
approver_roles: [qa_owner, technical_lead, security_owner, operations_owner]
introduced_in: M0
authority_for: [test_layers, integration_environments, release_regression, failure_injection]
related_adrs: [ADR-001, ADR-002, ADR-003, ADR-004, ADR-005, ADR-006]
related_specs: [../specs/openapi/control-plane.yaml, ../specs/openapi/runner.yaml, ../specs/asyncapi/events.yaml, ../specs/mcp/tools.schema.json]
related_tests: [mcp-test-guide.md, gitlab-sandbox-plan.md, agent-evaluation-redteam.md, pilot-acceptance.md]
last_verified_commit: null
---

# v3 集成与端到端测试计划

> 当前实现说明：M0 已建立真实 binary、严格 stdio/Streamable HTTP MCP 生命周期、REST auth/Origin、Web/WebSocket、真实 Git/worktree、MCP claim/heartbeat/submit、成功与失败 Evidence、上下文失败原子补偿、逐资源状态历史、并发、Session/Lease 清理及非空数据库重启恢复测试；另覆盖 24 路并发 migration、schema manifest 伪造/损坏、live server 与非 owner stdio Runner 共存、第二 maintenance owner 拒绝、本地 `done` 禁令、公共错误与 Secret canary 持久化脱敏。旧的宽松状态码、提前 return 和“REST equivalent MCP”套件已从活动测试中移除。本地还通过 race/vet/lint、核心覆盖率、干净源码快照、Docker/Compose 安全 smoke、SBOM 和源码/镜像扫描。完整 v3 Tool/RBAC、PostgreSQL、rootless OCI 和 GitLab sandbox 属于 M1/M2，当前工作树也尚无目标提交 CI 证据，因此保持 `partial/unverified`。

## 1. 目标与非目标

`TEST-REQ-001`：证明身份、项目隔离、任务、Runner、GitLab、质量 Gate、审计和恢复作为一个系统满足规范。`TEST-REQ-002`：测试必须在干净、可复现环境运行并验证真实副作用，而非只断言 HTTP 状态。本文不以 Mock-only 测试替代 PostgreSQL、GitLab、OCI 沙箱和网络故障验证。

## 2. 参与者、角色、权限和信任边界

QA Owner 维护计划和出口；各领域 Owner 提供 fixture/oracle；CI Runner 执行自动套件；Security/Operations 分别批准红队和恢复证据。测试账户覆盖全部角色并按项目分离；测试 Token、GitLab sandbox 和 Artifact Store 与生产完全隔离，Agent 输出一律作为不可信输入。

## 3. 触发条件、输入和前置条件

每个 MR 运行 fast suite；main 运行完整 integration；每日运行 GitLab/恢复/安全 suite；候选版本运行 pilot acceptance。前置条件：干净 clone、锁定工具链与镜像 digest、迁移从空库成功、fixture 版本固定、时钟可控、无生产凭据、所有服务 readiness 正常。

## 4. 正常交互及时序图

| 层 | 必测范围 | MR | main/nightly |
| --- | --- | --- | --- |
| Schema/static | Markdown、OpenAPI、AsyncAPI、JSON Schema、RBAC、迁移 lint | 是 | 是 |
| Unit/property | 状态机、策略合并、授权、幂等、解析器 | 是 | 是 |
| Component | PostgreSQL Store、Inbox/Outbox、GitLab client、Runner lease | 是 | 是 |
| Contract | REST/MCP/Runner/Event wire compatibility | 是 | 是 |
| Integration | Control Plane + PostgreSQL + Runner + fake/real GitLab | 精选 | 全量 |
| E2E/security/chaos | MR 到人工 merge、故障恢复、红队、备份恢复 | 否 | nightly/release |

主 E2E：创建项目映射→登录/授权→创建 WorkItem→Runner 领取 Lease→任务分支/MR→GitLab Pipeline→Gate→人类 merge→Webhook/对账确认 done→审计追踪。

## 5. 失败、取消、超时、重试、恢复和用户提示

测试失败必须分类 `product_regression/test_defect/infrastructure/flaky/security`。只有经规则识别的 infrastructure/flaky 可自动重试一次，初次结果保留；第二次失败阻断。套件超时终止完整进程树并保留诊断 Artifact。任何隔离、授权、数据完整性或 fail-closed 用例失败都不得 quarantine 或忽略。

## 6. 状态机、规则和不可变式

Run：`queued → provisioning → running → passed/failed/error/cancelled`；Evidence：`captured → verified → published`。

- `TEST-RULE-001`：每个测试独立 project/schema/namespace，随机顺序运行仍通过。
- `TEST-RULE-002`：所有写断言同时验证数据库状态、外部副作用、事件和审计。
- `TEST-RULE-003`：时间、UUID、重试和网络故障可控；不得依赖固定 sleep。
- `TEST-RULE-004`：测试不得放宽生产授权、TLS、Protected Branch 或沙箱配置。
- `TEST-RULE-005`：失败测试不能以重跑成功覆盖，必须计入 flaky 指标。

## 7. 字段、配置和格式校验

Fixture 使用版本化 synthetic 数据，至少含两个团队、三个项目、重复外部 ID、不可见项目、六种角色、两个 Runner、多个 SHA/MR/Pipeline 和中英文路径。每个 payload 覆盖合法边界、缺字段、未知 enum、超长、Unicode、NUL/路径穿越和重复幂等键。严禁复制生产源码、Token 或个人数据。

## 8. 并发、幂等和一致性

并发套件覆盖：双重 claim、Lease 过期与重派、重复/乱序 Webhook、Outbox 重投、成员撤销竞态、source/target SHA 漂移、策略切换和并发 Gate 聚合。每个场景验证最终状态、唯一业务效果、late/stale Evidence 与审计因果链。

## 9. 安全、Secret、隐私和审计

每次测试注入 canary Secret，并扫描 stdout/stderr、日志、trace、数据库、Artifact 和镜像层。测试销毁前吊销所有临时凭据。高风险测试记录审批、环境和执行人；测试报告只引用脱敏 Artifact，访问测试证据本身需授权。

## 10. 质量门禁、证据与 fail-closed 规则

MR Gate：静态、unit、race、contract 和核心集成全部通过；main Gate 再要求真实 PostgreSQL、Runner 隔离、GitLab sandbox 和安全用例。覆盖率要求遵循质量策略；关键授权/状态机/幂等模块低于 80% 阻断。缺少报告、测试未发现、skipped required suite、解析错误或 runner error 均失败。

## 11. 指标、SLO、告警和运维动作

记录通过率、duration P50/P95、flaky、覆盖率、失败分类、环境准备时间和缺陷逃逸率。MR fast suite 目标 15 分钟内，main 完整套件 45 分钟内；超过不是跳过理由，应拆分并行。连续两次环境故障由 operations_owner 处理，安全回归立即冻结发布。

## 12. 验收测试和需求追踪

- `TC-INT-001`：完整 MR/CI/Gate/人工 merge/审计链路。
- `TC-INT-002`：角色、项目和设备隔离矩阵。
- `TC-INT-003`：Runner 离线、GitLab/DB/队列故障、重启和重放恢复。
- `TC-INT-004`：重复/乱序/并发请求保持幂等和一致性。
- `TC-INT-005`：clean clone 可构建、迁移、启动、readiness、执行全套并清理。

每个 Requirement/Gate/Test/Evidence 必须登记 traceability matrix；未关联测试的需求不得声明 implemented。

## 13. 数据迁移、兼容、发布与回滚

所有迁移测试覆盖空库、v2 只读导入、上一 v3 minor 升级、失败中断、重跑和回滚二进制兼容。Control Plane/Runner 支持当前和前一 minor 的契约测试。发布候选必须保存不可变测试清单、配置 digest、镜像 digest 和报告；回滚后重跑安全、数据完整性和核心 E2E。
