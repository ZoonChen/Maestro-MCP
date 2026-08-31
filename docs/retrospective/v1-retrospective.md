# V1 收敛复盘（Control Plane、OIDC、PostgreSQL 与本地 Runner）

> 本目录不受权威文档模板/frontmatter 约束，作为 Evidence 记录载体。本复盘对应收敛点 V1（M1 Exit Gate）；仪式程序见 `plans/convergence/v1-control-plane.md`，就绪底账见 `plans/prep/v1/convergence-readiness.md`。

## 1. 目标提交与 CI Evidence

- 目标提交（PR head）：本收口提交（`last_verified_commit: HEAD` 自引用绑定字面量，与 V0 同模式；实际 SHA 以本 PR 的 head 记录为准，合流后 main 头上的 `docs-check` 对 HEAD 字面量校验成立）。
- 远程 CI Evidence：收口提交为纯文档变更（门禁翻转 + 复盘），路径过滤下仅 `ci.yml` 与 `docs.yml` 触发并在收口 PR 全绿；`m0-runtime.yml` / `m1-runtime.yml`（postgres service + `MAESTRO_TEST_POSTGRES_DSN`）按设计跳过 docs-only 变更，其 Evidence 落在最后一个代码携带提交 `5d70215`（#37 合流，四工作流 success）及 M1 全部切片 PR（#4–#37）的检查记录上。
- 本地全量门禁（收口提交上）：`make build test vet lint` 0 issues、`docs-check`（71 docs / 31 行）、`spec-consistency-check`（28 writes / 57 permissions / 14 tools）、`test-hygiene` 全绿；PG 门控套件对 compose PostgreSQL 实测通过（identity/v3-runner/store 三套件）。

## 2. 交付范围与 WG 移交（owner 决策 2026-08-31）

- **已交付（矩阵 M1 六行翻转 `implemented + passed`）**：M1-ARCH-001（CP/Runner 边界 + dependency health + 组合接线）、M1-AUTH-001（冻结 RBAC 决策点 + OIDC 验证 + 401/403/404 中间件 + 设备令牌 + PG 组合接线）、M1-DATA-001（PG 基线 schema + 迁移目录 + SQLite 四段式导入 + 备份/恢复实测）、M1-RUN-001（协议客户端 + daemon 生命周期 + CP v3 生命周期与 claim/heartbeat/complete 实装 + 加固 OCI 沙箱 + 执行器适配器）、M1-MCP-001（14/14 冻结目录工具 + registry 精确收口 + 策略守卫）、M1-DEP-001（Compose m1 拓扑 + 备份脚本 + 配置流修复）。
- **移交 V2（owner 决策记录 #36）**：M1-WGP/WGM/WGS（父子任务编排、会话-任务绑定实体、调度/Provider SPI）整体移交 V2；权威文档（ADR-009 + work-graph-model + work-graph-scheduler）保持 draft。M0.5 十项阻断中的 #2（Provider Adapter）、#6（会话-任务绑定实体）、#7（父子聚合）随 WG 在 V2 销号；#9 归 M2。
- **四面 authorize 一致性**：REST（#10）/ MCP Tool（#31）/ WebSocket（#33）实测一致；background 面口径登记——M0 维护任务是 maintenance owner 进程职责，background_worker 服务身份随 dispatcher 实装（V2+）接管。

## 3. 审计项处置（收敛手册 §4）

| 审计项 | 处置 |
|---|---|
| core-coverage 清单扩展 | #37 采纳：identity-authorize 80.2%（强制即 PASS）、pg-store 70.2% ADVISORY——PG 门控套件依赖 `MAESTRO_TEST_POSTGRES_DSN`（仅 m1-runtime 提供），强制将击穿其余 CI job；**W2 开局把 pg-store 提到 80% 后翻 `M1_CORE_COVERAGE_ENFORCE=1`**。检查器同时修复多包行区间去重缺陷 |
| M0.5 十项销号 | #20 底账 + #29（#3 claim/lease 收口）+ #32（live-stack 实测）+ §2 移交口径；7 项闭环/转化/移交 M2，3 项随 WG 移交 V2 |
| 静态 token 路径 | #22 扫描 + #30 实测（AUTH_* 稳定码）；RemoteWrite 措辞修订见 #38（仪式项） |
| 撤销传播 | #23 口径 + #34 实测 26ms 首次 410（P99 <60s SLO 达成） |

## 4. 过程偏离与教训

1. **三次"半成品不合并"回滚**：S2b 领取工具、执行器适配器两轮在测试未收敛时按纪律回滚 worktree 并以 WIP 分支交接（提交信息含根因与下一步），未产生带病合流。代价是每个切片多一轮会话；收益是 main 上零回退。
2. **测试缓存两次骗过本地门禁**（go test 缓存 + make lint 管道吞退出码各一次，均被 CI 拦下）：此后全部验证带 `-count=1`，且 lint 判定直接看 golangci-lint 输出。
3. **跨包 PG 测试互锁**（handler 与 store 套件并行 DROP 同库 schema 死锁、coverage 多包并行同样死锁）：处置为每包独立数据库 + `-p 1` 串行化。
4. **两处真实契约漂移被测试网捕获**：0001 把 connection_generation 建成 bigint（冻结规范为 UUID 字符串，迁移 0003 修正）；存储层哨兵经 fmt.Errorf 包装后 handler 用精确比较落空（改 errors.Is）。
5. **V0 遗留的四角色补签未完成即进入 V1**（悬置项登记于就绪清单 §4）；V1 签署同样以本 PR 评审批准承载，建议 V1/W2 开局时一并补齐。

## 5. 角色签署

按 m1 书 frontmatter `approver_roles`，签署方式为收口 PR 评审批准（每人一条 approving review）：

| 角色 | 签署面 |
|---|---|
| product_owner | 范围与 WG→V2 移交决策 |
| technical_lead | 六任务实现与总签 |
| security_owner | 身份/Runner Evidence（OIDC 矩阵、逃逸八项、撤销 26ms） |
| operations_owner | 部署/恢复（Compose m1、备份/恢复全损演练） |
| qa_owner | 跨 adapter 一致性（四面 authorize）与 M0 回归 |

（若实际合流先于评审，按 V0 先例在 §4 偏离登记并补签。）

## 6. W2 开局清单输出

1. **I2 契约冻结范围**：Webhook 验签/MR 元数据/Evidence 权威入边/done 入边；S4 偏差输入取 #14 验证报告。
2. **pg-store 覆盖率提到 80% 后翻 `M1_CORE_COVERAGE_ENFORCE=1`**（本复盘 §3 第一项的闭环动作）。
3. **S4a/S4b 分工确认**（GL+MR / WHK+QG）；S5 mock MR 客户端接口确认。
4. **V2 WG 三件套评审排期**（ADR-009 + model + scheduler：draft→approved 后登记 M1-WGP/WGM/WGS 对应的 V2 任务行）。
5. M0/V1 遗留补签（§4 第 5 条）。
