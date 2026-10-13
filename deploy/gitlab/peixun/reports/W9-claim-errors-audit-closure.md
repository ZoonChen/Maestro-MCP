# W9 claim 面错误码与审计补全：F64/F66 关闭声明（2026-10-13，会话 W9）

> **定位**：brief-W9 交接物。灰度首演暴露的 claim 面两缺口（F66 错误码 / F64 审计）关闭声明、首演时序对照与机制澄清。
> **一手证据**：`~/Works/yuandong/projects/peixun-g3/G3-1-timeline.md`（F64 一手观察：P3/P6 claim 零审计行、观察台盲区段）与 `~/Works/yuandong/projects/peixun-g2/G2-FRICTION-REGISTER.md`（F-G2-1：首演唯一一例 500 的复现与根因）。
> **服务版本**：分支 `w9/claim-errors-audit`（基线 main=27346ec）。

## 1. 关闭声明与实现要点

**F64（中·审计面）已关——claim 成功路径补审计落链。** 定向与不定向领取共用的 `claimWorkItem` 事务尾部（queue token 推进后、commit 前）新增 `recordGovernanceEvent`：`work_item.claimed` 审计行（actor=`runner:<runner_id>`；reason 含 `execution=<uuid> lease=<uuid> lease_expires=<RFC3339>`）+ `work_item.state.changed` outbox 事件，与状态变更**同事务**（WGM-INV-012，对齐 release 面先例形态）。要点：

- **契约早已声明、实现补齐**：OpenAPI claim 操作的 `x-maestro-audit-event: work_item.claimed` 自 W6 起就在冻结面里——F64 本质是「契约定义了、实现没落链」，本 PR 零 spec 变更。
- **载荷守契约**：outbox payload 严格在 `StateChangePayload` 冻结面内（`additionalProperties: false`）：`{work_item_id, from: "queued", to: "executing", version, reason_code: "WORK_ITEM_CLAIMED"}`——`version` 取转移后值（UPDATE 改为 `RETURNING version` 读出）；execution/租约详情只进审计 reason，不进 outbox。
- **守卫补强**：转移 UPDATE 零行命中（行锁下不可达）改为 fail-closed 拒绝 `CONCURRENCY_CONFLICT`，不再静默提交无状态转移的租约。

**F66（错误码）已关——claim 面零 500（两条漏径都补）。**

| 切片 | 实现 | 测试锚点 |
|---|---|---|
| uuid 形状栅栏（首演实测根因，F-G2-1 建议） | `workflowRequireRunnerUUID`：claim/complete/release 三端点的 `runner_id` 在任何 uuid 列查询前校验形状，非 UUID（如 display name）→ **400 `INVALID_PARAMETER`**，零 DB 触达、token 不动 | `TestW9ClaimNonUUIDRunnerIs400`（复刻首演原始 body：`runner_id:"s2a-claim-runner"`） |
| 租约唯一索引冲突映射（任务书点名的竞态路径） | `INSERT INTO leases` 撞 `idx_leases_one_active`（SQLSTATE 23505）→ 回滚 + 查已提交活跃租约 → **409 `CLAIM_TARGET_HELD`**（带 `held_by_runner`/`lease_id`/`lease_expires_at`；查无可见持有者时回落裸哨兵，仍 409 非 500） | `TestW9ClaimLeaseUniqueViolationMapsToHeld`（BEFORE INSERT 触发器注入 23505）+ `TestW9ClaimConcurrentRaceZero500`（6 轮放大竞争） |

## 2. 首演时序对照（交接物要求）

| 面 | 首演（2026-10-12，main 旧码） | 本 PR 后 |
|---|---|---|
| G2 首次 claim（runner_id 误传 display name） | **500 `INTERNAL_ERROR "Runner binding could not be read"`**（F-G2-1；绕行=改传 runner UUID 后 09:10:08Z 一次成功） | **400 `INVALID_PARAMETER "runner_id must be the runner's UUID (not its display name)"`**（`TestW9ClaimNonUUIDRunnerIs400` 原样复刻） |
| 并发定向 claim 竞争败者 | 行锁串行化下已是 409 `CLAIM_TARGET_HELD`（W8 语义在首演即正确——G3-1 观察点 B 错误面实战样本=0）；**租约唯一索引冲突路径漏映射为 500**（潜伏，对非 claim 写者可达） | 竞争 6 轮放大：每轮恰一 200 + 一 409（`held_by_runner`=实际持有者），**零 500**；唯一索引路径映射为 409 `CLAIM_TARGET_HELD`（带持有者） |
| claim 成功（G1 09:05:21Z / G2 09:10:08Z 两次） | **审计面零记录**（F64：全库动作枚举 23 项无 `work_item.claim`，S2B 46 次领取同样无；观察台对 claim 的可见性全靠执行侧交接物） | `work_item.claimed` 审计行可查（actor=runner:…，reason 含 execution/lease/到期）+ `work_item.state.changed` outbox 同事务；**审计链导出 verify 仍真**（`TestW9ClaimCommitsAuditAndOutboxAtomically`） |

