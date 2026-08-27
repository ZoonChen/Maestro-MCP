---
doc_id: SEC-THREAT-MODEL
spec_version: 3.0
spec_status: review
implementation_status: partial
verification_status: unverified
owner_role: security_owner
approver_roles: [security_owner, technical_lead, operations_owner]
introduced_in: M0
authority_for: [security_assets, trust_boundaries, abuse_cases, mandatory_security_controls]
related_adrs: [ADR-001, ADR-002, ADR-003, ADR-004, ADR-005]
related_specs: [../specs/openapi/control-plane.yaml, ../specs/openapi/runner.yaml, ../specs/asyncapi/events.yaml, ../specs/rbac/permissions.yaml]
related_tests: [../testing/integration-test-plan.md, ../testing/agent-evaluation-redteam.md, ../testing/pilot-acceptance.md]
last_verified_commit: null
---

# Maestro v3 威胁模型

> 本文规定 v3 目标安全边界。M0 已关闭匿名非健康入口、默认远程写、本地 merge、任意命令字符串和危险 workspace 路径，并将宿主 Profile 执行限制为显式本地 diagnostic 模式；共享本地 Token、未隔离 host executor 仍只适用于开发基线。OIDC/RBAC、设备身份、rootless OCI、GitLab/Webhook 和生产审计边界尚未实现。

## 1. 目标与非目标

`SEC-REQ-001`：保护公司源码、成员身份、GitLab 凭据、质量证据、审计记录与控制面状态，使一个被攻陷的 Agent、客户端或 Runner 不能跨项目、提升权限、伪造证据或合并保护分支。`SEC-REQ-002`：安全依赖异常时能力必须收缩并 fail-closed。本文不承诺抵御已完全控制 GitLab 管理面或企业身份提供方的攻击；这类风险通过职责分离、平台审计和灾难恢复降低。

## 2. 参与者、角色、权限和信任边界

| 边界 | 主要资产 | 攻击者能力假设 | 强制控制 |
| --- | --- | --- | --- |
| Browser/MCP Client | 用户会话、项目上下文 | 可构造任意请求和 Prompt | OIDC/OAuth、服务端授权、输入 Schema、限流 |
| Control Plane | 策略、任务、审计、集成状态 | 外部输入不可信；管理员也不自动拥有源码权限 | 无源码挂载、最小权限、事务审计、网络分段 |
| Runner/沙箱 | 工作区、临时凭据、执行输出 | Agent 和仓库内容均可能恶意 | 出站连接、设备身份、短 Lease、rootless 隔离、资源限制 |
| GitLab/Webhook | 远端 SHA、MR、Pipeline、审批事实 | 事件可伪造、重放、乱序或遗漏 | TLS、签名、Inbox 幂等、对账、精确 SHA 绑定 |
| Secret Store/Artifact Store | Token、密钥、证据产物 | 引用和 URL 可能泄漏 | 句柄化、短期授权、轮换、加密、访问审计 |

人类 Principal、委托 Agent、Runner Device 与服务账户是四类独立主体，凭据不得互换。最终合并只能由 GitLab 中有权限的人类执行。

## 3. 触发条件、输入和前置条件

新入口、Tool、Runner capability、GitLab 权限、Secret 类型、数据类别或跨边界数据流变更时必须更新威胁模型。进入设计评审前必须提供数据流、调用主体、资源范围、失败语义、日志字段和滥用预算；缺失任一项不得批准。

## 4. 正常交互及时序图

| STRIDE/滥用场景 | 目标控制 | 剩余风险与负责人 |
| --- | --- | --- |
| 伪造用户、Runner 或 Webhook | OIDC audience、设备密钥、Webhook HMAC/Secret | IdP/GitLab 管理面失陷；security_owner |
| 请求或事件篡改 | TLS、Schema、原始 Body 验签、digest | 端点主机失陷；operations_owner |
| 否认高风险操作 | 不可变 AuditEvent、correlation/causation ID | 审计存储管理员串谋；security_owner |
| 跨项目信息泄漏 | project-scoped 查询、资源隐藏、日志脱敏 | 业务层漏加 scope；technical_lead |
| Webhook/任务/输出 DoS | 限流、队列、配额、输出和时间上限 | 合法流量尖峰；operations_owner |
| Agent/Runner 提权 | 权限交集、固定 Profile、rootless OCI、无宿主 Socket | 内核/运行时漏洞；security_owner |
| 伪造测试或 stale SHA | GitLab CI 权威 Evidence、SHA/策略绑定 | GitLab Runner 池失陷；qa_owner |

