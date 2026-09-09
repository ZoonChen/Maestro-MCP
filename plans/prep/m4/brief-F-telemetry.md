# 任务书 F：遥测生产者切片（M4-OBS-001 收尾，关闭 REL-2 与 G2）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → 本任务书。自包含。

## 0. 工作区准备

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-f2tel -b f/m4-telemetry-producer origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-f2tel
make web-build
```

## 1. 使命与所有权

M4-OBS-001 的最后一块：把运行时指标真实采进 `telemetry_aggregates`，让 SLO 快照端点（#88 已上线）的声明目标有数据可评，同时关闭 D 登记的 G2（DLQ/收件箱深度无常驻指标面）。**已有地基**：`store.RecordTelemetry`（脱敏聚合 upsert，红版本随行）、`internal/slo`、SLO 端点、`/api/v1/metrics`（`bh.GetMetrics`）。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `docs/operations/observability-and-audit.md` §11 | 最小指标清单与告警阈值（Inbox/Outbox P95>30s、DLQ>0 即告警） |
| 2 | `internal/store/postgres_observability.go` + `postgres_slo.go` | 生产侧写入契约（TelemetryPoint、窗口唯一、p95 字段）与 SLO 读侧 |
| 3 | `internal/app/application.go` 的 `startWebhookDispatch`/`startDataGC` 与 `Options` | worker 接线模式（backgroundWG + nil=不启动） |
| 4 | `internal/handler/router.go` 中间件链 | 请求路径埋点挂载点（MaxBodySize/CORS 之后） |
| 5 | `docs/specs/schemas/slo-status.schema.json` + `maestro.yaml.example` 的 slo 段 | 指标命名必须与可配置的 slo 策略对上 |
| 6 | `plans/prep/m4/session-board.md` | 环境约定与纪律 |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| F1 请求埋点 | 进程内采样器：per（route 模板、方法、状态类、pid 可得时项目）记录时延样本，有界内存（环形缓冲，溢出丢弃并计数）；中间件零分配热路径 | 单测：样本归属、缓冲上限 |
| F2 生产者 worker | 周期 flush：按 (project, metric) 聚合窗口样本 → 计算 count/sum/min/max/p50/p95/p99 → `RecordTelemetry`；项目不可得的请求归入配置声明的"平台项目"或跳过（二选一，写明理由）；`telemetry:` 配置段（interval、redaction_version、平台项目），缺省不启动 | PG 门控：窗口 upsert、重报收敛 |
| F3 平台深度指标（G2） | 同一 worker 采样 webhook 收件箱积压、DLQ 深度、outbox 滞留（PG 计数查询），越阈值时结构化告警日志（slog，携带 runbook 引用）并汇入 `/api/v1/metrics` 输出；DLQ>0 即告警语义落测试 | PG 门控：注入 DLQ 行→深度与告警可见 |
| F4 端到端 | slo 配置示例接通：埋点→生产者→`GET /api/v3/projects/:pid/slo-snapshot` 返回非 no_data 的 api_p95 与 availability | PG 门控集成测试 |

## 4. 文件边界

- **可改**：`internal/app/**`（worker）、`internal/handler/**`（埋点中间件 + metrics 输出扩展）、`internal/config/config.go` + `docs/specs/schemas/config.schema.json`（telemetry 段）、`internal/store/**`（深度查询只读函数）、`maestro.yaml.example`
- **禁改**：`web/src/**`、`tests/eval/**`、`internal/webhook/**`、`internal/slo/**`（只消费）、矩阵
- 咽喉点（router.go/config.go）：本切片属契约面变更，PR 描述中逐条列出

## 5. DoD 与验收命令

同任务书 E 第 5 节全套（Go 套件含 PG、hygiene、lint、docs 四检、e2e 不回归）。

## 6. 交接物

1. M4-OBS-001 implemented 候选声明（含 G2 关闭证据：DLQ 深度指标与告警）
2. 指标命名对照表（埋点 ↔ slo 配置 ↔ 冻结 objective 枚举）
3. 偏离项清单
