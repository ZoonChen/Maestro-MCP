# M3 执行计划（波次 W3 → 收敛点 V3）

> 目标：OpenAPI 契约检查、跨仓 IntegrationRun、Finding/Defect 归一分派、受预算/工具/人工边界约束的 Agent 修复闭环。非目标：模型控制确定性状态/权限/Gate、无证据关闭 Defect、自动批准/合并。
> 权威依据：`docs/delivery/m3-integration-defect-automation.md`、ADR-007。W2 期间 S5 已对 mock MR 客户端完成大部分 P4 预备。

## P1 文档规划（I3 开局；W2 已预备）

- 文档推进：m3 任务书 review→approved；`prd/agent-remediation.md`、`prd/defect-and-test-issues.md`、`prd/end-to-end-workflows.md` not_started→approved；ADR-007（确定性 Workflow 管控 + Agent 灵活诊断）落地核对。
- 测试输入准备：至少两个试点仓库（Go/TypeScript）、OpenAPI golden cases（compatible/breaking 判定集）、红队注入集。
- 需求锚定卡：六任务（CTR/INT/DEF/DSP/AGT/BUD），Test ID 以 m3 书第 12 章为准逐卡提取。

## P2 实现方案（I3 契约冻结）

- 契约 PR：Finding 归一模型（六类来源）、Defect 唯一性 fingerprint/occurrence 契约、IntegrationRun（exact combination）模型、预算台账接口（pre-call gate / 真实用量记账 / 停止边界）、Agent 工具面（引用版本化 Command Profiles、无任意命令串、handoff 协议）。
- Agent 边界设计（红线）：Agent 不得改状态机/权限/Gate；无法复现时停止并 handoff 给人；高危及最终合并人检点。
- S5 拆分点：S5a（CTR + INT + DEF/DSP）/ S5b（BUD + AGT）。
- 出口 Gate：契约 PR 合入；spec-consistency/asyncapi/mermaid 检查通过。

## P3 数据模型建设（S5a 主导）

- 新表：api_contract 版本/hash、integration_run（组合键绑定 exact SHA 集）、finding（六类归一）、defect（fingerprint 唯一）、defect_occurrence、dispatch/responsibility 任务关联、budget_ledger（预算/实际用量/停止原因）、agent_run（轨迹/工具调用/产物引用）。
- 出口 Gate：迁移 + 回滚演练；`ruby scripts/schema-check.rb` 通过。

## P4 代码工程建设

| 承担 | 任务 | 落点 |
|---|---|---|
| S5a | M3-CTR-001 → M3-INT-001、M3-DEF-001 + M3-DSP-001 | `internal/contract/`（扩展 contract_service）、`internal/integration/`（新）、`internal/defect/`（新：归一/去重/分派） |
| S5b | M3-BUD-001、M3-AGT-001（收口，需 INT+DEF+BUD 就绪） | `internal/budget/`（新：pre-call gate/记账/停止）、`internal/agent/`（新：复现→修复→测试→MR→handoff 编排） |

出口 Gate：每流 `make build test vet lint` + test-hygiene 全绿；Agent 路径无任意命令串、无 token 透传。

## P5 测试验证（convergence/v3 剧本）

跨仓 breaking change 阻断并生成明确责任任务；Pipeline 失败归一/去重为唯一 Defect；Agent 在预算内复现→修复→创建 MR→CI 复测通过后才可关闭 Defect；预算耗尽/无法复现 → 停止交人，不输出无证据"已修复"；提示注入/恶意仓库文本不能扩大工具与数据权限；M0–M2 回归全量。

## P6 质量工程（V3 收敛仪式）

含阶段特定审计：预算对账审计（记账 vs 真实 provider 用量）、Agent 轨迹抽查（工具面越界检测）、红队用例覆盖核对；m3 书 + 矩阵 M3 六行翻转。

## 时序估算

P1–P2（承接 W2 预备）2–4 天 → P3 2 天 → P4 1–1.5 周 → P5 3–4 天 → P6 1–2 天。W3 总粗估 2 周。
