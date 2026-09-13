# W6 灰度前提三修 + 顺带清偿：A1–A6 关闭声明（2026-09-13，会话 W6）

> **定位**：brief-W6 交接物。六项影子期治理面缺口的关闭声明、W1 复现翻绿对比、灰度就绪核对单、S2B 驱动器退役确认与 spec 钉子变更记录。
> **输入**：S2C 零干扰清单（`S2C-zero-interference-W1.md`）F1–F4/O-1/O-2 分类；S2B 摩擦登记与 `s2b-evidence.json`；W1 周报。
> **服务版本**：本 PR 分支 `w6/gray-prereqs`（基线 main=cd4ba51）。

## 1. 关闭声明与实现要点

| # | 缺口 | 关闭面 | 实现 | 测试锚点 |
|---|---|---|---|---|
| A1（=F1） | `validating → ready_for_human_merge` 无生产写者，done 链断 | **已关** | 新写者 `MarkWorkItemReadyFromGates`（guarded UPDATE，机器自有边，状态+审计+outbox 同事务，action=`work_item.ready`/outbox=`work_item.state.changed`）；驱动=证据评估 verdict Ready（`evidence.Service` 钩子，每门 PASS 即推进）；merged webhook 经 `OnTupleComplete` 终评后接既有 done 边 | `TestReadyVerdictDrivesValidatingToReadyThenDone`（F1 场景全链）、`TestFailedGateLeavesValidatingUnmoved`（fail-closed 负例） |
| A2（=F2） | MR 绑定按映射主项目解析，跨治理域 FK 23503 | **已关** | 分支命名契约项目段权威化：`BranchMarker` 解析 `maestro/<project-key>/<item>`，`ResolveBranchBinding` 按 key 在映射项目团队内解析（无映射时全局唯一才认），未解析回退旧版绑定，绝不猜；MR 投影/evidence/tuple/终评全部落分支解析项目 | `TestCrossDomainMRBindsByBranchProjectKey`（F2 场景）、`TestLegacyMarkerFallsBackToMappingProject`、`TestUnresolvableMarkerLeavesProjectionUnbound`、`TestBranchTupleResolvesCrossDomainProjection` |
| A3（=F3） | claim/签核无 API 面，store 面唯一通路 | **已关** | /api/v3 四端点：`POST …/work-items/claim`（work_item.claim）、`POST …/executions/:id/complete`（work_item.submit）、`POST …/assets/:id/versions/:v/approve`（asset.approve，职能角色取自服务端 principal）、`POST …/work-items/:wid/gate-bindings`（workgraph.propose）；claim 前置 runner 绑定校验防跨域租约 | `TestWorkflowActionsS2BEquivalence`（**S2B 驱动器等价操作走 API 复现**：approve→bind→claim→complete→validating）、`TestWorkflowActionsPermissionDenials`（五负例） |
| A4（=F4） | /mcp 多会籍 fail-closed 且报 INTERNAL_ERROR | **已关** | 显式 `project` 工具参数（目录 3.4）：多会籍时须显式点名且必须是本人会籍之一（payload 只在已授权范围内选择，永不扩权）；缺参/外域选择→INVALID_PARAMETER+明确 message；单会籍零参数无感；零会籍维持 fail-closed | `TestIdentityScopeResolution`（带 scope 走通/不带拒绝/单会籍无感/外域拒绝/零会籍/委托面） |
| A5（=O-1） | outbox 域事件无 sink，空转重试（W1 实测 187 条） | **已关** | `DomainEventSink`（internal/app）：认领除 webhook 信道外的全部信道并标记 delivered（审计链已是持久记录，outbox 只欠 fan-out 收口）；`ClaimPendingExcluding` 新存储面含**孤儿 sending 租约 5 分钟回收**（W1 遗留 300+ attempts 孤儿的根因之一）；gitlab 消费者外类型事件改退避重排（不再即时重武装）；组合根在 PG 场景自动拉起（无新配置面） | `TestDomainSinkSettlesUnownedChannels`、W1 复现 Phase F |
| A6（=O-2） | SLO availability 样本饥饿 + 503 UNMEASURED 观察者效应 | **已关** | 可用性改为**窗口聚合**（`AvailabilityTotals` 汇总 lookback 全窗口，恢复声明窗口语义：单窗口读数让低流量项目一次 5xx 即翻 breached）；窗口内无数据→返回最近有效测量+`stale:true`（schema 新可选字段，90 天回看上限），仅从未有过遥测的项目维持 503 | `TestSLOSnapshotWindowAggregationAndStaleFallback`（抗饥饿/真实 breach 仍 breach/stale 三例）+ 既有 SLO 端点回归 |

**边界遵守**：冻结状态机入边未动（A1 只补驱动方法，WHERE 守卫机器自有边）；权限矩阵零新增串（A3 全部映射既有冻结串）；web 结构未改；矩阵未翻转（V4 仪式）。

