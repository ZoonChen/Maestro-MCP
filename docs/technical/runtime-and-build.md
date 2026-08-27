---
doc_id: TECH-BUILD-001
spec_version: 3.0
spec_status: review
implementation_status: partial
verification_status: unverified
owner_role: technical_lead
approver_roles: [qa_owner, operations_owner, security_owner]
introduced_in: M0
authority_for: [cli_contract, build_graph, runtime_lifecycle, health_checks, release_artifacts]
related_adrs: [ADR-001, ADR-008]
related_specs: [../specs/schemas/config.schema.json]
related_tests: [../testing/integration-test-plan.md, ../testing/pilot-acceptance.md]
last_verified_commit: null
---

# 运行时、构建与发布产物

> 当前实现说明：M0 已提供真实 `cmd/maestro`、五个 CLI 主入口、Web → Go → Docker 构建 DAG、`livez/readyz`、信号排空、真实 REST/MCP/Web/WS smoke、固定工具链和非 root distroless runtime。本地已通过干净源码快照构建、Docker/Compose 安全 smoke、CycloneDX SBOM、源码及镜像 High/Critical 扫描。SQLite 仅允许 server 初始化真正空库；旧版本必须显式 `migrate`，当前版本的 manifest/digest/完整性异常也会拒绝运行；迁移由跨进程锁与确定性计划保护。当前工作树尚无可引用提交和远程 CI 证据；`migrate import-sqlite`、跨平台可复现 digest、SLSA provenance、制品签名及验证属于后续发布闭环，因此仍为 `partial/unverified`。

## 1. 目标与非目标

- `BLD-REQ-001`：干净 clone MUST 通过单一入口构建 Web、Go binaries 与不可变镜像。
- `BLD-REQ-002`：`maestro server|runner|migrate|doctor|version` MUST 具有稳定参数、退出码、信号与日志契约。
- `BLD-REQ-003`：构建 MUST 可追溯到源码 commit、依赖 lock、工具链版本与镜像 digest。
- 非目标：不承诺不同主版本配置/数据库降级兼容，不在 server 容器内提供编译工具链。

## 2. 参与者、角色、权限和信任边界

| 参与者 | 权限 | 约束 |
| --- | --- | --- |
| developer | 本地 build/test/doctor | 不读取生产 Secret |
| release pipeline | 构建、签名、推送制品 | 固定 runner 与最小 registry scope |
| operator | migrate/server/version、受控 runner 注册 | migrate 与 server 运行身份分离 |
| `maestro server` | 网络与 PostgreSQL | 无 workspace/Docker Socket |
| `maestro runner` | 出站控制面、沙箱 runtime | 无监听公网端口、无控制面 DB 凭据 |

## 3. 触发条件、输入和前置条件

- 工具链锁定：Go `1.26.6`、Node `22.14.0`、npm `10.9.2`；后续升级 MUST 由独立 PR 同步 CI、容器与 SBOM。
- 必需输入：`go.sum`、`web/package-lock.json`、源码 commit、构建平台；缺失 lockfile MUST 失败。
- 运行前：配置通过 JSON Schema；server 连接 PostgreSQL、OIDC metadata 与 Secret Store；runner 具有有效设备身份、OCI runtime 与受控 workspace root。

## 4. 正常交互及时序图

```mermaid
flowchart LR
  LOCK[Lockfiles + pinned toolchains] --> WEB[npm ci && npm run build]
  WEB --> EMBED[verify web/dist manifest]
  EMBED --> GO[go test + go build server/runner]
  GO --> IMG[distroless/rootless images]
  IMG --> SBOM[SBOM + vulnerability scan]
  SBOM --> SIGN[sign + provenance]
  SIGN --> SMOKE[clean-container smoke]
```

CLI 契约：

| 命令 | 必需行为 | 禁止行为 |
| --- | --- | --- |
| `server` | 启动 HTTP/MCP/Web/dispatcher；支持 `--config` | 自动迁移破坏性 schema |
| `runner` | 注册/连接/排空；支持 `--config --runner-id` | 接受任意命令字符串 |
| `migrate up` | 获取 advisory lock，打印 plan，幂等执行 | 与另一个 migrate 并发 |
| `migrate import-sqlite` | 校验只读源、dry-run、生成报告 | 修改源 SQLite |
| `doctor` | 检查配置、依赖、权限、时钟、磁盘 | 输出 Secret |
| `version` | 输出 semver、commit、build time、schema/protocol range | 访问网络或数据库 |

## 5. 失败、取消、超时、重试、恢复和用户提示

