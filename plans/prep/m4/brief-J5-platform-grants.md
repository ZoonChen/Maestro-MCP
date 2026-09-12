# 任务书 J5：平台授权（CR-P5a-1，P5b 硬阻塞）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → `deploy/gitlab/peixun/READINESS.md` §5（CR-P5a-1 实证）→ `docs/specs/rbac/permissions.yaml` + `internal/identity`（J1 职能主体机制）→ 本任务书。自包含，小切片。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-j5plat -b j5/platform-grants origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-j5plat && make web-build
```

## 1. 使命

修复 CR-P5a-1：`pilot.write`、`gitlab_instance.configure`、`platform.configure`、`oidc.configure`、`company_policy.manage`、`security.emergency_stop`、`audit.export` 等平台级权限在 PG 部署下无合法授予路径的问题（memberships.role CHECK 禁止 platform_admin 入项目，职能主体 J1 机制只覆盖职能权限串）。试点 flags 的 shadow/gray/full 推进被此卡死。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| J5-1 | 平台授权存储：扩 J1 的职能主体机制（推荐：`functional_principals` 增 `scope` 列或新增 `platform_grants` 表，迁移 0020）——平台级主体绑定平台权限串，与项目角色正交 | PG 门控：授权/撤销/有效期 |
| J5-2 | 身份层接入：resolver 增平台授权解析；`authorizeRoute` 对平台级权限串（无 pid 路由 + 平台写路由）合并判定；审计主体带平台上下文 | 真实 OIDC principal 走通 `PUT pilot-flags` 200（复现 CR-P5a-1 的 403 场景翻绿） |
| J5-3 | 蓝图对齐：SOLUTION-BLUEPRINT §1.3 的 platform_admin 行从"Jira/GitLab 侧岗位"落为 Maestro 平台主体；授权书（§4.2）流程同样适用于平台授权 | 文档同步 + owner 评审标注 |
| J5-4 | e2e：pilot flags 全生命周期（shadow→gray→full→rolled_back）经 API 真实走通（用 P5a 的 peixun 项目） | PG 门控 + Playwright |

## 3. 边界与 DoD

可改：`internal/identity/**`、`internal/store/**`（迁移 0020）、`internal/handler/**`、`plans/prep/pilot/SOLUTION-BLUEPRINT.md`（§1.3/§4.2 对齐）、e2e。禁改：workgraph/jira 存储语义、web 结构、矩阵。DoD：brief-E 全套 + `make e2e`。

## 4. 交接物

CR-P5a-1 关闭声明（403→200 复现证据）；平台授权清单（哪些权限串、哪些主体、有效期策略）；Jira VPN 复测仍为 P5b 观察项（不阻塞）。
