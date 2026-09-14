# P3 收口：共享栈保护三道防线 + 主库恢复（2026-09-13，会话 P3）

> 任务书：`plans/prep/m4/brief-P3-shared-stack-guard.md`（灰度硬前置）。
> 本文是三道防线落地声明、主库恢复报告与 ART-incident-003 行动项关闭声明的合一交付物。
> 窗口登记：WIN-20260913-P3-01（pilot-stack/change-window.md）。

## 1. 三道防线落地声明

### 防线一：物理隔离（P3-1/P3-2/P3-3）

| # | 内容 | 验证 Evidence |
|---|---|---|
| P3-1 | compose 新增 `maestro-test-postgres`（profile `test-pg`，端口 127.0.0.1:5435，独立卷 `maestro-test-data`，独立凭据 `maestro-test`）；`make test`/`test-race`/`coverage` 经 `WITH_TEST_PG` 宏自动起停（`up -d --wait` → trap → `rm -sf`，不留孤儿容器，卷复用） | `make test` 全套绿（见 §3 门禁记录）；测试期间 5434 容器 `StartedAt` 不变、DDL 审计日志无测试期 DROP/CREATE DATABASE 记录 |
| P3-2 | `MAESTRO_TEST_POSTGRES_DSN` 默认指向 5435：Makefile 受管默认值 + README「PostgreSQL 端口纪律」+ `maestro.yaml.example` 注释 + CI（m1-runtime.yml service 端口 5432→5435）；CI 已设外部 DSN 时 Makefile 不再起本地容器 | README §本地构建与验证；`make -n test` 两种模式展开验证（受管/外部）；CI 全绿（PR 检查） |
| P3-3 | 主库 PG 服务/容器重命名 `maestro-postgres`→`maestro-resident-postgres`（compose 卷键经 `name:` 钉住原物理卷 `maestro-mcp_maestro-postgres-data`，数据原样保留；`maestro-migrate` 服务 DSN 主机名同步） | `docker ps`：`maestro-resident-postgres ... Up (healthy) 127.0.0.1:5434->5432`；恢复前计数确认卷数据在位（第三次清空后的空壳） |

### 防线二：逻辑防线（P3-4/P3-5）

| # | 内容 | 验证 Evidence |
|---|---|---|
| P3-4 | `maestro` 角色彻底非超级化：bootstrap 用户 RENAME 为 `maestro-dba`（强随机密码，存 pilot-stack/maestro-dba-password 0600），新建普通 `maestro` 应用角色（`NOSUPERUSER NOCREATEDB`，非库 owner），public/maestro_meta 两 schema 全量授权 + 默认权限；`REVOKE ALL ON DATABASE maestro FROM PUBLIC` | EV1 `DROP DATABASE maestro`（as maestro）→ `ERROR: must be owner of database maestro`；EV2 `CREATE DATABASE evil_test` → `permission denied to create database`；EV3/EV4 应用 DML/DDL 正常（110 项可读、可建表）；降权后常驻 server 重启全链路绿（readyz + audit-export 200） |
| P3-5 | DDL 审计：compose `command` 层 `log_statement=ddl` + `logging_collector=on`（等价 postgresql.conf，随容器定义持久），日志落卷内 `$PGDATA/log/` | `SHOW log_statement`=ddl / `SHOW logging_collector`=on；日志文件捕获本次恢复自身的 `CREATE TABLE maestro_meta.schema_migrations`（14:47:31Z）及探针 DDL——事故取证能力实证 |

**实现要点存档（供后续运维复用）**：
- PG 16 **禁止剥夺 bootstrap 用户的 SUPERUSER**（`must have the SUPERUSER attribute`），且 superuser 绕过 CREATEDB 检查——任务书原设想的单条 `ALTER USER maestro NOCREATEDB` 对本容器不构成防线。实际采用**角色对调**：bootstrap 改名 `maestro-dba`（改名不受属性限制，ownership 随行），新建普通 `maestro` 承接全部应用授权——所有现存 DSN 一字不改。
- `REASSIGN OWNED` 因 bootstrap 拥有系统必需对象被拒；改用「库 owner 保持 `maestro-dba` + `maestro` 仅获 schema 级授权」。副作用是更强的防线：`maestro` 连 owner 通道的 DROP 也被拒。
- 全新卷的等效固化：`scripts/pilot/resident-initdb/01-harden-maestro-role.sh`（compose 挂载到 initdb.d，`MAESTRO_DBA_PASSWORD` 提供时建 break-glass 超管 + 剥离应用角色）。
- 应用 schema 有两个：`public` 与 `maestro_meta`（schema_migrations 所在）——授权清单两者都要。

### 防线三：恢复底线（P3-6）

| # | 内容 | 验证 Evidence |
|---|---|---|
| P3-6 | `scripts/pilot/pg-backup.sh`：容器内 pg_dump -Fc → `pilot-backups/maestro-<ts>.dump`，保留 72h（只清本工具命名产出）；mkdir 原子锁防重叠；<4KB 视为可疑拒绝计为恢复点 | 手动跑两次成功：22:47 空壳快照 220826B、22:57 恢复后快照 394760B；部署副本 `pilot-stack/pg-backup.sh`（crontab 稳定路径）+ crontab `0 */4 * * *`（已装，`crontab -l` 可见） |

## 2. 主库恢复报告（P3-7）

**恢复点**：`pre-s2b-20260913-111737.dump`（S2A 全量，110 工作项）。

