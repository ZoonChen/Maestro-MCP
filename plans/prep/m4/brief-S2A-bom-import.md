# 任务书 S2A：BOM→WorkGraph 全量导入（影子期治理对象就位）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/pilot/PLAYBOOK.md`（阶段 2）→ `plans/prep/pilot/ARTIFACT-STANDARDS.md`（存量 BOM 资产已入台账为 ART-bom-001）→ `deploy/gitlab/peixun/READINESS.md`（常驻栈与凭据位置）→ 本任务书。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-s2a -b p5/bom-import origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-s2a && make web-build
```

常驻栈按 `p5b-server.sh` 状态（8080=maestro-main:local）；共享 PG 动前 pg_dump。

## 1. 使命

把 BOM 的 112 条功能条目变为 Maestro 的治理对象：一期 44 条（37 P0 + 7 P1）建为可领取 WorkItem（父子图挂一期 Milestone 节点下），二三期 46 条建为占位（status=blocked，不建详设）——**不代做详设与排期**（那是 S2B/团队的事），只建骨架 + 依赖边 + Jira 锚定。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| S2A-1 | 一期父节点 `peixun-m1` 封板（WorkPattern `peixun-m1-bom` v1：子节点=44 条 BOM 编号，A1/A2/A3/A5/B/C/D/E 域内 requires 依赖边按 BOM 说明列推导；BOM 编号保留为节点 display ID） | proposal applied 审计 + 图查询可遍历 |
| S2A-2 | 每条 WorkItem 绑定消费制品：locked_gate→`ART-bom-001`（详设 Gate 在 S2B 逐条补，本片只挂 BOM 锚）；`详设未 approved 不得开工`语义对 P0 生效 | draft 拒绑复现 |
| S2A-3 | 二三期占位：46 条建 blocked 占位节点（display ID + BOM 编号引用，无依赖边） | 图查询可见、claim 阻断 |
| S2A-4 | Jira 手工锚定批量建立：peixun Jira 项目（或回退表格）建 epic=一期、story=44 条，锚定关系批量导入 `jira_anchors` | 锚点 44+46 条、对账零未决 |
| S2A-5 | 导入脚本入库（`scripts/pilot/bom-import.ts` 或 Go——从 ART-bom-001 台账数据重放，可重复执行幂等） | 重放不重复建 |

## 3. 边界与 DoD

可改：`scripts/pilot/**`、试点仓只读引用、常驻栈数据（pg_dump 前）。禁改：Maestro 契约/internal（缺口走变更请求）、矩阵。DoD：brief-E 全套 + 图/锚点/审计三类查询的 Evidence 汇总。

## 4. 交接物

导入声明（44+46 计数 + 审计指针）；依赖边推导表（BOM 说明→requires 的映射理由，供团队勘误）；S2B 开工确认。
