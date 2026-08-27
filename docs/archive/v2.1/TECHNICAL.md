# Maestro-MCP 技术设计文档

> **注意:** 本文件为合并前的单体文档，已拆分为 `docs/technical/*.md`（12 个独立文件）。拆分文档为权威版本，本文件仅作参考，可能未同步最新修改。详见 [文档中心](README.md)。
>
> 版本: v2.1 | 2026-04-17 | 状态: 已拆分归档

---

## 1. 系统架构

### 1.1 架构图

```
┌──────────────────────────────────────────────────────────────┐
│              Maestro-MCP (Go 单二进制)                        │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │                  MCP Protocol Layer                    │  │
│  │  ┌──────────────┐  ┌──────────────┐                   │  │
│  │  │  stdio       │  │  SSE         │                   │  │
│  │  │  Transport   │  │  Transport   │                   │  │
│  │  │  (Claude Code)│  │  (OpenClaw)  │                   │  │
│  │  └──────┬───────┘  └──────┬───────┘                   │  │
│  │         └────────┬────────┘                            │  │
│  │         ┌────────▼────────┐                            │  │
│  │         │ Tool/Resource/  │                            │  │
│  │         │ Prompt Registry │                            │  │
│  │         └────────┬────────┘                            │  │
│  └─────────────────┼──────────────────────────────────────┘  │
│                    │                                         │
│  ┌─────────────────▼──────────────────────────────────────┐  │
│  │                Business Logic                          │  │
│  │  Task State Machine | Context Filter | Boundary Guard  │  │
│  │  Worktree Manager | Test Runner | Contract Parser      │  │
│  └─────────────────┬──────────────────────────────────────┘  │
│                    │                                         │
│  ┌────────┬────────▼────────┬──────────┬──────────────────┐  │
│  │REST API│  WebSocket Hub  │  SQLite  │  Static Web UI   │  │
│  │(Gin)   │  (nhooyr.io)   │  Store   │  (go:embed)      │  │
│  │:8080   │  :8080/ws      │          │  :8080           │  │
│  └────────┴─────────────────┴──────────┴──────────────────┘  │
└──────────────────────────────────────────────────────────────┘
        │              │                    │
   ┌────┴────┐    ┌────┴────┐          ┌───┴────┐
   │Claude   │    │OpenClaw │          │Browser │
   │Code     │    │(MCP)    │          │(Web UI)│
   │(stdio)  │    │(SSE)    │          │(HTTP)  │
   └─────────┘    └─────────┘          └────────┘
```

### 1.2 技术栈

| 组件 | 技术 | 说明 |
|---|---|---|
| **语言** | Go 1.22+ | 单二进制，零运行时依赖 |
| **MCP SDK** | `github.com/mark3labs/mcp-go` | Go 原生 MCP 实现，支持 stdio + SSE 双传输 |
| **HTTP 框架** | Gin | REST API + WebSocket + 静态文件托管 |
| **数据库** | SQLite (modernc.org/sqlite) | 纯 Go 实现，无 CGO 依赖，跨平台编译 |
| **前端** | Preact + Vite | 构建后通过 `go:embed` 嵌入二进制 |
| **Git 操作** | `go-git` 或命令行 git | Worktree 管理、diff 比对 |
| **覆盖率解析** | Go 标准库 XML 解析 | 读取 Cobertura/gocover 格式 |

### 1.3 进程模型

- **Docker**: 单进程，暴露 `:8080` (HTTP+WS) + `:3000` (SSE)
- **本地**: `maestro serve --config maestro.yaml` 单进程常驻

两种启动模式的区别:
- `maestro serve`: 启动完整服务 (HTTP+WS+SSE+SQLite), 不绑定特定项目
  - 适用于: Web 看板 + 多 Agent SSE 接入
- `maestro mcp --transport stdio`: 轻量 MCP-only 模式
  - 从 cwd 推断项目绑定
  - 共享 serve 的 SQLite 数据文件 (通过 `--data-dir` 指定)
  - 适用于: Claude Code 单实例直接连接
  - 如果 serve 未运行, mcp stdio 可独立工作 (使用同一 DB 文件)

---

## 2. 数据模型

### 2.1 ER 关系

```
Project 1───N Feature 1───N Task N───1 Role
   │                          │
   │                          ├── N ApiContract (任务依赖的 API)
   │                          ├── N TaskDependency (任务间依赖)
   │                          ├── 1 TaskResult (提交结果)
   │                          ├── N ActivityLog (操作日志)
   │                          └── 1 Worktree (资源隔离)
   │
   └── 1───N AgentSession 1───N AgentWorker 1───1 Task (认领关系)
```

### 2.2 SQL 建表语句

