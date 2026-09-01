# M2-P6 收敛审计记录（V2 仪式前置）

> 三项阶段特定审计的可执行化：每条审计线映射到 `internal/m2drill/p6_audit_test.go` 中的真实断言（SQL 不变量 + 行为复证），在填充了完整场景（验证证据、诊断证据、已批豁免、脱敏注册表、拒签路径）的 PostgreSQL 数据库上运行。门禁：18 包全绿含 PG、lint 0。

## 审计一：Evidence 权威性（diagnostic vs merge_gate 无混淆）

| 审计线 | 证据 |
| --- | --- |
| 每条 merge_gate 证据携带 provider 谱系（pipeline/job 数字 ID + gitlab_job producer + 完整 SHA 元组） | `TestAuditEvidenceAuthority` SQL 不变量 1 |
| diagnostic 证据不携带 provider 谱系 | SQL 不变量 2 |
| diagnostic PASS 永不满足 Required Gate（同一元组上 sast=diagnostic pass → gate 保持 pending；unit=merge_gate pass → passed） | 行为断言 |
| 结构性写入面：merge_gate 证据仅可经 `EvidenceIngestor`（webhook/reconcile 事实路径）与 store AppendEvidence 写入；HTTP 面无证据写端点 | 代码审查（quality_handler.go 仅 GET evidence） |
| 更正链（supersedes）与乱序收敛 | P5 剧本步骤 2/4（`TestP5ConvergencePlaybook`） |

## 审计二：豁免流程（审批人隔离 / 期限 / SHA 绑定）

| 审计线 | 证据 |
| --- | --- |
| 每条 approved/active 豁免的审批人非空且 ≠ 申请人（SQL 不变量 + 自审批在 SQL WHERE 中结构性拒绝） | `TestAuditWaiverProcess` |
| 期限自批准起 ≤ 7 天 | SQL 不变量 2 |
| 每条豁免绑定真实存在的 gate 快照行及其精确 SHA | SQL 不变量 3 |
| 不可豁免四原则拒绝、waived 状态由有效豁免真实驱动 | 引擎单测（waiver_test.go）+ 审计行为断言（lint gate → waived） |
| 职能审批人矩阵（waiver.approve 仅 security_owner/qa_owner）| 冻结权限 + routeAction（身份层职能角色建模为后续切片；今日对人类主体正确 403） |

## 审计三：Webhook Secret 管理

| 审计线 | 证据 |
| --- | --- |
| Secret 只以 `env:MAESTRO_*` 引用存在（两列正则不变量） | `TestAuditWebhookSecretManagement` SQL 不变量 |
| 脱敏注册表视图无 secret 字段 | 行为断言（ListInstances） |
| 拒签不回显凭据、不留业务行 | 行为断言（401 响应体无凭据、inbox 零行） |
| 常量时间比较 + TTL 令牌 + 验签前置任何解析 | 引擎实现（verify.go/ingest.go）+ P5 剧本步骤 1 |

## 审计期间发现并修复

**[真实缺陷] DB 装载的豁免永不生效**：`waivers` 表无 check 列，store 装载的豁免 `Check` 恒空，`applicableWaiver` 的 gate 身份 + check 双重校验永不匹配——经 DB 读回的豁免在任何后续评估中都无法 waive 其 gate（引擎单测因内存构造豁免而未暴露；审计以"装载路径"夹具首次真实复证时命中）。修复：`ListWaiversForWorkItem`/`WaiverByID` 以 LEFT JOIN `gate_snapshots` 回填 check（豁免绑定的 gate 行消失时 check 为空 → 永不适用，fail-closed）。

**[用法锐边，已文档化] 引擎级豁免构造必须用带策略版本的元组铸造 gate 身份**（HTTP 路径按行绑定无此问题）——审计夹具即为正确示范。

## 收敛仪式剩余（状态翻转）

m2 任务书与追溯矩阵 M2 六行（M2-GL/WHK/GIT/MR/QG/SEC-001）的 `implementation_status`/`verification_status` 翻转与 `last_verified_commit` 绑定，在本审计与待并分支（keychain、review-fix）合入后的收敛 PR 中执行。
