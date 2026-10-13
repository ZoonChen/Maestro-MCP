# DG 关闭声明：第五道防线——DSN 连接串断言（2026-10-13，会话 DG）

> **定位**：brief-DG 交接物。四次同族库事故的根因根治声明、断言覆盖面说明与门禁 Evidence。
> **一句话总结：第五道防线 = 在伤害发生前喊停的哨兵。**

## 1. 四次事故时间线回顾（根因完全相同）

四次 live `maestro` 库清空，机制同族：**破坏性测试 DDL 的连接源头指向常驻主库（5434/maestro）而非隔离测试实例（5435）**。P3 三道防线（物理隔离/角色对调/自动备份）修的是后果与爆炸半径；本片在 DDL 执行前检测目标 DSN，把事故拦在第 0 秒。

| # | 时间 | 现场 | 取证/登记 |
|---|---|---|---|
| 1 | 2026-09-12 | P5a 种子数据被 J5 并行会话重建共享库抹除 | ART-incident-002（根因=共享栈无会话隔离） |
| 2 | 2026-09-13 早 | live 库第二次清空 | ART-incident-003 |
| 3 | 2026-09-13 20:14 UTC | W6 门禁轮第三次清空；主库 21 条迁移 12:14:24-25Z 同秒重放指纹 | ART-incident-003 取证锁定：`MAESTRO_TEST_POSTGRES_DSN` 指向 `...5434/maestro`，测试夹具 `DROP DATABASE <DSN库名> WITH (FORCE)` 直连重放（P3 报告 §4） |
| 4 | 2026-10-13 13:17 本地 | 并行 EV 评测会话清空 live 库；迁移 19→23 同秒重放指纹 | W9 关闭声明 §8：`tests/eval/scripts/roundtrip.sh` **旧默认 DSN 自带 `5434/maestro`**——防线一/二拦不住「脚本内置错误默认值」这一变体；防线三（F62 修复后的 cron 备份）首次兜底成功 |

## 2. 断言函数与规则（`internal/testguard`）

```go
func AssertTestDSN(dsn string) error
```

- 错误哨兵 `ErrDSNGuardBlocked`（brief 原文消息），`errors.Is` 可判。
- **fail-closed**：URL 解析失败/无 host/无显式端口 → 一律拒。
- 规则 1（拒）：端口 **5434**（常驻实例）无条件拒绝——`_test` 命名也不放行（共享实例即共享风险）。
- 规则 2/3（需正向标记）：端口 **5435**（受管测试实例，任意库名）**或** 库名含 `_test`；裸 `maestro` 库挂非 5435 端口（事故签名去掉 5434 之外的形态）在此被拒。
- 逃生口：`MAESTRO_ALLOW_ANY_DSN=1`（精确等值；CI 一次性无名库专用）。
- **对 brief 规则 2 的一处解释性偏离（如实登记）**：brief 字面规则「库名 `maestro` 无 `_test` 后缀即拒」与受管默认 DSN `...5435/maestro`（compose `POSTGRES_DB: maestro`）冲突——字面实现会击穿 DoD「正确 DSN 下零行为变更」。实现为端口主判别：`maestro` 库仅在**非 5435 端口**时被拒（规则 2 的意图由此保持），5435 上任意库名放行（规则 3 正向半边）。

## 3. 断言覆盖面（哪些 DDL 前有断言）

**Go 夹具：55 个文件、56 处断言调用**（`git diff` 计数；断言统一插在「DSN env 读取→skip 判空」之后、**任何连接建立之前**——每夹具的全部后续连接（admin 会话、派生库 DSN）都由该 base DSN 改写而来，开根设卡即覆盖下游全部 DROP/CREATE）：

| 接入面 | 覆盖的破坏性 DDL |
|---|---|
| store 包咽喉 `testPostgresDB`（`maestro_store_suite_test`）+ `testPostgresStore`（`DROP SCHEMA public` 直刷） | store 全部 26 个 PG 夹具文件，含经 `resetWorkItemSchema` 的 workitems/workgraph/assets/w5/w9 系 |
| handler 12 文件、gitlab 3、app 2、identity 3、jira 2、m2drill/m3drill/m4drill 4、mcp/tools 1、runner 1、cmd/maestro 1、store 逐文件 20、tests/m0 1 | 各自 `DROP DATABASE IF EXISTS <suite>_test WITH (FORCE)` + `CREATE DATABASE` 前的 admin 连接源头 |
| **Shell：`tests/eval/scripts/roundtrip.sh`**（第四次事故触发器，W9 遗留②根修） | 默认 DSN 改为受管 5435；`:5434/` 模式直接 exit 3 拒绝（`MAESTRO_ALLOW_ANY_DSN=1` 逃生口与 Go 侧同款）；无 psql 回退改走 `maestro-test-postgres` 容器、用户名从 DSN 派生 |
| **TS e2e：`tests/e2e/specs-m0/console-governance.spec.ts`** | 该 spec 原在 **5434 常驻实例**上 psql `DROP DATABASE ... WITH (FORCE)`；现从 `MAESTRO_TEST_POSTGRES_DSN` 派生（默认受管 5435）、拒绝 5434（skip 原因明示拒绝）、容器/用户随 DSN 派生；`make e2e` 挂 `WITH_TEST_PG`（与 `make test` 同款受管生命周期） |

