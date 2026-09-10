# 任务书 J2c：Work Graph 与资产的工具面/控制台（ADR-009 激活·其三）

> **用法**：同 J2a。**前提：J2a、J2b 合入**。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-j2c -b j2/workgraph-surface origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-j2c && make web-build && npm --prefix web ci
```

## 1. 使命

把 Work Graph 与资产台账接到人机两面：MCP 工具目录扩展（登记/评审/放行/查询四类，ROLE-CATALOG 工具面分工原则）与控制台视图。**消费 ROLE-CATALOG §2 的工具清单**：本任务书只建 Maestro 目录内工具，创作类工具明确不建。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| J2c-1 | MCP 工具：`worktree_graph_query`（图/状态/证据视图）、`decomposition_propose`（Coordinator 提案）、`asset_register/asset_review/asset_approve`（含机检与职能审批，权限冻结映射）、`asset_query`——tools.schema.json 钉子 19→24，权限逐工具映射 | 真实 MCP 协议测试（禁 REST 替代） |
| J2c-2 | 控制台：Work Graph 视图（父子树/状态/JoinPolicy 等待）、资产台账视图（生命周期/版本链/sensitivity 筛选）、拆解提案审批（HITL） | Playwright DOM 测试 |
| J2c-3 | capability routing：工具与 WorkPattern 的能力面路由声明（消费 ADR §2 的边界） | 契约测试 |

## 3. 边界与 DoD

可改：`internal/mcp/**`、`web/src/**`、`tests/e2e/specs-m0/**`、`docs/specs/mcp/tools.schema.json`、spec-consistency 钉子。禁改：workgraph 存储语义（只消费）、矩阵。DoD：brief-E 全套 + `make e2e`。

## 4. 交接物

工具面 implemented 候选 + 钉子更新记录；矩阵新增行（Work Graph 三任务）的收敛仪式登记建议。
