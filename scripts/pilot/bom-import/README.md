# bom-import —— BOM→WorkGraph 全量导入（S2A）

> **定位**：brief-S2A 的 S2A-5 交付物。把《企业学堂平台建设BOM清单规划.xlsx》01 表的 105 条功能条目变为 Maestro 治理对象：一期 44 条建为可领取 WorkItem（父子图挂 `peixun-m1` 里程碑下、封板），二三期 61 条建为 blocked 占位（`peixun.m23`，无依赖边、不封板），全部 105 条手工锚定 Jira，一期 44 条挂 `locked_gate: bom → ART-bom-001`。**不代做详设与排期**——只建骨架 + 域内依赖边 + Jira 锚定。

## 用法

```bash
# 前置：常驻栈与共享 PG 按 deploy/gitlab/peixun/READINESS.md §7-8；
# 动共享库前 pg_dump（ART-incident-002 纪律）。
export MAESTRO_TEST_POSTGRES_DSN='postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable'
export S2A_PILOT_TOKEN="$(cat ~/Works/yuandong/projects/maestro-p5a-bases/pilot-stack/pilot-token)"   # fetch-token.sh 900s
export S2A_GITLAB_PAT="$(cat ~/Works/yuandong/projects/maestro-p5a-bases/pilot-stack/gitlab-bot-pat)" # baseline SHA 读取
go run ./scripts/pilot/bom-import
```

**幂等**：每步先查后写（种子 ON CONFLICT、提案按 project 命名空间的幂等键重放、绑定/锚点 check-then-insert、封板查 sealed_at）。重跑一遍零新建（已实测：proposals/nodes/items/bindings/anchors/audit 六项计数前后不变）。

## 重放底册与台账锚定

`data/bom-manifest-20260909.json` 是唯一重放源（105 条、含 BOM 说明列全文），其 sha256 与 xlsx 原件 sha256 一并登记进 `ART-bom-001@2` 的 summary——台账是权威锚，底册是机器可重放投影。改 BOM 后的再导入 = 重新生成底册 + 新版本登记（supersede 链）+ 新提案重放（图的既有结构不可变，需重规划路径，另行任务书）。

## 执行步骤 ↔ brief 切片

| 步骤 | 内容 | brief 切片 |
|---|---|---|
| S0 seeds | peixun 治理域项目/会籍/专用 claim runner + technical_lead 职能核验 | 前置 |
| S0b plans | WorkPattern `peixun-m1-bom` v1 + 两计划（MST-WP-00601/00602） | S2A-1/3 |
| S1 asset | ART-bom-001 v1→approved、v2 真_digest 重登记（draft 态留给拒绑探针） | S2A-2 |
| S1 m1 | 5 个字母域提案（MCP decomposition_propose）+ technical_lead 封板 | S2A-1 |
| S2 flat | 61 blocked 先落 → claim 阻断探针 → 44 queued 落 | S2A-2/3 |
| S2 gate | draft 拒绑复现 → v2 review+approve → 44 绑定；superseded v1 拒绑持续复证 | S2A-2 |
| S3 m23 | 5 个占位提案 + 61 节点翻 blocked（CAS） | S2A-3 |
| S4 jira | 复测 Jira（回退）→ 105 手工锚定 → 对账零未决 | S2A-4 |
| S5 audit | 双图 MCP 可遍历 + 审计链导出/验证 + Evidence JSON | DoD |

Evidence 输出默认 `deploy/gitlab/peixun/s2a-evidence.json`（`S2A_EVIDENCE_OUT` 可覆盖）。

## 关键事实（新会话必读）

- **治理域**：一切落在项目 `peixun`（`018fb5b0-0000-7000-8000-000000000006`）；backend/web 双仓项目仍只作代码/管线面。ART-bom-001 台账行在 peixun-poc 项目（P5a 摄取），跨项目消费是台账既有能力（绑定校验只看 approved+digest）。
- **双层结构**：图节点（work_nodes，结构/依赖/封板）与扁平 work_items（领取/锚定/Gate 绑定挂点，jira_anchors 与 asset_gate_bindings 外键指向后者）各 105 条 1:1 对齐（确定性 UUID `018fb5b4-…`）。
- **领取路径**：S2B 开发会话走 `work_item.claim`（扁平路径，Gate 在领取时 fail-closed 校验）。blocked 条目结构性不可领取（claim 只扫 queued）。
- **探针的重放语义**：draft 拒绑与 blocked 阻断探针只在首跑态可执行；重放以 superseded-v1 拒绑持续复证同族不变量（WGM-INV-015），evidence 保留首跑记录。
- **P0 语义**：37 条 P0 的「详设未 approved 不得开工」由 S2B 逐条补挂 detailed-design Gate（本片只挂 BOM 锚——BOM 已 approved 故当前可领取，这是影子期有意留口）。

## 勘误入口

- 依赖边推导与跨域候选：[DERIVATION.md](DERIVATION.md)
- brief 数字勘误（二三期 46→61）：DERIVATION.md §4
- 2026-09-13 运行前事故（maestro 库被清+恢复）与 0021 迁移修复：见 PR 描述「运行前事故处置」节
