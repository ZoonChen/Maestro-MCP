# 影子期周报 W1（2026-09-12T00:00:00Z .. 2026-09-13T04:37:23Z）—— 项目：企业学堂 backend/web

- 采样人：shadow-report/1.0.0（全自动，2026-09-13T04:37:25Z）；数据面：常驻控制面 http://127.0.0.1:8080 + pg 直查（maestro-mcp-maestro-postgres-1）
- 服务镜像：maestro-main:local；影子期第 1 周（期起点 2026-09-12T00:00:00Z）
- 审计链指纹（治理域 …006）：134 条 / sha256:1ee4acad7d1ef5f5ab3220d11ad03b86f7f543575a2be704ecfd491b7bea6b07 / 验证=true（134 条）

## 任务流

- work-graph 计划（API 快照）：
  - 企业学堂 backend（RuoYi 底座）：（无计划）
  - 企业学堂 web：（无计划）
  - D1 PoC 治理域：MST-WP-00501（sealed）；MST-WP-00502（sealed）；MST-WP-00503（sealed）；MST-WP-00504（sealed）
  - PlayEdu 部署评估：（无计划）
  - 企业学堂平台（BOM 治理域）：MST-WP-00601（sealed）；MST-WP-00602（draft）
- work_items 状态分布（全量）：

| 项目 | 状态 | 条数 |
|---|---|---|
| D1 PoC 治理域 | executing | 2 |
| D1 PoC 治理域 | queued | 3 |
| 企业学堂平台（BOM 治理域） | blocked | 61 |
| 企业学堂平台（BOM 治理域） | queued | 42 |
| 企业学堂平台（BOM 治理域） | validating | 2 |

- 本周变更：新建 110 / 状态变更 4 / 图节点翻转（审计）
- 领取与完成（claim→done）：窗口内领取 5（累计 5：cancelled×1 completed×2 running×2 ）；done 0；周期 不适用（done=0；F1/F2 修复后首条 done 产生周期样本）
- 团队 MR 投影（真实开发活动旁路记录）：

| 项目 | 累计 | 本周新增 | 已合并 | 本周合并 |
|---|---|---|---|---|
| 企业学堂 backend（RuoYi 底座） | 1 | 1 | 1 | 1 |

## 证据

- 管线投影：

| 项目 | 状态 | 累计 | 本周 |
|---|---|---|---|
| 企业学堂 backend（RuoYi 底座） | success | 6 | 6 |

- 形式 Evidence 行：0（本周 +0）
- Gate 评估快照：0（无分布（零快照））——0 为预期（v3 无 merge-gate 评估写者，F1）；治理侧锁定 Gate 绑定累计 49

## 对账

- Jira 对账（API，手工锚定口径）：合计未决 0
  - 企业学堂 backend（RuoYi 底座）：未决 0
  - 企业学堂 web：未决 0
  - D1 PoC 治理域：未决 0
  - PlayEdu 部署评估：未决 0
  - 企业学堂平台（BOM 治理域）：未决 0
- Jira 锚点累计：110
- webhook 收件：processed×106 |合计 106｜本周 106｜DLQ 0
- 投递结果：accepted×106 rejected×1 ｜拒绝原因：TOKEN_MISMATCH×1 
- MR reconcile 操作：审计动作目录无该动作（勘误 E4），维持人工抽查口径
- outbox：delivered×102 retry_wait×178 sending×9 （非交付 187 条，最老 22.6h）

## SLO（rolling_7d 快照，逐项目）

| 项目 | availability（预算余） / api_p95 / ingest_p95 / inbox_lag |
|---|---|
| 企业学堂 backend（RuoYi 底座） | healthy 100%（预算余 100%）；api_p95_latency_ms=7.350009ms/healthy；webhook_ingest_p95_latency_ms=0ms/no_data；inbox_lag_p95_seconds=0s/no_data；rpo_minutes=0minutes/no_data；rto_minutes=0minutes/no_data；backup_success_rate_percent=0percent/no_data |
| 企业学堂 web | healthy 100%（预算余 100%）；api_p95_latency_ms=4.208005ms/healthy；webhook_ingest_p95_latency_ms=0ms/no_data；inbox_lag_p95_seconds=0s/no_data；rpo_minutes=0minutes/no_data；rto_minutes=0minutes/no_data；backup_success_rate_percent=0percent/no_data |
| D1 PoC 治理域 | healthy 100%（预算余 100%）；api_p95_latency_ms=4.173047ms/healthy；webhook_ingest_p95_latency_ms=8.029718ms/healthy；inbox_lag_p95_seconds=1.38827s/healthy；rpo_minutes=0minutes/no_data；rto_minutes=0minutes/no_data；backup_success_rate_percent=0percent/no_data |
| PlayEdu 部署评估 | healthy 100%（预算余 100%）；api_p95_latency_ms=4.176213ms/healthy；webhook_ingest_p95_latency_ms=0ms/no_data；inbox_lag_p95_seconds=0s/no_data；rpo_minutes=0minutes/no_data；rto_minutes=0minutes/no_data；backup_success_rate_percent=0percent/no_data |
| 企业学堂平台（BOM 治理域） | healthy 100%（预算余 100%）；api_p95_latency_ms=5.359673ms/healthy；webhook_ingest_p95_latency_ms=0ms/no_data；inbox_lag_p95_seconds=0s/no_data；rpo_minutes=0minutes/no_data；rto_minutes=0minutes/no_data；backup_success_rate_percent=0percent/no_data |

- rpo/rto/backup_rate：no_data 属预期（勘误 E5）；判读主面=webhook 主流量项目

## 异常与摩擦（→调度板裁决队列）

- outbox 非交付事件 187 条（最老 22.6h）——常驻栈域事件（asset.\*/workgraph.\*）无 sink，dispatcher 每分钟空转重试（观察项 O-1，见口径勘误）
- work_items 卡 validating 2 条（000000000001(企业学堂平台（BOM 治理域）) 000000000002(企业学堂平台（BOM 治理域）) …）——done 链 F1（validating→ready 无写者）+F2（MR 绑定 FK）双重结构缺口（S2B 登记）

## 度量口径勘误

- E3 遥测历史起点 2026-09-13 01:13:49+00（S2A 库恢复事故 ART-incident-003 候选在案）：rolling_7d 实际覆盖自该时刻起算，跨事故周 SLO 为部分窗口
- E4 MR reconcile 操作不在审计动作目录（12 动作枚举无 reconcile）——对账操作数自动面不可见，维持人工抽查口径
- E5 rpo/rto/backup_rate no_data 属预期（试点备份节奏=阶段 pg_dump，backup_runs 无记录）——shadow-observation.md §4 注 1

## 采集指纹

- 脚本：shadow-report/1.0.0；窗口 2026-09-12T00:00:00Z..2026-09-13T04:37:23Z；输出 deploy/gitlab/peixun/reports/shadow-report-W1-20260913.md
- 报告 sha256：（注册时由 report-register 计算并登记台账）
