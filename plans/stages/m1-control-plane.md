# M1 执行计划（波次 W1 → 收敛点 V1）

> 目标：可信单机基线拆分为中央 Control Plane 与成员侧安全 Runner；OIDC Principal、project-scoped RBAC、PostgreSQL 真源、双 MCP transport。
> 权威依据：`docs/delivery/m1-control-plane-runner.md`（含已并入的 M0.5 漂移修复范围）、ADR-001/002/003/004/008。

## P1 文档规划（I1 主导，波次开局 1–2 天）

### 文档推进清单

| 文档 | 当前状态 | 目标 | 动作 |
|---|---|---|---|
| `docs/delivery/m1-control-plane-runner.md` | review | approved | 评审签署（M0.5 范围已并入第 7 章细分清单） |
| `docs/decisions/ADR-001/002/003` | review/not_started | ≥review 全部、关键项 approved | 补齐评审记录 |
| `docs/security/identity-rbac.md`、`docs/security/runner-security.md` | not_started | ≥review | 按 P2 设计补齐实施级细节 |
| `docs/technical/data-model.md` | partial | review | 补 PG schema 与导入映射章节 |
| `docs/specs/openapi/runner.yaml`、`docs/specs/rbac/permissions.yaml` | 既有 | 冻结版本 | P2 契约 PR 定稿 |

### 需求锚定卡（六任务）

| 任务 | 权威文档 | 关键验收（摘自 M1 Exit Gate 与第 7 章细分清单） | Test ID 范围 |
|---|---|---|---|
| M1-ARCH-001 | technical/architecture.md | 逻辑/部署/信任/故障域骨架；CP 不接触源码的请求/代码/Evidence 流；Runner outbound protocol 与版本协商 | TC-DEP 系列 |
| M1-AUTH-001（含 M0.5） | security/identity-rbac.md、prd/roles-and-scenarios.md | Authorization Code + PKCE；signature/iss/aud/exp/nbf/scope/state/nonce 校验；统一 `authorize(principal, action, resource)`；401/403/404；**会话-任务绑定、Session/Worker 注册恢复协议对齐 runner.yaml、消除参数自报身份** | TC-ROLE-001..004、TC-CLIENT-001..004 |
| M1-DATA-001 | technical/data-model.md | User/Team/Membership/Project/Runner/WorkItem/Lease/Execution/AuditEvent/Inbox/Outbox；project scope 外键；迁移锁；SQLite dry-run/import/report/reconcile | TC-PROJ-001..004、TC-DEP 系列 |
| M1-RUN-001（含 M0.5） | prd/multi-client.md、security/runner-security.md | 10 分钟注册码、Keychain 设备 key、generation/heartbeat/epoch fencing、rootless OCI/cap drop/no-new-privileges/默认无网、Profile-only 命令、**Command Profile 实例配置下发** | TC-CLIENT 系列 + runner-security 威胁用例 |
| M1-MCP-001（含 M0.5） | prd/mcp-protocol.md、technical/api-spec.md | 真实 stdio 与 Streamable HTTP、Origin/auth、cursor 恢复、删除自报 scope、**领取接口幂等键/队列版本对齐 tools.schema.json、返回精确 worktree 路径** | TC-MCP-001..005 |
| M1-DEP-001 | technical/deployment.md、prd/deployment.md | Compose（PG+OIDC+GitLab CE+maestro）、TLS/代理、Secret refs、每日备份/WAL、升级回滚步骤 | TC-DEP-001..004 |

出口 Gate：`ruby scripts/docs-check.rb` 通过；锚定卡 ID 全部可解析；缺口文档有 owner 与时限。

## P2 实现方案（I1 契约冻结 sprint，2–4 天）

1. **M1-ARCH-001 骨架**：按 technical/architecture.md 拆包（control-plane / runner 边界、transport 抽象、dependency health）。
2. **契约 PR（咽喉点一次性合入）**：
   - `internal/config/config.go`：DB（sqlite/postgres 双驱动配置）、OIDC、runner 配置段
   - `internal/store/interfaces.go`：PG 化接口签名（保持 SQLite 实现可用直至切换）
   - `internal/model/model.go`：principal/session/device 实体与会话-任务绑定字段
   - `internal/handler/router.go`：/auth、/api/v1 授权中间件挂载点
   - `docs/specs/mcp/tools.schema.json`：领取接口幂等键/队列版本字段定稿（M0.5 对齐目标）
   - `docs/specs/openapi/runner.yaml`：注册/心跳/恢复协议冻结
