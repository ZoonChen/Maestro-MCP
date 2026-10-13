# 任务书 W9：claim 面错误码与审计补全（F64/F66——灰度期摩擦小切片）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（灰度首演段）→ `~/Works/yuandong/projects/peixun-g3/G3-1-timeline.md`（F64/F66 一手观察）→ 本任务书。咽喉点变更=契约 PR。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-w9 -b w9/claim-errors-audit origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-w9 && make web-build
```

## 1. 使命

灰度首演暴露的两个 claim 面缺口：并发领取时一例 **500 INTERNAL_ERROR**（F66，绕行成功但错误面应为 409 类）与 **claim 成功无审计行**（F64，W8 新语义路径缺审计落链——影子期 claim 审计行为先例存在）。

## 2. 切片

| # | 项 | 内容 | 验收 |
|---|---|---|---|
| W9-1 | F66 错误码 | 复现并发定向 claim 竞争（两 runner 同项，租约唯一索引一胜一败）；败者当前返回 500——应映射为 **409 CLAIM_TARGET_HELD**（W8 已定义的错误面，竞态路径漏映射） | 竞争测试：一胜一 409（带 held_by 信息），零 500 |
| W9-2 | F64 审计补全 | claim 成功路径补 `work_item.claimed` 审计行（含 execution_id/runner/租约到期）+ outbox 同事务（对齐 release 面的先例形态） | claim 后审计行可查；审计链 verify 仍真 |

## 3. 边界与 DoD

可改：`internal/store/postgres_workitems.go`（claim 错误映射+审计）、`internal/handler/workflow_actions_endpoint.go`、测试。禁改：W8 语义本身、租约结构。DoD：brief-E 全套 + **pg-store 覆盖门必查**（白名单 12 文件）+ e2e + 合并后常驻栈重建（切片间隙，pg_dump 前置）。

## 4. 交接物

F64/F66 关闭声明（附首演时序对照：500→409、无审计→有审计）；摩擦登记归档。
