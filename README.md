# Maestro MCP

> 公司内部 AI 开发协作控制面：任务编排、Runner 执行、GitLab 协作与质量治理

[![Go](https://img.shields.io/badge/Go-1.26.6-00ADD8.svg)](https://go.dev/)
[![MCP](https://img.shields.io/badge/Protocol-MCP-blue.svg)](https://modelcontextprotocol.io/)
[![Spec](https://img.shields.io/badge/Docs-v3.0%20review-orange.svg)](docs/README.md)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## 当前状态

仓库已形成 **M0 本地实现候选**，但仍不满足内部试点上线条件：

- 已提供真实 `maestro server/runner/migrate/doctor/version` 入口，以及 Web → Go → Docker 构建链。
- REST、Streamable HTTP MCP、本地 stdio MCP、Web、WebSocket 和后台恢复服务由同一 composition root 装配。
- Task/Session/Worker/Worktree 使用集中状态迁移、逐资源 CAS/历史、Lease epoch、队列版本、幂等键和原子启动恢复；活跃 Worktree 仅允许安全重绑定。
- SQLite schema catalog 绑定版本、迁移名称和 digest；空库仅由 server 初始化，旧库必须显式迁移，损坏或伪造 manifest 一律拒绝启动。
- Git、worktree、上下文、diff、coverage、policy 或 Evidence 缺失/异常时验证 fail-closed；领取后的上下文失败会原子补偿，本地 Evidence 仅为 `diagnostic`。
- 非健康端点默认要求认证，远程写和本机命令执行默认关闭；命令只能来自版本化 Command Profile。公开错误、代理头、日志字段和排空阶段均有安全边界。
- 真实 binary/MCP/Git/并发/Heartbeat/重启恢复测试、竞态检查和核心覆盖率门禁已建立；干净源码快照、Docker/Compose、SBOM、源码与镜像安全扫描已在本地通过。

M0 仍标记为 `partial/unverified`，因为当前工作树尚未绑定目标提交、远程 CI Evidence 和规定角色审批。OIDC、PostgreSQL、Control Plane/Runner 隔离、rootless OCI Runner、GitLab 与 merge-gate Evidence 属于 M1/M2，完成前不得接入真实团队仓库。改造前的失败证据保留在[当前实现基线](docs/governance/current-state-baseline.md)。

## v3.0 目标

Maestro v3 为一个团队的 2–5 个 Go/TypeScript 仓库提供：

- 中央 Control Plane：身份、项目、WorkItem、策略、GitLab、审计与 Web 控制台。
- 成员侧 Runner：本地隔离 Workspace、受控 Command Profile 和 Agent 工具接入。
- GitLab 原生闭环：远端 baseline SHA、任务分支、MR、Pipeline、保护分支和人工合并。
- 前后端联调：OpenAPI 兼容检查、跨仓 IntegrationRun 和联合 E2E。
- 缺陷闭环：测试/扫描失败归一、去重、下发、Agent 修复、CI 复测。
- 严格门禁：缺失、错误、跳过或过期 Evidence 一律阻断。
- 可审计 Agent：预算先于调用、真实 token 记账、完整工具轨迹和人类检查点。

## 目标架构

```text
AI 开发工具 ─stdio MCP→ 本地 maestro runner ─出站 HTTPS→ Control Plane
                            │                              │
                   rootless OCI Workspace            PostgreSQL
                            │                              │
                 宿主 Git broker──任务分支──→ GitLab API/Webhook/CI
                                                           ↑
                                      Control Plane Bot──MR/状态同步
                                                   │
                                          人工审批与最终合并
```

固定原则：

1. Control Plane 不挂载或读取项目源码。
2. Agent 权限是“人类主体∩项目成员关系∩Runner 能力∩Tool Policy”。
3. Agent 不得定义命令、网络、Secret、豁免或最终合并。
4. Runner 本地结果仅供诊断；GitLab CI Evidence 才可进入合并 Gate。
5. Evidence 只对精确 source/target SHA 和 policy version 有效。
6. `done` 只能由 GitLab merged Webhook 或对账确认。
7. 成员 GitLab credential 只在 Runner OS Keychain 中由宿主 broker 使用；中央 Bot 不推送源码。

## M0–M4 路线

| 阶段 | 目标 | 任务书 |
| --- | --- | --- |
| M0 | 可运行工程基线、可信状态机、fail-closed 验证 | [M0](docs/delivery/m0-foundation.md) |
| M1 | Control Plane、OIDC、PostgreSQL、本地 Runner | [M1](docs/delivery/m1-control-plane-runner.md) |
| M2 | GitLab baseline、MR、Pipeline、质量门禁 | [M2](docs/delivery/m2-gitlab-quality-loop.md) |
| M3 | 前后端联调、缺陷下发、Agent 修复 | [M3](docs/delivery/m3-integration-defect-automation.md) |
| M4 | 控制台、Agent 评测、审计、可靠性和试点 | [M4](docs/delivery/m4-governance-console.md) |

阶段必须按出口 Gate 逐级通过。M0 未通过前不得把当前服务暴露给团队；M2 未通过前不得让系统操作真实团队仓库；M4 未通过前不得扩大试点范围。

## 文档与机器规范

- [v3 文档中心](docs/README.md)
- [文档治理](docs/governance/documentation-policy.md)
- [需求追踪矩阵](docs/governance/traceability-matrix.csv)
- [产品需求](docs/prd/)
- [技术设计](docs/technical/)
- [安全设计](docs/security/)
- [质量门禁](docs/quality/)
- [测试与评测](docs/testing/)
- [运维与恢复](docs/operations/)
- [OpenAPI、AsyncAPI、JSON Schema 与 RBAC](docs/specs/)
- [架构决策](docs/decisions/)
- [v2.1 历史归档](docs/archive/v2.1/)

字段、状态码和消息结构以 `docs/specs/` 为准；业务语义以相应领域权威文档为准。每个代码 MR 必须引用 Stage Task ID、Requirement ID、Test ID 和验证证据。

## 本地构建与验证

使用 `.go-version` 与 `.node-version` 中锁定的 Go `1.26.6`、Node `22.14.0`，并使用 npm `10.9.2`。`web/dist` 不提交；受支持的构建入口会先生成前端资源，再编译嵌入这些资源的 Go binary。

```bash
make build
./bin/maestro version
make smoke
make coverage
```

Docker Compose 是受支持的单命令本地启动路径。默认仅监听宿主 `127.0.0.1:8080`；端口被占用时通过 `MAESTRO_HTTP_PORT` 覆盖。未配置 Token 时除健康端点外均 fail-closed 返回 401。

```bash
MAESTRO_HTTP_PORT=18080 \
MAESTRO_AUTH_TOKEN='replace-with-a-local-random-token' \
make compose-up
```

### 前端开发调试

后端启动后，可以单独运行 Vite 进行热更新。开发代理只接受 loopback 后端，并在代理进程中添加与后端相同的 Bearer Token；Token 不会进入浏览器 bundle、localStorage 或 URL。禁止使用 `VITE_*` 变量传递 Secret。

```bash
MAESTRO_DEV_BACKEND_URL='http://127.0.0.1:28080' \
MAESTRO_DEV_AUTH_TOKEN='<与后端 MAESTRO_AUTH_TOKEN 相同的新 Token>' \
npm --prefix web run dev
```

打开 `http://127.0.0.1:5173/dashboard/`。Vite 只代理 `/api` 的 `GET/HEAD` 和 WebSocket，其他 HTTP 方法固定返回 `405 DEV_PROXY_WRITE_DISABLED`；缺少 Token、目标 URL 非法或目标不是 loopback 时启动会 fail-closed。当前 M0 Dashboard 是只读监控界面；写流程应使用显式认证的 REST/MCP 客户端，在隔离的可丢弃数据库中验证，且不得连接真实团队仓库。

完整本地候选门禁为 `make release`，包含 unit、race、lint、真实协议 E2E、源码/依赖/镜像扫描和 CycloneDX SBOM。签名与 provenance 需要发布环境的受保护凭据，不由本地目标伪造。

开发约束：

- 不得提交 `web/dist` 来绕过构建 DAG。
- 不得增加兼容路径接受调用方自报的身份、角色或资源范围。
- 不得使用 REST equivalent 代替 MCP 协议测试。
- 不得把本地 `diagnostic` Evidence 用作合并授权。

## License

[MIT](LICENSE)