```sql
-- Project 表 (顶层实体)
CREATE TABLE projects (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    workspace_path  TEXT NOT NULL UNIQUE,
    description     TEXT DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'active',
    config          TEXT DEFAULT '{}',
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Feature 表
CREATE TABLE features (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    title       TEXT NOT NULL,
    description TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'planning',
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
    role                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',       -- pending/in_progress/submitted/verifying/ready_to_merge/merge_conflicted/merged/done/blocked/rejected
    allowed_directories TEXT NOT NULL,
    forbidden_patterns  TEXT DEFAULT '[]',
    required_apis       TEXT DEFAULT '[]',
    dependencies        TEXT DEFAULT '[]',                     -- JSON: [{ "task_id": "T-001", "require_state": "done" }]
    test_requirements   TEXT DEFAULT '{}',
    assigned_session_id TEXT REFERENCES agent_sessions(id),
    assigned_worker_id  TEXT,
    blocker_reason      TEXT,
    merge_commit        TEXT,                                  -- merge 完成后的 commit hash
    verified_by         TEXT REFERENCES agent_sessions(id),
    verified_at         TEXT,
    priority            TEXT NOT NULL DEFAULT 'normal',        -- low/normal/high/urgent
    summary             TEXT,
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(id, project_id)
);
CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_status ON tasks(project_id, status);
CREATE INDEX idx_tasks_role ON tasks(project_id, role, status);

-- Task 提交结果
CREATE TABLE task_results (
    id               TEXT PRIMARY KEY,
    task_id          TEXT NOT NULL UNIQUE REFERENCES tasks(id),
    project_id       TEXT NOT NULL REFERENCES projects(id),
    base_commit      TEXT NOT NULL,                            -- 取证时的基线 commit
    changed_files    TEXT NOT NULL,                            -- JSON array, 服务端取证结果
    test_command     TEXT NOT NULL,
    test_output      TEXT NOT NULL,
    coverage         REAL,
    submitted_at     TEXT NOT NULL DEFAULT (datetime('now')),
    validated_at     TEXT,
    validation_errors TEXT
);
CREATE INDEX idx_task_results_project ON task_results(project_id);

-- Worktree 资源状态表
CREATE TABLE worktrees (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         TEXT NOT NULL REFERENCES tasks(id),
    project_id      TEXT NOT NULL REFERENCES projects(id),
    session_id      TEXT REFERENCES agent_sessions(id),
    worktree_path   TEXT NOT NULL,                             -- 绝对路径
    branch_name     TEXT NOT NULL,                             -- 如 task/T-00042
    base_commit     TEXT NOT NULL,                             -- 创建时的 HEAD commit
    status          TEXT NOT NULL DEFAULT 'allocated',        -- allocated/active/submitted/stale/merged/abandoned/cleaned
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    cleaned_at      TEXT,
    UNIQUE(task_id, project_id)
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
    UNIQUE(project_id, method, path)
);
CREATE INDEX idx_contracts_lookup ON api_contracts(project_id, method, path);

-- Agent Session 表
CREATE TABLE agent_sessions (
    id              TEXT PRIMARY KEY,
    project_id      TEXT NOT NULL REFERENCES projects(id),
    role            TEXT NOT NULL,
    client_type     TEXT NOT NULL DEFAULT 'other',
    capacity        INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL DEFAULT 'online',
    last_heartbeat  TEXT NOT NULL DEFAULT (datetime('now')),
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE(id, project_id)
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
CREATE TABLE activity_log (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id  TEXT NOT NULL REFERENCES projects(id),
    session_id  TEXT,
    task_id     TEXT,
    action      TEXT NOT NULL,   -- created/split/claimed/submitted/verified/merged/blocked/canceled
    detail      TEXT,            -- JSON detail
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_activity_project ON activity_log(project_id, created_at DESC);

-- 安全审计日志 (append-only, 用于窜台检测和安全告警)
CREATE TABLE audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT,
    bound_project   TEXT NOT NULL,
    target_project  TEXT,            -- 仅在跨项目访问时记录
    action          TEXT NOT NULL,   -- tool_call/resource_access/cross_project_denied/...
    path            TEXT,            -- 请求路径
    result          TEXT NOT NULL,   -- ALLOWED / DENIED
    detail          TEXT,            -- JSON
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX idx_audit_time ON audit_log(created_at DESC);
CREATE INDEX idx_audit_denied ON audit_log(result, created_at DESC);
```

### 2.3 ID 与命名规范

| 实体 | 格式 | 示例 | 生成方 |
|---|---|---|---|
| Project ID | kebab-case slug | `user-service` | 用户指定 |
| Feature ID | `F-{4位序号}` | `F-0001` | SQLite AUTOINCREMENT 格式化 |
| Task ID | `T-{5位序号}` | `T-00042` | SQLite AUTOINCREMENT 格式化 |
| Session ID | `sess_{8位hex}` 或用户指定 | `sess_a3f8b2c1` | 用户指定或系统 |
| Worker ID | `default`, `sub-{N}` | `sub-1` | 系统默认或隐式注册 |
| Worktree Branch | `task/{task_id}` | `task/T-00042` | 系统生成 |

---

## 3. 核心机制实现

### 架构原则: Service 层为唯一真源

```
请求流转:
MCP Tool ──┐
REST API ──┼──► Service Layer ──► Store Layer ──► SQLite
WebSocket ──┘     (唯一状态机入口)   (强制 project_id)

规则:
- Handler / MCP Tool 禁止直接操作 Store
- 所有状态流转只能通过 Service 层统一方法
- Service 层负责: 状态校验、权限校验、审计日志、事件推送
```

### 3.1 项目访问隔离 (四层防御)

```
                    Agent 请求
                        │
    ┌───────────────────▼────────────────────┐
    │  L1: 连接绑定层 (Connection Binding)    │  连接建立时锁定 project_id
    └───────────────────┬────────────────────┘
                        │ project_ctx (不可变)
    ┌───────────────────▼────────────────────┐
    │  L2: API 中间件层 (Request Guard)       │  每个请求校验 :pid
    └───────────────────┬────────────────────┘
                        │ 强制携带 project_id
    ┌───────────────────▼────────────────────┐
    │  L3: 业务逻辑层 (Service Enforcement)   │  task_id 归属校验
    └───────────────────┬────────────────────┘
                        │ WHERE project_id = ?
    ┌───────────────────▼────────────────────┐
    │  L4: 数据存储层 (Store Scoping)         │  SQL 强制隔离
    └────────────────────────────────────────┘
```

