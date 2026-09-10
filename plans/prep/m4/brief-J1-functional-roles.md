# 任务书 J1：职能角色建模（G-α 关闭）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → `plans/prep/pilot/SOLUTION-BLUEPRINT.md` §1.3/§4.2 → 本任务书。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-j1role -b j1/functional-roles origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-j1role && make web-build
```

## 1. 使命

身份层建模职能主体（security_owner/qa_owner/operations_owner/product_owner/technical_lead），使 `waiver.approve`、`waiver.approve.security` 等冻结职能权限真实可达（UI-4 关闭），并区分项目角色与职能角色的审计主体。设计依据：蓝图 §1.3 映射表、§4.2 授权书（授权书资产本身依赖 J2a 台账——**本任务书先行交付职能主体绑定，授权书追溯作为 J2a 合入后的补强切片**，登记交接）。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| J1-1 | 职能主体存储：`functional_principals` 表（迁移 0016：主体 ID/职能枚举/有效期/撤销位/授权来源引用文本），与 memberships 并列不混用 | PG 门控：生命周期+撤销传播 |
| J1-2 | 身份层接入：resolver 同时解析项目角色与职能角色；`authorizeRoute` 决策点合并两类 grant（职能角色仅授予冻结职能权限，不叠加项目权限） | 单测：职能主体走通 waiver.approve 真实 200；项目角色不越权 |
| J1-3 | OIDC 声明映射：职能绑定来源配置（先支持显式配置表，企业 IAM 组映射登记为后续） | 配置校验 fail-closed |
| J1-4 | 审计主体区分：audit_events 落职能上下文（decision/policy_version 字段复用） | 断言 |

## 3. 边界与 DoD

可改：`internal/identity/**`、`internal/store/**`（新表）、`internal/handler/**`（decision 点）、openapi（如端点变化）、迁移 README。禁改：web、矩阵、`internal/webhook|slo|eval`。DoD 同 brief-E 全套命令。

## 4. 交接物

implemented 候选声明；授权书追溯补强切片登记（依赖 J2a）；蓝图 §1.3 表的实测修订建议。
