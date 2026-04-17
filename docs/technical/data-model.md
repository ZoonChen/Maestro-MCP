# 2. 数据模型

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 数据模型
> **相关文档:** [架构](architecture.md) | [项目隔离](project-isolation.md) | [任务管理 PRD](../prd/task-management.md)

---

## 2.1 ER 关系

```
Project 1───N Feature 1───N Task (role 为 TEXT 字段，非独立表)
   │                          │
   │                          ├── 1 TaskResult (提交结果)
   │                          ├── N ValidationRun (验证历史，append-only)
   │                          ├── N ActivityLog (操作日志)
   │                          └── 1 Worktree (资源隔离)
   │
   ├── N ApiContract (按 project_id 索引，Task 通过 required_apis JSON 间接引用)
   │
   ├── 1───N AgentSession 1───N AgentWorker 1───1 Task (认领关系)
   │
   └── N AuditLog (安全审计，按 bound_project 索引)

注: Task.dependencies 为 JSON 字段存储依赖关系（非独立表），Task.required_apis 为 JSON 字段通过 method+path 查询 ApiContract（无直接 FK）
```

## 2.2 SQL 建表语句

```sql
-- Project 表 (顶层实体)
CREATE TABLE projects (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    workspace_path  TEXT NOT NULL UNIQUE,
    description     TEXT DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'active',        -- active/archived
    config          TEXT DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Feature 表
CREATE TABLE features (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    title       TEXT NOT NULL,
    description     TEXT NOT NULL,
    reference_urls  TEXT DEFAULT '[]',               -- JSON array: 关联文档/URL 列表
    status          TEXT NOT NULL DEFAULT 'planning',   -- planning/active/completed/closed
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(id, project_id)
);
CREATE INDEX idx_features_project ON features(project_id);

-- Task 表
CREATE TABLE tasks (
    id                  TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(id),
    feature_id          TEXT NOT NULL REFERENCES features(id),
    title               TEXT NOT NULL,
    description         TEXT NOT NULL,
    role                TEXT NOT NULL,                       -- backend/frontend/devops/verifier
    status              TEXT NOT NULL DEFAULT 'pending',       -- pending/in_progress/submitted/verifying/ready_to_merge/merge_conflicted/done/blocked/cancelled（另有 rejected 瞬时伪状态，不持久化到 DB）
    allowed_directories TEXT NOT NULL,
    forbidden_patterns  TEXT DEFAULT '[]',
    required_apis       TEXT DEFAULT '[]',                    -- JSON: [{ "method": "GET", "path": "/api/users" }]，通过 method+path 查询 ApiContract
    dependencies        TEXT DEFAULT '[]',                     -- JSON: [{ "task_id": "T-001", "require_state": "done" }]
    parent_task_id      TEXT,                                  -- 父任务 ID（可选）
    relation_type       TEXT,                                  -- followup/retry/replacement/conflict_resolution
    test_requirements   TEXT DEFAULT '{}',                     -- JSON: { "command": "go test ./...", "coverage_format": "go-cover", "coverage_path": "cover.out", "min_coverage": 80.0 }
    assigned_session_id TEXT REFERENCES agent_sessions(id),
    assigned_worker_id  TEXT,
    assigned_at         TEXT,                                  -- 首次认领时间（get_next_task 时设置）
    blocker_reason      TEXT,
    cancel_reason       TEXT,                                  -- cancel_task 时的原因
    merge_commit        TEXT,                                  -- merge 成功后的 commit hash（done 时记录）
    verified_by         TEXT REFERENCES agent_sessions(id),
    verified_at         TEXT,
    priority            TEXT NOT NULL DEFAULT 'normal',        -- low/normal/high/urgent
    summary             TEXT,                                  -- 纯文本摘要（从 submit_task_result 结构化 JSON 的 summary 字段提取），用于 dependency_summaries
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(id, project_id)
);
CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_status ON tasks(project_id, status);
CREATE INDEX idx_tasks_role ON tasks(project_id, role, status);
CREATE INDEX idx_tasks_feature ON tasks(project_id, feature_id);

-- Task 提交结果
CREATE TABLE task_results (
    id               TEXT PRIMARY KEY,
    task_id          TEXT NOT NULL UNIQUE REFERENCES tasks(id),
    project_id       TEXT NOT NULL REFERENCES projects(id),
    base_commit      TEXT NOT NULL,                            -- 取证时的基线 commit
    changed_files    TEXT NOT NULL,                            -- JSON array, 服务端取证结果
    test_command     TEXT NOT NULL DEFAULT '',                  -- 空字符串表示无测试命令（跳过测试）
    test_output      TEXT NOT NULL DEFAULT '',                  -- 空字符串表示无测试输出（测试被跳过）
    coverage         REAL,
    summary          TEXT,                                     -- JSON: Agent 提交的完整结构化摘要 (summary/outputs/notes)，与 tasks.summary 不同
    submitted_at     TEXT NOT NULL DEFAULT (datetime('now')),
    validated_at     TEXT,
    validation_errors TEXT,
    verifier_notes   TEXT                                     -- Verifier 审核备注（submit_verification 的 notes 参数）
);
CREATE INDEX idx_task_results_project ON task_results(project_id);

-- 验证尝试记录 (append-only, 每次 submit_task_result 追加一条，保留完整历史)
CREATE TABLE validation_runs (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    attempt         INTEGER NOT NULL,                    -- 第几次提交验证（从 1 开始自增）
    base_commit     TEXT NOT NULL,
    changed_files   TEXT NOT NULL,                        -- JSON array: 本次变更文件
    test_command    TEXT NOT NULL DEFAULT '',             -- 空字符串表示跳过测试
    test_exit_code  INTEGER,                             -- 测试退出码（null 表示跳过测试）
    test_output     TEXT,                                -- 截断后的测试输出摘要（null 表示跳过测试）
    coverage        REAL,
    boundary_ok     INTEGER NOT NULL,                    -- 1=边界校验通过, 0=未通过
    test_ok         INTEGER NOT NULL,                    -- 1=测试通过, 0=未通过或跳过
    coverage_ok     INTEGER NOT NULL,                    -- 1=覆盖率达标, 0=未达标或跳过
    summary         TEXT,                                -- JSON: Agent 提交的结构化摘要 (本次提交的 summary/outputs/notes)
    result          TEXT NOT NULL,                        -- submitted/rejected/error（注: 此处 rejected 为服务端自动校验拒绝，非 Verifier 人工驳回）
    error_code      TEXT,                                -- 失败时的错误码
    duration_ms     INTEGER NOT NULL DEFAULT 0,          -- 验证总耗时（毫秒）
    log_path        TEXT,                                -- .maestro/logs/tests/{task_id}/attempt-{N}.log
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE UNIQUE INDEX idx_validation_runs_task ON validation_runs(project_id, task_id, attempt);

-- Worktree 资源状态表
CREATE TABLE worktrees (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    session_id      TEXT REFERENCES agent_sessions(id),
    worktree_path   TEXT NOT NULL,                             -- 绝对路径
    branch_name     TEXT NOT NULL,                             -- 如 task/T-00042
    base_commit     TEXT NOT NULL,                             -- 创建时的 HEAD commit
    status          TEXT NOT NULL DEFAULT 'allocated',        -- allocated/active/submitted/stale/merged/abandoned
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_worktrees_status ON worktrees(project_id, status);
CREATE INDEX idx_worktrees_stale ON worktrees(status, created_at);

-- API 契约索引
CREATE TABLE api_contracts (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    method      TEXT NOT NULL,
    path        TEXT NOT NULL,
    request_schema  TEXT,
    response_schema TEXT,
    description TEXT,
    source_file TEXT NOT NULL,
    parsed_at   TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(project_id, method, path)         -- 隐含唯一索引，idx_contracts_lookup 为冗余覆盖（保留以便查询计划显式引用）
);
CREATE INDEX idx_contracts_lookup ON api_contracts(project_id, method, path);

-- Agent Session 表
CREATE TABLE agent_sessions (
    id              TEXT PRIMARY KEY,                   -- 全局唯一，同一 session_id 不可跨项目
    project_id      TEXT NOT NULL REFERENCES projects(id),
    role            TEXT NOT NULL,                              -- coordinator/backend/frontend/devops/verifier
    client_type     TEXT NOT NULL DEFAULT 'other',             -- claude-code/openclaw/other
    capacity        INTEGER NOT NULL DEFAULT 1,               -- 单 Session 最大并行 Worker 数（上限 5）
    status          TEXT NOT NULL DEFAULT 'online',           -- online/offline
    last_heartbeat  TEXT NOT NULL DEFAULT (datetime('now')),
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_sessions_project ON agent_sessions(project_id);
CREATE INDEX idx_sessions_heartbeat ON agent_sessions(status, last_heartbeat);

-- Agent Worker 表
CREATE TABLE agent_workers (
    id              TEXT NOT NULL,
    session_id      TEXT NOT NULL REFERENCES agent_sessions(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    current_task_id TEXT REFERENCES tasks(id),
    status          TEXT NOT NULL DEFAULT 'idle',
    tasks_completed INTEGER NOT NULL DEFAULT 0,
    last_active     TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (id, session_id)
);
CREATE INDEX idx_workers_session ON agent_workers(session_id);
CREATE INDEX idx_workers_task ON agent_workers(current_task_id);

-- 业务活动日志 (append-only, 用于 Web 看板展示)
-- 注: WS 事件中 project.registered/archived 和 agent.online/offline 不记录到此表（属于项目/Agent 级别事件，非任务维度）
CREATE TABLE activity_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    session_id  TEXT,
    task_id     TEXT,
    action      TEXT NOT NULL,   -- created/claimed/submitted/approved/rejected/blocked/unblocked/verifying/merge_conflicted/merged/merge_requested/reopened/cancelled/followup_created/done
                                -- 注: approved 对应 WS 事件 task.ready_to_merge（强调 Verifier 批准动作 vs 状态变更）
    detail      TEXT,            -- JSON detail
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_activity_project ON activity_log(project_id, created_at DESC);

-- 安全审计日志 (append-only, 用于窜台检测和安全告警)
CREATE TABLE audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT,
    bound_project   TEXT NOT NULL,      -- Agent 绑定的 project_id（命名区别于其他表的 project_id，强调"绑定"语义）
    target_project  TEXT,            -- 仅在跨项目访问时记录
    target_task     TEXT,            -- 关联任务 ID（可选，记录操作目标）
    action          TEXT NOT NULL,   -- tool_call/resource_access/cross_project_denied/soft_limit_warning/...（包括但不限于上述值）
    path            TEXT,            -- 请求路径
    result          TEXT NOT NULL,   -- ALLOWED / DENIED / WARNED（软限制警告：允许操作但记录告警）
    detail          TEXT,            -- JSON
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_audit_time ON audit_log(created_at DESC);
CREATE INDEX idx_audit_denied ON audit_log(result, created_at DESC);
```