#### L1: 连接绑定层

连接建立时锁定 project_id，后续不可更改。Agent 无法在 Tool 参数中指定或覆盖。

```go
type ProjectContext struct {
    ProjectID, ProjectName, WorkspacePath string
    BoundAt    time.Time
    BindMethod string // "cwd_match" | "flag" | "handshake"
}

func BindProject(connInfo ConnectionInfo) (*ProjectContext, error) {
    // 1. 显式指定优先 (flag/handshake)
    if connInfo.ExplicitProject != "" {
        proj, err := store.GetProject(connInfo.ExplicitProject)
        if err != nil || proj.Status != "active" { return nil, ErrProjectNotFound }
        return &ProjectContext{ProjectID: proj.ID, BindMethod: "flag"}, nil
    }
    // 2. cwd 匹配 (精确匹配优先，最长路径匹配)
    matches := store.FindProjectByPath(connInfo.WorkingDir)
    switch len(matches) {
    case 1:  return &ProjectContext{ProjectID: matches[0].ID, BindMethod: "cwd_match"}, nil
    case 0:  return nil, ErrProjectNotBound
    default: return nil, ErrAmbiguousProject{Candidates: matches}
    }
}
```

#### L2: API 中间件层

URL 路径中的 `:pid` 必须与绑定的 project_id 一致，不匹配返回 403。

```go
func ProjectGuard() gin.HandlerFunc {
    return func(c *gin.Context) {
        boundPID := c.GetString("project_id")
        pathPID := c.Param("pid")
        if pathPID == "" { c.Next(); return }
        if pathPID != boundPID {
            audit.Log(c, AuditEvent{Action: "cross_project_access_denied", BoundPID: boundPID, TargetPID: pathPID})
            c.AbortWithStatusJSON(403, gin.H{"error": "access denied: project mismatch"})
            return
        }
        if proj, ok := store.GetProject(pathPID); !ok || proj.Status == "archived" {
            c.AbortWithStatusJSON(403, gin.H{"error": "project not found or archived"}); return
        }
        c.Set("project", proj); c.Next()
    }
}
```

#### L3: 业务逻辑层

所有 Service 方法第一参数均为 `projectID`。`get_next_task` 自动追加项目过滤；`submit_task_result` 校验 task 归属，不区分 "不存在" 和 "不属于本项目"。

```go
type TaskService struct { store TaskStore }
func (s *TaskService) GetNextTask(projectID, role string) (*Task, error) {
    return s.store.FindNextAvailable(projectID, role)
}
func (s *TaskService) SubmitTaskResult(projectID, taskID string, result TaskResult) error {
    task, err := s.store.GetByID(projectID, taskID) // store 层已含 project_id
    if err != nil { return ErrTaskNotFound }        // 不泄漏跨项目信息
    if task.Status != "in_progress" { return ErrInvalidState }
    // ... 验证逻辑
}
```

#### L4: 数据存储层

Store 接口中**不存在**不带 `projectID` 参数的查询方法。所有 SQL 自动注入 `WHERE project_id = ?`。

```go
type TaskStore interface {
    Create(projectID string, task *Task) error
    GetByID(projectID, taskID string) (*Task, error)
    FindNextAvailable(projectID, role string) (*Task, error)
    List(projectID string, filter TaskFilter) ([]*Task, error)
    UpdateStatus(projectID, taskID, newStatus string) error
    // 禁止：GetByID(taskID) 或 List(filter) 不带 projectID
}

func (s *SQLiteTaskStore) GetByID(projectID, taskID string) (*Task, error) {
    // WHERE project_id = ? AND id = ? — 统一返回 ErrTaskNotFound，不泄漏跨项目信息
}
func (s *SQLiteTaskStore) FindNextAvailable(projectID, role string) (*Task, error) {
    // 运行时动态判断依赖是否满足, 不使用冗余字段
    // SELECT id FROM tasks
    // WHERE project_id = ?
    //   AND role = ?
    //   AND status = 'pending'
    //   AND NOT EXISTS (
    //       SELECT 1 FROM json_each(tasks.dependencies) AS dep
    //       LEFT JOIN tasks AS dep_task ON dep_task.id = dep->>'task_id'
    //       WHERE dep_task.status NOT IN (
    //           CASE dep->>'require_state'
    //             WHEN 'submitted' THEN 'submitted,verifying,ready_to_merge,merged,done'
    //             ELSE 'done'
    //           END
    //       )
    //   )
    // ORDER BY
    //   CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
    //   created_at ASC
    // LIMIT 1
}
```

#### 审计日志

每次请求记录 `{ timestamp, session_id, bound_project, target_project?, action, target_task?, result: ALLOWED|DENIED }`。同一 Agent 5 分钟内 3 次 DENIED 触发看板告警。

#### 协调者跨项目访问

```go
type CoordinatorAccess struct {
    BoundProjectID string
    GlobalRead     bool     // 全局只读
    WriteProjects  []string // 可写项目列表
}
```

---

### 3.2 Git Worktree 物理隔离

**Worktree 生命周期：**

```
1. get_next_task() → git worktree add .maestro/worktrees/T-005 -b task/T-005
2. 返回上下文中路径映射到 worktree 目录
3. Agent 在独立 worktree 中修改代码
4. submit_task_result() → Maestro 在 worktree 中执行测试和校验
5. 校验通过 → Verifier 将 worktree 分支 merge 回主分支
6. merge 完成 → git worktree remove .maestro/worktrees/T-005
```

