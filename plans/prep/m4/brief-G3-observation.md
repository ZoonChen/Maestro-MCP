# 任务书 G3（灰度首演·观察台）：首演观察 + 零干扰分类 W6 + 评估面就绪核对

> **用法**：新会话依次读 `CLAUDE.md` → `PLAYBOOK.md` 阶段 3 → `deploy/gitlab/peixun/shadow-observation.md`（观察口径）→ `deploy/gitlab/peixun/reports/exit-assessment-FINAL-20261013.md`（首演观察项清单）→ 本任务书。**只读治理面**（API+psql 读；零写操作）。

## 1. 使命

灰度首演（G1/G2 双缺陷片并行执行中）的旁路观察与证据归集——补齐出口评估标准①的证据缺口（自然/灰度流量零干扰确认）与标准③的待填项（预算/评测面就绪）。

## 2. 任务

| # | 项 | 内容 | 产出 |
|---|---|---|---|
| G3-1 | 首演时序观察 | 轮询 G1/G2 工单状态流转（claim 时间差=并发实战证据）、审计事件、evidence 落链节奏、六门评估对无绑定缺陷项的行为（观察点 A） | 首演时序表 |
| G3-2 | 预算面就绪核对 | Agent 运行预算台账（M3 预算域）当前结构/写者/告警面；对照 PLAYBOOK 阶段 3 要求给出就绪判定（出口评估标准③待填项） | 就绪判定+缺口登记 |
| G3-3 | 评测面就绪核对 | internal/eval + tests/eval 当前状态（H 交付的入库接线+数据集）；同上判定 | 同上 |
| G3-4 | 零干扰分类表 W6 | 沿 `S2C-zero-interference-W1.md` 结构建 W6 表：首演期全部治理面拒绝/摩擦逐条三桶分类（误伤/合理阻断/环境），G1/G2 的观察点 A/B/C 归入 | 分类表入库 |
| G3-5 | 首演报告 | `deploy/gitlab/peixun/reports/gray-rehearsal-report-1.md`：时序+分类+预算/评测判定+人工合并事件（首例！推广门槛指标起点）+遗留清单 | 报告+（可选）report-register 入账 |

## 3. 边界

只读：`GET /api/v3/*`、psql SELECT、报告写入 `deploy/gitlab/peixun/reports/` 与本任务交接物目录。禁改：任何治理状态、Maestro 仓库代码、G1/G2 的工作区。发现 G1/G2 卡壳**只记录不介入**（它们的会话自治处置）。

## 4. 交接物

`peixun-g3/` 下观察记录 + 分类表 + 报告；首演双片收口后汇成终版（等它们的停车点报告再收尾）。
