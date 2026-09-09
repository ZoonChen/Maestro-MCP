# 任务书 G：试点后端切片（M4-PILOT-001 后端面）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → 本任务书。自包含。

## 0. 工作区准备

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-g2pilot -b g/m4-pilot-backend origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-g2pilot
make web-build
```

## 1. 使命与所有权

M4-PILOT-001 的后端面：rollout flags 的 API 与影子/灰度语义。**地基已在**：迁移 0012 的 `pilot_flags` 表（stage 枚举 off/shadow/gray/full/rolled_back + gray-only 百分比 CHECK + 每项目每 flag 唯一）。**注意：RBAC 里没有任何 pilot 权限**（已核对）——本切片需要契约增补（新权限 + 角色映射 + spec 同步），沿 audit.export 先例走本切片自己的 PR。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `docs/testing/pilot-acceptance.md` | 试点验收权威：影子/灰度/人工验收语义与 PILOT-RULE |
| 2 | `docs/prd/nfr-milestones.md` 的试点段 | 范围（2–5 仓库、影子先行） |
| 3 | `internal/store/migrations/postgresql/0012_m4_governance.up.sql`（pilot_flags 表） | 已冻结的表级不变量 |
| 4 | `internal/handler/controlplane.go` + `docs/specs/rbac/permissions.yaml` + `scripts/spec-consistency-check.rb` | 路由/权限映射模式与契约检查 |
| 5 | `docs/delivery/m4-governance-console.md` 的 M4-PILOT-001 行 | 审计事件 `pilot.decision.recorded`、Test ID TC-PILOT-001 |
| 6 | `plans/prep/m4/session-board.md` | 纪律 |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| G1 存储 | `Pilot()` 存储：List/Get flag、PutFlag（stage 转换守卫：只允许合法迁移，`rolled_back` 终态语义）、变更写 `audit_events`（action=`pilot.decision.recorded`，actor+reason 必填） | PG 门控：非法 stage 迁移拒绝、审计行同事务 |
| G2 API | `GET /api/v3/projects/:pid/pilot-flags` + `PUT .../pilot-flags/:flag`（body：stage、gray_percent、reason≥16 字符）；新冻结权限 `pilot.read`/`pilot.write`（角色：platform_admin 写、project 级读；`pilot.decision.recorded` 审计） | PG 门控 handler 测试 + RBAC 负测试 |
| G3 影子语义 | `shadow` 阶段的判定函数：给定项目 + 功能面，返回 shadow/gray(x%)/full/off——供后续消费方调用（本切片只交付纯函数 + 存储，不做业务面接入） | 纯函数表测试 |
| G4 契约同步 | openapi 两端点 + 权限入 RBAC + spec-consistency 钉子随行（29→30）+ docs 四检 | 全部本地检查绿 |

## 4. 文件边界

- **可改**：`internal/store/**`（pilot 存储新文件）、`internal/handler/**`、`docs/specs/rbac/permissions.yaml`、`docs/specs/openapi/**`、`scripts/spec-consistency-check.rb`、`maestro.yaml.example`（如需）
- **禁改**：`web/src/**`、`tests/eval/**`、矩阵、`internal/slo|eval|m4drill/**`

## 5. DoD 与验收命令

同任务书 E 第 5 节全套。

## 6. 交接物

1. M4-PILOT-001 后端面 implemented 候选声明 + 新权限契约决策记录
2. 影子/灰度判定函数的接入建议清单（供后续消费方）
3. 偏离项清单
