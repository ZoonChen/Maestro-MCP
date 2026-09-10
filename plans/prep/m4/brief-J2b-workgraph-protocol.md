# 任务书 J2b：Work Graph 拆解协议与调度（ADR-009 激活·其二）

> **用法**：同 J2a，另读 `docs/technical/work-graph-scheduler.md`（草案）。**前提：J2a 合入**。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-j2b -b j2/workgraph-protocol origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-j2b && make web-build
```

## 1. 使命

DecompositionProposal → 服务端校验（作用域/环/预算/资源）→ 封板 PlanRevision → ExecutionEnvelope（Lease+worktree+context digest）→ 纯函数聚合父状态。Coordinator 只能提案；Agent 不可改图、不可直接标完成。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| J2b-1 | 拆解协议：proposal 的 wire 契约（WorkPattern 版本引用）、四类校验（含 contains 无环、requires DAG、slot_key 唯一）、稳定错误码 | 单测：非法拆解全枚举 |
| J2b-2 | 封板与重规划：seal 生成不可变 PlanRevision；重规划=新 Revision（旧 Attempt 绑旧 NodeRevision+spec_digest 不变） | PG 门控：sealed 不可改、stale 传播 |
| J2b-3 | 执行绑定：ExecutionAttempt 固定绑定 principal/session/worker/worktree/context_digest（ADR §9 最小上下文：精确 SHA/目录边界/直接依赖制品/验收条件/预算） | 与既有 Lease/预算台账集成测试 |
| J2b-4 | 聚合：JoinPolicy/failure_policy/cancel_policy 的纯函数投影 + 幂等重放一致 | 性质测试 |
| J2b-5 | `work-graph-scheduler.md` 定稿 | docs 门禁 |

## 3. 边界与 DoD

可改：`internal/`（新包建议 `internal/workgraph/`）、scheduler 文档。禁改：web、J2a 已合存储语义（只消费）。DoD 同 brief-E 全套。

## 4. 交接物

协议+调度 implemented 候选；capability routing 的消费面说明（J2c 落工具）；BOM 拆解（112 条→父子图）的首次实战留给 P5b。
