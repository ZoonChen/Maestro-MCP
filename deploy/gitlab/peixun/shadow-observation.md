# 影子期观察手册（PLAYBOOK 阶段 2 出口度量）

> **定位**：brief-S2 S2-4 交付物。影子期（阶段 2）每周采集口径、数据面与首周基线。
> 数据面=常驻控制面 `http://127.0.0.1:8080`（认证：`pilot-stack/fetch-token.sh` 取 pilot-admin bearer）。
> 治理归属：本文件属部署侧运维配置（deploy/gitlab/peixun/），阶段复盘时按 ops 类资产入账。

## 1. 四面度量清单（周采口径）

| 面 | 指标 | 数据面（API/库） | 影子期预期 |
|---|---|---|---|
| 任务流 | 工作图计划数/状态（sealed 等） | `GET /api/v3/projects/:pid/work-graph` | D1 治理域 4 计划（sealed）；backend/web 随团队任务拆解增长 |
| 任务流 | 团队 MR 流量（真实开发活动） | `merge_requests` 投影行数/周增量 | 团队照常开发的旁路记录完整性 |
| 证据 | 管线投影完整度（成功率） | `pipelines` 行 + status；`GET …/work-items/:wid/test_report_summary`（P5a 面） | 100% 投影（收件→投影零丢失） |
| 证据 | 形式 Evidence 行数（SHA+策略版本绑定） | `evidence` 表 | **0 为预期**：maestro/* 任务分支 MR 属灰度期流量，影子期不产生元组证据 |
| 对账 | Jira 对账未决数 | `GET …/jira-reconcile-items` | 空清单（手工锚定口径，Jira 不可达回退维持） |
| 对账 | MR 对账操作数/结果 | `POST …/gitlab/merge-requests/:iid/reconcile`（审计+投影 updated_at） | 投影漂移时人工触发；影子期抽查 |
| 对账 | webhook 收件健康 | `webhook_inbox`（processed/dead letter）、`webhook_deliveries`（outcome 分布） | 100% processed；DLQ=0 |
| SLO | 可用性/错误预算 | `GET …/slo-snapshot`（rolling_7d） | availability healthy；目标 99.5% |
| SLO | api_p95 / webhook_ingest_p95 / inbox_lag_p95 | 同上 objectives | healthy（初值：800/500/60ms·s，待两周数据校准） |
| SLO | rpo/rto/备份成功率 | 同上（backup 台账计算） | 无数据直至 backup_runs 有记录（试点实际节奏=阶段 pg_dump，见 §4 注） |

**首演基线（2026-09-12）**：收件 18/18 processed、DLQ 0；pipelines 投影 2/2 success；merge_requests 投影 1（merged，含 merge_commit）；evidence 0（预期）；Jira 对账空；SLO availability 100%（error budget 100%）、api_p95=12.6ms healthy、ingest/inbox no_data（生产者 11:34 起步）、rpo 无数据。

## 2. 周报骨架（影子期周报，每周五填）

```markdown
# 影子期周报 W<N>（<日期区间>）—— 项目：企业学堂 backend/web
- 采样人：<会话/角色>；数据面：常驻控制面 + pg 直查
## 任务流
- work-graph 计划：<数>（状态分布）；本周新增/变更：<…>
- 团队 MR：<本周打开/合并数>（backend <n> / web <n>）；投影完整度 <n/n>
## 证据
- 管线投影：success <x/y>；失败管线与原因：<…>
- 形式 Evidence 行：<0 或 n>（灰度前预期 0）
## 对账
- Jira 未决：<0 或清单>；MR reconcile 操作：<n 次，结果>
- webhook：收件 <n>（processed <n>/dead <0>）；拒绝事件与原因：<…>
## SLO
- availability <%，状态>；error budget 剩余 <%>
- api_p95 <ms> / webhook_ingest_p95 <ms> / inbox_lag_p95 <s>（状态）
## 异常与摩擦（→调度板裁决队列）
- <事件/缺口/团队反馈，附 correlation_id 或 ART 指针>
## 度量口径勘误
- <本周发现的口径问题与修正>
```

## 3. 采集命令（操作备忘）

```bash
~/Works/yuandong/projects/maestro-p5a-bases/pilot-stack/fetch-token.sh   # 900s bearer
TOKEN=$(cat ~/Works/yuandong/projects/maestro-p5a-bases/pilot-stack/pilot-token)
PID=018fb5b0-0000-7000-8000-000000000002   # backend；web=…003；D1 治理域=…004
curl -s http://127.0.0.1:8080/api/v3/projects/$PID/work-graph          -H "Authorization: Bearer $TOKEN"
curl -s http://127.0.0.1:8080/api/v3/projects/$PID/slo-snapshot         -H "Authorization: Bearer $TOKEN"
curl -s http://127.0.0.1:8080/api/v3/projects/$PID/jira-reconcile-items -H "Authorization: Bearer $TOKEN"
# 库侧直查（控制面容器宿主）：docker exec maestro-mcp-maestro-postgres-1 psql -U maestro -d maestro -tAc "…"
```

## 4. 已知口径注记（如实）

1. **rpo_minutes=15 为冻结 schema 上界**，试点实际备份节奏=每阶段 pg_dump 快照（pilot-backups/），backup_runs 台账尚未产生记录（备份调度 worker 按诚实砍掉归运维工具链——会话 I 裁决）；rpo/rto/backup_rate SLO 呈 no_data 属预期。
2. **证据面 API（quality.read）**：pilot-admin（project_admin）不持有该权限（冻结矩阵：worker/verifier/viewer 持有）——403 为正确行为；影子期证据面经控制面只读直查采集，控制台证据视图待 verifier/viewer 角色账号开通后核验。
3. **控制台浏览器登录**：宿主浏览器不可达 IdP（keycloak-pilot:8443 未发布宿主端口、无 hosts 条目）——SPA 与数据面已验证（API 同构 + /dashboard 200），团队浏览器接入路径（发布 8443 + hosts 或内网 DNS）已登记为待裁决项。
4. **config 形状分歧**：resident-config.yaml 按 internal/config 运行时形状（扁平顶层）编写；config.schema.json 的 server/security/gitlab/observability 目标段运行时不受（KnownFields 拒绝）——契约-实现缺口已登记调度板（W5 裁决队列外新条目）。
