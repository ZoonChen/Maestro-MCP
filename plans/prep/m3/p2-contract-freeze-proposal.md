# M3 契约冻结提案（P2 预备输入，供 I3 契约 PR）

> 本文件只登记**变更请求**，不直接修改任何咽喉点文件（`docs/specs/**`、`internal/handler/router.go`、`internal/store/interfaces.go`、`internal/model/model.go`、状态机）。I3 评审采纳后由契约 PR 一次合入，各流 rebase。

## CR-1 事件目录扩展（docs/specs/asyncapi/events.yaml）

新增事件提案（命名对齐既有 `<domain>.<past-tense>` 风格，最终以 I3 冻结为准）：

| 事件 | 载荷要点 | 触发 |
|---|---|---|
| `contract.versioned` | service、version、canonical_hash、source_sha | CTR 解析归一后 |
| `contract.breaking_detected` | 两侧 hash、责任方映射 | breaking 判定时（阻断 + 责任任务生成） |
| `integration_run.completed` | manifest_hash、组合 SHA 集、status、evidence_ref | INT 终态 |
| `finding.created` | source_type、severity、source_event_id（幂等键） | 六类 adapter 归一后 |
| `defect.uniqued` / `defect.reopened` | fingerprint_hash、occurrence 计数 | DSP upsert / 复发 |
| `agent.run.handoff` | run_id、handoff 原因分类、checkpoint digest | AGT 停止交人 |
| `budget.exhausted` | ledger_id、reserve/spent、stop_reason | BUD 停止边界触发 |

## CR-2 机器 Schema 扩展（docs/specs/）

- `schemas/finding.schema.json`（新）：六类 source_type 枚举、severity、evidence_ref、repro 字段。
- `schemas/defect.schema.json`（新）：fingerprint 版本化结构、occurrence、reopen 语义、SLA 字段。
- `schemas/integration-run.schema.json`（新）：manifest 组合键、状态枚举 `waiting/running/pass/fail/cancel/expired`。
- `schemas/budget-ledger.schema.json`（新）：reserve/spent/actual_usage、四类 stop_reason。
- `mcp/tools.schema.json`（改）：新增只读工具 `list_defects`、`get_defect`、`get_integration_run`、`get_agent_run`；Agent 写路径仅 `report_agent_progress`（结构化，无自由文本命令）。身份一律服务端绑定，沿用 M1-MCP-001 口径。

## CR-3 store 接口扩展（internal/store/interfaces.go）

新增接口签名提案（实现随 P4 落地）：`ContractStore`、`IntegrationRunStore`、`FindingStore`、`DefectStore`、`BudgetLedgerStore`、`AgentRunStore`；均以 `AuthorizationContext` 入参，不接受客户端拼接 scope。

## CR-4 模型实体（internal/model/model.go）

新实体：`APIContract`、`IntegrationRun`、`Finding`、`Defect`、`DefectOccurrence`、`DefectTaskLink`、`BudgetLedger`、`BudgetEntry`、`AgentRun`。状态机仅新增 Defect 生命周期（`triaged/assigned/fixing/verified/resolved/reopened/quarantined`）与 IntegrationRun 五态；**不改** WorkItem 状态机。

## Agent 边界红线核对表（P2 出口必过）

| # | 红线 | 权威依据 |
|---|---|---|
| 1 | Agent 不得写 Workflow state；resolved/ready/done 由 Evidence 与外部事实决定 | m3 书 §6、ADR-007 |
| 2 | 无任意命令串——任务引用版本化 Command Profiles | CLAUDE.md 不变量、m3 书 §7 |
| 3 | 工具面结构化 Schema，错误结构化返回；不接受 shell/网络/Secret/权限自由文本 | m3 书 §7 |
| 4 | 预算先检后调、真实用量全计（并行/流式） | CLAUDE.md、`WF-REQ-003` |
| 5 | 无法复现 → handoff 且不输出"已修复"；高危及最终合并人检点 | m3 书 §10 |
| 6 | 同 Defect/SHA 单 active remediation | m3 书 §8 |
| 7 | 注入/恶意文本不扩大工具与数据权限 | m3 书 §9/§10 |
| 8 | Agent 轨迹脱敏加密保留 30 天 | m3 书 §9 |

## S5 拆分确认（供 I3 排期）

- **S5a**：M3-CTR-001 → M3-INT-001 → M3-DEF-001 + M3-DSP-001（`internal/contract/`、`internal/integration/`、`internal/defect/`）
- **S5b**：M3-BUD-001、M3-AGT-001 收口（`internal/budget/`、`internal/agent/`；AGT 收口需 S5a 的 INT+DEF 就绪并接真实 S4 MR 通道）
- W2 期间对 mock MR 客户端的 P4 预备分支允许先行（不触咽喉点、不合入）。
