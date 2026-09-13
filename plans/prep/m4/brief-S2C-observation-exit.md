# 任务书 S2C：周观察自动化与影子期出口评估

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/pilot/PLAYBOOK.md` 阶段 2 出口标准 → `deploy/gitlab/peixun/shadow-observation.md`（S2 交接的度量清单与周报骨架）→ S2A/S2B 交接物 → 本任务书。**时机：S2A 完成后即可开工建采集，出口评估在 ≥2 周观察数据后做**。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-s2c -b p5/shadow-observe origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-s2c && make web-build
```

## 1. 使命

把影子期观察从手工 spot-check 变为**自动化周报**，并在数据足够时产出影子期出口评估包（零干扰确认 + 四面数据完整 + 灰度就绪建议）——这是进灰度（阶段 3）的决策输入。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| S2C-1 | 周报采集脚本（`scripts/pilot/shadow-report.[sh|go]`：任务流[claim/done 计与周期]、证据链[Gate 评估/阻断原因分布]、对账[webhook 投影覆盖/未决数]、SLO[快照四指标]——输出 Markdown 周报到 `deploy/gitlab/peixun/reports/`） | 首份全自动周报产出 |
| S2C-2 | 零干扰确认清单核验（S2B 摩擦登记逐条分类：治理面误伤 vs 合理阻断 vs 环境问题——误伤项进 Maestro 待办） | 分类表 + 误伤项登记 |
| S2C-3 | 出口评估包（≥2 周数据后）：出口三标准逐项证据 + 灰度就绪建议（flags=gray 的范围建议：Agent 缺陷闭环首演准备度）+ 影子期复盘资产（retrospective 类制品，双签） | 评估包资产入台账 |
| S2C-4 | 报告归档为资产（每周报=internal 资产，digest 入账） | 台账可查 |

## 3. 边界与 DoD

可改：`scripts/pilot/**`、`deploy/gitlab/peixun/reports/**`、PLAYBOOK 实测勘误。禁改：Maestro internal/契约。DoD：brief-E 全套（涉及 Go 时）+ 首份周报 + 清单核验表。

## 4. 交接物

自动化周报（可持续）；零干扰分类表；出口评估包（灰度决策输入）；影子期复盘资产。
