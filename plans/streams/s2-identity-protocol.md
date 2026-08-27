# S2 身份与协议流任务书（会话输入）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/PIPELINE.md` → 本文件 → 当前任务对应的 `plans/stages/m1-control-plane.md` 与其权威文档。做 brief 之外的事之前先登记偏离项。

## 1. 使命与所有权

端到端拥有**身份、授权与协议层**：OIDC Principal、统一 RBAC（`authorize(principal, action, resource)`）、REST/MCP/WebSocket/background 四面统一授权、MCP 双 transport（stdio + Streamable HTTP）、会话模型。**M0.5 漂移修复的主责流**：把代码对齐既有权威 spec，而不是改 spec。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | 默认拒绝、身份来自服务端上下文等红线 |
| 2 | `docs/README.md`、`plans/PIPELINE.md`、`plans/DISCIPLINE-PHASES.md` | 治理与管线位置 |
| 3 | `docs/security/identity-rbac.md`、`docs/specs/rbac/permissions.yaml` | RBAC 权威 |
| 4 | `docs/prd/roles-and-scenarios.md`、`docs/prd/multi-client.md` | 角色与多客户端语义 |
| 5 | `docs/prd/mcp-protocol.md`、`docs/technical/api-spec.md`、`docs/specs/mcp/tools.schema.json`、`docs/specs/openapi/control-plane.yaml`、`docs/specs/openapi/runner.yaml` | 协议 wire shape 权威（M0.5 对齐目标） |
| 6 | ADR-003（OIDC+RBAC）、ADR-004（MCP transports） | 架构决策 |
| 7 | `docs/delivery/m1-control-plane-runner.md` 第 6/7 章 | 任务范围（含已并入的 M0.5 项） |

## 3. 任务序列

| 波次 | 任务 | 主轴位置 | 收敛点 |
|---|---|---|---|
| W1 | M1-AUTH-001（OIDC/PKCE、Principal、统一 RBAC、审计；**含 M0.5：会话-任务绑定、Session/Worker 注册恢复协议对齐 runner.yaml、消除参数自报身份**） | M1-P4 | V1 |
| W1 | M1-MCP-001（transport 授权、取消/恢复；**含 M0.5：领取接口幂等键/队列版本对齐 tools.schema.json、返回精确 worktree 路径**） | M1-P4 | V1 |
| W2+ | 各阶段协议评审支持（M2 事件目录、M3 Agent 工具面授权、M4 控制台登录态） | 各阶段 P2 参与 | V2–V4 |

**第一步动作（W1 开工时）**：做一次漂移审计——逐条 diff 现有代码接口与 `tools.schema.json`/`runner.yaml`/`control-plane.yaml`，输出漂移清单作为 M0.5 修复的工作清单；禁止反向修改 spec。

## 4. 文件边界

- **可改**：`internal/identity/`（新：OIDC 客户端、principal、authorize 决策点）、`internal/handler/auth.go`（重写为 OIDC 流程）、`internal/mcp/`（server.go 授权装配、tools 各文件的授权上下文改造）、`internal/mcp/tools/worker_tools.go`（幂等键/队列版本/worktree 路径）
- **需协调**：`internal/handler/middleware.go`、`internal/handler/router.go`（路由挂载）、`internal/service/task_lease_service.go`（会话-任务绑定）、`internal/service/session_service.go`（注册协议）
- **禁改**（只随契约 PR）：`internal/store/interfaces.go`、`internal/model/model.go`、`internal/config/config.go`、`docs/specs/**`

## 5. DoD 与本地验收命令

- 流内门禁：`make build && make test && make vet && make lint && ruby scripts/test-hygiene-check.rb`
- 协议专项：`ruby scripts/spec-consistency-check.rb`（tools/权限/OpenAPI 一致性）；新增 MCP 测试用真实协议交互（禁止 REST 等价替代）
- 负测试必须覆盖：伪造 scope/audience/state/nonce、过期 token、CSRF/Origin、跨项目枚举、撤销传播

## 6. 交接物契约（向集成会话）

1. implemented 候选 Task ID 与 Evidence 指针
2. 授权矩阵实测记录（401/403/404 × REST/MCP/WS/background 一致性表）
3. M0.5 漂移修复清单（代码↔spec 对齐前后对照）
4. 契约变更请求（如发现 spec 本身有错：提 MR 而非改代码迁就）

## 7. 与其他流的接口

- **S1**：principal/session/audit 存储语义 → 存储实现
- **S3**：Runner 注册/心跳/恢复协议（S2 定义协议，S3 实现 client 侧）
- **S4**：GitLab 连接器与 Webhook 服务的服务端身份
- **S6**：控制台登录态（OIDC 授权码流复用）

## 8. 内部拆分点

W1 可拆两会话：**S2a**（OIDC + RBAC + 会话绑定）/ **S2b**（MCP transport 授权 + M0.5 对齐）。S2b 可先行（漂移审计不依赖 S2a）。