**不在覆盖面（如实声明）**：仓库 Go 测试无 SQL `TRUNCATE`（仅 `time.Truncate` 方法误名）；`schema_manifest`/`runtime_integration` 的 `DROP TABLE` 是 SQLite 本地库腐蚀用例，非共享栈面；写 live 库的运维脚本（`pg-backup.sh`/`report-register`/W6 重放等 `MAESTRO_PILOT_POSTGRES_DSN`/`MAESTRO_W6_VERIFY_DSN` 通路）有意指向 5434，属运维域不设卡。TS/Shell 哨兵为窄面（仅拒 5434），完整 fail-closed 规则在 Go 侧。

**边界遵守与登记的偏离项**：零生产代码改动（`store.OpenPostgres` 等未触碰）、P3 三道防线零改动、无迁移、spec 钉子全部不变（writes 38/RBAC 68/MCP 25，docs 四检实证）。三处超出 brief 可改清单的扩展已按协作纪律登记：①`tests/eval/scripts/roundtrip.sh`（第四次事故根因载体，任务书使命的必然覆盖）②`tests/e2e/specs-m0/console-governance.spec.ts`（同族风险：常驻实例上的 DROP DATABASE）③`Makefile` e2e target（②的功能前提）。README「PostgreSQL 端口纪律」段建议随下个切片补一行（本片未动，避免再越界）。

## 4. 验证 Evidence

| 项 | 结果 |
|---|---|
| 断言正负例（`dsn_assert_test.go`） | 15 表例 + 覆盖口精确等值例 + 逃生口精确 `1` 判例全绿；gosec G101 用 `#nosec -- 理由` 按仓库先例处理（测试 DSN 用 `url.URL` 程序化构造，无凭据字面量） |
| **负例证明（事故签名实弹）** | `MAESTRO_TEST_POSTGRES_DSN=...5434/maestro` 下：store 夹具与 handler 夹具均 **0.00s FAIL 于 `ErrDSNGuardBlocked`，零网络拨号**（在连接建立前拦截）；roundtrip.sh 输出拒绝并以 exit 3 停止；brief-E 字面 DoD 命令（其 DSN 恰为 5434/maestro）在第五道防线生效后已不可再执行——这本身就是本片的存在理由 |
| 零行为变更（DoD） | `MAESTRO_TEST_POSTGRES_DSN=...5435/maestro make test`：33 包全 ok、0 FAIL、test-hygiene PASS（正确 DSN 下全量套件与 main 行为一致） |
| roundtrip.sh 正路径 | 默认 5435：DROP/CREATE 一次性库 + migrate applied=23 全通（sql() 无 psql 回退经 `maestro-test-postgres` 容器实证）；后续 harness 步骤 exit 1 为本 worktree 未装 `tests/eval` npm 依赖（环境事，非哨兵拦截，如实登记） |
| e2e | `MAESTRO_E2E_BROWSER_CHANNEL=chrome` + 5435：**33/33 全过（21.0s）**——含改造后的 console-governance 六个 spec 在隔离实例上真跑（治理树/审计校验全绿），无 skip |
| lint / docs | 全仓 golangci-lint（隔离缓存 `GOLANGCI_LINT_CACHE=/tmp/...`）**0 issues**；docs 四检 PASS（docs-check 71 文档/spec 一致性 writes=38 rbac=68 mcp=25/schema 13） |
| gofmt/vet | 改动包全清洁（main 既有 5 个文件的 gofmt 漂移为 Go 1.26 注释对齐版本差，非本片引入，未触碰） |

## 5. 已知限制与交接

- **keyword-value 形态 DSN**（`host=... port=...`）会被 fail-closed 拒绝（仓库内外 DSN 均为 URL 形态；如需该形态走 `MAESTRO_ALLOW_ANY_DSN=1` 并登记）。
- **多 worktree 并行的受管容器名冲突**（`container_name: maestro-test-postgres` 全局唯一，P3-1 既有设计）：并行会话各跑 `make test`（受管模式）会撞名；解法=外部 DSN 模式复用同一实例（本片门禁即如此执行）。CI 不受影响（外部 DSN 模式）。
- CI 的 e2e 中 console-governance 自本片起**真跑**（受管 WITH_TEST_PG 在 runner 上拉起测试实例）；随之暴露并已修复一个潜伏预算问题：beforeAll 完整治理栈编排（冷缓存 `go run` 迁移 + IdP/服务容器）超出 Playwright 钩子默认 30s——显式提至 300s（钩子内 waitForHTTP 本就给服务 240s）。改造前 CI 在 5434 探针处早退，该预算从未被检验。
- 常驻栈：**无需重建**（纯测试侧变更，不影响运行时二进制）——brief §4 预期一致。

## 6. 门禁记录

本机：`make web-build`、全量 Go（PG 5435，33 包 ok）、test-hygiene、lint 隔离缓存 0、docs 四检、gofmt/vet、负例实弹×3、roundtrip 正/负路径、**e2e 33/33（chrome，console-governance 在 5435 真跑零 skip）**。build-test-sbom 随 PR CI（红为 main 既有状态，见 W8 记录）。