**路径映射：** `get_next_task` 返回的 `workspace.root` 指向 `.maestro/worktrees/{task_id}/`，`allowed_directories` 相对于该路径。

**冲突处理策略：**

| 场景 | 策略 |
|---|---|
| Worktree 创建失败（有未提交修改） | 返回错误："请先 commit 或 stash 当前修改" |
| Merge 时产生冲突 | 通知 Verifier/协调者处理冲突 |
| Worktree 磁盘空间不足 | 配置 `max_worktrees` 限制（默认 10） |
| 无 Git 仓库的项目 | 回退到"目录隔离"模式，要求 `allowed_directories` 之间无交集 |

---

### 3.3 零信任验证闭环 (服务端取证)

**submit_task_result 流程：**

```
Agent 调用 submit_task_result(task_id, summary?)
    │
    ▼
┌──────────────────┐
│ 1. Git Diff 取证  │  服务端从 worktrees 表获取 base_commit 和 worktree_path
└────────┬──────────┘
         │
┌────────▼──────────┐  失败 → 拒绝：返回越界文件列表
│ 2. 文件边界校验    │
└────────┬──────────┘
         │ 通过
┌────────▼──────────┐
│ 3. 执行测试       │  task.test_requirements.command
└────────┬──────────┘
         │
┌────────▼──────────┐  失败 → 拒绝：返回真实 stderr 摘要
│ 4. 测试结果校验    │  exit code = 0?
└────────┬──────────┘
         │ 通过
┌────────▼──────────┐  失败 → 拒绝：实际覆盖率 < 阈值
│ 5. 覆盖率校验      │  读取结构化覆盖率文件
└────────┬──────────┘
         │ 通过
┌────────▼──────────┐
│ 6. 状态 → submitted │ 保存 changed_files / test_result / coverage
└───────────────────┘
```

**边界取证方式：**

```
1. 从 worktrees 表获取 task 的 base_commit 和 worktree_path
2. 在 worktree 目录执行:
   git diff --name-only {base_commit}     -- 已暂存的变更
   git diff --name-only                   -- 未暂存的变更
   git status --porcelain                 -- 完整状态 (新增/修改/删除/重命名)
3. 合并三部分结果，去重得到真实 changed_files
4. 每个 changed_file 路径必须在 task.allowed_directories 内
```

**TestRequirements 结构：**

```go
type TestRequirements struct {
    Command        string  `json:"command"`          // "go test ./... -coverprofile=coverage/cover.out"
    CoverageFormat string  `json:"coverage_format"`  // "go-cover" / "cobertura" / "jacoco" / "istanbul"
    CoveragePath   string  `json:"coverage_path"`    // "coverage/cover.out"
    MinCoverage    float64 `json:"min_coverage"`     // 80.0
}
```

**RunTestAndCheck 函数：** 在 worktree 中执行 `req.Command`，捕获 exit code，然后读取 `req.CoveragePath` 下的结构化覆盖率文件，返回 `TestResult{ExitCode, Output, Coverage}`。

**覆盖率文件格式：**

| 语言 | 覆盖率文件格式 | 路径示例 |
|---|---|---|
| Go | `cover.out` / `coverage.txt` | `coverage/cover.out` |
| TypeScript/JS | Cobertura XML / Istanbul JSON | `coverage/cobertura-coverage.xml` |
| Python | coverage.xml (Cobertura) | `coverage.xml` |
| Java | JaCoCo XML | `target/site/jacoco/jacoco.xml` |

---

### 3.4 API 契约解析引擎

**解析流程：**

```
启动时 / 文件变更时:
     │
     ▼
┌──────────────────┐
│ 1. 扫描 contract  │  读取配置的 contract_paths
│    _paths         │  支持文件/目录/通配符
└────────┬─────────┘
         │
┌────────▼─────────┐
│ 2. 解析契约文件   │  OpenAPI 3.x → { method, path, request_schema,
│    (格式识别)     │                response_schema, description }
└────────┬─────────┘
         │
┌────────▼─────────┐
│ 3. 写入 SQLite    │  INSERT INTO api_contracts
│    契约索引表     │  (project_id, method, path, schema, ...)
└────────┬─────────┘
         │
┌────────▼─────────┐
│ 4. 组装时查询     │  task.required_apis = ["GET /api/v1/orders"]
│    (毫秒级)       │  → SELECT * FROM api_contracts
│                   │    WHERE project_id=? AND method=? AND path=?
└──────────────────┘
```

**api_contracts 建表见第 2 节。**

**无契约降级：** 未配置 `contract_paths` 时，`required_apis` 失效，上下文降级为 `description` + `allowed_directories` + 文件列表，其他功能不受影响。

---

### 3.5 Session + Worker 并发模型

**数据模型：**

```typescript
interface AgentSession {
    session_id: string;
    project_id: string;
    role: Role;
    client_type: "claude-code" | "openclaw" | "other";
    capacity: number;         // 最大并发 Worker 数
    status: "online" | "offline";
    last_heartbeat: string;
    workers: AgentWorker[];
}

interface AgentWorker {
    worker_id: string;        // "default", "sub-1", ...
    session_id: string;
    project_id: string;
    current_task_id: string | null;
    status: "idle" | "busy";
    tasks_completed: number;
    last_active: string;
}
```

**三种并行场景映射：**

