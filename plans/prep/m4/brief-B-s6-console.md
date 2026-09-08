# 任务书 B：S6-console 会话（M4-UI-001 治理控制台）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（调度板，含环境约定）→ 本任务书。做任务书之外的事之前先登记偏离项。本任务书自包含。

## 0. 工作区准备

在主 checkout 执行（调度板第 5 节）：

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-s6ui -b s6/m4-console i4/m4-session-board
cd ~/Works/yuandong/projects/Maestro-MMP-s6ui
npm --prefix web ci
```

后续所有工作在该 worktree 内；分支 `s6/m4-console`。

## 1. 使命与所有权

把 M0 时代的只读 dashboard 演进为治理控制台第一代：OIDC 登录态接线、受认证的写操作、HITL 审批队列、MR/Pipeline 视图与八场景骨架，全部过真实浏览器 DOM 测试。**本会话只做 `web/src/**` 与 e2e 用例，禁改一切后端 Go 代码**；后端缺口登记交接物由集成会话/归属流处理。

现状事实（已勘察确认）：`web/src` 为只读 Preact SPA（约 16 文件/1100 行，无路由库、零写操作）；`web/src/auth/` 已有 OIDC AuthCode+PKCE 半成品（`AppWithAuth.jsx`/`LoginGate.jsx`/`authClient.js` 走 HttpOnly cookie session），但 `main.jsx` 仍渲染匿名只读入口，注释明说"until M4-UI-001 wires the frozen /auth contract"；`api/client.js` 仅 `apiGet()`。后端 `/auth` OIDC 路由组与 `/api/v3` 的 waiver（HITL）端点**已存在**。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | 默认拒绝、无 token 进 bundle 等红线 |
| 2 | `docs/prd/web-dashboard.md` | 控制台产品权威：角色 IA、八场景用户旅程、HITL 审批队列、冲突/错误/降级展示、a11y |
| 3 | `plans/prep/m4/s6-console-ia-design.md` | P2 冻结的控制台 IA 设计 |
| 4 | `docs/specs/openapi/control-plane.yaml` | 端点真源（waiver 三端点、quality-policy、gitlab MR/reconcile 等） |
| 5 | `web/src/auth/` 现有四个文件 + `web/src/api/client.js` | 接线对象 |
| 6 | `docs/delivery/m4-governance-console.md` 的 M4-UI-001 行 | 任务范围与 Test ID（TC-UI-001：角色与八场景 UI E2E） |
| 7 | `plans/prep/m4/session-board.md` | 环境与协作纪律 |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| B1 登录态接线 | `main.jsx` 切换到 `AppWithAuth`；未认证 → LoginGate；会话过期 → 重新登录不白屏。开发代理约定不变（Bearer 只进代理进程，禁 `VITE_*` 传 secret） | 匿名访问受保护路由被引导到登录 |
| B2 API 写方法 | `api/client.js` 扩展 `apiPost/apiPut`（same-origin cookie、JSON body、公开错误码透传）；401/403/404 稳定码映射到 UI 提示 | 错误码单测/组件测试 |
| B3 HITL 审批队列 | 豁免请求/批准/撤销页：消费 `/api/v3` work-items gates waivers 三端点；审批人隔离提示（自批被 403 的展示）；列表-详情-操作流 | Playwright DOM 测试覆盖批准与被拒路径 |
| B4 MR/Pipeline 视图 | 消费 gitlab instances/mapping/MR/reconcile 端点的只读视图：MR 状态、Pipeline 阶段、Evidence 权威标记（merge_gate vs diagnostic） | Playwright 断言关键元素 |
| B5 八场景骨架 | 按 PRD 八场景的用户旅程组织导航（可先空态+占位），角色可见性差异（admin/qa/security/ops） | 路由骨架快照测试 |
| B6 DOM 测试基建 | `tests/e2e/specs-m0/` 新增 spec（真实浏览器、真实二进制，复用 playwright.config 的 Bearer 注入与串行约定） | `make e2e` 全绿 |

依赖说明：B3/B4 用既有端点即可开工，不等待集成会话的契约 PR；契约 PR 提供审计导出/SLO 端点后再补对应视图（交接物登记）。

## 4. 文件边界

- **可改**：`web/src/**`、`web/vite.config.js`（如需）、`tests/e2e/specs-m0/**`（新增用例）
- **需协调**：`web/package.json`（新增依赖需在交接物说明理由与体量）、`tests/e2e/playwright.config.ts`、`Makefile`
- **禁改**：`internal/**`、`cmd/**`、`tests/eval/**`、`docs/specs/**`、`docs/governance/traceability-matrix.csv`

## 5. DoD 与验收命令

```bash
npm --prefix web ci && npm --prefix web run build   # 构建绿
make e2e                                             # 真实浏览器 DOM 测试绿
ruby scripts/test-hygiene-check.rb
make build                                           # 嵌入构建链完整
```

## 6. 交接物（回集成会话）

1. M4-UI-001 implemented 候选声明（首代范围）与 Playwright Evidence 指针
2. 登记项：审计导出/SLO 快照的视图（依赖契约 PR 端点）、pilot flags 视图（依赖 M4-PILOT-001 后端）、a11y 完整达标声明
3. 偏离项清单（如有）