## 2. W1 复现翻绿对比表（DoD 验收）

方法：pre-s2b 试点备份（2026-09-13 11:17）还原到临时库 `maestro_w6_verify`，用 W6 代码重放 S2B 切片（claim→complete）复现 W1 卡死态，再走 W6 修复链。测试=`TestW6PilotReplayComparison`（`MAESTRO_W6_VERIFY_DSN` 门控，本地验证专用）。

| 指标 | W1 周报/还原态 | W6 重放后 | 判定 |
|---|---|---|---|
| validating（A1-1/A1-2） | 2 | **0** | ✅ 2→0 |
| done | 0 | **2**（merged fact 终评链走通） | ✅ |
| evidence 行 | 0 | **24**（12 门 × 2 项） | ✅ 非零 |
| 治理域下 MR 投影 | 0（FK defer 循环） | **2** | ✅ 投影落位 |
| outbox 积压 | 172 pending + 9 孤儿 sending | **0**（sink 3 轮排空，含孤儿回收） | ✅ lag=0 |

常驻栈（8080）的翻绿随合并后重建窗口落地（先例：S2 于 #115 合入后换镜像）；**注意**：本会话发现 live `maestro` 库再次被清空（见 §5 事故登记），常驻栈重建前需先按备份恢复。

## 3. 灰度就绪核对单（阶段 3 硬前提）

- [x] **A1 done 链闭合**：validating→ready 有生产写者，merged webhook/对账→done 全链测试绿（含 W1 两条翻绿复现）
- [x] **A2 MR 投影正确**：分支契约项目段权威解析，跨治理域 MR 投影/证据落位
- [x] **A3 领取通路真实可达**：claim/complete/approve/bind 四端点真路由 + RBAC + S2B 等价 API 复现绿
- [x] （顺带）A4 多会籍显式作用域、A5 outbox 收敛、A6 SLO 度量语义修正

**灰度推进的剩余非代码前提**（不属本切片）：出口评估需 ≥2 周周报（最早 2026-09-26）；常驻栈重建+库恢复（§5）；试点侧 F5 verifier/viewer 账号、F6 egress 配方、F7 手册增补（调度板在案）。

## 4. S2B 驱动器退役确认

S2B 临时驱动器（`~/Works/yuandong/projects/peixun-s2b/tools/s2b-dev-preserve.tgz`，store 面 approve/bind/claim/complete）的四个操作全部有 /api/v3 等价端点且经 `TestWorkflowActionsS2BEquivalence` 复现验证——**正式退役**（无需再从保全档拉起；jira/status 两个只读诊断子命令可随时用 psql 替代）。开发切片会话此后一律走 API 面。

## 5. 事故登记（新发现，如实）

- **live `maestro` 库第三次被清空**（2026-09-13 W6 会话发现）：`maestro` 库 0 projects/0 work_items/0 audit，仅 1 条孤儿 outbox。时间窗在 S2C 周报（含 187 积压/审计链 123 条）之后。根因未明（与 ART-incident-002/003 同族：共享栈无会话隔离）。**未单方处置**；最新可恢复点=`pre-s2b-20260913-111737.dump`（110 工作项/S2A 全量），S2B/S2C 会话期数据需按各交接物重建。建议随常驻栈重建窗口一并处理并升级 ART-incident-003 行动项（库隔离/自动备份）。
- 备份沿用清单：`maestro-p5a-bases/pilot-backups/`；本次复现即用其还原，证明备份可用。

## 6. spec 钉子变更记录

| 钉子 | 旧 | 新 | 备注 |
|---|---|---|---|
| OpenAPI 写操作数 | 32 | **36** | +claim/complete/approve/bind 四写端点（control-plane.yaml 四路径+四 schema+Gone 响应组件）；spec-consistency-check 同步 |
| MCP 工具目录版本 | 3.3 | **3.4** | 全部 25 工具 input_schema 增可选 `project`（uuid，仅限本人会籍；非 `project_id` 避开冻结禁用字段清单）；examples 同步 |
| slo-status schema | — | +可选 `stale` | 窗口外最近有效测量标记；`additionalProperties:false` 仍闭合 |
| RBAC 权限数 | 68 | 68（不变） | A3 只映射既有串：work_item.claim/submit、asset.approve、workgraph.propose |
| outbox 事件类型 | 23（events.yaml） | 23（不变） | `work_item.state.changed` 本就在册，A1 写者首次成为其生产者 |

**解读备注**（供集成会话裁决）：任务书「claim→work_item.create 域」按 v1 先例与语义精确串落为 `work_item.claim`（complete 落 `work_item.submit`）——同属 work_item.* 冻结域，未铸新串；如需改钉请在裁决时示下。
