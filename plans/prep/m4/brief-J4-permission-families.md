# 任务书 J4：资产与工作图权限族（CR-1 裁决，P5b 前必须合入）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（W4.5 收口节的 J2c 裁决）→ `docs/specs/rbac/permissions.yaml` + `internal/identity` 的策略装载机制 → `plans/prep/pilot/ROLE-CATALOG.md` §2（权限语义的最终消费方）→ 本任务书。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-j4perm -b j4/permission-families origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-j4perm && make web-build
```

## 1. 使命

关闭 J2c 登记的 CR-1/DEC-2 终态：建立 `asset.*`/`workgraph.*` 冻结权限族，补齐 product_owner/technical_lead/operations_owner 的职能授权，seal 摆脱过渡映射。当前 J2c 的六件工具与 seal 端点全部复用既有权限串（单一 qa_owner 审批的 fail-closed 窄面）——P5b 的十角色工作流首演前必须换成真实权限族。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| J4-1 | **权限族入 RBAC**（冻结 machine spec + 内嵌策略同步）：`asset.read` / `asset.register` / `asset.review` / `asset.approve` / `workgraph.read` / `workgraph.propose` / `workgraph.seal` | 两处同步一致，spec-consistency 权限计数钉子随行 |
| J4-2 | **角色授权**：读族→viewer 级项目角色；register/propose→developer 级；`asset.review`→technical_lead+qa_owner；`asset.approve`→**四个职能角色**（product/technical/qa/operations——按制品类型的审批分工由资产 frontmatter 的 reviewers 名单约束，权限保持粗粒度）；`workgraph.seal`→technical_lead；**职能授权**：为 product_owner/technical_lead/operations_owner 补职能主体的权限面（J1 机制已有，grant 面补齐） | 授权测试：每串逐角色正负断言；职能主体经 `AllowFunctionalRole` 走通 |
| J4-3 | **工具与路由换族**：六件 MCP 工具的 `required_permission` 换到新族（tools.schema.json 同步，钉子 25/14 不变）；控制台 seal 端点换 `workgraph.seal`（DEC-2 终态）；assets/workgraph 读端点换读族 | spec-consistency/docs-check/schema-check 全绿 |
| J4-4 | **多角色审批 e2e**：扩既有 PG 门控与 Playwright 套件——同一资产由 qa_owner 与 technical_lead 各自走通 review→approve；seal 由 technical_lead 走通、qa_owner 被 403；委托主体（Agent）对 `asset.approve` 的否决语义保持 | 正负路径全断言 |

## 3. 边界与 DoD

- **可改**：`docs/specs/rbac/permissions.yaml`、`internal/identity/**`（内嵌策略与授权测试）、`internal/mcp/**` 与 `internal/handler/**`（权限映射换族，不动业务语义）、`docs/specs/mcp/tools.schema.json`、`scripts/spec-consistency-check.rb`（钉子）、`docs/security/identity-rbac.md`（权威文档同步，走 owner 评审标注）、e2e
- **禁改**：workgraph/assets 存储与协议语义、web 视图结构（仅权限提示文案）、矩阵（收敛仪式管）
- DoD：brief-E 全套 + `make e2e`
- 注意：权限计数从 60 升（预计 67），写操作数不变（32）

## 4. 交接物

CR-1/DEC-2 关闭声明 + 权限↔角色↔制品类型三方对照表（供 ROLE-CATALOG 修订引用）；identity-rbac.md 的 owner 评审请求件；观察项清单。
