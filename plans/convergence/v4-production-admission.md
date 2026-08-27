# V4 收敛手册：M4 Exit = Production Admission Gate（生产准入）

> 执行者：I4 集成会话 + 全流联审。权威判定：`docs/delivery/m4-governance-console.md` Production Admission Gate。这是全管线最终"打通与联调"的总验收。

## 1. 入口条件

- S6/S1/S3 交接物齐备：控制台真实浏览器测试清单、评测报告、pilot 验收材料、四类 Runbook 与演练记录
- M0–M3 全部能力零回归

## 2. 准入联调剧本（全要素彩排）

| # | 场景 | 预期 |
|---|---|---|
| 1 | 功能 Gate | 八场景用户旅程逐条走通（角色 × 场景矩阵） |
| 2 | 权限隔离 Gate | V1 剧本 1/2 复演 + 控制台 UI 层可见性一致（无越权渲染） |
| 3 | 质量 Gate | 公司默认 Required Gate 清单在试点仓库全量生效 |
| 4 | Agent 评测 Gate | 四层评测（质量/轨迹/安全/能力）达标线全过；judge 校准记录在案 |
| 5 | 安全 Gate | Critical/High 安全问题为零（扫描 + 红队 + 威胁建模复核） |
| 6 | 备份恢复演练 | PG 备份 → WAL 回放 → 恢复 Evidence（RPO/RTO 达标） |
| 7 | Runner compromise 演练 | 单 Runner 失陷 → 撤销 → 影响隔离 → Runbook 走查 |
| 8 | GitLab 中断演练 | V2 剧本 10 复演（含控制台降级展示） |
| 9 | DLQ replay 演练 | 死信重放 → 恰好一次语义保持 |
| 10 | 审计链验证 | 全链路审计事件完整、只追加、可导出（含 Evidence 关联与 correlation ID） |
| 11 | 试点影子运行 | 2–5 个 Go/TypeScript 仓库影子运行（不产生真实变更）→ 灰度 → 人工验收 |
| 12 | HITL 复核 | 高危动作与最终合并人检点在控制台可操作、留痕 |
| 13 | SLO | 99.5% 可用性口径与告警联动验证 |

## 3. 全量门禁

V0 第 3 节命令集 + 全部 CI 工作流（ci/docs/m0-runtime/m1+/eval）在目标 SHA 全绿。

## 4. 查缺补漏审计（终审）

- [ ] 矩阵 31 行全部 implemented+passed、Verified Commit 非空
- [ ] 全部权威文档 approved（含 operations/testing 域）
- [ ] 四类 Runbook 各有至少一次真实演练记录
- [ ] 遗留风险登记（Low/Medium 可带条件准入，须有 owner/期限/补偿措施）
- [ ] 全管线复盘（见第 7 节）

## 5. Exit Gate 状态翻转

m4 任务书 + 矩阵 M4 六行 + `docs/README.md` 第 4.2 表（模式同 V0）。此后 `docs/README.md` 第 10 章的"M1–M4 未实现"表述整体更新。

## 6. 角色签署

全部五角色：product_owner（试点验收）、technical_lead（总架构）、qa_owner（评测/演练）、security_owner（安全 Gate）、operations_owner（SLO/恢复/Runbook）。人工验收报告归档。

## 7. 项目级复盘

`docs/retrospective/v4-pipeline-retrospective.md`：全管线五波次复盘——偏离统计（目标/契约/实现三类各自总数与根因）、并行度收益评估、质量环有效性、对下一版本（含 Work Graph/ADR-009 是否启动）的建议。

## 8. 准入后动作（不属于本手册 Gate）

生产 Rollout（灰度扩大）、公司 GitLab 全面接入、监控值班与告警值班转移运维。
