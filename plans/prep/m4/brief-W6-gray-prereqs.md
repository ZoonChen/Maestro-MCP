# 任务书 W6：灰度前提三修 + 顺带清偿（S2C-A1/A2/A3 硬前提 + A4/A5/A6）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（S2A/S2B/S2C 收口节 + §7 S2C 段待办队列）→ `deploy/gitlab/peixun/reports/S2C-zero-interference-W1.md`（F1–F4 摩擦详情）→ `docs/technical/work-graph-model.md`（done 链不变量）→ 本任务书。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-w6 -b w6/gray-prereqs origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-w6 && make web-build
```

常驻栈动数据前 pg_dump（变更窗口纪律不变）。

## 1. 使命

关闭影子期 W1 暴露的六项治理面缺口——前三项（A1/A2/A3）是灰度期（阶段 3）的**硬前提**：Agent 接手缺陷流转依赖 done 链闭合、MR 投影正确、领取通路真实可达。

## 2. 切片

| # | 项（摩擦源） | 内容 | 验收 |
|---|---|---|---|
| W6-1 | A1（F1）done 链写者 | `validating → ready_for_human_merge` 的生产写者：MR merged webhook（或对账）驱动 Gate 终评——全 Required Gate merge_gate PASS → ready_for_human_merge；人工合并（已在 F1 场景发生）→ merged webhook → done。冻结状态机入边不变，补的是驱动方法+接线 | PG 门控：复现 F1 场景（validating 卡死）→ 修复后全链走通 done；**W1 周报两条 validating 翻绿** |
| W6-2 | A2（F2）MR 绑定解析 | MR→WorkItem 绑定按**分支命名契约的项目段**解析（`maestro/<project-key>/<item>`），而非映射表主项目；跨治理域 MR 不再 FK 23503 | PG 门控：复现 F2 场景（BOM 域 MR 投影 defer 循环）→ 修复后投影落位 |
| W6-3 | A3（F3）claim/签核 API 面 | `ClaimNextWorkItem`/`CompleteExecution`/`ApproveAsset`/`BindAssetGate` 补 REST 端点（/api/v3，权限映射冻结族：claim→work_item.create 域、approve→asset.approve、bind→workgraph.propose）；S2B 临时驱动器从此退役 | 真路由测试 + Playwright；**S2B 驱动器等价操作走 API 复现** |
| W6-4 | A4（F4）多会籍+错误码 | `/mcp` 多项目会籍 fail-closed 改为**显式 scope 参数**（请求带 project 明确选择，未带才拒绝，错误码 INTERNAL_ERROR→INVALID_PARAMETER + 明确 message） | 协议测试三例：带 scope 走通/不带拒绝（400 类）/单会籍无感 |
| W6-5 | A5（O-1）outbox 域事件 sink | workgraph/asset 域 outbox 事件的消费面（投影到 work-graph 状态视图或至少标记 processed），消除 187 条空转重试 | outbox lag 归零 + 新事件不再积压 |
| W6-6 | A6（O-2）SLO 采样修正 | availability 样本饥饿（指标源窗口太宽样本太少）+ 503 UNMEASURED 观察者效应（无 fresh 数据时返回最近有效快照+stale 标记，而非裸 503） | 指标窗口调优 + stale 标记 e2e |

## 3. 边界与 DoD

可改：`internal/handler/**`、`internal/workgraph/**`（W6-1 驱动方法）、`internal/webhook/**`（W6-2 解析）、`internal/mcp/**`（W6-4）、`internal/store/**`、`internal/app/**`（W6-5 消费者）、`internal/slo|handler/slo_endpoint.go`（W6-6）、`docs/specs/openapi/**`（W6-3 新端点，钉子随行）、e2e。禁改：冻结状态机语义/权限矩阵不变（W6-3 只映射既有串）/web 结构/矩阵。DoD：brief-E 全套 + `make e2e` + **W1 周报复跑对比**（validating 2→0、evidence 覆盖非零、outbox lag=0）。

## 4. 交接物

A1–A6 关闭声明（各附 W1 复现翻绿对比表）；灰度就绪核对单（三硬前提打钩）；S2B 驱动器退役确认；spec 钉子变更记录。
