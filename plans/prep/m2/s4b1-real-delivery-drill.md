# M2-P4 预演：真 GitLab 投递离线演练（#43+#44 合并态）

> 工作层预演记录（离线时段本地完成）。环境：本地 GitLab CE 17.9.1 沙箱（Docker，127.0.0.1:8181）→ 宿主直跑 server（`m2/webhook-rehearsal @ c6abfbc`，即 #43+#44 合并态）→ 钻孔 PG（迁移 0001–0005）。全程无外网依赖。

## 1. 演练链路

沙箱项目 webhook 指向 `host.docker.internal:28084/webhooks/gitlab/{instance_id}`（共享 token 与 `webhook_secret_ref=env:MAESTRO_GITLAB_WEBHOOK_TOKEN` 一致）→ 真实操作触发：

| 真实事件 | 触发动作 | ingest 结果 |
|---|---|---|
| `push` | git push 新分支 | ✅ accepted |
| `merge_request` | API 创建 MR !#2 | ✅ accepted |
| `pipeline` | push 触发 CI（无 runner，pending） | ✅ accepted |

随后三事件均按预期转 `dead_letter(UNMAPPED_PROJECT)`——沙箱项目未做 mapping（S4a 连接器的 onboarding 正是补这一环）。

## 2. 关键发现

1. **载荷静态加密实测**：明文约 300 字符的 push 载荷在 `webhook_inbox` 中以 1836 字节二进制密文存储（`\xb655…` 前缀）——`MAESTRO_WEBHOOK_PAYLOAD_KEY` 的 AES 落库加密真实生效，DBA/备份侧无法读到明文。
2. **真实事件种类与 #44 手工报文一致**：收件器将 header `Push Hook/Merge Request Hook/Pipeline Hook` 规范化为小写 `push/merge_request/pipeline`，与 #14 捕获的 header 实名和 #44 手工形状完全吻合——手工演练形状可作为 CI fixture 基础。
3. 离线可用性：收件链路（沙箱→server→PG）全本地可演练，内网恢复后可直接复跑 S4a 端到端（mapping→MR 同步→merged→done）。

## 3. 后续（回网/依赖解锁后）

- S4a 落地后同拓扑复跑：补 `gitlab_project_mappings` → 验证 accepted 全链与 UNMAPPED_PROJECT 死信 replay。
- 本文与 #44 手工矩阵共同构成 P5 Webhook 用例的证据基础。