## 3. 机制澄清（诚实记录）

- **任务书表述与一手证据的张力**：brief 称「两 runner 同项、租约唯一索引一胜一败，败者当前返回 500」。机制上 claim-vs-claim 竞争双方都要先取 `work_items` 行的 `FOR UPDATE`（且租约 INSERT 的 FK 会拿父行 KEY SHARE），谁后到谁阻塞、解除后即见胜者已提交状态走 holder 查询→409——W8 的竞争测试（`TestW8TargetedClaimConcurrentRace`）与 G3-1 观察点 B（错误面实战样本=0）都印证此路径不产生 500。
- **首演实测 500 的真实根因**是 F-G2-1（非 UUID 串进 uuid 列的 PG 类型错误被兜底），本 PR 按其登记建议落为 400；任务书点名的唯一索引路径作为**防御性最后一线**同步补映射（对不经 claim 路径的租约写者——SQLite 导入、直连手术——可达），两条漏径都关，验收「零 500」成立。
- **观察面改善**：F64 的次生影响（「观察台对错误面的可见性依赖执行侧交接物」）随审计落链缓解——claim 的成败从此都在治理面留痕。

## 4. spec 钉子与边界

| 钉子 | 旧 | 新 | 备注 |
|---|---|---|---|
| OpenAPI 写操作数 | 38 | **38（不变）** | 无新路径/schema；`work_item.claimed` 审计动作系既有 `x-maestro-audit-event` 声明的实现补齐 |
| RBAC 权限数 | 68 | 68（不变） | — |
| MCP 工具目录 | 25 | 25（不变） | — |
| outbox 事件类型 | 23 | 23（不变） | 复用已登记的 `work_item.state.changed` |
| 迁移 | 0023 | **无新迁移** | 纯逻辑变更 |

**边界遵守**：改动仅在任务书可改清单（`postgres_workitems.go`、`workflow_actions_endpoint.go`、测试）+ 本报告；W8 定向语义（判定序、四拒绝面、不定向路径）逐字未动；租约结构零变更。

## 5. 门禁记录（brief-E 全套 + 覆盖门 + e2e）

- 全量 Go 套件（PG DSN、`-p 1`）：**exit 0**（含 store/handler/gitlab/runner 四个受影响面包单独复跑全绿；W6 重放对比测试无回归）。
- `gofmt`/`go vet`：clean；`ruby scripts/test-hygiene-check.rb`：PASS files=7。
- golangci-lint v2.12.2（隔离缓存，W5 纪律）：**0 issues**。
- `make coverage`（`M1_CORE_COVERAGE_ENFORCE=1`）：四组全 PASS——state-machine 100.0% / zero-trust-validation 80.8% / identity-authorize 82.9% / **pg-store 80.2%**（917/1144；W8 基线 80.1%）。
- e2e（`MAESTRO_E2E_BROWSER_CHANNEL=chrome`）：**33/33**。
- 工程注记：worktree 内 `make coverage` 若不带外部 DSN 会以不同 compose 项目名起 5435 容器致容器名冲突——按 Makefile 设计导出 `MAESTRO_TEST_POSTGRES_DSN` 即走既有实例。

## 6. 摩擦登记归档

- **F64 关闭**（G3-1-timeline §2 P3/P6、观察点 B 附带发现）；**F66/F-G2-1 关闭**（G2-FRICTION-REGISTER F-G2-1）。
- **新登记观察**（本会话产生，供裁决队列）：
  1. **Mimosa 误报 3 例**：断言行（`require.NoError`）与 UUID `fmt.Sprintf` 帮手行被报「SQL 注入·高危」——G2 观察点 C「拦截面以启发式误伤只读/断言」同族；本文件全部查询均参数化，保持原写法。
  2. **execId 路径参数无形状栅栏**：complete/release 的 `:execId` 路径参数未做 uuid 校验（同族潜在 500，未观察到实例）——登记为后续小切片，未随本波扩面。
  3. **覆盖门边界效应重演**：白名单文件（`postgres_workitems.go`）新增防御分支未同步看画像即把 80.1% 拉到 79.97% 破线——W7 教训「新增分支须看画像」本轮再咬一次；已补两个确定性触发测试（触发器注入 23505/RETURN NULL、取消 context）回到 80.2%。

## 7. 常驻栈重建（合并后切片间隙执行）

按 WIN 纪律执行：pg_dump 前置快照 → 镜像自合并后 main 重建 → `migrate up` 预期 no-op（无迁移）→ 换容器 → readyz/smoke（claim 活体探针：非 uuid runner_id 应 400、claim 后审计行应可查）。结果见 §8（执行后补记）。

## 8. 常驻栈窗口记录（待执行）
