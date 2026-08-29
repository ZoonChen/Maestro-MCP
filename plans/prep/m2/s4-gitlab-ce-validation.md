# S4 GitLab CE 连接器设计验证报告（W1 后半预备）

> 工作层验证记录，不是权威真源。验证对象：`docs/technical/gitlab-integration.md` 的连接器/权限模型/webhook 假设。环境：`docker compose --profile m1-gitlab` 的 `gitlab/gitlab-ce:17.9.1-ce.0`（`127.0.0.1:8181`，一次性沙箱容器，可 `down -v` 丢弃）。全部 token 为本地一次性值，不入库、不入本报告正文。

## 1. 验证矩阵

| # | 设计假设 | 实测结果 | 结论 |
|---|---|---|---|
| 1 | capability detection 按版本/套餐（§13） | `GET /version` → `17.9.1`；`GET /metadata` → `enterprise:false` | ✅ 可行 |
| 2 | Instance/mapping 字段（numeric id、namespace、clone_url、default_branch，§7） | `GET /projects/:id` 全部可得 | ✅ 可行 |
| 3 | 不信任 API 返回的 URL（expected_host/allowlist，§7） | `http_url_to_repo` 返回容器内部主机名（`http://b165a37d7024/...`），非外部可达地址 | ✅ 假设被实证为必要 |
| 4 | Bot 最小 token（read_api）：只读 | 读 MR/pipelines 200；建 MR 403；git push 403 "You are not allowed to upload code" | ✅ scope 层强制 |
| 5 | Bot 无源码 push capability（`GL-INV-005`/`GL-GATE-003`） | `api` scope token（Developer）推**保护** `maestro/*` 被 pre-receive 拒绝；推**未保护**分支成功 | ⚠️ 见偏差 3 |
| 6 | 保护分支纵深 | `main` 与 `maestro/*`（push_access_level=0）连 root 也拒推；merge_access_level=40 可限 Maintainer | ✅ 纵深实测有效 |
| 7 | merged 真相字段（`GL-INV-003`） | MR 返回 `state`、`detailed_merge_status`、`merge_commit_sha`、`merged_at`、`sha` | ✅ 可行 |
| 8 | Pipeline Evidence 绑定字段 | pipeline 返回 `sha`、`ref`、`status`（无 runner 时 `pending`）、`source` | ✅（真实 CI 通过/失败路径留 W2 接 runner） |
| 9 | webhook 幂等键 `(instance_id, external_event_id)`（§8） | 真实投递含 `X-Gitlab-Event-UUID`（每次投递唯一）与 `Idempotency-Key` | ✅ 键存在且唯一 |
| 10 | webhook 事件订阅 | push / merge_request / pipeline / tag_push 均可订阅；payload 键：push=`before/after/checkout_sha/ref/project_id`，pipeline=`builds/commit/merge_request/object_attributes` | ✅ 与事件目录设计可对齐 |

## 2. 与设计的偏差（I2 契约冻结必须吸收）

1. **无时间戳 header**：GitLab 17.9 投递不含 `X-Gitlab-Timestamp`。§9 的 "timestamp/replay window" 不能依赖 GitLab 时间头 → 改为：接收侧记录 `received_at` + `X-Gitlab-Event-UUID` 去重 + 短重放窗口（同 UUID 窗口内拒收）。
2. **`X-Gitlab-Token` 是静态共享密钥，非 HMAC 载荷签名**："对原始 bytes 验签"应调整为：常量时间 token 比对（设计已含）+ 强制 HTTPS/TLS + payload 一律按不可信处理（设计本有此立场，需把措辞从"验签"改为"token 校验"以免实现误导）。
3. **CE 无 "MR 可写但仓库不可推" 的 scope 组合**：`api` scope 同时具备建 MR 与推未保护分支能力。Bot 边界（`GL-INV-005`）的强制层 = 保护分支配置：`main` 与 `maestro/*` 通配保护、push 授权仅给 Runner broker 专用 principal（本测试 push_access_level=0 连 root 都拒，证明保护层是硬边界；生产配置需显式建 broker 授权）。代码层红线（中央代码无 push/merge 调用）保持不变。
4. **本地/内网 webhook 目标默认禁止**：需 `allow_local_requests_from_web_hooks_and_services`。生产 CP 用正式域名不涉及；但 CI/联调环境（CP 跑在 compose 网络内）需要把该开关列入 GitLab onboarding runbook。

## 3. 真实 webhook header 清单（17.9.1 实测）

```text
Content-Type:        application/json
X-Gitlab-Event:      Push Hook | Pipeline Hook | ...
X-Gitlab-Event-UUID: <per-delivery uuid>        ← Inbox external_event_id
X-Gitlab-Webhook-UUID:<per-webhook stable uuid> ← hook 身份/映射校验
Idempotency-Key:     <per-delivery uuid>        ← 重试幂等参考
X-Gitlab-Instance:   <instance base url>
X-Gitlab-Token:      <共享密钥, 常量时间比对>
```

## 4. 给 I2 / S4 W2 实现的建议输入

- Inbox 幂等主键采用 `(instance_id, X-Gitlab-Event-UUID)`；`X-Gitlab-Webhook-UUID` 用于 mapping→hook 一致性校验（防串号）。
- webhook 创建/更新归 project_admin（Maintainer+）；Bot（Developer + `api`）只做 MR/pipeline 读写——两身份分离与设计 §2 一致，已在实测中验证可行。
- MR 对账轮询字段最小集：`iid/state/detailed_merge_status/merge_commit_sha/merged_at/sha`；pipeline 为 `id/sha/ref/status/source`。
- onboarding 时以配置的 `base_url` 为准覆盖 API 返回的 `*_url_to_repo`（偏差 3 的必然推论）。

## 5. 复现与环境处置

- 复现：`docker compose --profile m1-gitlab up -d gitlab-ce` → rails 创建 root PAT → 建 project/保护分支/Bot → hook 指向 `host.docker.internal:<port>`（容器内 127.0.0.1 不是宿主机）。
- 处置：本沙箱容器与卷可整体丢弃（`docker compose --profile m1-gitlab down -v`）；maestro-pg-drill（15432）为另一独立钻孔容器。
