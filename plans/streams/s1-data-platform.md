# S1 数据与平台流任务书（会话输入）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/PIPELINE.md` → 本文件 → 当前任务对应的 `plans/stages/m*.md` 与其权威文档。本文件是自包含会话任务书；做 brief 之外的事之前先登记偏离项。

## 1. 使命与所有权

端到端拥有**数据层与运行平台**：PostgreSQL 真源（store/迁移/导入/Outbox）、部署（Compose/备份/readiness）、可观测与审计管道、可靠性（SLO/备份恢复）。是全部数据模型建设工作（各里程碑 P3）的主责流。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | 工程不变量红线 |
| 2 | `docs/README.md` | 权威顺序与三状态纪律 |
| 3 | `plans/PIPELINE.md`、`plans/DISCIPLINE-PHASES.md` | 管线位置与环节出口 Gate |
| 4 | `docs/technical/data-model.md`、ADR-002（PostgreSQL + Outbox/Inbox） | 数据权威 |
| 5 | `docs/technical/deployment.md`、`docs/prd/deployment.md` | 部署权威 |
| 6 | `docs/operations/observability-and-audit.md`、`docs/operations/reliability-and-recovery.md` | W3/W4 任务权威 |
| 7 | 当前任务书 `docs/delivery/m1|m4-*.md` 第 6/7 章 | 任务范围与必需输出 |

## 3. 任务序列

| 波次 | 任务 | 主轴位置 | 收敛点 |
|---|---|---|---|
| W1 | M1-DATA-001（PG store/迁移/Outbox/SQLite 导入） | M1-P3 主责 + M1-P4 | V1 |
| W1 | M1-DEP-001（Compose：PG+OIDC+GitLab CE+maestro；备份/readiness） | M1-P4 | V1 |
| W2 | M4-OBS-001 / M4-REL-001 的 P1–P3 预备（telemetry 设计、SLO/备份方案） | M4-P1..P3 提前量 | — |
| W2 | 支持各流的 P3（新表迁移评审与落地） | M2-P3 协作 | V2 |
| W3 | M4-OBS-001 实装（脱敏 telemetry、审计链导出） | M4-P4 提前量 | — |
| W4 | M4-OBS-001、M4-REL-001 收口（SLO 告警、恢复 Evidence、演练） | M4-P4..P6 | V4 |

## 4. 文件边界

- **可改**：`internal/store/`（PG 实现新文件、迁移目录）、`internal/store/sqlite.go`（导入相关扩展）、`docker-compose.yaml`、`Dockerfile`、`scripts/`（部署/导入/备份脚本）、`cmd/maestro/main.go` 中 migrate/doctor 子命令扩展
- **需协调**（修改前在交接物中登记，经集成会话确认）：`internal/store/interfaces.go`、`internal/config/config.go`、`Makefile`、`internal/model/model.go`
- **禁改**（只随契约 PR）：`internal/handler/router.go`、`docs/specs/**`、`docs/governance/**`

## 5. DoD 与本地验收命令

- 统一 DoD：`docs/delivery/README.md` 第 10 章（代码/迁移/文档/测试/Evidence/追踪全覆盖）
- 流内门禁：`make build && make test && make vet && make lint && ruby scripts/test-hygiene-check.rb`
- 数据专项：迁移前向+回滚在 Compose PG 演练；SQLite 导入 dry-run/import/reconcile；`ruby scripts/schema-check.rb`
- 提交规范：PR 描述含 Stage Task ID、Test ID、迁移与回滚说明

## 6. 交接物契约（向集成会话）

1. 已达 implemented 候选的 Task ID 清单及 Evidence 指针（CI job/本地报告路径）
2. 契约变更请求（如需改 interfaces/config/model：原因/影响/方案）
3. 新增表/列 → Requirement 映射表（供矩阵更新）
4. 部署/导入/回滚操作说明（供 Runbook 与收敛手册引用）

## 7. 与其他流的接口

- **S2**：principal/session 存储接口（S2 定义语义，S1 落存储）
- **S3**：Runner 部署与注册数据落库
- **S4**：Inbox/Outbox/gate_snapshot/waiver 表（S4 定语义，S1 落迁移）
- **S5**：finding/defect/budget/integration_run 表
- **S6**：事件流与审计导出 API

## 8. 内部拆分点

W1 可拆两会话：**S1a**（PG store + 迁移 + 导入）/ **S1b**（Compose + 部署 + 备份 + readiness）。拆分时各自以本文件为纲，在 PR 中标注 a/b。
