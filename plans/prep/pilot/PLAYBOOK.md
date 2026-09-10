# 试点运行手册（PLAYBOOK）——七阶段从 0 到 100

> **定位**：`plans/prep/pilot/` 工作层手册，试点全程的操作与验收总纲。消费 BLUEPRINT/STANDARDS/ROLE-CATALOG 与 brief-P5a/P5b。

## 阶段总览

| # | 阶段 | 范围 | 出口标准 | Command Profile 增补 |
|---|---|---|---|---|
| 0 | **准备**（brief-P5a） | 权威 MR/双仓/onboarding/flags=shadow/存量摄取 | 就绪清单全绿 | 一期集：maven-build/npm-build/playwright-e2e |
| 1 | **PoC 首演**（brief-P5b） | D1 两周对比，0→1 全链路 | 选型 Gate 双签 + 首演复盘 | 无新增 |
| 2 | **影子**（一期前段，A1/A2 账本域） | 团队照常开发，Maestro 旁路记录 | 零干扰确认 + 控制台数据完整（任务流/证据/对账/SLO） | — |
| 3 | **灰度**（一期中后段） | flags=gray：Agent 接手测试/扫描类缺陷（M3 闭环真实上场） | Agent 修复 MR 人工合并率 100%、预算超限 <2% | — |
| 4 | **全量**（二期，BOM 246→238 人日域） | flags=full；直播/移动全治理；IntegrationRun 常态 | BOM 二期量化验收（2000 并发等） | **二期集任务书**：ffmpeg-transcode/srs-loadtest/im-ops（发布时点=影子期结束前一周下发） |
| 5 | **深水**（三期，学习地图/等保） | 全域治理 + 等保证据包生产 | BOM 三期验收 + 等保差距清单闭环（BLUEPRINT §2.1 [待评审]项全部裁决） | **三期集任务书**：等保证据导出/信创适配验证（D2 决策后） |
| 6 | **持续运营**（第 13 月起） | 内容/讲师/活动运营域纳管（运营角色制品=retrospective/release-note 扩类） | 月活≥60% 等运营指标 + Maestro 度量看板 | 按运营工具实测增补 |
| 7 | **验收/V4** | PILOT-001 人工验收 + V4 收敛仪式（矩阵/任务书/DOC-INDEX/v4 复盘同 PR） | 生产准入 | — |

## 每阶段固定动作

1. **阶段审计章节**（本手册的核心承诺）：每阶段结束导出并验证该阶段的资产集（asset.* 事件）+ 证据集（Pipeline/HITL）+ 决策集（Gate/豁免/放行），三条线一次审计链导出全量可验（#88 端点）；导出报告作为 `retrospective` 类资产入账。
2. **复盘→待办反馈回路**（持续迭代的机制化）：每阶段复盘产出的 Maestro 缺陷/缺口 → 调度板裁决队列 → 影响下阶段任务书（含 Command Profile 增补的发布时点）。
3. **角色工作流覆盖度盘点**：对照 ROLE-CATALOG 十域，记录本阶段实际运转的角色与摩擦（目标 ≥8/10 于灰度期末达成）。

## 风险与回退

- 内网 Jira 不可达 → 手工锚定（蓝图回退），镜像 worker 降级不阻塞
- 沙箱 GitLab 容量/性能 → 迁移公司 GitLab 的决策点（试点后）
- Agent 灰度质量不达标 → 停留 gray 延长一阶段，复盘后重试
