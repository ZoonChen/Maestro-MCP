# 任务书 J2a：Work Graph 模型与资产台账（ADR-009 激活·其一）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → `docs/decisions/ADR-009-versioned-typed-work-graph.md` + `docs/technical/work-graph-model.md`（草案全文）→ `plans/prep/pilot/ARTIFACT-STANDARDS.md`（台账语义）→ 本任务书。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-j2a -b j2/workgraph-model origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-j2a && make web-build
```

## 1. 使命

**第 0 步：走 ADR-009 评审**（draft→approved 需 product/security/qa/operations 四方批准——产出评审请求件，owner 批准后翻 frontmatter，同 MR 内完成）。随后落地模型与存储层（不含拆解协议与调度）。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| J2a-1 | `work-graph-model.md` 修订定稿（按 ADR §6-§9 固化四类关系分表、不变量、UUIDv7/slot_key/spec_digest 规则），`work-graph-scheduler.md` 标注"由 J2b 定稿" | docs 门禁过 |
| J2a-2 | 迁移 0017（与 J1 的 0016 并行编号协调，先合者定号）：`plan_revisions`（sealed 不可变）、`work_nodes`/`node_revisions`（contains/requires/consumes/produces/血缘分表）、**资产台账表**（asset_id/version/status/lifecycle/sensitivity/supersedes/digest/locked_gate，机检规则在 DB 约束+应用层双层） | PG 门控：不可变触发器、supersede 链、机检拒绝 |
| J2a-3 | store 层：图 CAS（expected_graph_version/node_version）、资产注册/评审/审批/替换（每次流转落 `asset.*` 审计事件，同事务）、**存量摄取命令**（`maestro asset-intake`：md 原文/xlsx digest+摘要/会话记录指针三类模式，消费 ARTIFACT-STANDARDS §5 首批清单） | PG 门控全链路 |
| J2a-4 | Gate 绑定：locked_gate 消费校验 + 制品 supersede 使下游 stale（复用既有 stale 语义） | 集成测试 |

## 3. 边界与 DoD

可改：`internal/store/**`、`cmd/maestro/**`（intake 命令）、`docs/decisions/ADR-009*`、`docs/technical/work-graph-model.md`、迁移 README。禁改：web、handler 路由（J2c 管）、矩阵。DoD 同 brief-E 全套。

## 4. 交接物

ADR 评审记录；台账/图存储 implemented 候选；与 J1/J3 的编号与接口协调记录；J2b 的输入（模型定稿版）。
