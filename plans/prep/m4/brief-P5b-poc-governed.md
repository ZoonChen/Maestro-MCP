# 任务书 P5b：PoC 治理接入（0→1 全链路首演，依赖 P5a）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/pilot/` 四件 + `brief-P5a` 交接物 → 本任务书。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-p5b -b p5/poc-governed origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-p5b && make web-build
```

## 1. 使命

D1 路线 PoC（PlayEdu vs RuoYi 两周对比，BOM 07 决策点）作为**首批全链路 WorkItem**：从存量摄取（0）到选型决策 Gate（1）的完整治理首演，按 ROLE-CATALOG §3 对照表逐角色走通。这是"从 0-1"的验证样本；1→100 由 PLAYBOOK 七阶段接力。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| P5b-1 | 拆解首演：用 `decomposition_propose` 把 PoC 工作拆为父子图（父=PoC 决策，子=对比评测/直播压测/工时评估/风险评估），WorkPattern v1 封板 | 图封板 + 审计链 |
| P5b-2 | 角色工作流逐个走通：blueprint（PoC 决策卡）→ detailed-design（对比维度/压测方案，**详设 approved 才能开工子任务**——locked_gate 首次真实生效）→ test-plan/report（压测）→ release-note（选型结论） | 每制品的 asset.* 事件完整 |
| P5b-3 | 工程链路真实跑：对比实验在两仓以 Command Profile 执行（PlayEdu 部署评估仓 vs RuoYi 底座仓）、Pipeline Evidence、决策 Gate 双签（product+technical 职能角色，J1 通路首用） | merge_gate Evidence + 双签审计 |
| P5b-4 | Jira 镜像：PoC epic/story 锚定与镜像、对账零未决 | 对账清单空 |
| P5b-5 | 首演复盘（retrospective 制品）：角色工作流摩擦点、工具缺口实单（含创作类 skill 的真实需求清单——"接入或创造"的依据数据） | 复盘资产 + Maestro 待办登记 |

## 3. 边界与 DoD

可改：试点仓内容、`plans/prep/pilot/` 的实战修订（ROLE-CATALOG/CATALOG 实测勘误）。禁改：Maestro 契约（缺口走变更请求）。DoD：brief-E 全套 + 首演 Evidence 汇总报告（release-note 资产）。

## 4. 交接物

D1 选型结论（进 BOM 决策）；全链路首演的量化记录（各 Gate 时长/摩擦/工具缺口）；PLAYBOOK 影子期开工确认。
