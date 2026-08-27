# M4 执行计划（波次 W4 → 收敛点 V4 = 生产准入）

> 目标：各角色治理控制台、Agent 四层评测/红队、完整可导出审计、SLO/备份恢复/降级 Runbook、2–5 仓库试点生产准入。非目标：绕过 GitLab 人工合并、以试点承诺大规模 SaaS。
> 权威依据：`docs/delivery/m4-governance-console.md`。W2/W3 期间 S6/S1/S3 已完成大量 P1–P4 预备与实装。

## P1 文档规划（I4 开局）

- 文档推进：m4 任务书 review→approved；`operations/observability-and-audit.md`、`operations/reliability-and-recovery.md`、`testing/agent-evaluation-redteam.md`、`testing/pilot-acceptance.md` not_started→approved。
- 需求锚定卡：六任务（UI/EVAL/OBS/REL/RBK/PILOT），Test ID 以 m4 书第 12 章为准逐卡提取。

## P2 实现方案（I4 契约冻结）

- 控制台契约：角色 IA、八场景用户旅程、HITL 审批队列、冲突/错误/降级展示、a11y 标准、事件流订阅复用。
- 评测契约：四层（质量/轨迹/安全/能力）评测的 harness 接口、数据集格式、judge 校准记录、报告格式。
- 审计契约：只追加审计链导出格式（含 Evidence 关联与 correlation ID）。
- SLO 契约：99.5% 可用性、RPO/RTO、告警阈值与 Runbook 触发联动。

## P3 数据模型建设（S1 支持）

- 新表：telemetry 聚合（脱敏规则落地）、evaluation_record（数据集/运行/评分）、pilot_flags（影子/灰度）。
- **显式记录**：控制台 UI 本身无破坏性数据迁移；审计链导出不改审计表结构。

## P4 代码工程建设

| 承担 | 任务 | 落点 |
|---|---|---|
| S6 | M4-UI-001、M4-EVAL-001、M4-PILOT-001 | `web/src/`（登录态/写操作/HITL 队列/MR 视图/八场景）、`tests/eval/`（新：harness+数据集+judge）、rollout flags |
| S1 | M4-OBS-001、M4-REL-001 | telemetry 管道、审计链导出、SLO 告警、备份/WAL/恢复 Evidence |
| S3 | M4-RBK-001 | 四类 Runbook（Runner 离线、Webhook/Pipeline 故障、数据库恢复、紧急停止）+ 演练记录 |

出口 Gate：每流 `make build test vet lint` + test-hygiene 全绿；控制台过真实浏览器 DOM 测试（补齐 M0 遗留增强项）。

## P5 测试验证（convergence/v4 剧本 = 生产准入彩排）

功能/权限隔离/质量/Agent 评测 Gate 全通过；Critical/High 安全问题为零；四大演练：备份恢复、Runner compromise、GitLab 中断、DLQ replay；审计链完整、只追加、可导出验证；2–5 个 Go/TypeScript 仓库影子运行 + 灰度 + 人工验收。

## P6 质量工程（V4 收敛仪式 = Production Admission Gate）

全要素彩排 + 人工验收报告；m4 书 + 矩阵 M4 六行翻转；项目级收尾复盘（全管线 retrospective）。

## 时序估算

P1–P2 2–3 天 → P4 1–1.5 周（大量预备已就绪）→ P5 3–5 天（演练与试点是长项）→ P6 2–3 天（含人工验收等待）。W4 总粗估 2–3 周。
