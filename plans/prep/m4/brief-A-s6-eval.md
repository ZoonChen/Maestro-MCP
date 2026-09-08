# 任务书 A：S6-eval 会话（M4-EVAL-001 评测 harness）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（调度板，含环境约定）→ 本任务书。做任务书之外的事之前先登记偏离项。本任务书自包含。

## 0. 工作区准备

在主 checkout 执行（调度板第 5 节）：

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-s6eval -b s6/m4-eval-harness i4/m4-session-board
cd ~/Works/yuandong/projects/Maestro-MCP-s6eval
```

后续所有工作在该 worktree 内；分支 `s6/m4-eval-harness`。

## 1. 使命与所有权

从零建立 `tests/eval/`：Agent 四层评测 harness 的第一代——版本化数据集、deterministic/rule_based 判分器、符合冻结 wire 的记录产出与 pass^k 报告。**本会话只做 npm/TS 侧，禁改一切 Go 代码**；持久化到 `evaluation_records` 表的路径由 S1 已备好（`internal/eval` + `Evaluation()` 存储），本会话产出的 JSONL 记录即为将来入库的输入（在交接物中登记入库接线需求，由集成会话裁决）。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | 评测红线：预算先检后调、无 ground truth 不声称修复 |
| 2 | `docs/testing/agent-evaluation-redteam.md` | **评测权威**：四层定义、120 场景配比（40 质量/30 轨迹/40 安全/10 能力，70% 回归+30% holdout）、每关键场景 ≥3 trial、pass^3 初始门 ≥80%、judge 校准 kappa ≥0.70 |
| 3 | `docs/specs/schemas/evaluation-record.schema.json` | 冻结 wire：记录字段、两个条件必填（security⇒risk、fail⇒notes） |
| 4 | `docs/delivery/m4-governance-console.md` 的 M4-EVAL-001 行 | 任务范围与 Test ID（TC-EVAL-001） |
| 5 | `internal/eval/eval.go` 的包文档与 `PassPowerK` | Go 侧已定语义：**pass^k = k 次 trial 全部通过**（C(passes,k)/C(n,k)）；npm 侧报告计算必须同语义 |
| 6 | `plans/prep/m4/session-board.md` | 环境与协作纪律 |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| A1 harness 骨架 | `tests/eval/package.json`（独立 npm 项目）+ runner：读数据集 → 逐 case 执行适配器 → 判分 → 写记录 → 汇总报告。执行适配器首版为 `mock`（无真实 Agent 依赖），接口预留 `mcp-stdio` 适配器位 | `npm --prefix tests/eval ci && npm --prefix tests/eval test` 绿 |
| A2 数据集格式 | 版本化 JSON：`schema_version/cases[]`，每 case 携带权威必填字段（case_id/category/risk/initial_state/inputs/allowed_tools/forbidden_actions/expected_outcome/trajectory_constraints/budget/scorer/version）；数据集 digest = 规范化序列化后的 `sha256:` 值；种子数据集含少量示范 case（正式 120 场景属后续任务，交接物登记） | digest 稳定性测试：重排无关空白不改 digest |
| A3 判分器 | `deterministic`（期望结果精确比对）与 `rule_based`（禁用动作/轨迹约束检查）两类；verdict 五值（pass/fail/error/blocked/skipped）；fail 必带 notes、security 层必带 risk——在 harness 内 fail-closed 拒绝不合规记录 | 负测试：构造违规记录被拒 |
| A4 记录产出 | JSONL：每 trial 一行，字段与冻结 schema 一致；`npx ajv-cli` 对产出记录全量校验通过（schema-check 同款工具链） | AJV 校绿 |
| A5 报告 | 汇总 JSON：样本数、各层 pass 率、**pass^3（同 Go 语义：全过概率）**、禁用动作观测清单 | 报告与手算一致的单测 |

## 4. 文件边界

- **可改**：`tests/eval/**`（全新目录）
- **需协调**：`Makefile`（新增 eval target）——登记变更请求，不直接改
- **禁改**：`internal/**`、`cmd/**`、`web/**`、`docs/specs/**`、`tests/e2e/**`、`docs/governance/traceability-matrix.csv`

## 5. DoD 与验收命令

```bash
npm --prefix tests/eval ci
npm --prefix tests/eval test          # 含 AJV 校验、digest 稳定性、判分器负测试
ruby scripts/test-hygiene-check.rb
```

（本会话不涉及 Go 代码，无需 Go 门禁；但不得使任何既有门禁变红。）

## 6. 交接物（回集成会话）

1. M4-EVAL-001 implemented 候选声明（harness 部分）与测试 Evidence 指针
2. 登记项：正式 120 场景数据集建设（QA/Security 协作）、LLM judge 校准（需 100 双标注样本）、JSONL → `evaluation_records` 入库接线（走集成会话）、`mcp-stdio` 真实适配器（M4 后续）
3. 偏离项清单（如有）
