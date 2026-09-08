# 任务书 D：S4-webhook-drill 会话（M4-RBK-001 webhook 侧演练锚点）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（调度板）→ 本任务书。做任务书之外的事之前先登记偏离项。本任务书自包含。

## 0. 工作区准备与开工前提

**前提**：`s1/m4-backup-metadata` 与 `s1/m4-drill-restore` 已合入 main（`internal/m4drill` 包在基线上）。在主 checkout 执行：

```bash
git checkout main && git pull
git worktree add ~/Works/yuandong/projects/Maestro-MCP-s4wh -b s4/m4-webhook-drill origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-s4wh
make web-build
```

## 1. 使命与所有权

为 M4-RBK-001 的 `webhook-pipeline-failure` Runbook 建立 `internal/m4drill` 的可执行演练锚点：故障注入（拒签/重复/乱序/DLQ 积压/GitLab 中断语义）→ Runbook 动作（重放/对账/升级）→ 审计与恢复断言。本会话是**演练锚点会话**，不重写 webhook 引擎（M2 已收敛），只在其上按 Runbook 章节组织断言；引擎缺口登记交接物。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | fail-closed、Evidence 纪律 |
| 2 | `docs/operations/runbooks/webhook-pipeline-failure.md` | Runbook 权威：DLQ 阈值、重放步骤、升级路径 |
| 3 | `internal/webhook/`（引擎）与 `internal/m2drill/p5_test.go`、`p6_audit_test.go` | 既有断言基础：拒签/去重/乱序/DLQ 已有 V2 级锚点，本任务是 Runbook 视角的重组+补缺 |
| 4 | `internal/m4drill/db_restore_test.go` | 锚点模式：runbook 章节 → 可执行断言 → 实测证据 |
| 5 | `docs/delivery/m4-governance-console.md` 的 M4-RBK-001 行 | Test ID：TC-WHK-001 |
| 6 | `plans/prep/m4/session-board.md` | 环境与协作纪律（PG 栈在主 checkout 5434；m4drill 文件互斥） |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| D1 故障注入矩阵 | `internal/m4drill/webhook_failure_test.go`：拒签风暴、重复投递、乱序版本、DLQ 积压超阈值四种注入，各对应 Runbook 的检测点 | 每种注入被正确分类并留审计 |
| D2 重放动作 | DLQ 重放路径：恰好一次语义、幂等键不破坏、重放后状态收敛；重放期间的新投递不串 | PG 门控断言 |
| D3 对账与升级 | 中断后对账（无丢失/无重复）；不可自动恢复项进入人工清单而非静默丢弃；升级事件的审计 | 断言 + 审计链 |
| D4 证据输出 | 锚点测试输出实测指标（检测时延、重放收敛时长）作为演练 Evidence | 测试日志留档 |

## 4. 文件边界

- **可改**：`internal/m4drill/webhook_failure_test.go`（本会话专属文件）
- **需协调**：若发现引擎缺口需改 `internal/webhook/**`——登记变更请求给集成会话裁决，不在本会话直接改
- **禁改**：咽喉点、`web/**`、`tests/eval/**`、`internal/m4drill/` 下 C 会话的两个锚点文件与已合入的 `db_restore_test.go`、`docs/governance/traceability-matrix.csv`

## 5. DoD 与验收命令

```bash
make web-build
MAESTRO_TEST_POSTGRES_DSN='postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable' \
  go test -count=1 -p 1 ./internal/...
ruby scripts/test-hygiene-check.rb
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
```

## 6. 交接物（回集成会话）

1. M4-RBK-001 webhook 侧 implemented 候选声明 + 锚点 Evidence 指针（含实测时延）
2. 登记项：引擎缺口（如有）、DLQ 重放的控制台视图需求（B 会话联动）
3. 偏离项清单（如有）