```
场景 1: 跨模块并行 (不同角色，不同连接)
├─ Session A: cc-backend-01  (role=backend, capacity=1)
│  └─ Worker: default → T-005
└─ Session B: cc-frontend-01 (role=frontend, capacity=1)
   └─ Worker: default → T-007

场景 2: 同模块多实例并行 (同角色，不同连接)
├─ Session A: cc-backend-01  (capacity=1)
│  └─ Worker: default → T-005
└─ Session C: cc-backend-02  (capacity=1)
   └─ Worker: default → T-008

场景 3: 单实例子 Agent 并行 (一个连接，多个 Worker)
└─ Session A: cc-backend-01  (capacity=5)
   ├─ Worker: default → T-005  (主 Agent)
   ├─ Worker: sub-1    → T-008  (子 Agent 1)
   ├─ Worker: sub-2    → T-009  (子 Agent 2)
   ├─ Worker: sub-3    → null   (空闲)
   └─ Worker: sub-4    → null   (空闲)
```

**get_next_task 原子认领 (SQLite 事务)：**

```go
func (s *TaskService) GetNextTask(projectID, role, workerID string) (*Task, error) {
    tx, _ := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
    // 1. 查找下一个 pending 任务 (动态依赖检查)
    var taskID string
    err := tx.QueryRowContext(ctx, `
        SELECT id FROM tasks
        WHERE project_id = ?
          AND role = ?
          AND status = 'pending'
          AND NOT EXISTS (
              SELECT 1 FROM json_each(tasks.dependencies) AS dep
              LEFT JOIN tasks AS dep_task ON dep_task.id = dep->>'task_id'
              WHERE dep_task.status NOT IN (
                  CASE dep->>'require_state'
                    WHEN 'submitted' THEN 'submitted,verifying,ready_to_merge,merged,done'
                    ELSE 'done'
                  END
              )
          )
        ORDER BY
          CASE priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
          created_at ASC
        LIMIT 1`, projectID, role).Scan(&taskID)
    if err == sql.ErrNoRows { tx.Rollback(); return nil, ErrNoAvailableTask }
    // 2. 原子更新：仅 status='pending' 时更新
    result, _ := tx.ExecContext(ctx, `UPDATE tasks SET status='in_progress',
        assigned_session_id=?, assigned_worker_id=?, updated_at=datetime('now')
        WHERE id=? AND project_id=? AND status='pending'`, sessionID, workerID, taskID, projectID)
    if affected, _ := result.RowsAffected(); affected == 0 {
        tx.Rollback(); return s.GetNextTask(projectID, role, workerID) // 重试 (max 3)
    }
    tx.Commit()
    s.workerStore.UpdateCurrentTask(sessionID, workerID, taskID)
    return s.buildTaskContext(projectID, taskID)
}
```

**隐式 Worker 注册：** `get_next_task` 调用时若未注册 Worker，自动以 `worker_id="default"` 注册。

Worker 注册方式说明:
- MCP Tool: 隐式注册 (`get_next_task` 带新 worker_id 自动注册)
- REST API: 显式注册 (`POST /sessions/{sid}/workers`) — 供运维/调试使用

**Session 超时与任务释放：** 后台 goroutine 每 30s 扫描 `status='online' AND last_heartbeat < now - timeout` 的 session，标记 offline，释放其所有 `in_progress` 任务回 `pending`，清空 Worker 状态，审计日志 + WebSocket 广播 `agent.offline`。

---

### 3.6 测试执行安全模型

```go
type TestExecutionConfig struct {
    TestTimeout      time.Duration     // 默认 120s
    OutputMaxBytes   int               // stdout/stderr 各最大字节数, 默认 100KB
    AllowedEnvVars   []string          // 白名单: PATH, HOME, GOPATH, NODE_PATH 等
    WorkingDir       string            // 必须在 worktree 内
    KillOnTimeout    bool              // 默认 true: SIGTERM → 5s → SIGKILL
}

func (r *TestRunner) Execute(worktreePath, command string, cfg TestExecutionConfig) (*TestResult, error) {
    // 1. 校验 workingDir 在 worktree 内
    if !isSubPath(worktreePath, cfg.WorkingDir) {
        return nil, ErrWorkingDirEscape
    }

    // 2. 构建命令，注入覆盖率路径
    cmd := exec.Command("sh", "-c", command)
    cmd.Dir = worktreePath

    // 3. 过滤环境变量
    cmd.Env = r.filterEnv(os.Environ(), cfg.AllowedEnvVars)

    // 4. 带超时执行
    ctx, cancel := context.WithTimeout(context.Background(), cfg.TestTimeout)
    defer cancel()
    cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 进程组，便于 kill 整棵树

    output, err := cmd.CombinedOutput()

    // 5. 截断输出
    output = truncateBytes(output, cfg.OutputMaxBytes)

    // 6. 提取 exit code
    exitCode := 0
    if err != nil { exitCode = extractExitCode(err) }

    return &TestResult{ExitCode: exitCode, Output: string(output)}, nil
}
```

安全约束规则:

| 约束 | 实现 |
|---|---|
| 工作目录限制 | `filepath.Abs(cmd.Dir)` 必须以 worktree_path 为前缀 |
| 超时强杀 | `context.WithTimeout` + `syscall.Kill(-pgid)` 杀进程树 |
| 输出截断 | `bytes` 超过 OutputMaxBytes 后截断，尾部追加 `[TRUNCATED]` |
| 环境变量白名单 | 只保留 `PATH`, `HOME`, `GOPATH`, `NODE_PATH`, `PYTHONPATH` 等 |
| 命令来源 | 只接受 `task.test_requirements.command`，不接受 Agent 动态传入 |