3. **Compose 拓扑**：`docker-compose.yaml` 扩展为 maestro-postgres + keycloak（或 dex）+ gitlab-ce + maestro（本地优先原则）。
4. **CI 扩展设计**：`m1-runtime.yml` 草案（services: postgres；OIDC 用测试 mock；GitLab 留 W2）。
5. 各流文件边界表定稿（见各流 brief 第 4 节）。

出口 Gate：契约 PR 经 technical_lead 评审合入；`ruby scripts/spec-consistency-check.rb`、`node scripts/asyncapi-check.mjs`、`node scripts/mermaid-check.mjs` 通过。

## P3 数据模型建设（S1 主导，与 P2 后半并行）

- PG schema：按锚定卡 M1-DATA-001 表清单建表；全部资源外键含 project scope；Outbox/Inbox 与事务同提交语义；迁移锁与版本表。
- SQLite→PG 导入：dry-run → import → report → reconcile 四段式脚本；按 source row identity 幂等；失败整体回滚。
- 回滚方案：expand → dry-run/import → read-only compare → cutover；feature flags 分离 remote MCP / Runner lease / PG write。
- 出口 Gate：本地 Compose PG 上前向迁移 + 导入 dry-run + 回滚演练通过；`ruby scripts/schema-check.rb` 通过；technical_lead 评审。

## P4 代码工程建设（三流并发，1–1.5 周）

| 流 | 任务 | 主要落点（详见各流 brief） |
|---|---|---|
| S1 | M1-DATA-001、M1-DEP-001 | `internal/store/postgres*.go`（新）、`cmd/maestro` migrate/doctor 扩展、`docker-compose.yaml`、导入脚本 |
| S2 | M1-AUTH-001、M1-MCP-001（含 M0.5） | `internal/identity/`（新：OIDC/RBAC/authorize）、`internal/handler/auth.go` 重写、MCP transport 授权、`internal/mcp/tools/worker_tools.go` 幂等键/队列版本、`internal/service/task_lease_service.go` 会话绑定 |
| S3 | M1-RUN-001（含 M0.5） | `internal/runner/`（新：daemon/注册/心跳）、`internal/sandbox/`（新：rootless OCI）、`internal/service/command_profile.go` 配置下发 |

W1 后半启动下阶段预备（只到 P1–P3）：S4 连本地 GitLab CE 做 M2 连接器设计验证；S5 做 Finding/Defect/预算契约设计；S6 做控制台 IA 与登录态组件底座。

每流出口 Gate：`make build test vet lint` + `ruby scripts/test-hygiene-check.rb` 全绿；无咽喉点变更；互审通过。

## P5 测试验证（I1 收敛，3–5 天）

联调场景（剧本详见 [convergence/v1-control-plane.md](../convergence/v1-control-plane.md)）：

- 401/403/404 矩阵；伪造 scope、错误 audience、被撤销 Runner、跨项目枚举全部拒绝并审计
- REST / MCP Tool / MCP Resource / WebSocket / background 授权决定一致（同一 authorize）
- Runner 文件/环境/网络/进程/容器逃逸测试；Control Plane 无源码挂载断言
- PG 备份/恢复/readiness；SQLite 导入演练（dry-run/import/reconcile/回滚）
- M0 回归：既有 tests/m0 全量 + e2e smoke 不回归

## P6 质量工程（V1 收敛仪式）

按 [convergence/v1-control-plane.md](../convergence/v1-control-plane.md) 执行：合流 → 全量门禁 → 审计（含 core-coverage 清单扩展评审：新核心文件纳入 80% 门禁）→ 补丁 → Exit 翻转（m1 书 + 矩阵 M1 六行）→ 复盘。

## 时序估算

I1 冻结 sprint（P1–P2，3–5 天）→ P3 与 P4 前半并行 → P4 三流（1–1.5 周）→ P5 收敛（3–5 天）→ P6（1–2 天）。W1 总粗估 2–3 周。
