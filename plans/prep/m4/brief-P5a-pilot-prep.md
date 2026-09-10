# 任务书 P5a：试点准备（依赖 W4.5 全部合入）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → `plans/prep/pilot/`（BLUEPRINT/STANDARDS/CATALOG/PLAYBOOK 四件）→ 本任务书。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-p5a -b p5/pilot-prep origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-p5a && make web-build
```

## 1. 使命

试点地基一次铺完：权威 MR、双仓初始化、沙箱 onboarding、一期 Command Profiles、flags=shadow、存量资产摄取。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| P5a-1 | **权威 MR**：`docs/testing/pilot-acceptance.md` 仓库范围 Go/TypeScript → 增加 Java/Vue（含理由与 Evidence 口径不变） | docs 门禁 + owner 批准 |
| P5a-2 | 双仓初始化：沙箱 GitLab（CE 1931）建 `peixun-backend`（RuoYi-Vue3 底座首导入，License 评审随 BOM 02 结论引用）与 `peixun-web`（Vue3+Element Plus 模板）；两仓首条 Pipeline 全绿（含最小 junit/vitest 报告产物） | Pipeline Evidence |
| P5a-3 | Maestro onboarding：两仓 projects/mappings 注册；pilot flags 建 `peixun` 项目并置 `shadow` | 控制台可见 |
| P5a-4 | **一期 Command Profiles**（版本化）：`maven-build`（JDK17/内网镜像源/单测+覆盖率）、`npm-build`（Node18+/vite build+vitest）、`playwright-e2e`（浏览器钉版本）；沙箱网络白名单（镜像源域名）实测 | 三 profile 在两仓真实执行记录（diagnostic Evidence） |
| P5a-5 | **存量资产摄取**：按 ARTIFACT-STANDARDS §5 执行 `asset-intake`（调研报告/BOM/会话记录三件，sensitivity 分级落实） | 台账可查、审计事件落链 |
| P5a-6 | Jira 实测：内网 Server PAT 连通性（失败按蓝图回退并登记决策） | 实测记录资产 |

## 3. 边界与 DoD

可改：`deploy/gitlab/**`（沙箱脚本）、`docs/testing/pilot-acceptance.md`、试点仓内容（两新仓）、Command Profile 配置。禁改：Maestro 契约文件（如需变更走契约请求）。DoD：brief-E 全套 + 沙箱栈 `make gitlab-up` 可重复拉起。

## 4. 交接物

试点就绪清单（逐项 Evidence 指针）；Jira 实测结论；P5b 的开工确认。