## 2.3 Project Config Schema

`projects.config` 为 JSON 字符串字段，支持以下配置项。优先级：`Task 配置` > `Project.config` > `全局配置 (maestro.yaml)`

| 配置项 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `default_test_command` | string | null | 项目级默认测试命令模板（覆盖全局配置） |
| `default_coverage_format` | string | null | 覆盖率文件格式: `go-cover` / `cobertura` / `jacoco` / `istanbul` |
| `default_coverage_path` | string | null | 覆盖率文件路径（相对于 worktree root） |
| `default_min_coverage` | number | 0 | 默认最低覆盖率要求 (0-100) |
| `default_test_timeout` | integer | 120 | 默认测试超时时间（秒） |
| `max_worktrees` | integer | 10 | 项目最大并行 Worktree 数量 |
| `merge_target_branch` | string | 自动检测 main/master | merge 目标分支 |
| `contract_paths` | string[] | [] | API 契约文件路径列表 |
| `contract_provider` | string | null | 契约源类型: `openapi` / `manual_json` |
| `contract_watch` | boolean | false | 是否监听契约文件变更并自动重新解析 |
| `allowed_test_commands` | string[] | [] | 允许的测试命令白名单模板（如 `["go test ./...", "npm test"]`），为空表示不限制 |

未列出的字段由全局配置提供（部分字段如 `coverage_format`/`coverage_path` 无全局回退，仅在 Task 和 Project 两级配置），逐字段的完整回退规则见 [验证 PRD](../prd/validation.md) 的测试要求配置回退链表。

## 2.4 ID 与命名规范

| 实体 | 格式 | 示例 | 生成方 |
|---|---|---|---|
| Project ID | kebab-case slug | `user-service` | 用户指定 |
| Feature ID | `F-{4位序号}` | `F-0001` | Service 层序号格式化 |
| Task ID | `T-{5位序号}` | `T-00042` | Service 层序号格式化 |
| Session ID | `sess_{8位hex}` 或用户指定 | `sess_a3f8b2c1` | 用户指定或系统 |
| Worker ID | `default`, `sub-{N}` | `sub-1` | 系统默认或隐式注册 |
| Worktree Branch | `task/{task_id}` | `task/T-00042` | 系统生成 |