本地模式风险声明: 测试命令拥有当前用户权限，Maestro 不提供沙箱。生产环境建议 Docker 模式。

---

### 3.7 Worktree 资源状态模型

```
Worktree 状态机:
┌───────────┐  get_next_task   ┌──────────┐  Agent 工作中  ┌──────────┐
│ allocated │─────────────────►│  active  │◄──────────────│ active   │
│ (已创建)  │                  │(工作中)  │               │ (续用)   │
└───────────┘                  └────┬─────┘               └──────────┘
                                    │ submit_task_result()
                               ┌────▼──────┐
                               │ submitted │ ← 等待验证
                               └────┬──────┘
                                    │ merge 成功
                               ┌────▼──────┐
                               │  merged   │
                               └────┬──────┘
                                    │ 自动 GC
                               ┌────▼──────┐
                               │ cleaned   │
                               └───────────┘

异常路径:
active → stale     Session 超时, 任务回退 pending
stale → active     同一 Session 恢复连接
stale → abandoned  stale 超过 N 小时 (默认 24h)
abandoned → cleaned 定期 GC 清理磁盘
```

Session 超时处理规则:
1. Session 心跳超时 → session.status = offline
2. 其下所有 Worker 的 current_task_id 清空
3. 对应 tasks.status 回退为 pending
4. 对应 worktrees.status 标记为 stale (不立即删除)
5. 新 Agent 领取同一 task 时:
   - 若 worktree 为 stale → 评估是否可复用 (base_commit 是否过期)
   - 可复用 → worktree.status = active, task 继续
   - 不可复用 → 标记 abandoned, 创建新 worktree

定期 GC (后台 goroutine):
- 清理 abandoned 超过 1h 的 worktree
- 清理 merged 超过 1h 的 worktree
- `git worktree remove` + 目录删除

---

### 3.8 恢复与灾难处理

进程重启时的恢复流程:

```
启动时:
1. 打开 SQLite
2. 扫描 agent_sessions WHERE status='online'
   → 全部标记 offline (进程重启意味着所有连接断开)
3. 扫描 tasks WHERE status='in_progress'
   → 回退为 pending (没有 Agent 在执行了)
4. 扫描 worktrees WHERE status='active'
   → 标记为 stale (对应的 session 已不存在)
5. 启动后台 GC goroutine
```

不一致状态处理:

| 场景 | 检测方式 | 恢复策略 |
|---|---|---|
| Task in_progress 但无 session | 启动时扫描 | 回退到 pending |
| Worktree active 但 session offline | 启动时扫描 | 标记 stale |
| Worktree stale 超过 24h | GC goroutine | 清理并标记 abandoned |
| Task submitted 但 worktree 不存在 | 验证时检查 | 标记 blocked, 通知协调者 |
| Merge 到一半崩溃 | worktree status=merged 但 task 未 done | 启动时检查 git log, 补偿完成或回滚 |

---

## 4. 接口规范

### 4.1 REST API 端点

| 方法 | 路径 | 说明 |
|---|---|---|
| **全局端点** |||
| GET | `/api/v1/projects` | 列出所有项目 |
| POST | `/api/v1/projects` | 注册新项目 |
| GET | `/api/v1/projects/:id` | 项目详情 |
| PATCH | `/api/v1/projects/:id` | 更新项目 |
| POST | `/api/v1/projects/:id/archive` | 归档项目 |
| POST | `/api/v1/projects/:id/restore` | 恢复项目 |
| GET | `/api/v1/overview` | 跨项目总览 |
| GET | `/api/v1/sessions` | 全局 Session 列表 |
| **Feature (项目级)** |||
| POST | `/api/v1/projects/:pid/features` | 创建 Feature |
| GET | `/api/v1/projects/:pid/features` | 列出 Feature |
| GET | `/api/v1/projects/:pid/features/:id` | Feature 详情 |
| PATCH | `/api/v1/projects/:pid/features/:id` | 更新 Feature |
| **Task (项目级)** |||
| POST | `/api/v1/projects/:pid/tasks` | 创建 Task |
| GET | `/api/v1/projects/:pid/tasks` | 列出 Task (支持 `?status=&role=&feature_id=`) |
| GET | `/api/v1/projects/:pid/tasks/next?role=backend` | 获取下一个可执行任务 |
| GET | `/api/v1/projects/:pid/tasks/:id` | Task 详情 |
| POST | `/api/v1/projects/:pid/tasks/:id/claim` | 认领任务 |
| POST | `/api/v1/projects/:pid/tasks/:id/submit` | 提交结果 |
| POST | `/api/v1/projects/:pid/tasks/:id/block` | 上报阻塞 |
| POST | `/api/v1/projects/:pid/tasks/:id/resolve` | 解除阻塞 |
| POST | `/api/v1/projects/:pid/tasks/:id/verify` | 提交验证 |
| **Session (项目级)** |||
| POST | `/api/v1/projects/:pid/sessions` | 注册 Session |
| PUT | `/api/v1/projects/:pid/sessions/:sid/heartbeat` | 心跳 |
| GET | `/api/v1/projects/:pid/sessions` | 列出 Session |
| GET | `/api/v1/projects/:pid/sessions/:sid` | Session 详情 |
| DELETE | `/api/v1/projects/:pid/sessions/:sid` | 断开 Session |
| **Worker (Session 级)** |||
| POST | `/api/v1/projects/:pid/sessions/:sid/workers` | 注册 Worker |
| GET | `/api/v1/projects/:pid/sessions/:sid/workers` | 列出 Workers |
| DELETE | `/api/v1/projects/:pid/sessions/:sid/workers/:wid` | 释放 Worker |
| **Board (项目级)** |||
| GET | `/api/v1/projects/:pid/board` | 看板数据聚合 |
| GET | `/api/v1/projects/:pid/board/activity` | 活动日志 (`?limit=&since=`) |
| **WebSocket** |||
| WS | `/ws` | 实时事件推送 (`?project_id=` 过滤) |

