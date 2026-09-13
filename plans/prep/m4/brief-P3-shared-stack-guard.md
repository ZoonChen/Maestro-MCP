# 任务书 P3：共享栈保护切片（三次数据丢失的根治 + 库恢复）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（W6 收口节事故登记）→ 本任务书。自包含，**灰度硬前置**。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-p3 -b p3/shared-stack-guard origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-p3 && make web-build
```

## 1. 使命

根治三次 live `maestro` 库被清空的根因（测试 DSN 指向主库 → 测试夹具 DROP 主库），并从备份恢复当前库。三道防线一次落地。

## 2. 排查结论存档（供实施参考）

**根因**：测试框架从 `MAESTRO_TEST_POSTGRES_DSN` 推导目标库名（取 DSN 最后一个 `/` 后的段替换为 `maestro_*_test`），但部分测试（如 `fifth_test`）**直接用 DSN 原文连接**——当 DSN 为 `...@5434/maestro`（主库）时，DROP 的就是主库本身。

**时间线**：三次事故分别在 09-12（J5 会话 migrate up）、09-13 早（未定位）、09-13 20:14 UTC（W6 最后一轮门禁）。机制相同：测试夹具连接到 DSN 指向的库 → `DROP DATABASE <库名> WITH (FORCE)` → `CREATE DATABASE` → 21 条迁移全量重放。

## 3. 三道防线

### 防线一：物理隔离（测试 PG 与主库 PG 分容器）

| # | 内容 | 验收 |
|---|---|---|
| P3-1 | docker-compose 增加测试专用 PG 服务 `maestro-test-postgres`（端口 5435，独立卷 `maestro-test-data`，独立密码）；`Makefile` 的 `test`/`test-race`/`coverage` 目标改为自动起停 5435 测试 PG（结束时停掉，不留孤儿容器） | `make test` 只触碰 5435；`docker ps` 无 5434 测试容器 |
| P3-2 | 环境变量约定：`MAESTRO_TEST_POSTGRES_DSN` 默认指向 5435（写入 README + maestro.yaml.example 注释）；CI workflow 同步改为 5435 | CI 全绿 |
| P3-3 | 5434 主库 PG 容器 `maestro-postgres` 重命名为 `maestro-resident-postgres`（compose 服务名变更，保留原卷数据），明确"常驻/非测试"语义 | 容器重命名 + 常驻栈配置同步 |

### 防线二：逻辑防线（主库 PG 禁止 DROP）

| # | 内容 | 验收 |
|---|---|---|
| P3-4 | 在主库 PG（5434）执行：`ALTER USER maestro NOCREATEDB;` + `REVOKE ALL ON DATABASE maestro FROM PUBLIC;`——`maestro` 用户从此无法 DROP 或 CREATE 任何数据库，只有 `postgres` 超级用户可以 | 用 `maestro` 用户尝试 `DROP DATABASE maestro` → **Permission denied** |
| P3-5 | 在主库 PG 的 `postgresql.conf` 中开启 `log_statement = 'ddl'` + `logging_collector = on`（DDL 审计日志，后续事故可取证） | 日志文件有 DROP/CREATE 记录 |

### 防线三：恢复底线（定时备份 + 库恢复）

| # | 内容 | 验收 |
|---|---|---|
| P3-6 | `scripts/pilot/pg-backup.sh`：pg_dump 主库到 `pilot-backups/maestro-$(date +%Y%m%d-%H%M%S).dump`，保留最近 72h（自动清理过期）；crontab 条目每 4h 执行 | 手动跑一次产出 dump；crontab -l 有条目 |
| P3-7 | **库恢复**：从 `pre-s2b-20260913-111737.dump` 恢复主库（110 工作项/S2A 全量），然后按各会话交接物重放增量：S2B 切片数据（A1-1/A1-2 状态）+ S2C 周报资产 + W6 审计事件 | 恢复后 work_items≥105、审计链可导出验证 |

## 4. 边界与 DoD

可改：`docker-compose.yaml`、`Makefile`、`scripts/pilot/**`、`deploy/gitlab/peixun/resident-server.sh`（端口同步）、`docs/` 相关段、`.github/workflows/*`（端口变更）。禁改：测试代码本身（不改库名推导——防线一/二已根治）、`internal/**`、矩阵。DoD：brief-E 全套 + `make test` 只触碰 5435 + **用 maestro 用户尝试 DROP 主库被拒**的 Evidence。

## 5. 交接物

三道防线落地声明（各附验证 Evidence）；主库恢复报告（恢复点/重放操作/数据量对比）；DSN 配置约定文档；ART-incident-003 行动项关闭声明。
