# 任务书 E：身份/会话后端切片（关闭 B 登记的 UI-AUTH、UI-2、UI-1、UI-3）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（§2.6 B 登记与 §6 纪律）→ 本任务书。做任务书之外的事先登记偏离项。自包含。

## 0. 工作区准备

在主 checkout 执行（调度板第 5 节；本任务书基于 main `0120c09`）：

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-e2auth -b e/m4-auth-backend origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-e2auth
make web-build
```

## 1. 使命与所有权

关闭 B 会话登记的四个后端缺口，使 M4-UI-001 从"候选"具备翻 `implemented` 的前提：

1. **UI-AUTH**：实装 `/auth` 协议端点（OIDC 授权码流 + PKCE + HttpOnly cookie 会话 + Authenticate 接受浏览器会话）。
2. **UI-2**：`GET /api/v3/projects/:pid/work-items/:wid/waivers`（store 已有 `ListWaiversForWorkItem`，未暴露）。
3. **UI-1**：`POST /api/v3/webhooks/dead-letters/:inbox_id/replay`（G1 的 `ReplayApproval` 存储契约已在 #88 落地，缺 HTTP 面）。
4. **UI-3**：ES256 验签改为 RFC 7515 raw R||S 编码（现用 `ecdsa.VerifyASN1` DER，真实 IdP 互操作会失败）。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | 默认拒绝、token 不进浏览器、状态变更+审计原子 |
| 2 | `internal/handler/router.go` 38-135 行区段 | `/auth` 组挂载点已存在：`opts.Identity.RegisterRoutes != nil` 即挂载——本任务书就是实现它 |
| 3 | `web/src/auth/`（authClient.js/AppWithAuth.jsx/LoginGate.jsx） | B 已按冻结客户端语义写好消费端：`/auth/session`、`/auth/authorize`、`/auth/logout`、HttpOnly cookie、`credentials: 'same-origin'` |
| 4 | `internal/identity/`（TokenVerifier、OIDCMiddleware、EmbeddedPolicy） | 现有 Bearer 验证链；cookie 会话必须汇入同一 authorize 决策点 |
| 5 | `docs/specs/openapi/control-plane.yaml` 的 auth 相关段 + `scripts/spec-consistency-check.rb` | 契约同步与写操作计数钉子（29→30/31） |
| 6 | `docs/security/identity-rbac.md` | 会话安全权威：cookie 属性、CSAF/CSRF 边界、撤销传播 |
| 7 | `internal/webhook/store.go` 的 `ReplayApproval` + `docs/operations/runbooks/webhook-pipeline-failure.md` §8/§9 | 重放端点的双人审批语义（已在 #88 落库） |
| 8 | `tests/e2e/specs-m0/console-governance.spec.ts` | B 的治理拓扑（HTTPS IdP 容器 + maestro 容器 + PG）可复用做真实登录 e2e |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| E1 cookie 会话 | `/auth/authorize`（302 到 IdP，state+PKCE）、`/auth/callback`（code 交换→服务端不透明会话，PG 表，带过期与撤销）、`GET /auth/session`、`POST /auth/logout`；会话 cookie：HttpOnly + Secure + SameSite=Lax；Authenticate 在 Bearer 缺席时接受会话 cookie，但**仅当 Origin 匹配 AllowedOrigins**（CSRF 边界写进负测试：跨 Origin cookie 请求 403） | 单测 + 负测试（CSRF、过期、撤销后 401） |
| E2 waivers 列表 | `GET .../work-items/:wid/waivers`：权限 `quality.read`，返回请求中/已批/已撤销与剩余期限；只读 | PG 门控 handler 测试 |
| E3 DLQ 重放端点 | `POST .../webhooks/dead-letters/:inbox_id/replay`：body = `{requested_by, approved_by, reason}`（服务端以认证主体为 approved_by 并拒绝与 requested_by 相同——双人控制），权限用既有冻结字面量中最贴切的运维权限（若无，登记契约请求勿自创）；调用 `ReplayDeadLetter` 并透传其审计 | PG 门控：自批 403、成功 200、审计行落库断言 |
| E4 ES256 修正 | 验签按 RFC 7515 固定长度 raw R‖S（`ecdsa.Verify` + 手工定长拆分），删除 DER 路径；e2e 铸 token 处同步改 raw；用 RFC 7515 附录 A.7 测试向量锁定 | 向量单测 |
| E5 契约同步 | openapi 增补三组端点；spec-consistency 写计数钉子随行更新；docs 四检全绿 | 本地 docs-check/spec-consistency/schema-check |

## 4. 文件边界

- **可改**：`internal/handler/**`（含 router.go 挂载、auth 新文件、controlplane.go 路由/权限映射）、`internal/identity/**`、`internal/store/**`（会话表迁移 + 会话存储；不动他人已收敛文件语义）、`cmd/maestro/main.go`（RegisterRoutes 装配）、`docs/specs/openapi/**`、`tests/e2e/specs-m0/**`（真实登录 e2e）、`scripts/spec-consistency-check.rb`（钉子）
- **禁改**：`web/src/**`（B 会话领地——E 合入后 B2 消费）、`internal/webhook/**`（只消费其契约）、`docs/governance/traceability-matrix.csv`、`tests/eval/**`
- 迁移注意：会话表 = 0015，偏差记入迁移 README

## 5. DoD 与验收命令

```bash
make web-build
MAESTRO_TEST_POSTGRES_DSN='postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable' \
  go test -count=1 -p 1 ./internal/... ./tests/m0
ruby scripts/test-hygiene-check.rb
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
ruby scripts/docs-check.rb && ruby scripts/spec-consistency-check.rb && ruby scripts/schema-check.rb
make e2e   # 浏览器套件含真实登录流
```

## 6. 交接物

1. UI-AUTH/UI-2/UI-1/UI-3 的 implemented 候选声明与测试 Evidence 指针
2. 会话表迁移记录与偏差说明；权限映射决策（含未冻结权限时的契约请求登记）
3. 偏离项清单
