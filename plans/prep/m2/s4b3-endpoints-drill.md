# M2-P4 预演：S4b-3 质量 REST 端点验证（s4b/quality-endpoints @ 06a195d）

> 工作层预演记录（离线本地完成）。I1 提交后即时验证。

## 1. 真实授权套件（3/3 PASS）

- `TestQualityPolicyEndpoints`：公司基线生效 / overlay 只增强（削弱拒绝）。
- `TestQualityGatesAndEvidenceReads`：exact-SHA 门禁快照与不可变证据读。
- `TestQualityWaiverLifecycle`：**审批仅限功能性审批人**（"approval is held by functional approvers only"）——distinct-approver 语义经策略字段 `waiver.requires_distinct_approver=true` + `non_waivable_gates` 四类不可豁免清单（身份隔离/SHA 完整性/策略完整性/Webhook 真实性）在真实 OIDC 授权树（mock IdP + 静态成员解析）下验证。

## 2. 组合层发现（runbook 口径）

- **质量 REST 面为 OIDC 组合专属**：静态 token 组合下所有 `/api/v1` 质量端点返回 **404 `PROJECT_NOT_FOUND`**——静态 principal 无成员关系，防跨项目枚举语义正确生效（无可见性→404 而非 403）。与 #30 发现 A/D（v3 Runner 面）同口径：**运维 runbook 应集中登记"OIDC 组合专属面"清单**（v3 admin + 质量 REST），本地静态组合的演练范围以此为准。
- 迁移 0006 在本分支被修订扩充（含 policy 存储 DDL），全新应用 applied=6 正常（0006 未进 main 前修订合法）。

## 3. 结论

S4b-3 的授权语义（waiver.request/approve 权限、功能性审批人、四类不可豁免、≤7 天）已有真实授权树级测试覆盖；我预写的 HTTP 演练脚本（#48）保留给 **Keycloak 栈**就绪后的组合级复验——那也是 V2 剧本 #5/#6（不可豁免负测试/豁免流程）的正式执行环境。
