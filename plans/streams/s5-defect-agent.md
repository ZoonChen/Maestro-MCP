# S5 缺陷与 Agent 流任务书（会话输入）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/PIPELINE.md` → 本文件 → 当前任务对应的 `plans/stages/m3-defect-agent.md` 与其权威文档。做 brief 之外的事之前先登记偏离项。

## 1. 使命与所有权

端到端拥有**缺陷归一与 Agent 修复闭环**：OpenAPI 契约检查（compatible/breaking）、跨仓 IntegrationRun、六类 Finding 归一与 Defect 唯一化分派、LLM 预算台账（pre-call gate + 真记账 + 停止边界）、Agent 复现→修复→MR→handoff 编排。红线：Agent 不得控制确定性状态/权限/Gate；无 Evidence 不关闭 Defect；预算先检后调；无法复现即停止交人。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | 预算、Agent 边界、Evidence 红线 |
| 2 | `docs/README.md`、`plans/PIPELINE.md`、`plans/DISCIPLINE-PHASES.md` | 治理与管线位置 |
| 3 | `docs/prd/agent-remediation.md` | Agent 闭环语义权威 |
| 4 | `docs/prd/defect-and-test-issues.md`、`docs/prd/end-to-end-workflows.md` | Finding/Defect 与端到端流程 |
| 5 | `docs/technical/contract-engine.md`、`docs/technical/defect-ingestion.md` | 实现权威 |
| 6 | ADR-007（Workflow 管控 + Agent 灵活） | 架构决策 |
| 7 | `docs/delivery/m3-integration-defect-automation.md` 第 6/7 章 | 任务范围 |

## 3. 任务序列

| 波次 | 任务 | 主轴位置 | 收敛点 |
|---|---|---|---|
| W2 | 预备：契约/数据模型设计（P1–P3）+ 对 mock MR 客户端的 P4 预备分支 | M3-P1..P4 提前量 | — |
| W3 | M3-CTR-001（OpenAPI hash、compatible/breaking 判定） | M3-P4 | V3 |
| W3 | M3-INT-001（exact-combination IntegrationRun/Evidence） | M3-P4 | V3 |
| W3 | M3-DEF-001 + M3-DSP-001（六类 Finding 归一、fingerprint 唯一 Defect、责任任务分派） | M3-P4 | V3 |
| W3 | M3-BUD-001（pre-call Gate、真记账、预算/停止边界） | M3-P4 | V3 |
| W3 | M3-AGT-001（复现、候选修复、测试、MR、handoff；收口需 INT+DEF+BUD 就绪，接真实 S4 MR 通道） | M3-P4 | V3 |

## 4. 文件边界

- **可改**：`internal/contract/`（扩展自 `internal/service/contract_service.go`）、`internal/integration/`（新）、`internal/defect/`（新：归一/去重/分派）、`internal/budget/`（新）、`internal/agent/`（新：编排、工具面、handoff）、`tests/` 下相关集成测试
- **需协调**：`internal/service/context_service.go`（Agent 上下文装配）、`internal/model/model.go`（新实体走契约 PR）
- **禁改**（只随契约 PR）：`docs/specs/**`、`internal/handler/router.go`、`internal/store/interfaces.go`、状态机

## 5. DoD 与本地验收命令

- 流内门禁：`make build && make test && make vet && make lint && ruby scripts/test-hygiene-check.rb`
- Agent 红线专项负测试：提示注入/恶意仓库文本不扩大工具与数据权限；无任意命令串；预算耗尽停止；无法复现 handoff 且不输出"已修复"
- 预算对账专项：记账值与 provider 返回的真实用量一致（用 mock provider 断言）

## 6. 交接物契约（向集成会话）

1. implemented 候选 Task ID 与 Evidence 指针
2. 红队注入用例执行记录（注入集逐条结果）
3. 预算对账报告（样本运行的真实用量对照）
4. Agent 轨迹样本（供 S6 评测与 V3 审计抽查）

## 7. 与其他流的接口

- **S4**：pipeline 失败事件 → Finding；MR 创建走 S4 通道；merge_gate 结果作为 Defect 关闭条件
- **S1**：finding/defect/budget/integration_run 表
- **S2**：Agent 工具调用的授权上下文（不得自报 scope）
- **S3**：Agent 执行经 Command Profile + 沙箱
- **S6**：评测 harness 消费 agent_run 轨迹

## 8. 内部拆分点

W3 可拆两会话：**S5a**（CTR + INT + DEF/DSP）/ **S5b**（BUD + AGT）。依赖顺序：S5b 的 AGT 等 S5a 的 INT/DEF 就绪后收口。
