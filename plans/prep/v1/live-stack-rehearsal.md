# V1 预演：真实服务栈 P5 联调预演（认证矩阵 / Runner 生命周期）

> 工作层预演记录。环境：宿主直跑 `maestro server`（main@961ab38 构建）+ compose `maestro-postgres`（PG16），**静态 token 本地组合**（无 OIDC/Keycloak）。目的：P5 剧本 #1（认证矩阵）与 #3（Runner 生命周期）的提前实测，并摸清"可用形态"的配置前提。

## 1. 通过项

| 探针 | 结果 |
|---|---|
| `/readyz`（匿名）与 `/healthz`（需认证）分离 | ✅ |
| 认证矩阵四负例 | ✅ 全部稳定错误码 + correlation_id：无 token→`AUTH_REQUIRED`、错 token→`AUTH_INVALID_TOKEN`、畸形/空 Bearer→`AUTH_INVALID_FORMAT`×2 |
| RemoteWriteGuard | ✅ 写操作默认 403 `REMOTE_WRITE_DISABLED` |
| 迁移幂等 | ✅ 对已恢复库 `migrate up` → applied=0 |
| v3 enroll 端到端 | ✅ 注册码（sha256+base64url 哈希入库）→ 设备令牌铸造（24h）→ `pending_approval`；`MAESTRO_RUNNER_TOKEN_SECRET` 缺失/过短时诚实拒启（≥32 字节） |
| claim 端点诚实降级 | ✅（源码确认）`claimNotReady` → 503 `LEASE_DISPATCH_UNAVAILABLE`，不假装成功 |

## 2. 发现（按影响排序）

### A（能力边界，需 runbook 明确）：静态 token 组合下 v3 Runner 设备面不可用
- **现象**：设备令牌走 `Authorization` 头（v3runner.go:42），被全局 `AuthMiddleware`（auth.go）当作 CP token 先行校验并拒绝（401 `AUTH_INVALID_TOKEN`），永远到不了 `requireDevice`。
- **对照**：OIDC 组合的 `Identity.Authenticate` 对 `/api/v3/` 有显式豁免（identity_middleware.go:121），v3 自管鉴权——设计正确。
- **结论与建议**：v3 Runner 面目前是 **OIDC 组合专属**。两条路二选一：① 给静态 AuthMiddleware 加同样的 `/api/v3/` 前缀豁免（对齐两套中间件语义，本地组合可演练设备面）；② 运维文档明确"Runner 生命周期验证需 Keycloak 栈"。**P5 剧本 #3 必须在 OIDC 栈下执行**。
- 本预演因此未能实测吊销 fencing 时延（V1 撤销传播实测的 Runner 设备分量）。

### B（语义澄清）：RemoteWriteGuard 是引擎级"全局写总闸"
- `r.Use(RemoteWriteGuard(...))` 覆盖一切变更——包括 v3 enroll/approve/revoke 与 MCP 写工具。生产 OIDC 形态必须 `MAESTRO_REMOTE_WRITE=true` 才能工作。
- 建议：文档口径从"本地开发显式开关"改为"全局写应急闸（默认关）"；#22 审计表同句联动修订。应急停止价值反而更清晰。

### C（缺口登记）：注册码无管理创建面
- 生产代码仅有 `identity.NewEnrollmentCode()` 铸造函数（devicetoken.go:99），无 HTTP/MCP 创建入口；本预演以 SQL 直插 `runner_enrollments`（演练技巧）。
- 建议：注册码签发管理工具随 claim/lease 一并落地（M1-MCP-001 管理面），否则 OIDC 栈就绪后 P5 仍无法自助发码。

### D（小项）：approve/revoke 在静态组合 fail-closed
- `admin := options.Identity != nil`（main.go:606）——无 OIDC 时 admin 中间件为 nil，管理路由拒绝一切（AUTH_REQUIRED）。与 A 同口径：本地形态无法管理 Runner。

## 3. 可用形态的配置前提（当前 main 的实测结论）

```text
PG 模式：   MAESTRO_DATABASE_DSN（env-only；文件只声明 driver）
本地静态：  MAESTRO_AUTH_TOKEN + MAESTRO_REMOTE_WRITE=true（写）
v3 Runner： MAESTRO_RUNNER_TOKEN_SECRET（≥32B）+ OIDC 栈（admin/设备面）
每日备份：  scripts/pg-backup.sh / pg-restore.sh（#28 修复后含全损场景）
```

## 4. 给 P5 的执行清单增量

1. 剧本 #1 认证矩阵：静态面已预演通过；OIDC 面（错 audience/伪造 scope/过期）需 Keycloak 栈。
2. 剧本 #3 Runner 生命周期：待 OIDC 栈 + 注册码管理工具（发现 C）后执行，含吊销 fencing 时延测量（预演受发现 A 所限）。
3. 发现 A/B 的处置决定建议在 claim/lease PR 中一并带出（中间件豁免一行级改动 + 文档措辞）。