每项高/严重威胁必须有 Owner、测试 ID 与关闭证据；只写“由网络或 UI 保证”不构成控制。

## 5. 失败、取消、超时、重试、恢复和用户提示

OIDC、授权器、Secret Store、GitLab 身份校验、Webhook 验证、策略解析或审计事务不可用时，写操作与 Lease 发放必须拒绝。重试只允许使用同一幂等键且不得绕过重新授权；超时结果保持 unknown/error，不推断成功。用户提示包含安全错误类别、correlation ID 和人工动作，不泄漏目标资源、策略细节或 Secret。

## 6. 状态机、规则和不可变式

- `SEC-INV-001`：默认拒绝；不存在“认证关闭”生产模式。
- `SEC-INV-002`：Control Plane 不挂载源码、SSH 目录、Docker Socket，也不执行仓库命令。
- `SEC-INV-003`：Agent 有效权限是用户、项目成员关系、Runner capability 与 Tool Policy 的交集。
- `SEC-INV-004`：身份隔离、SHA 完整性、策略完整性和 Webhook 真实性不可豁免。
- `SEC-INV-005`：本地 Runner Evidence 只用于诊断，不能使合并 Gate 通过。
- `SEC-INV-006`：安全控制失败不能降级为匿名、共享 Token、任意命令或本地合并。

威胁状态为 `identified → mitigated → verified → accepted/closed`；风险接受必须有到期时间和不同于申请人的 security_owner 批准，critical 风险不得接受后进入试点。

## 7. 字段、配置和格式校验

安全事件必须含 `event_id/occurred_at/principal_id_hash/project_id/action/resource/decision/reason/policy_version/correlation_id/source_ip_hash`。URL 仅允许批准的 HTTPS Host，SHA 与 digest 必须满足对应固定格式，外部 ID 与内部 ID 分列。未知 enum、算法、签名版本、issuer 或 audience 一律拒绝。

## 8. 并发、幂等和一致性

授权决策绑定资源版本；角色、成员、Runner 或 Token 撤销必须主动失效缓存。Webhook、Outbox 和 Runner 结果采用至少一次投递，以唯一事件 ID 和 compare-and-swap 保证一次业务效果。旧 Lease epoch、旧 source/target SHA 或旧策略版本只能保存为 late/stale evidence，不得推进状态。

## 9. 安全、Secret、隐私和审计

Secret 只以 Secret Store reference 存储，日志与事件不得包含源码、原始 Prompt、Token、Cookie、私钥或完整 Webhook body。审计必须覆盖认证、授权 allow/deny、成员和 Runner 变更、策略/豁免、Secret 轮换、Webhook 验证/重放、Evidence/Gate 和应急操作。审计保留至少 365 天并导出到权限独立的集中存储。

## 10. 质量门禁、证据与 fail-closed 规则

`SEC-GATE-001`：高/严重威胁无验证控制、越权测试失败、Secret 扫描发现有效凭据、容器为 privileged/root 或生产配置允许共享 Token 时阻断发布。`SEC-GATE-002`：任何跨边界消息缺少签名/身份、project、版本或 digest 时阻断处理。Gate 证据必须绑定构建 SHA、测试版本和策略版本。

## 11. 指标、SLO、告警和运维动作

监控认证失败、授权拒绝、跨项目探测、签名失败、Webhook 重放、Runner 撤销延迟、Secret 访问、策略解析错误和高风险发现。撤销传播 P99 小于 60 秒；严重告警 5 分钟内通知值班人。异常时执行 `emergency-stop-credential-revoke` Runbook，禁止通过关闭安全控制恢复服务。

## 12. 验收测试和需求追踪

- `TC-SEC-001`：伪造身份、role/project/session 与跨项目 IDOR 全部失败并留审计。
- `TC-SEC-002`：恶意仓库和 Agent 不能访问宿主、其他项目、Secret 或保护分支。
- `TC-SEC-003`：Webhook 伪造、过期、重复、乱序和重放不产生错误业务效果。
- `TC-SEC-004`：任一安全依赖故障均 fail-closed，恢复后无丢失或重复副作用。
- `TC-SEC-005`：红队覆盖 Prompt injection、工具滥用、证据伪造、数据外传和拒绝服务。

## 13. 数据迁移、兼容、发布与回滚

上线前移除共享 Token、进程内任意命令、本地直接 merge 和跨项目扫描。安全控制先 shadow 记录差异，再 enforce；shadow 不得用于放行。回滚只能回到满足同等安全不变量的 v3 版本，保留审计、撤销表、Inbox 水位和 stale 标记，不得恢复 v2 fail-open 行为。