退出码：`0` 成功，`2` 参数/配置错误，`3` 依赖不可用，`4` 认证/授权失败，`5` 迁移失败，`6` 不兼容版本，`10` 内部错误，`130/143` 对应 SIGINT/SIGTERM。错误输出 MUST 含稳定 code 与 correlation ID，不含 Secret。

SIGTERM 后 server MUST 立即将 readiness 置 false，停止新 Lease，在 30s 内排空 HTTP/Outbox；runner MUST 停止领 Lease、请求取消/续约现有执行并在 60s 后强制终止沙箱。非幂等迁移不得自动重试。

## 6. 状态机、规则和不可变式

运行态为 `starting → ready → draining → stopped`，依赖失败可进入 `degraded_read_only`，不得从 degraded 发出新授权或完成事件。

- `BLD-INV-001`：`livez` 仅表示进程事件循环存活；`readyz` 必须检查 DB、migration compatibility、dispatcher leader 与配置有效性。
- `BLD-INV-002`：版本信息、OpenAPI 和前端资源 MUST 来自同一 commit。
- `BLD-INV-003`：构建期间不得下载未锁定的 `latest` 工具或修改 lockfile。

## 7. 字段、配置和格式校验

配置优先级为 `CLI 非敏感参数 > MAESTRO_* 环境变量 > 配置文件 > 编译默认值`；Secret 值只允许 `secret_ref`，不得由 CLI 参数传入。未知配置字段、重复 YAML key、无效 duration/URL/CIDR MUST 失败。监听地址、OIDC issuer、GitLab host、数据库 DSN 和 workspace root 必须分别校验。

版本采用 SemVer；镜像 tag 仅为展示，部署 MUST 使用 digest。`SOURCE_DATE_EPOCH` 固定为 commit timestamp；生成文件的顺序和时间戳 MUST 可复现。

## 8. 并发、幂等和一致性

- migrate 使用 PostgreSQL advisory lock；已有同版本成功记录时幂等返回。
- server 多副本的 scheduler/dispatcher 使用 leader lease，业务写仍由数据库约束防重。
- shutdown 与 in-flight write 通过请求 context 协作；响应已提交后不得回滚成“未知”，客户端凭 Idempotency-Key读取。

## 9. 安全、Secret、隐私和审计

发布镜像 MUST 非 root、只读 rootfs、无 shell/package manager；构建产生 SPDX/CycloneDX SBOM、SLSA provenance 和签名。CI Secret 不进入 build args/layers。`doctor --json` 默认脱敏。启动、停止、迁移、配置 digest、二进制版本与操作者 MUST 审计。

## 10. 质量门禁、证据与 fail-closed 规则

- `BLD-GATE-001`：`git clean` 后执行标准 build 命令，Web→Go→镜像→smoke 任一步失败即失败。
- `BLD-GATE-002`：Go test `-race`、lint、前端 build、schema lint、SBOM、Critical/High scan 与签名验证为 Required。
- `BLD-GATE-003`：binary 启动真实 REST health、MCP `initialize/tools/list`、静态资源并通过 graceful shutdown。
- 未生成 provenance、版本为 dirty（正式发布）或 runtime image 使用 root MUST 阻断发布。

## 11. 指标、SLO、告警和运维动作

记录启动耗时、ready 状态、shutdown 排空时长、build duration/cache hit、镜像大小、迁移耗时与失败数。ready 连续失败 2 分钟、migration lock 超 5 分钟、runner 时钟偏差 > 30s MUST 告警。发布流水线保留日志与制品摘要至少 365 天。

## 12. 验收测试和需求追踪

| 测试 ID | 场景 |
| --- | --- |
| `TC-BLD-001` | 无 `web/dist` 的 clean clone 完成全构建 |
| `TC-BLD-002` | 五个子命令 help、成功、错误与退出码契约 |
| `TC-BLD-003` | SIGTERM 排空且不再分配 Lease |
| `TC-BLD-004` | 两次相同 commit 构建产生等价 digest/provenance |
| `TC-BLD-005` | 配置未知字段与明文 Secret fail-closed |

只有目标提交上的 clean-clone CI、镜像扫描、签名/provenance 与审批全部通过后，本文件才可标 `implemented/passed`；本地运行结果只能作为候选证据。

## 13. 数据迁移、兼容、发布与回滚

先新增真实 `cmd/maestro` 装配和子命令，再统一工具链与 Web build DAG，最后替换镜像/Compose。每个二进制声明 `min/max_schema_version` 与 Control Plane/Runner protocol range；超范围启动失败。发布采用 canary，ready 后切流；回滚使用上一 digest。数据库已经执行不可逆 contract migration 时禁止二进制回滚，必须执行预演过的 forward-fix。
