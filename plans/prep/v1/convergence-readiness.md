# V1 收敛仪式就绪清单（证据索引 + 唯一待决项）

> 工作层清单，供 I1 收敛仪式与 owner 决策直接消费。基线：`origin/main @ 6536055`。权威判定仍是 `docs/delivery/m1-control-plane-runner.md` 第 10 章 M1 Exit Gate 与 `plans/convergence/v1-control-plane.md`。

## 0. 唯一待决项（owner 决策）：Work Graph 三件套处置

> **决策记录（2026-08-31，owner）：选 B——移交 V2，立即收敛 V1。** ADR-009 状态注记与本清单同步登记；矩阵仅翻转已登记的 M1 六行；M0.5 #2/#6/#7 随 WG 在 V2 销号。

M1-WGP/WGM/WGS（父子任务编排、会话绑定、调度/Provider 适配）实现未动；权威文档（ADR-009 + 三份设计，我方 8/27 产出、I1 语义修订）为 draft。二选一：

- **A. 本波落地**：五角色评审三件套 → approved → 实装 → 矩阵 34 行登记 → 全量 P5。周期最长（实装体量大）。
- **B. 移交 V2**（I1 已提示）：V1 以"单层任务闭环"形态收敛——M0.5 十项中 #2/#6/#7（Adapter/会话绑定/父子聚合）随 WG 移交 V2，在 V1 复盘与矩阵登记移交口径。周期最短，可用形态（认证/Runner/领取/执行/验证/部署/备份）已全部实测。
- 无论 A/B：决策需 owner + technical_lead 签署记录（建议落在 V1 复盘或 ADR-009 状态注记）。

## 1. V1 手册 §4 审计项 × 证据

| 审计项 | 证据 | 状态 |
|---|---|---|
| core-coverage 清单扩展 | #19 底账（分组+缺口排序）；identity 77.6% / cp-auth-v3 75.0% / store 70.5% 待补测或定阈值 | 待仪式决策 |
| M0.5 十项销号 | #20 底账（4 闭环+1 基本闭环+1 转化+3 随 WG+1 移交 M2）+ #29（#3 收口）+ #32（实测闭环） | 随 §0 决策定稿 |
| 无残余静态 token 路径 | #22 扫描底账 + #30 实测（AUTH_* 稳定码、RemoteWrite 语义改述待文档） | ✅ 待措辞修订 |
| 授权缓存撤销传播 | #23 口径 + **#34 实测 26ms/410** | ✅ 可测分量闭环 |

## 2. 联调剧本 #1–#13 × 状态

| # | 剧本 | 状态 |
|---|---|---|
| 1 | 认证矩阵 | 静态面 ✅（#30 实测）；OIDC 面（错 audience/伪造 scope）待 Keycloak 栈 |
| 2 | 授权一致性（五面） | REST/MCP Tool ✅（#31）、WS ✅（#33）；background 待 dispatcher 实装（口径已登记） |
| 3 | Runner 生命周期 | ✅ enroll/approve/claim/heartbeat/complete 全实测（#29+#32）；吊销 fencing 26ms（#34）；API revoke 的 admin 面需 OIDC |
| 4 | 沙箱逃逸 | ✅ #21 八项真实逃逸测试 |
| 5 | CP 无源码 | ✅ #26 静态断言 |
| 6 | PG 备份恢复 | ✅ #27+#28（全损场景修复后全周期实测）；WAL/PITR 口径属 DEP 已收尾 |
| 7 | SQLite 导入 | ✅ 在库测试 + 真实 PG 钻孔（dry-run/import/reconcile/回滚） |
| 8 | M0.5 验收 | 领取幂等/CAS ✅ 实测（#32：claim 200/重放 409/available:false）；会话续接待 §0 决策 |
| 9 | 图与版本 | ⏸ 随 §0（选 A 则全测，选 B 移交 V2） |
| 10 | 并行与恢复 | 部分：claim CAS 冲突 ✅；canary 隔离/Attempt 恢复随 §0 |
| 11 | Adapter 与评价 | ⏸ 随 §0（Provider SPI 未实装） |
| 12 | 迁移影子 | 导入器 ✅；graph 影子随 §0 |
| 13 | M0 回归 | ✅ CI 每次全量 |

## 3. 收敛仪式剩余动作（照单执行）

1. §0 决策签署 → （选 B）登记 WG 移交与 M0.5 #2/#6/#7 移交口径。
2. core-coverage 组扩展采纳（#19）+ 补测到阈值（或技术负责人签署暂缓阈值）。
3. RemoteWrite 语义措辞修订（#30 发现 B）与 #22 检查表联动。
4. 全量门禁在目标提交（`make release` + `m1-runtime`）全绿。
5. Exit 翻转：矩阵 M1 行（选 B 则 6 行）`implemented+passed` + `last_verified_commit` 绑定；docs-check 对 passed 行校验。
6. 角色签署（qa/technical/security/operations 按签署矩阵）+ `docs/retrospective/v1-retrospective.md` 复盘。
7. （并行）W2 开局清单输出：I2 契约冻结范围（含 S4 偏差 #14）、S4a/S4b 分工、mock MR 接口确认。

## 4. 悬置人工项（不阻塞仪式但应登记）

- M0 四角色补签（V0 遗留）。
- B0.1 凭据与 3+3 trial（Provider 实证，喂 V2 的 WG/WGS）。
