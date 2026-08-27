# 3.2 & 3.7 Git Worktree 模型

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 核心机制 > Worktree 物理隔离与资源状态
> **相关文档:** [零信任验证](zero-trust-validation.md) | [恢复与灾难处理](recovery.md) | [边界控制 PRD](../prd/validation.md)

---

## Worktree 生命周期

```
1. get_next_task() → git worktree add .maestro/worktrees/T-005 -b task/T-005
2. 返回上下文中路径映射到 worktree 目录
3. Agent 在独立 worktree 中修改代码
4. submit_task_result() → Maestro 在 worktree 中执行测试和校验
5. 校验通过 → Verifier 将 worktree 分支 merge 回主分支
6. merge 完成 → git worktree remove .maestro/worktrees/T-005
```

**路径映射：** `get_next_task` 返回的 `workspace.root` 指向 `.maestro/worktrees/{task_id}`，`allowed_directories` 相对于该路径。

## 冲突处理策略

| 场景 | 策略 |
|---|---|
| Worktree 创建失败（有未提交修改） | 返回错误："请先 commit 或 stash 当前修改" |
| Merge 时产生冲突 | 通知 Verifier/协调者处理冲突 |
| Worktree 磁盘空间不足 | 配置 `max_worktrees` 限制（默认 10） |
| 无 Git 仓库的项目 | 回退到"目录隔离"模式，要求 `allowed_directories` 之间无交集 |

## Worktree 资源状态模型

```
Worktree 状态机:
┌───────────┐  get_next_task   ┌──────────┐  submit_task_result  ┌──────────┐
│ allocated │─────────────────►│  active  │────────────────────►│ submitted │
│ (已创建)  │   自动变 active  │(工作中)  │                      │ (等待验证)│
└───────────┘                  └────┬─────┘                      └────┬─────┘
                                    │                                  │ merge 成功
                               ┌────▼──────┐                      ┌──▼───────┐
                               │  merged   │◄─────────────────────│  merge   │
                               └────┬──────┘                      └──────────┘
                                    │ 自动 GC (物理清理 + 删行)

注: allocated → active 在 get_next_task 返回前自动完成（Worktree 创建后立即标记 active），实现中无需区分这两个状态。

异常路径:
active → stale     Session 超时, 任务回退 pending
active → abandoned resolve_merge_conflict(action=cancel) 或 cancel_task
submitted → active reject 后 Task 回到 in_progress, Worktree 恢复活跃
submitted → stale  followup 创建后原 Worktree 不再活跃
allocated → stale  Session 在 allocated 阶段超时
stale → active     同一 Session 恢复连接
stale → abandoned  stale 超过 N 小时 (默认 24h)
abandoned → GC清理  物理清理 + 删除数据库行
```

**注意:** GC 清理时直接删除数据库行（而非标记为 cleaned），确保同一 task 可重新创建 worktree 而不受 UNIQUE 约束冲突。

## Session 超时处理规则

1. Session 心跳超时 → session.status = offline
2. 其下所有 Worker 的 current_task_id 清空
3. 对应 tasks 按状态分级处理（遵循 [多客户端支持 PRD](../prd/multi-client.md) 超时恢复规则）:
   - in_progress → status 回退为 pending
   - verifying（仅验证者 Session）→ status 回退为 submitted
   - blocked → 保持 blocked，清空 assigned_session_id/assigned_worker_id
   - merge_conflicted → 保持 merge_conflicted，清空 assigned_session_id/assigned_worker_id
   - ready_to_merge 及之后 → 不受影响
4. 对应 worktrees.status 标记为 stale (不立即删除)
5. 新 Agent 领取同一 task 时:
   - 若 worktree 为 stale → 评估是否可复用 (base_commit 是否过期)
   - 可复用 → worktree.status = active, task 继续
   - 不可复用 → 标记 abandoned, 创建新 worktree

## 定期 GC

后台 goroutine 负责清理:
- 清理 abandoned 超过 1h 的 worktree
- 清理 merged 超过 1h 的 worktree
- 执行 `git worktree remove` + 目录删除 + **删除数据库行**（不留 cleaned 状态记录，避免 UNIQUE 冲突阻碍同一 task 创建新 worktree）
- **GC 清理失败处理:** 若 `git worktree remove` 或目录删除失败（磁盘 I/O 错误、权限问题），返回 `WORKTREE_CLEAN_FAILED` 错误码，记录日志，保留 abandoned 状态等待下次 GC 周期重试。不改变 Worktree 状态（保持 abandoned），避免丢失待清理记录

**资源回收时间线:** active → stale（Session 超时，默认 300s）→ abandoned（stale 超过 24h）→ GC 物理清理 + 行删除（abandoned 超过 1h）。从 active 到彻底清理最长约 25h。
