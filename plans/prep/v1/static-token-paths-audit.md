# V1 预审：无残余静态 token 路径扫描底账

> 工作层审计输入，对应 `plans/convergence/v1-control-plane.md` §4 审计项"无残余静态 token 路径（除本地开发显式开关且默认关闭）"。取证基线：`origin/main @ 93d26cb`（含 #21 OCI 沙箱）。结论：**通过预审口径**——v3 身份面无静态 token；静态 token 仅作为无 Identity 挂载时的本地回退，空值 fail-closed，写操作另受显式开关门控。

## 1. 逐面取证

| 面 | 取证 | 判定 |
|---|---|---|
| HTTP v3 身份 | `handler/auth.go` 仅 OIDC Bearer 验签，稳定错误码 `AUTH_NOT_CONFIGURED/AUTH_REQUIRED/AUTH_INVALID_FORMAT/AUTH_INVALID_TOKEN`；access token 固定 15 分钟（config.go:463 冻结校验） | ✅ 无静态路径 |
| HTTP 中间件选择 | `router.go:123-129`——挂载 Identity（OIDC）则整体替换为 `Identity.Authenticate`；否则回退 `AuthMiddleware(authToken)`，注释明确"Authentication fails closed when authToken is empty" | ✅ 静态路径=显式本地回退，默认关 |
| Secret 注入通道 | `MAESTRO_AUTH_TOKEN` 仅环境变量（config.go:137 `yaml:"-"` 禁止落 YAML） | ✅ 无配置文件残留 |
| 写操作门控 | `RemoteWriteGuard`（middleware.go:72）显式开启才放行 HTTP 变更，默认 false | ✅ 纵深第二层 |
| MCP 工具面 | `AuthToken` 在 `internal/mcp/`、`internal/handler/` 生产代码零消费；工具身份仅 `TransportBinding`（组合期服务端绑定，payload 不可覆盖，未绑定 fail-closed） | ✅ 无自报/静态旁路 |
| 本地 stdio Runner | 组合期以宿主启动配置注入单用户委托上下文（binding.go 文档语义） | ✅ 归入"本地开发显式开关"口径 |

## 2. V1 审计建议核对动作

1. 部署面复核：compose/生产配置不得同时挂 Identity 与提供 `MAESTRO_AUTH_TOKEN`（静态回退仅存在于未挂 Identity 的本地形态）——建议在 V1 审计表加一行"生产 compose 无 AUTH_TOKEN 环境变量"的静态检查。
2. 运行时复核：对目标部署实例以空/错 Bearer 请求 `/api/v1/*` 与 `/mcp`，断言 401 稳定错误码（联调剧本 #1 已覆盖，此处只是指向）。

## 3. 相关预审索引

- core-coverage 扩展预审：`plans/prep/v1/core-coverage-preaudit.md`（PR #19）
- M0.5 十项销号底账：`plans/prep/v1/m05-blockers-preaudit.md`（PR #20）
- 本文完成后，V1 §4 四个专项审计项中三个已有预审底账；"授权缓存撤销传播实测（P99<60s）"仍需运行时测量（属 P5 联调范畴）。
