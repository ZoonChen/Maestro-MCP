# M2-P4 预演：GitLab webhook 收件箱真实投递演练（s4b/webhook-ingest @ d786ba6）

> 工作层预演记录，基于 PR #43 分支头（CI 四绿）。环境：宿主直跑 server + 钻孔 PG（migration 0001–0005 全新应用）；投递为本地构造的真实 GitLab 投递形状（header/体逐字段对齐 #14 实测捕获）。沙箱容器已预热待 S4a 连接器联调用。

## 1. 实测矩阵（全部命中）

| 用例 | 请求 | 结果 |
|---|---|---|
| 合法 token 投递 | 正确 `X-Gitlab-Token` + Event-UUID | **202 `EVENT_PERSISTED`** |
| 同 Event-UUID 重放 | 完全重复投递 | **202 `EVENT_DUPLICATE`**（幂等，无业务副作用） |
| 错误 token | `X-Gitlab-Token: wrong` | **401 `WEBHOOK_TOKEN_INVALID`**（常量时间比对） |
| 未知实例 | 不存在的 instance_id | **404 `INSTANCE_UNKNOWN`** |
| 缺事件头 | 无 `X-Gitlab-Event` | **400 `EVENT_HEADER_MISSING`** |
| 未知事件种类 | `Unknown Hook` | **`UNSUPPORTED_EVENT_KIND`** 拒绝（回复码 `EVENT_KIND_ARCHIVED`） |
| 未映射项目 | token 合法但 project 无 mapping | ingest 202 → **dispatcher 转 `dead_letter`（`UNMAPPED_PROJECT`）** |

## 2. 关键行为发现

1. **两阶段收件设计实测确认**：验签通过即 202 落箱（accepted），后台 dispatcher 再按映射判定——无 `gitlab_project_mappings` 的项目投递转 dead_letter 隔离留证，**不静默丢弃**（`webhook_deliveries` 全量四态在库：accepted×3 / duplicate×1 / rejected×3 / dead_letter×3，含 reject_reason）。
2. **secret ref 设计良好**：`webhook_secret_ref=env:MAESTRO_*` 每请求解析（轮换免重启）；解析失败 fail-closed；env 名白名单正则防进程状态探测。
3. **载荷加密前提**：`MAESTRO_WEBHOOK_PAYLOAD_KEY` 缺失时收件面整体不暴露（诚实降级）。

## 3. 发现（一处，随本分支修复）

**静态组合 `/webhooks/` 豁免缺失**（#32 同类）：OIDC 中间件有 `/webhooks/` 前缀豁免，静态 AuthMiddleware 没有——本地/静态组合下 GitLab 投递会被 401 拦在收件器之前。本分支已镜像补齐（与 #32 的 `/api/v3/` 豁免同一模式）。

## 4. 建议

- S4a 连接器落地时补 `gitlab_project_mappings` 种子，UNMAPPED_PROJECT 死信应在 onboarding 后可 replay（dispatch 面已有 bookkeeping）。
- P5 剧本 Webhook 用例可直接引用本矩阵；沙箱容器（8181，token `s4-v2`）已预热可做端到端真投递。
