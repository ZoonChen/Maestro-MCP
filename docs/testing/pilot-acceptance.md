---
doc_id: TEST-PILOT-ACCEPTANCE
spec_version: 3.0
spec_status: review
implementation_status: not_started
verification_status: unverified
owner_role: qa_owner
approver_roles: [product_owner, qa_owner, security_owner, operations_owner, technical_lead]
introduced_in: M4
authority_for: [pilot_entry_criteria, pilot_exit_criteria, go_no_go, acceptance_evidence]
related_adrs: [ADR-001, ADR-002, ADR-003, ADR-005, ADR-006, ADR-007]
related_specs: [../specs/openapi/control-plane.yaml, ../specs/openapi/runner.yaml, ../specs/asyncapi/events.yaml, ../specs/rbac/permissions.yaml]
related_tests: [integration-test-plan.md, mcp-test-guide.md, gitlab-sandbox-plan.md, agent-evaluation-redteam.md]
last_verified_commit: null
---

# 公司内部试点验收与 Go/No-Go

> 本文件定义 v3 试点门槛，不表示当前项目已满足进入试点条件。任何阶段都保留人工最终合并，试点不得用生产核心仓库验证未完成能力。

## 1. 目标与非目标

`PILOT-REQ-001`：用有限项目和真实团队流程证明 Maestro 能安全协调 Agent、Runner、GitLab MR/Pipeline、缺陷和质量 Gate。`PILOT-REQ-002`：形成可审计的 Go/No-Go 决策和退出路径。试点不验证全公司规模、自动合并、跨组织 SaaS 或无人值守高风险修复。

## 2. 参与者、角色、权限和信任边界

Product Owner 负责业务价值；Technical Lead 负责功能完整；Security/QA/Operations Owner 分别拥有安全、质量、运行否决权；两个试点项目各指定 Project Admin、Coordinator、Developer、Verifier。最终 merge 由 GitLab 人类 Maintainer 执行，Agent、Maestro Bot 和 Runner 均无此权限。

## 3. 触发条件、输入和前置条件

进入条件：M0–M4 对应 required Gate 全部有当前提交 Evidence；clean clone 构建/部署通过；威胁模型无未缓解 HIGH/CRITICAL；身份、项目隔离、Runner、GitLab Sandbox、备份恢复和应急演练通过；Runbook/值班人就绪。选择两个非核心、私有且可回滚项目，至少 6 名成员覆盖全部业务角色，试点持续 10 个工作日。

## 4. 正常交互及时序图

| 阶段 | 时长 | 允许能力 | 出口 |
| --- | ---: | --- | --- |
| Shadow | 2 日 | 只读同步、策略影子计算，不创建远端变更 | 与 GitLab/人工结果一致 |
| Assisted | 3 日 | 人确认后创建任务分支/MR，所有 Tool 可见 | 无越权，Gate 与人工判断一致 |
| Controlled Active | 5 日 | 批准的低风险 Agent 修复与联调流程 | 达到业务、质量、安全、SLO 门槛 |

每天进行 15 分钟运行评审，记录问题、Waiver、人工接管和下一步；阶段升级必须由五个 Owner 共同确认。

## 5. 失败、取消、超时、重试、恢复和用户提示

发生跨项目访问、Secret 泄漏、保护分支写入、错误 merge、不可追溯 Evidence、数据损坏或未缓解 HIGH 事件立即停止全部写能力。单 Runner/Webhook/DB 故障按 Runbook 处理；恢复前验证审计、幂等、SHA 和 Gate。用户始终看到系统状态、last sync、责任人、人工接管和恢复预计，不允许静默降级。

## 6. 状态机、规则和不可变式

Pilot：`planned → entry_review → shadow → assisted → controlled_active → exit_review → accepted/rejected/suspended`。

- `PILOT-RULE-001`：任何否决 Owner 可因其领域阻断升级；解除需原 Owner 复核。
- `PILOT-RULE-002`：所有远端变更经 MR，最终 merge 为人类动作。
- `PILOT-RULE-003`：生产依赖异常时只读或停止，不关闭认证、Gate、Webhook 验证或沙箱。
- `PILOT-RULE-004`：试点项目、成员、Runner 和预算固定，扩围视为新决策。
- `PILOT-RULE-005`：Waiver 不得用于通过试点安全或恢复门槛。

## 7. 字段、配置和格式校验

试点清单固定 GitLab Instance/project numeric ID、目标分支、成员/角色、Runner、Quality Policy digest、Command Profiles、预算、允许 Tool 和退出联系人。每个验收场景记录 requirement/test ID、source/target SHA、pipeline/job、Evidence digest、执行时间、结果、缺陷和批准人；不得用截图替代机器记录。

## 8. 并发、幂等和一致性

试点至少验证两个项目、两个 Runner、多客户端、并发 WorkItem、Webhook 重投、Runner 重连、Pipeline retry 和 target branch 前进。断言无跨项目串扰、重复副作用或 stale Gate；停机恢复后 Inbox/Outbox/Lease/WorkItem 与 GitLab 对账一致。

## 9. 安全、Secret、隐私和审计

试点只使用企业批准数据，Prompt/日志/Artifact 按项目访问和保留；启用 canary Secret 与日志扫描。所有高风险动作、授权拒绝、Runner/Profile、GitLab API/Webhook、Gate/Waiver、人工 merge、Runbook 和配置变更可由单一 correlation ID 追踪。试点结束轮换 Bot/Runner 临时凭据。

## 10. 质量门禁、证据与 fail-closed 规则

Go 条件全部满足：0 次跨项目/未授权写/Secret 泄漏/保护分支违规；0 个未关闭 HIGH/CRITICAL；Required Gate 100% fail-closed；任务/缺陷/MR/Pipeline/merge 审计可追踪率 100%；关键 Agent 安全和质量评测达到既定门；最终合并 100% 由人完成。业务目标为至少 20 个代表性 WorkItem，≥90% 无平台缺陷完成，前后端联调与缺陷闭环各至少 5 个。

## 11. 指标、SLO、告警和运维动作

试点期间 Control Plane 可用性 ≥99.5%；普通 API P95 <500ms；Webhook 持久化 P95 <2s、正常事件 60s 内收敛；Runner offline 在 90s 内识别；审计覆盖 100%。完成一次 RPO≤15 分钟、RTO≤4 小时的恢复演练和一次凭据吊销演练。SLO 未达但无安全风险时延长试点，不得直接 Go。

## 12. 验收测试和需求追踪

- `TC-PILOT-001`：M0–M4 required traceability 与 Evidence 完整。
- `TC-PILOT-002`：两个真实项目完成分支/MR/Pipeline/Gate/人工 merge 闭环。
- `TC-PILOT-003`：联调、测试问题下发、缺陷发现/修复/复验闭环。
- `TC-PILOT-004`：Runner/Webhook/DB/Token 故障 Runbook 与恢复演练。
- `TC-PILOT-005`：Agent 评测、人工反馈、SLO 和审计出口达到阈值。

Go 需五个 Owner 签署；任何 required Evidence 缺失即 No-Go，不接受口头例外。

## 13. 数据迁移、兼容、发布与回滚

试点启用按 project feature flag，先 shadow 后 assisted；数据库采用 expand/migrate/contract。停止时关闭新 Lease/WorkItem，排空或取消执行，保留 MR 供人工处理，撤销 Token/Runner，导出审计并恢复 GitLab 原配置。回滚不得删除 v3 数据、Inbox 水位或 Evidence，也不得重新启用本地 merge、共享 Token或任意命令。
