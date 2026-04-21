# 3.8 恢复与灾难处理

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 核心机制 > 恢复与灾难处理
> **相关文档:** [Worktree 模型](worktree-model.md) | [并发模型](concurrency-model.md)

---

## 进程重启时的恢复流程

```
启动时:
1. 打开 SQLite
2. 扫描 agent_sessions WHERE status='online'
   → 全部标记 offline (进程重启意味着所有连接断开)
3. 扫描 tasks WHERE status='in_progress'
   → 回退为 pending，清空 assigned_session_id/assigned_worker_id (没有 Agent 在执行了)
4. 扫描 tasks WHERE status='verifying'
   → 回退为 submitted (验证者 Session 已断开，清空验证者 Worker 的 current_task_id，其他验证者可重新领取)
5. 扫描 worktrees WHERE status='active'
   → 标记为 stale (对应的 session 已不存在)
6. 扫描 tasks WHERE status='blocked'
   → 保持 blocked (等待协调者解除)，清空 assigned_session_id/assigned_worker_id
7. 扫描 tasks WHERE status='merge_conflicted'
   → 保持 merge_conflicted (等待协调者决定 reopen/cancel/followup)
   → 对应 worktree 保持 stale（步骤 5 已标记），reopen 时需检查 session 在线状态
8. 启动后台 GC goroutine
```

## 后台 GC goroutine 职责

除了 Worktree 生命周期清理外，GC goroutine 还负责以下数据生命周期管理（保留策略见 [NFR](../prd/nfr-milestones.md)）：

| 清理对象 | 保留期限 | 清理方式 |
|---|---|---|
| Worktree（abandoned） | abandoned 后 1h | `git worktree remove` + 目录删除 + 删除数据库行 |
| Worktree（merged） | merged 后 1h | 同上 |
| `activity_log` 记录 | 90 天 | `DELETE FROM activity_log WHERE created_at < datetime('now', '-90 days')` |
| `audit_log` 记录 | 180 天 | `DELETE FROM audit_log WHERE created_at < datetime('now', '-180 days')` |
| 测试日志文件 | 30 天 | 删除 `.maestro/logs/tests/` 下超过 30 天的文件 |
| done/cancelled Task 数据 | 永久保留 | 不清理 |

## 不一致状态处理

| 场景 | 检测方式 | 恢复策略 |
|---|---|---|
| Task in_progress 但无 session | 启动时扫描 | 回退到 pending |
| Task verifying 但 verifier session 不存在 | 启动时扫描 | 回退到 submitted，其他验证者可重新领取 |
| Task blocked 但 session 已 offline | 启动时扫描 | 保持 blocked，清空 assigned_session_id/assigned_worker_id，Worktree 保持 stale |
| Task merge_conflicted | 启动时扫描 | 保持 merge_conflicted，Worktree 保持 stale。协调者 reopen 时按 resolve_merge_conflict 条件判断处理（原 session 离线则回退 pending） |
| Worktree active 但 session offline | 启动时扫描 | 标记 stale |
| Worktree stale 超过 24h | GC goroutine | 标记为 abandoned；abandoned 超过 1h 后 GC 物理清理并删除数据库行 |
| Task submitted 但 worktree 不存在 | 验证时检查 | 标记 blocked, 通知协调者 |
| Merge 到一半崩溃 | worktree status=merged 但 task 未 done | 启动时检查 git log, 补偿完成或回滚 |
| Task ready_to_merge | 启动时保留 | 不受影响，任何验证者可执行 merge |