### 4.2 MCP Tools

| Tool 名称 | 参数 | 说明 |
|---|---|---|
| **项目管理** |||
| `register_project` | `{ name, workspace_path, description?, config? }` | 注册新项目 |
| `list_projects` | `{ include_archived? }` | 列出所有项目 |
| **协调者** |||
| `create_feature` | `{ title, description }` | 创建 Feature |
| `split_task` | `{ feature_id, role, title, description, allowed_directories, required_apis?, dependencies?, test_requirements? }` | 拆分子任务 |
| `update_task` | `{ task_id, title?, description?, allowed_directories?, required_apis?, test_requirements? }` | 修改任务参数 |
| `cancel_task` | `{ task_id, reason }` | 取消任务 |
| `resolve_blocker` | `{ task_id, resolution }` | 解除 blocked 状态 |
| **执行者** |||
| `get_next_task` | `{ role, worker_id? }` | 领取下一个任务（含降噪上下文 + Worktree 路径） |
| `submit_task_result` | `{ task_id, summary? }` | 声明完成（服务端自动取证） |
| `report_blocker` | `{ task_id, reason }` | 上报阻塞 |
| `claim_batch` | `{ role, count }` | 批量认领任务 |
| `release_worker` | `{ worker_id }` | 释放子 Worker |

**claim_batch 语义：**
- 本质是多次 `get_next_task` 的便利封装
- 部分成功语义: 返回 `{ claimed: [...], failed: [...] }`
- 不承诺全有或全无事务性 (worktree 创建非数据库事务)
- 如果某个 worktree 创建失败, 已认领的任务保留, 失败的任务留在 pending

| **验证者** |||
| `get_verification_task` | `{}` | 领取 submitted 状态任务 |
| `submit_verification` | `{ task_id, passed, notes }` | 提交验证结果 |

### 4.3 MCP Resources

| URI | 返回内容 |
|---|---|
| `project://list` | 所有已注册项目列表及状态概览 |
| `project://{project_id}` | 单项目详情：配置、进度统计、Agent 列表 |
| `board://active` | 当前项目看板摘要 |
| `board://all` | 跨项目全局看板 |
| `task://{task_id}/context` | 任务纯净上下文（动态组装） |
| `feature://{feature_id}/summary` | Feature 级进度 |

### 4.4 MCP Prompts

| Prompt 名称 | 说明 |
|---|---|
| `start-coordinator` | 注入协调者角色：需求分析、任务拆分、定期检查 Blocked 队列 |
| `start-worker` | 注入执行者角色：绑定 role，专注执行，Worktree 内操作 |
| `start-verifier` | 注入验证者角色：领取 submitted 任务，检查代码质量，决定 merge 或打回 |

### 4.5 MCP 传输模式

| 传输模式 | 端口/方式 | 适用场景 | 配置示例 |
|---|---|---|---|
| **stdio** | 标准输入输出 | Claude Code | `"command": "maestro", "args": ["mcp", "--transport", "stdio"]` |
| **SSE** | `:3000/sse` | OpenClaw / 远程 | `"url": "http://localhost:3000/sse"` |

### 4.6 WebSocket 事件类型

```typescript
type WSEvent =
  | { type: "project.registered"; project_id: string; project: Project }
  | { type: "project.archived"; project_id: string }
  | { type: "task.created"; project_id: string; task: Task }
  | { type: "task.claimed"; project_id: string; task_id: string; agent: string }
  | { type: "task.submitted"; project_id: string; task_id: string; result: TaskResult }
  | { type: "task.blocked"; project_id: string; task_id: string; reason: string }
  | { type: "task.done"; project_id: string; task_id: string }
  | { type: "task.rejected"; project_id: string; task_id: string; errors: string[] }
  | { type: "agent.online"; project_id: string; agent_id: string; role: string }
  | { type: "agent.offline"; project_id: string; agent_id: string }
```

### 4.7 错误码规范

MCP Tool、REST API、WebSocket 使用统一错误模型:

```go
type MaestroError struct {
    Code    string `json:"code"`              // 机器可读
    Message string `json:"message"`           // 人类可读
    Detail  any    `json:"detail,omitempty"`  // 附加上下文
}
```

