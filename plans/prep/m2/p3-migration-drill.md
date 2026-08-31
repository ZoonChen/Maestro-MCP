# M2-P3 预演：migration 0004 真实 PG 钻孔记录

> 工作层预演记录。对象：`s4/m2-schema @ dcb36fd`（PR #41 内容，网络中断期间从本地分支验证）。环境：隔离 PostgreSQL 16（`maestro-pg-drill`，127.0.0.1:15432）+ 真实 `maestro` 二进制。

## 1. 前向迁移

- 全新库 `migrate up` → `applied=4 target_schema=4`；18 → **28 表**。
- 新增十表：`gitlab_instances`、`gitlab_project_mappings`、`webhook_inbox`、`webhook_deliveries`、`merge_requests`、`pipelines`、`pipeline_jobs`、`evidence`、`gate_snapshots`、`waivers`。

## 2. 不可变触发器实测（数据库层强制）

| 操作 | 结果 |
|---|---|
| `UPDATE evidence SET producer=…` | **拒绝**：`ERROR: EVIDENCE_IMMUTABLE`（HINT: append a new fact instead of rewriting history） |
| `DELETE FROM evidence` | **拒绝**：同上 |
| `UPDATE webhook_deliveries SET outcome=…` | **拒绝**：`ERROR: WEBHOOK_DELIVERY_IMMUTABLE` |

Evidence 权威性（`GL-INV`/ADR-006 口径）在触发器层闭环——应用层旁路无法改写历史事实。

## 3. 回退演练

- `migrate revert --steps 1` → `reverted=1`，十表全部移除（**精确回到 18 表**，无残留对象）。
- 随后 `migrate up` 重新前向 `applied=1 target_schema=4`——回退后再应用无漂移。

## 4. 结论

P3 出口 Gate 的"迁移 + 回滚演练"实质完成（compose 正式环境复验仍按收敛手册归 P6）。schema 与 S4 偏差吸收（#40 注记的 Bot push 边界、`webhook_deliveries` 的 outcome 四态：accepted/duplicate/rejected/dead_letter 与 #14 的 Event-UUID 幂等键设计吻合——`external_event_id` 字段在库）对齐。

## 5. 环境处置

钻孔容器保留（复用于 M2-P4/P5 演练）；无产品文件改动。
