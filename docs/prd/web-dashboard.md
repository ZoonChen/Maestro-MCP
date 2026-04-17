# 3.6 M6: Web 看板

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 功能需求 > Web 看板
> **相关文档:** [配置与部署](deployment.md) | [多项目管理](project-management.md) | [接口规范](../technical/api-spec.md)

---

## 页面布局

```
┌───────────────────────────────────────────────────────────────┐
│  Maestro MCP                    [user-svc ▼]       [Settings] │
├──────────┬────────────────────────────────────────────────────┤
│          │                                                     │
│ PROJECTS │  ┌─ Summary ────────────────────────────────────┐  │
│          │  │  Features: 3  │ Tasks: 12 │ Done: 7 │ Active │  │
│ ● user   │  │  ████████████████████░░░░░░  58%             │  │
│   service│  └──────────────────────────────────────────────┘  │
│          │                                                     │
│          │  ┌─ Active Sessions ─────────────────────────────┐  │
│ ● order  │  │  cc-backend-01 (backend)  3/5 workers        │  │
│   service│  │  ├── default  │ T-005: 支付API      23min    │  │
│ ● admin  │  │  ├── sub-1    │ T-008: 订单查询     15min    │  │
│   web    │  │  ├── sub-2    │ T-009: 退款接口     12min    │  │
│ ○ shared │  │  ├── sub-3    │ idle                        │  │
│   libs   │  │  └── sub-4    │ idle                        │  │
│          │  └──────────────────────────────────────────────┘  │
│          │                                                     │
│          │  ┌─ Task Board ─────────────────────────────────┐  │
│          │  │  Pending (3)    In Progress (2)    Done (7)   │  │
│          │  │  ┌─────────┐   ┌──────────────┐  ┌────────┐ │  │
│          │  │  │ T-008   │   │ T-005        │  │ T-001  │ │  │
│          │  │  │ 实现支付│   │ 订单查询API  │  │ 用户模型│ │  │
│          │  │  │ backend │   │ backend-01   │  │ backend │ │  │
│          │  │  └─────────┘   └──────────────┘  └────────┘ │  │
│          │  └──────────────────────────────────────────────┘  │
│          │                                                     │
│          │  ┌─ Activity Log ───────────────────────────────┐  │
│          │  │  14:32  backend-01  submitted T-005 (92%)    │  │
│          │  │  14:28  frontend-01 claimed T-007            │  │
│          │  └──────────────────────────────────────────────┘  │
└──────────┴────────────────────────────────────────────────────┘
```

## 功能清单

| 功能 | 说明 |
|---|---|
| **项目侧边栏** | 左侧列出所有已注册项目，点击切换看板视图。实心圆=活跃，空心圆=已归档 |
| **项目下拉选择器** | 顶部下拉框快速切换项目，支持键盘快捷跳转 |
| **跨项目总览页** | 首页展示所有项目的进度概览（缩略卡片），无需逐个点进去 |
| **总览面板** | Feature/Task 统计、整体进度条、Agent 在线状态、软限制告警（如 Task 数超过建议上限） |
| **看板视图** | Kanban 风格，按状态列排布 Task 卡片 |
| **Agent 监控** | 实时显示各 Agent 当前任务、已耗时、Worker 分配、历史完成数 |
| **活动日志** | WebSocket 推送的实时操作流：任务创建/认领/提交/阻塞等事件 |
| **Task 详情** | 点击卡片弹出详情：描述、边界、API 契约、测试结果、变更文件列表 |
| **Feature 视图** | 按 Feature 聚合查看，展示各 Feature 下所有 Task 的进度 |
| **暗色/亮色主题** | 跟随系统或手动切换 |
| **软限制告警** | Summary 区域显示项目规模警告（Feature/Task 超建议上限），点击查看详情和优化建议 |

## 任务状态在看板中的展示规则

### 默认看板列

| 看板列 | 包含的状态 | 说明 |
|---|---|---|
| Pending | `pending`, `blocked` | 等待认领和被阻塞的任务 |
| In Progress | `in_progress` | 正在执行的任务 |
| Review | `submitted`, `verifying`, `ready_to_merge` | 验证流程中的任务 |
| Done | `done` | 已完成的任务。merge 成功作为事件记录在 Activity Log 中 |
| Conflicts | `merge_conflicted` | 独立 Conflicts 列，卡片标橙色警告，显示冲突摘要。协调者可通过"解决冲突"操作选择 reopen/cancel/followup |

### 特殊状态处理

| 状态 | 看板行为 |
|---|---|
| `cancelled` | **默认隐藏**，通过筛选器"显示已取消"切换可见 |
| `blocked` | 显示在 Pending 列，卡片标红色阻塞标记，hover 显示 blocker_reason |
| `merge_conflicted` | 独立 Conflicts 列，卡片标橙色警告，显示冲突摘要 |
| `rejected` | 瞬时伪状态（非稳定状态），回退 in_progress 后保持原列，活动日志记录 reject 事件 |
| 全部 cancelled | Feature 进度显示 N/A，标记为"无有效任务"，不自动关闭 |

### Activity Log 事件展示

| 事件 | 图标 | 展示格式 |
|---|---|---|
| 任务创建 | + | `session_id created T-{id}: {title}` |
| 任务认领 | → | `session_id/worker_id claimed T-{id}` |
| 任务提交 | ✓ | `session_id/worker_id submitted T-{id} ({coverage}%)` |
| 阻塞解除 | → | `coordinator unblocked T-{id}` |
| 验证领取 | ◉ | `session_id/worker_id verifying T-{id}` |
| 验证通过 | ✓✓ | `session_id/worker_id approved T-{id}` |
| 验证拒绝 | ✗ | `session_id/worker_id rejected T-{id}: {notes摘要}` |
| 任务阻塞 | ⚠ | `session_id blocked T-{id}: {reason摘要}` |
| 合并冲突 | ⚡ | `system merge_conflicted T-{id}` |
| 冲突重开 | ↺ | `coordinator reopened T-{id}` |
| 任务取消 | ⊘ | `coordinator cancelled T-{id}: {reason摘要}` |
| 合并成功 | ✓✓✓ | `system merged T-{id}` |
| 合并执行 | ↔ | `session_id/worker_id merge_requested T-{id}` |
| 任务完成 | ✓✓✓ | `system done T-{id}` |
| 冲突跟进 | ↗ | `coordinator followup_created T-{id} → T-{new_id}` |

## 人工运维能力 (Phase 4 规划)

初期 Web 看板为只读。Phase 4 起逐步增加以下人工干预能力：

| 操作 | 说明 | Phase |
|---|---|---|
| 强制释放 Session | 将卡死的 Session 标记离线，释放其所有任务 | Phase 4 |
| 强制回退 Task | 将 in_progress/block 任务回退到 pending | Phase 4 |
| 强制清理 Worktree | 删除 stale 状态的 Worktree | Phase 4 |
| 查看测试日志 | 展示服务端执行测试的完整输出 | Phase 3 |
| 下载 Diff/Patch | 下载任务的代码变更 | Phase 4 |

Phase 1-3 阶段如需人工干预，可直接操作 SQLite 数据库（文档提供应急 SQL）。