**操作序列**（防御性快照先行；全程窗口登记 WIN-20260913-P3-01）：

1. 防御性 dump 第三次清空后的空壳（`maestro-20260913-224710.dump`，220826B）
2. 常驻 server 暂停 → `DROP DATABASE maestro WITH (FORCE)` → `CREATE DATABASE maestro OWNER maestro` → dump 实为 **text 格式**，`psql --single-transaction` 恢复（0 ERROR）
3. 重放 S2B 切片 + W6 done 链：`MAESTRO_W6_VERIFY_DSN=...5434/maestro go test ./internal/gitlab -run TestW6PilotReplayComparison` → PASS（A1-1/A1-2 的 claim→complete→validating→MR/管线/门禁→ready→merged→done 全链，与 W6 关闭声明同款常量）
4. 重放 S2C 周报资产：`MAESTRO_PILOT_POSTGRES_DSN=... go run ./scripts/pilot/report-register --report deploy/gitlab/peixun/reports/shadow-report-W1-20260913.md --week 1` → `ART-shadow-report-001@1` 重新入账（digest=sha256:916270af…，与 W1 周报一致）
5. W6 审计事件：重放 3 的每步状态变更自然落审计链（+3 条，179 总量）
6. 降权（防线二）→ 常驻 server 迁移至新镜像后重启全绿

**常驻栈镜像同步**：旧 `maestro-main:local`（8c8d82e，只认 20 条迁移）对恢复库 fail-closed（`SCHEMA_INTEGRITY_FAILED applied=21 expected=20`）——按调度板预告的重建窗口，从当前 main（46e9f8e，21 条迁移）重建镜像并 `migrate`（no-op，target=21）后 `up` 就绪。这正是调度板「常驻栈重建窗口需先恢复库再换 W6 镜像」的兑现。

**数据量对比**：

| 指标 | 事故后空壳 | pre-s2b 恢复后 | 重放后（终态） |
|---|---|---|---|
| work_items | 0 | 110 | 110（done 2 / validating 0） |
| projects | 0 | 5 | 5 |
| audit_events | 0 | 176 | 179（治理域 126） |
| assets | 0 | 13 | 14（+ART-shadow-report-001@1） |
| evidence | 0 | 0 | 24 |
| outbox 悬置 | 1（孤儿） | 172 | **0**（sink 排空） |
| 治理域 MR 投影 | 0 | 0 | 2 |
| schema 迁移 | 21（空表） | 21 | 21 |

**验收线**：work_items 110 ≥ 105 ✓；审计链可导出验证 ✓——`GET /api/v3/projects/:pid/audit-export?from_seq=1&to_seq=179` → 126 entries，`chain_digest=sha256:bbad1b08…`，`POST …/audit-export/verify` → `{"verified":true}`。

**如实登记的恢复缺口**：S2B/S2C 会话期部分过程数据不可复原——(a) 遥测 `telemetry_aggregates` 历史自恢复重放起点重新起算（S2C 勘误 E3 的断点后移到 2026-09-13T14:4xZ）；(b) S2B 的 gate_snapshots 为空（重放走 webhook 投影而非真实 GitLab 管线，evidence 24 条为重放事件落链）；(c) 审计 179 条 < 事故前在案的 S2C 周报口径 123 条治理域+平台事件——pre-s2b 恢复点本身早于 S2C 周报。以上不影响治理面正确性，出口评估读数时须知口径。

## 3. DSN 配置约定

权威位置：README「PostgreSQL 端口纪律」+ `maestro.yaml.example` 头部注释。要点：

- **5434 = 常驻主库**（`maestro-resident-postgres`，live 数据，测试 DSN 禁入；`maestro` 角色无 DROP/CREATE DATABASE 能力）
- **5435 = 一次性测试库**（`maestro-test-postgres`，`MAESTRO_TEST_POSTGRES_DSN` 默认指向；make 自动起停；CI service container 同端口语义）
- 写 live 库的运维脚本用 `MAESTRO_PILOT_POSTGRES_DSN`（`report-register` 已支持，旧变量名兼容回退）
- 定时备份：pg-backup.sh 每 4h，dump 保留 72h

## 4. ART-incident-003 行动项关闭声明

三次 live `maestro` 库清空（09-12 J5 会话 / 09-13 早 / 09-13 20:14 UTC W6 门禁轮）的根因与根治状态：

- **根因**（集成会话取证）：`MAESTRO_TEST_POSTGRES_DSN` 指向 `...@5434/maestro`（主库），部分测试（如 `fifth_test`）直接用 DSN 原文连接并 `DROP DATABASE <DSN库名> WITH (FORCE)` 后重建——三次事故机制相同（主库 21 条迁移同秒重放为证）。
- **关闭**：防线一（物理隔离：测试再想连主库也无 DSN 可指）+ 防线二（逻辑防线：即便 DSN 指错，`maestro` 角色无 DROP 能力，PG 权限层拒绝）+ 防线三（恢复底线：4h 级 RPO + 本切片已验证的恢复剧本）三道同时落地，且互相独立成立——任一道失效不放大事故。
- **遗留观察项**：5434 容器停止期间常驻 server 的重连行为未做长时间观测（本窗口内 server 按流程 down/up）；不构成行动项。

## 5. 门禁记录

见本 PR 描述与 CI。本机已跑：`make web-build`、`make test`（受管 5435）、恢复/降权 Evidence 全集；其余 brief-E 项（lint 隔离缓存/docs 四检/e2e 33）随 PR 门禁执行。
