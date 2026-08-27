# S6 控制台与评测流任务书（会话输入）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/PIPELINE.md` → 本文件 → 当前任务对应的 `plans/stages/m4-governance-pilot.md` 与其权威文档。做 brief 之外的事之前先登记偏离项。

## 1. 使命与所有权

端到端拥有**治理控制台、Agent 评测与试点**：角色化 IA 与八场景、HITL 审批队列、MR/pipeline 视图、写操作界面（受 remote_write 与 RBAC 约束）、四层评测 harness（质量/轨迹/安全/能力）、试点影子/灰度/验收。起点是既有只读 Preact Dashboard（`web/src/`），逐步演进。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | HITL、无绕过红线 |
| 2 | `docs/README.md`、`plans/PIPELINE.md`、`plans/DISCIPLINE-PHASES.md` | 治理与管线位置 |
| 3 | `docs/prd/web-dashboard.md` | 控制台语义权威 |
| 4 | `docs/prd/roles-and-scenarios.md` | 角色 IA 与八场景 |
| 5 | `docs/testing/agent-evaluation-redteam.md` | 评测权威 |
| 6 | `docs/testing/pilot-acceptance.md` | 试点验收权威 |
| 7 | `docs/operations/observability-and-audit.md` | 审计链展示与导出 |
| 8 | `docs/delivery/m4-governance-console.md` 第 6/7 章 | 任务范围 |

## 3. 任务序列

| 波次 | 任务 | 主轴位置 | 收敛点 |
|---|---|---|---|
| W1 | 预备：控制台 IA 设计、登录态组件底座（对接 S2 OIDC 授权码流） | M4-P1..P3 提前量 | — |
| W2 | 演进：MR/pipeline 视图（先 mock 数据）、HITL 队列原型 | M4-P4 预备 | — |
| W3 | 评测数据集与 judge 校准准备、agent_run 轨迹接入 | M4-P3..P4 提前量 | — |
| W4 | M4-UI-001（八场景、HITL/冲突/错误展示、a11y、真实浏览器 DOM 测试） | M4-P4 | V4 |
| W4 | M4-EVAL-001（四层评测 harness/datasets/judges/report） | M4-P4 | V4 |
| W4 | M4-PILOT-001（rollout flags、影子、灰度、人工验收报告） | M4-P4..P5 | V4 |

## 4. 文件边界

- **可改**：`web/src/**`、`web/vite.config.js`、`web/package.json`（依赖需评审）、`tests/eval/`（新）、`tests/e2e/specs-m0/` 下的浏览器用例新增
- **需协调**：`tests/e2e/playwright.config.ts`、`Makefile`（e2e/eval target）
- **禁改**：后端 Go 代码（`internal/**`、`cmd/**`）——后端缺口走交接物登记，由归属流实现；`docs/specs/**`

## 5. DoD 与本地验收命令

- 流内门禁：`make web-build && make e2e && ruby scripts/test-hygiene-check.rb`（后端相关回归由集成会话统一跑）
- UI 专项：真实浏览器 DOM 断言（补齐 M0 遗留增强）、a11y 基线（键盘可达 + 可访问名）、错误/降级状态永不为空
- 评测专项：judge 校准记录、数据集版本化、报告可复现（同数据集同版本同结论）

## 6. 交接物契约（向集成会话）

1. implemented 候选 Task ID 与 Evidence 指针
2. 后端缺口登记表（控制台需要的 API/事件，指明期望归属流）
3. 评测报告与 pilot 验收材料（供 V4 人工验收）
4. 真实浏览器测试清单与运行方式

## 7. 与其他流的接口

- **S2**：登录态与角色上下文
- **S4**：MR/pipeline/gate 数据 API 与事件
- **S1**：审计链导出、telemetry 展示
- **S5**：agent_run 轨迹供评测；Defect 分派/关闭的 HITL 界面

## 8. 内部拆分点

W4 可拆两会话：**S6a**（控制台 UI）/ **S6b**（评测 harness + 试点编排）。两者仅通过数据契约耦合，可完全并行。
