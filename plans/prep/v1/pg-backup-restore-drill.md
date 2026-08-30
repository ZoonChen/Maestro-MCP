# V1 预演：PostgreSQL 备份/破坏/恢复演练记录（剧本 #6 机制级预验证）

> 工作层演练记录，对应 `plans/convergence/v1-control-plane.md` §2 剧本 #6（PG 备份与恢复）的机制级预演。环境：隔离 PostgreSQL 16 容器（127.0.0.1:15432，一次性钻孔实例）；二进制：`origin/main @ d1bb937` 真实构建（`make build`）。**产品文件零改动**。

## 1. 演练步骤与结果

| 步骤 | 命令/动作 | 结果 |
|---|---|---|
| 迁移 | `MAESTRO_DATABASE_DSN=... maestro migrate up`（env-only，无 config 文件） | `applied=2 target_schema=2`；18 表；catalog v1+v2 带 sha256 digest |
| 种子 | users/teams/memberships/projects/work_items×2/audit_events×1 | 6 表有数据 |
| 备份 | `pg_dump -Fc`（自定义格式，52KB） | 成功 |
| 破坏 | `DROP SCHEMA public CASCADE; DROP SCHEMA maestro_meta CASCADE` | tables=0（完全丢失，含迁移目录） |
| 恢复 | `CREATE SCHEMA public` → `pg_restore --no-owner` | 18 表全回 |
| 校验 | work_items/audit_events/projects/users 四表逐表 MD5（排序聚合） | **四表校验和全部一致** |
| 完整性 | 恢复后再次 `maestro migrate up` | `applied=0 target_schema=2`——恢复出的 catalog 与内嵌迁移 digest 校验通过、零漂移（同时是恢复后 schema 完整性的权威验证手段） |

## 2. 发现（供 I1 / M1-DEP 处置）

**配置流缺陷**：`config.Load(path)` 在 `ApplyEnvOverrides` **之前**执行全量 `Validate()`（config.go:190-215）。因此配置文件一旦声明 `database.driver: postgres` 而没有 `dsn_secret_ref`，即使设置了 `MAESTRO_DATABASE_DSN` 也会在 Load 阶段直接 `CONFIG_INVALID`——错误消息"requires a dsn_secret_ref (or MAESTRO_DATABASE_DSN)"与实际行为矛盾（env 救不回来）。本演练以 env-only（无 config 文件）绕过。**建议**：Load 只做解码级校验，把依赖 env 解析后的校验统一放到 `loadRuntimeConfig` 的最终 Validate；或为 migrate/pg-import 子命令放宽 Load 期断言。V1 剧本 #6/#7 的 runbook 应先写明 env-only 启动方式。

**运维口径备注**：本演练覆盖逻辑备份（pg_dump -Fc）路径；**WAL/PITR 与"每日备份"自动化属 M1-DEP-001 未竟范围**，V1 正式审计时需以 compose 化的真实拓扑复跑（I1 收尾项）。恢复后用 `migrate up` 的幂等输出（applied=0）作为 schema 完整性断言，建议写进运维 runbook。

## 3. V1 预审/预演索引

| 项 | 位置 |
|---|---|
| core-coverage 扩展 | `core-coverage-preaudit.md`（#19） |
| M0.5 十项销号 | `m05-blockers-preaudit.md`（#20） |
| 静态 token 路径 | `static-token-paths-audit.md`（#22） |
| 撤销传播口径 | `revocation-propagation-preaudit.md`（#23） |
| PG 备份恢复预演 | 本文 |

## 4. 环境处置

钻孔容器（`maestro-pg-drill`）保留供后续钻孔复用；`docker rm -f maestro-pg-drill` 可回收。dump 文件为容器内 `/tmp/drill_backup*.dump`，随容器丢弃。
