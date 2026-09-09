# 任务书 H：评测入库与数据集切片（EVAL-1 前半）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → 本任务书。自包含。

## 0. 工作区准备

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-h2eval -b h/m4-eval-ingest origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-h2eval
make web-build && npm --prefix tests/eval ci
```

## 1. 使命与所有权

EVAL-1 登记项的工程面：**入库接线**（A 会话的 JSONL 记录 → `evaluation_records` 表）与**数据集骨架**（120 场景的格式与工具链）。judge 校准（100 双标注样本、kappa≥0.70）与场景内容撰写属 QA/Security 人工协作，只登记不代做。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `docs/testing/agent-evaluation-redteam.md` §3/§12 | 数据集权威：120 场景配比（40 质量/30 轨迹/40 安全/10 能力）、70% 回归+30% holdout、每关键场景 ≥3 trial、pass^3 门 ≥80% |
| 2 | `internal/eval/eval.go` + `internal/store/postgres_evaluation.go` | 记录校验与存储（已冻结，只消费）；`PassPowerK` 的全过语义 |
| 3 | `tests/eval/`（A 会话交付：README、datasets、src） | 现有 harness、数据集格式、JSONL 产出 |
| 4 | `docs/specs/schemas/evaluation-record.schema.json` | 入库 wire 契约 |
| 5 | `plans/prep/m4/session-board.md` | 纪律 |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| H1 入库命令 | `maestro eval-import --file run.jsonl --project <uuid>`：逐行解析 → `internal/eval.Record` 校验 → `Evaluation().AppendRecord`；重复键聚合报告（N 跳过）；driver=postgres 时可用，否则 fail-closed | PG 门控：合法/非法/重复三类行为 |
| H2 数据集骨架 | tests/eval 侧：数据集分片目录（regression/holdout 两目录 + 层别标记）+ 计数校验工具（断言 40/30/40/10 与 70/30 配比，不足时显式 FAIL 列缺口，不虚报）——种子场景每层 ≥3 个示范，正式内容登记为 QA/Security 协作 | `npm --prefix tests/eval test` 绿 |
| H3 round-trip | harness 产 JSONL → eval-import → `VerdictCounts` 读回与本地汇总一致（一条龙脚本） | PG 门控集成测试 |

## 4. 文件边界

- **可改**：`cmd/maestro/**`（子命令）、`internal/eval/**`（仅当需要 JSONL 解析辅助，不动校验语义）、`tests/eval/**`
- **禁改**：`web/src/**`、`internal/handler|store/**`（除新增只读辅助）、矩阵、`internal/slo|m4drill/**`

## 5. DoD 与验收命令

同任务书 E 第 5 节全套，另加 `npm --prefix tests/eval ci && npm --prefix tests/eval test`。

## 6. 交接物

1. 入库命令使用说明 + round-trip Evidence
2. 数据集缺口清单（供 QA/Security 撰写内容的精确工单）
3. 偏离项清单
