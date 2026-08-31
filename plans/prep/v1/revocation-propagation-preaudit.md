# V1 预审：授权缓存撤销传播实测口径与前置发现

> 工作层审计输入，对应 `plans/convergence/v1-control-plane.md` §4 审计项"授权缓存撤销传播实测（P99 <60s）"——§4 四个专项的最后一个预审。取证基线：`origin/main @ 99b2595`。

## 1. 现状取证

| 层 | 实现 | 撤销传播语义 |
|---|---|---|
| 授权决策 | `identity/authorize.go` PDP **无决策缓存**，每次判定直读策略与身份面 | 服务端状态变更（角色/成员/Runner 吊销）**即时生效**，天然满足 <60s |
| JWKS 密钥信任 | `identity/token.go` jwksCacheTTL=5min；未知 kid 强制一次刷新 | 仅影响**签名密钥**信任，不是 principal 撤销通道 |
| OIDC access token | 冻结 15 分钟（config.go:463） | **已签发 token 在到期前不因服务端状态失效**——这是与 60s 要求的潜在张力点 |

## 2. 口径建议（供 V1 审计定稿）

1. **撤销的对象**应明确为三类并分别测：①Runner 设备/注册（v3 revoke）→ 旧 generation 连接被 fencing 拒绝（剧本 #3 已覆盖）；②principal 角色/项目成员（PDP 直读，即时）；③已签发 access token（受 15min 寿命约束）。
2. **P99<60s 的合理度量对象是 ①②**（服务端撤销→授权面可见拒绝的时延）。若审计口径包含 ③，则需要在 Identity 层加 token denylist 或将 runner 短 token 与吊销联动——这是**设计决策**，建议 V1 审计时由 technical_lead + security_owner 明确取舍，而不是在 P5 现场发现口径缺口。
3. **测量设计**（P5 可直接执行）：compose 起真实服务（PG + OIDC mock/Keycloak）→ 预热 N 个并发会话循环请求 REST/MCP/WS → 注销/降权/吊销 → 记录每请求从撤销时刻到首个 401/403 的时延 → 报 P99；同时断言审计事件齐全。WS 面注意长连接需显式断言断开或降权后消息拒绝。

## 3. 发现与建议动作

- **发现**：无决策缓存使 ①② 即时满足，但 15min token 与"撤销 60s 可见"若被解释为包含 ③，则存在缺口。建议在 V1 审计表中把 ③ 显式登记为"接受 15min 上界"或"补 denylist"二选一，附本预审为依据。
- 若未来为性能引入决策缓存（当前无此实现），jwksCacheTTL 的 5min 先例表明缓存必须显式声明撤销通道；引入时应同步扩展本实测。

## 4. V1 §4 预审索引（四项齐备）

| 审计项 | 预审 |
|---|---|
| core-coverage 清单扩展 | `core-coverage-preaudit.md`（PR #19） |
| M0.5 十项销号 | `m05-blockers-preaudit.md`（PR #20） |
| 无残余静态 token 路径 | `static-token-paths-audit.md`（PR #22） |
| 授权缓存撤销传播 | 本文（口径 + 15min token 张力登记） |


## 5. 实测补录（2026-08-31，main@84730cf）

Runner 设备吊销分量的实测（静态组合 + compose PG；`/api/v3` 豁免合入后本地可测）：

- 流程：enroll → approve → `/me` 200 → 吊销（runners.status='revoked'）→ 立即循环探测 `/me`。
- **结果：26ms 首次请求即 410 `RUNNER_REVOKED`**（无决策缓存，逐请求直读注册表）——远低于 60s P99 要求。
- 局限：吊销动作经 DB 行翻转模拟（静态组合的 API revoke 需 OIDC admin，见 #30 发现 D）；运行时效果与 API revoke 等价（同一行状态）。角色/成员撤销分量同机制（PDP 直读），P5 用 OIDC 栈补测即可。