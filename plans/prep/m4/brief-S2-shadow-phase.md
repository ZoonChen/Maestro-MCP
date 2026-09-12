# 任务书 S2：影子期开工（PLAYBOOK 阶段 2 操作）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/pilot/PLAYBOOK.md`（阶段 2）→ `plans/prep/pilot/SOLUTION-BLUEPRINT.md` §3.2（常驻栈责任）→ `deploy/gitlab/peixun/p5b-server.sh`（常驻栈配方）→ 本任务书。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-s2 -b p5/shadow-phase origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-s2 && make web-build
```

## 1. 使命

把试点推进到 PLAYBOOK 阶段 2（影子期）：常驻栈换新、flags=shadow 置位、webhook 同步链路首演、影子期观察面就位。**本任务书含共享栈变更——执行前在调度板登记窗口（ART-incident-002 纪律），变更前后各留 pg_dump 最小备份。**

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| S2-1 | 常驻栈重建 | 按 p5b-server.sh 从 main 构建新镜像（maestro-main:local），**先迁移后换二进制**（J5 红线），退役 8081 对照实例；8080 常驻栈升级后 smoke（/health、控制台、MCP tools 列表） | smoke 记录 + 变更登记 |
| S2-2 | flags=shadow 置位 | 经 J5 平台授权通路（platform_grants 授予→API PUT）将 peixun 项目 rollout flag 置 shadow；审计事件落链 | PUT 200 + `pilot.decision.recorded` 审计 |
| S2-3 | webhook 链路首演 | 常驻栈配 `MAESTRO_WEBHOOK_PAYLOAD_KEY` + 沙箱 GitLab 双仓 webhook 回调（host.docker.internal 通路）；真实 MR 触发→收件箱→投影→对账全链 | 一次真实 MR 的全链证据 |
| S2-4 | 影子观察面 | 阶段 2 出口度量的采集就位：任务流/证据/对账/SLO 四面数据可查（控制台+API spot check）；观察日志模板（周报口径） | 度量清单+首份周报骨架 |
| S2-5 | 团队开工准备 | 影子期团队操作手册一页（如何领任务/提 MR/看控制台——按 ROLE-CATALOG 首演版）；培训材料作为资产登记 | ART 登记 |

## 3. 边界与 DoD

可改：`deploy/gitlab/**`（栈脚本与配置）、常驻栈环境（登记窗口内）、`plans/prep/pilot/PLAYBOOK.md`（实测勘误）、试点仓配置。禁改：Maestro 契约与 internal/**（缺口走 W5/变更请求）。DoD：brief-E 全套 + 栈可重复拉起验证。

## 4. 交接物

阶段 2 开工声明（flags=shadow 审计指针 + webhook 首演证据）；常驻栈变更登记（窗口/备份/回滚点）；观察度量基线（首周数据）；W5 之外的缺口登记。