| 错误码 | HTTP Status | 含义 |
|---|---|---|
| `PROJECT_NOT_FOUND` | 404 | 项目不存在 |
| `PROJECT_ARCHIVED` | 403 | 项目已归档 |
| `PROJECT_NOT_BOUND` | 400 | Agent 未绑定项目 |
| `PROJECT_AMBIGUOUS` | 400 | cwd 匹配到多个项目 |
| `FEATURE_NOT_FOUND` | 404 | Feature 不存在 |
| `TASK_NOT_FOUND` | 404 | Task 不存在（含不属于当前项目） |
| `TASK_NOT_OWNED` | 403 | Task 不属于当前 Session |
| `TASK_STATE_INVALID` | 409 | 当前状态不允许此操作 |
| `TASK_DEPENDENCY_UNMET` | 412 | 前置依赖未满足 |
| `SESSION_NOT_FOUND` | 404 | Session 不存在 |
| `SESSION_CAPACITY_FULL` | 429 | Session Worker 数已达上限 |
| `WORKTREE_CREATE_FAILED` | 500 | Worktree 创建失败 |
| `WORKTREE_CLEAN_FAILED` | 500 | Worktree 清理失败 |
| `TEST_EXECUTION_FAILED` | 422 | 测试执行失败 |
| `TEST_EXECUTION_TIMEOUT` | 408 | 测试执行超时 |
| `COVERAGE_BELOW_THRESHOLD` | 422 | 覆盖率低于阈值 |
| `COVERAGE_FILE_NOT_FOUND` | 422 | 覆盖率文件不存在 |
| `BOUNDARY_VIOLATION` | 422 | 文件变更越界 |
| `CROSS_PROJECT_ACCESS_DENIED` | 403 | 跨项目访问被拒绝 |
| `VALIDATION_REJECTED` | 422 | 验证未通过 |
| `MERGE_CONFLICT` | 409 | 合并冲突 |

---

## 5. 项目结构

```
Maestro-MCP/
├── cmd/maestro/main.go              # 入口：serve / mcp / project 子命令
├── internal/
│   ├── config/config.go             # maestro.yaml 加载
│   ├── mcp/
│   │   ├── server.go                # MCP Server 注册 (mcp-go)
│   │   ├── transport.go             # stdio + SSE 双传输
│   │   ├── tools/                   # { project, feature, task, submit, blocker }.go
│   │   ├── resources/               # { board, task_context }.go
│   │   └── prompts/                 # { coordinator, worker, verifier }.go
│   ├── handler/                     # HTTP: { feature, task, session, board, websocket }.go
│   ├── service/                     # 业务逻辑: { feature, task, session, context_filter,
│   │                                #   boundary_guard, test_runner, coverage_parser,
│   │                                #   contract_parser, worktree }.go
│   ├── store/                       # 数据层: { sqlite, project, feature, task, session, contract }_store.go
│   ├── model/model.go
│   └── ws/hub.go
├── web/                             # Preact + Vite → go:embed
│   └── src/components/              # { Board, TaskCard, SessionList, ActivityLog }.tsx
├── data/                            # SQLite (gitignore)
├── Dockerfile / docker-compose.yaml / maestro.yaml / Makefile
```

---

## 6. 部署方案

### 6.1 Dockerfile

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o maestro ./cmd/maestro

FROM alpine:3.19
RUN apk add --no-cache ca-certificates git
COPY --from=build /app/maestro /usr/local/bin/maestro

EXPOSE 8080 3000
ENTRYPOINT ["maestro"]
CMD ["serve", "--config", "/etc/maestro/maestro.yaml"]
```

### 6.2 docker-compose.yaml

```yaml
version: "3.8"
services:
  maestro:
    build: .
    ports:
      - "8080:8080"    # Web UI + REST API + WebSocket
      - "3000:3000"    # MCP SSE
    volumes:
      - ./maestro.yaml:/etc/maestro/maestro.yaml
      - ./data:/app/data                # SQLite 持久化
      - ~/projects:/workspace           # 项目工作区 (供 Worktree 创建)
    command: serve --config /etc/maestro/maestro.yaml
```

### 6.3 本地部署

```bash
# 下载单二进制即可
curl -L https://github.com/xxx/maestro-mcp/releases/latest/download/maestro -o maestro && chmod +x maestro
./maestro serve --config maestro.yaml          # 启动全部服务
./maestro mcp --transport stdio                # 仅 MCP stdio (Claude Code)
```

### 6.4 maestro.yaml 配置示例

```yaml
server: { host: "0.0.0.0", port: 8080, ws_port: 8080 }
mcp: { sse_port: 3000 }
storage: { type: "sqlite", path: "./data/maestro.db" }
validation: { require_tests: true, default_min_coverage: 80, reject_on_boundary_violation: true }
agents: { heartbeat_timeout: 300 }
logging: { level: "info", format: "json" }

# 项目级配置 (含 contract_paths)
projects:
  user-service:
    workspace_path: "~/projects/user-service"
    contract_paths: ["docs/openapi.yaml", "docs/api-contracts/"]
    contract_watch: true
```

---

## 7. 风险与缓解

| 风险 | 影响 | 缓解措施 |
|---|---|---|
| MCP 协议规范变动 | Go MCP SDK 需要适配 | 锁定 `mcp-go` 版本，关注 spec 更新 |
| Agent 异常断连 | 任务卡在 in_progress | 心跳超时自动释放 Worker 和 Worktree，回退到 pending |
| 测试执行超时 | Agent 提交后阻塞等待测试 | 配置 `test_timeout`（默认 120s），超时视为失败 |
| Git Worktree 磁盘占用 | 大量并行任务占用磁盘 | 配置 `max_worktrees`（默认 10），LRU 清理 |
| OpenAPI 解析失败 | 契约索引不完整，上下文降噪降级 | 降级为纯 description 模式，不影响其他功能 |
| SQLite 写并发 | 多 Session 同时提交可能锁表 | WAL 模式 + 写操作串行化 |
| 项目 workspace_path 冲突 | 多项目指向同一目录 | UNIQUE 约束 + 注册时检测 |
| Agent 绑定错项目 | cwd 匹配到错误项目 | 支持显式 `--project` 覆盖，启动时校验并警告 |
