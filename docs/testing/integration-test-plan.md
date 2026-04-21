# Maestro-MCP v1.0 真实项目集成测试计划

## 1. 当前状态审计

### 1.1 已完成的功能清单

| 模块 | 文件 | 状态 | 说明 |
|------|------|------|------|
| 数据模型 | `internal/model/model.go` | 完成 | 12 个结构体，完整的状态/角色/优先级常量 |
| 项目管理 | `internal/service/project_service.go` | 完成 | 8 个方法：CRUD + Archive/Restore + Bind/FindByPath |
| 任务状态机 | `internal/service/task_service.go` | 完成 | 16 个方法：完整状态机 + 串行化事务 + 重试逻辑 |
| 会话管理 | `internal/service/session_service.go` | 完成 | 12 个方法：注册/心跳/断连/过期清理/容量控制 |
| Feature 管理 | `internal/service/feature_service.go` | 完成 | 5 个方法：CRUD + 自动状态转换 |
| 零信任验证 | `internal/service/validation_service.go` | 完成 | 2 个方法：SubmitAndValidate 全流程 + 历史查询 |
| API 契约 | `internal/service/contract_service.go` | 完成 | 4 个方法：OpenAPI + 手动 JSON 解析 |
| 上下文服务 | `internal/service/context_service.go` | 完成 | 2 个方法：依赖摘要 + API 契约注入 |
| Worktree | `internal/service/worktree_service.go` | 完成 | 6 个方法：完整生命周期 + GC |
| 数据 GC | `internal/service/gc_service.go` | 完成 | 4 个方法：activity_log/audit_log/test_log 清理 |
| 启动恢复 | `internal/service/recovery_service.go` | 完成 | 1 个方法：重置全部异常状态 |
| 边界检查 | `internal/service/boundary_checker.go` | 完成 | allowed_dirs + forbidden_patterns |
| 覆盖率解析 | `internal/service/coverage_parser.go` | 完成 | 4 格式 + 自动检测 |
| Git 操作 | `internal/service/git_helper.go` | 完成 | 5 个函数：CLI git 全部操作 |
| REST API | `internal/handler/` (6 文件) | 完成 | 35+ 端点 + WebSocket + 嵌入式 Web UI |
| MCP Tools | `internal/mcp/tools/` (5 文件) | 完成 | 13 个工具 + 6 个资源 + 3 个 Prompt |
| 数据存储 | `internal/store/` (12 文件) | 完成 | 11 个 Store 接口 + SQLite 实现 |
| WebSocket | `internal/ws/hub.go` | 完成 | Hub + Client + 20 种事件类型 |
| 前端 | `web/src/` (12 文件) | 完成 | Preact + Vite + 暗色主题 + go:embed |
| 部署 | `Dockerfile` + `docker-compose.yaml` | 完成 | 多阶段构建 + 健康检查 |

### 1.2 已有测试覆盖

| 类型 | 数量 | 文件 |
|------|------|------|
| Go 单元测试 | 96 个 | `internal/service/*_test.go` (6 文件) + `internal/store/*_test.go` (2 文件) |
| Playwright E2E | 98 个 | `tests/e2e/specs/` (19 spec 文件) |
| Handler 单元测试 | **0 个** | 缺失 |
| MCP 工具测试 | **0 个** | 缺失 |
| Store 单元测试 | 仅 task_store | 其他 10 个 Store 无测试 |
| Git 操作测试 | **0 个** | 缺失 |
| WebSocket 单元测试 | **0 个** | 缺失 |

### 1.3 已发现的 Bug

| # | 严重度 | 位置 | 描述 |
|---|--------|------|------|
| 1 | **高** | `task_handler.go:482-494` | `GetTaskResult` 返回 Task 而非 TaskResult。应查询 `task_results` 表，实际返回了任务本身 |
| 2 | **中** | `task_service.go:609` | 隐式注册 worker 时使用了 `model.WorktreeStatusActive` (worktree 状态常量) 作为 worker 状态 |
| 3 | **低** | `CLAUDE.md` | 文档说 "Go 1.22+" 但 go.mod 是 1.25.0；提到 go-git 但实际用 os/exec |
| 4 | **低** | `Dockerfile` | 使用 golang:1.24-alpine 但 go.mod 要求 1.25.0 |

---

## 2. 测试项目矩阵

| 项目 | 路径 | 规模 | 语言/框架 | Git 状态 | 测试定位 |
|------|------|------|-----------|----------|----------|
| mcp_test | `$MAESTRO_TEST_PATH_MCP_TEST` | 微型(4文件) | 无 | 有(master, 1 commit) | Git 操作基础验证，快速迭代 |
| x_blog | `$MAESTRO_TEST_PATH_X_BLOG` | 小型(43文件) | TS/Next.js 14 + Supabase | **无，需 init** | 前端场景 + 零信任验证 |
| jcai | `$MAESTRO_TEST_PATH_JCAI` | 中型(3126文件) | Java 25 + TS + 多模块 | 有(master, 70+ commits) | 复杂场景 + 多 Agent 编排 |
| jiuxi | `$MAESTRO_TEST_PATH_JIUXI` | 大型(3子项目) | Java 21 + Maven | 各自独立 git | 多仓库工作空间 + 实时特性 |

---

## 3. Phase A: 自动化模拟 Agent（基础设施验证）

### 3.0 测试基础设施

**新建文件：**

| 文件 | 职责 |
|------|------|
| `tests/e2e/helpers/git-helper.ts` | git 操作封装：initGitRepo, makeFileChange, gitCommit, getWorktreeList, getBranchList, fileExists, cleanupWorktrees, gitLog |
| `tests/e2e/helpers/real-project-data.ts` | 4 个项目的路径常量、register_project 配置参数、项目特有的 test_requirements |
| `tests/e2e/helpers/mock-agent.ts` | MockAgent + MockVerifier 类，通过 REST API 模拟完整 Agent 行为 |
| `tests/e2e/playwright.real-world.config.ts` | 独立 Playwright 配置，超时 120s，testDir: `./specs-real-world/` |

**MockAgent 核心接口：**
```
MockAgent:
  connect(projectId, role, sessionId)  → 注册 session
  pickupTask()                         → get_next_task + 返回含上下文富化的完整响应
  doWork(changes[])                    → 在 worktree 中执行预设文件修改
  submit(summary)                      → submit_task_result + 触发零信任验证
  executeFullLifecycle(changes, summary) → 完整流程
  claimBatch(count)                    → claim_batch 批量领取
  releaseWorker()                      → release_worker 释放

MockVerifier:
  connect(projectId)
  pickupVerification()                 → get_verification_task
  approve(taskId)                      → submit_verification(passed=true)
  reject(taskId, notes)                → submit_verification(passed=false)
  merge(taskId)                        → merge_task
```

### 3.1 R01: mcp_test — Git 核心操作 (14 场景)

**文件**: `tests/e2e/specs-real-world/R01-git-worktree.spec.ts`

| # | 场景 | 步骤摘要 | 验证点 |
|---|------|----------|--------|
| 1 | Worktree 创建 | Agent claim 任务 | `git worktree list` 含新条目，`git branch` 含 `task/<id>`，物理目录存在且包含主工作区文件 |
| 2 | Worktree 隔离 | 3 Agent 各 claim 1 任务，各创建不同文件 | 每个 worktree `getTaskDiff` 只含自己的文件 |
| 3 | Git Diff: 已提交 | worktree 新增文件 → git commit | `getTaskDiff` 返回已提交文件列表 |
| 4 | Git Diff: 4 来源 | worktree 新增(已提交) + staged + unstaged + untracked | `getTaskDiff` 返回所有 4 种变更来源的文件 |
| 5 | 成功 Merge | Agent 完成完整生命周期 | merge_commit 为 40 位 SHA，主工作区含新文件，`git log` 显示 merge commit |
| 6 | Merge 冲突 | 两 Agent 改同一文件，第二 Agent merge | 状态 → `merge_conflicted`，主工作区 clean（merge --abort） |
| 7 | 冲突解决: reopen | resolve_merge_conflict(action=reopen) | 回到 in_progress，worktree 保留 |
| 8 | 冲突解决: cancel | resolve_merge_conflict(action=cancel) | 任务取消，资源释放 |
| 9 | 冲突解决: followup | resolve_merge_conflict(action=followup) | **原任务保持 merge_conflicted，新任务被创建** (parent_task_id, relation_type=conflict_resolution, priority=high) |
| 10 | Worktree GC | cancel + merge 后触发 GC | abandoned/merged 物理目录删除 + DB 清理，**active worktree 不受影响** |
| 11 | ForceRollback 清理 | ForceRollback submitted 任务 + GC | worktree → abandoned → GC 清理 |
| 12 | Blocker 全流程 | report_blocker(in_progress→blocked) → resolve_blocker(reassign=true→in_progress) | blocked 状态正确转换，resolve 时 task 重新分配 |
| 13 | Blocker + cancel | report_blocker → cancel_task | 从 blocked 直接取消 |
| 14 | Feature auto-transition | 创建 feature(规划中) + 任务 → 验证 active → 全部 done → completed | Feature 状态自动流转 3 个阶段 |

### 3.2 R02: x_blog — 零信任验证 (13 场景)

**前置**: beforeAll 中 `git init && git add -A && git commit -m "init"`
**文件**: `tests/e2e/specs-real-world/R02-zero-trust-validation.spec.ts`

| # | 场景 | 步骤摘要 | 验证点 |
|---|------|----------|--------|
| 1 | Boundary 拒绝 | `allowed_dirs: ["src/components/"]` + 改根目录 `package.json` | submit 返回 BOUNDARY_VIOLATION，task 留在 in_progress |
| 2 | Forbidden 拒绝 | `forbidden_patterns: ["*.env*"]` + 创建 `.env.local` | submit 返回 BOUNDARY_VIOLATION |
| 3 | Boundary 通过 | 改 `src/components/` 内文件 | submit 成功到 submitted |
| 4 | 空 allowed_dirs | 不设 allowed_dirs + 改任意文件 | 默认放行所有文件（无限制模式） |
| 5 | 复杂 glob 模式 | `forbidden_patterns: ["*.secret*", "config/prod-*"]` | filepath.Match 正确匹配 |
| 6 | 测试命令执行 | `test_command: "npm run build"` 在 worktree 中执行 | test_ok=1, test_output 有内容, duration_ms > 0 |
| 7 | 测试失败拒绝 | test_command 故意执行失败 | task 留在 in_progress，test_ok=0，validation result=rejected |
| 8 | 覆盖率自动检测 | 提供不同格式文件，不指定 format | auto-detect 正确识别 istanbul/jacoco/cobertura/go-cover |
| 9 | 覆盖率 istanbul | 合成 istanbul JSON 文件 | coverage_percent 正确计算 (3/4=75%) |
| 10 | 覆盖率阈值 | min_coverage=90 + 实际 75% | COVERAGE_BELOW_MIN 拒绝 |
| 11 | 测试命令超时 | test_command="node -e setTimeout(300000)" + timeout=5s | 进程被 kill，duration ≈ 5s |
| 12 | 输出截断 | test_command 输出 >64KB | 输出含 [TRUNCATED]，head+tail 保留 |
| 13 | Rework 循环 | submit→verify(reject)→resubmit→verify(pass)→merge | validation_runs 有 2+ 条记录，GetValidationHistory 按 attempt 升序 |

### 3.3 R03: jcai — 多 Agent + 依赖链 (14 场景)

**文件**: `tests/e2e/specs-real-world/R03-multi-agent-dependency.spec.ts`

| # | 场景 | 步骤摘要 | 验证点 |
|---|------|----------|--------|
| 1 | 依赖链 T1→T2→T3 | T1 done 后 T2 才可 claim | getNextTask 在 T1 done 前返回 no-available |
| 2 | 上下文富化 | T2 claim 时检查响应 | dependency_summaries 含 T1 的 summary |
| 3 | 摘要降级 | T1 没有 summary 只有 title | 依赖摘要降级显示 title |
| 4 | 摘要截断 | T1 summary > 2000 字符 | 摘要末尾含 [TRUNCATED] |
| 5 | API 契约注入 | T2 有 required_apis + 项目有 contract | api_contracts 数组非空 |
| 6 | require_state | T2 require_state=submitted | T1 到 submitted 即可 claim T2，不需 done |
| 7 | 3 Agent 并发 | 3 session 各 claim 1 任务 | 3 个独立 worktree，各自 git diff 互不干扰 |
| 8 | claim_batch | Agent 调 claim_batch(count=3) | 批量领取 3 任务，各创建独立 worktree，claimed 数组长度=3 |
| 9 | release_worker | Agent 领取后 release_worker | worker 被删除，task 的 assigned_worker_id 清空 |
| 10 | 优先级排序 | 创建 urgent/high/normal/low 各 1 任务 | getNextTask 按 urgent→high→normal→low 顺序返回 |
| 11 | 循环依赖检测 | T1 dep T2, T2 dep T1 | split_task/update_task 返回 CIRCULAR_DEPENDENCY 错误 |
| 12 | 项目配置降级链 | Task 无 test_req → 项目有 default_test_command | SubmitAndValidate 使用项目级配置执行测试 |
| 13 | JaCoCo 解析 | 合成 JaCoCo XML (`<counter type="INSTRUCTION" missed="30" covered="70"/>`) | coverage=70% |
| 14 | Cobertura 解析 | 合成 Cobertura XML (`<coverage line-rate="0.85">`) | coverage=85% |

### 3.4 R04: jiuxi — 多仓库 + 实时 (11 场景)

**文件**: `tests/e2e/specs-real-world/R04-multi-repo-realtime.spec.ts`

| # | 场景 | 步骤摘要 | 验证点 |
|---|------|----------|--------|
| 1 | 多项目注册 | 3 个 git repo 各注册为 Maestro project | 各自 worktree 在各自目录下创建 |
| 2 | 空项目容错 | frontend(空目录) 注册 + claim | claim 成功，worktree 创建静默失败，task 正常流转 |
| 3 | Archive/Restore | archive → 验证 POST 操作被拒 → restore | archived 项目 POST 返回 403，GET 放行，restore 后恢复 |
| 4 | BindProject 路径 | 同一 workspace_path 注册两次 | 第二次返回 ambiguous 或正确绑定 |
| 5 | WebSocket 事件 | 全生命周期 WS 订阅 | task.claimed/submitted/verifying/approved/merged/done 按序到达 |
| 6 | Activity Log | 全生命周期后查询 | created/claimed/submitted/verifying/approved/merged 都有记录 |
| 7 | ForceRollback | submitted → ForceRollback + GC | worktree 物理清理完成，task 回到 pending |
| 8 | Stale Session | 注册 session → 不发 heartbeat → 等待超时扫描 | session → offline，in_progress task → pending |
| 9 | Worker 容量 | session capacity=2 → 注册 3 worker | 第 3 个返回 SESSION_CAPACITY_FULL |
| 10 | Disconnect 清理 | session disconnect 时有 in_progress 任务 | task → pending，worktree → stale |
| 11 | ForceRelease | force-release 有活跃任务的 session | 强制释放，所有 task 重置 |

### 3.5 R05: MCP 协议验证 (8 场景)

**文件**: `tests/e2e/specs-real-world/R05-mcp-protocol.spec.ts`

| # | 场景 | 验证点 |
|---|------|--------|
| 1 | Resource: `project://list` | 返回所有项目列表 JSON |
| 2 | Resource: `board://active` | 返回活跃项目看板聚合数据 |
| 3 | Resource: `task://{id}/context` | 返回任务上下文含依赖摘要 |
| 4 | Resource: `feature://{id}/summary` | 返回 feature 进度摘要 |
| 5 | Prompt: `start-worker` + role 参数 | 返回 worker 角色指令文本 |
| 6 | Prompt: `start-verifier` | 返回 verifier 角色指令文本 |
| 7 | Prompt: `start-coordinator` | 返回 coordinator 角色指令文本 |
| 8 | update_task 字段限制 | in_progress 任务更新 title 被拒，只允许 description/summary |

---

## 4. Phase B: 真实 Claude Code 执行（端到端可用性验证）

### 4.0 准备工作

1. **编译**: `go build -o maestro.exe ./cmd/maestro`
2. **启动**: `./maestro.exe serve --db data/test-real.db --http :8080 --sse :3000`
3. **配置 MCP**: 在 Claude Code 的 settings.local.json 中添加 Maestro SSE Server
4. **注册项目**: 通过 Claude Code 调用 `register_project` 注册真实项目

### 4.1 B1: mcp_test — 最小验证

**任务**: "在项目中创建一个 CHANGELOG.md 文件，记录项目初始化"

**验证要点**:
- Claude Code 通过 MCP `get_next_task` 获得任务 + 上下文
- Agent 理解 mcp_test 的空项目结构
- Agent 创建文件 → `submit_task_result` → 零信任验证通过
- `merge_task` → 主分支合并
- Dashboard 实时显示完整流程

### 4.2 B2: x_blog — 前端任务

**任务**: "在 src/components/ 中添加 Footer.tsx 组件，显示版权信息"

**验证要点**:
- `allowed_directories: ["src/components/"]` 约束生效
- Agent 创建的文件在正确目录，boundary check 通过
- merge 后主工作区有 Footer.tsx
- `git diff` 确认只改了允许的目录

### 4.3 B3: jcai — 复杂多模块

**3 个有依赖关系的任务**:

| 任务 | Role | 描述 | 依赖 |
|------|------|------|------|
| T1 | backend | "在 health 包添加 /api/v1/health 端点" | 无 |
| T2 | backend | "为 health 端点添加单元测试" | T1 done |
| T3 | frontend | "在 dashboard 页面展示 health 状态指示器" | T2 done |

**验证要点**:
- 依赖链正确执行（T2 等 T1 done）
- Agent 收到 dependency_summaries 上下文
- 每个任务的 worktree 在对应模块目录
- 全流程 Dashboard 可追踪

### 4.4 B4: jiuxi — 多项目并行

**跨仓库任务**:
- trading-signal: "在 service 层添加交易信号的简单缓存装饰器"
- ws-sdk: "为 WebSocket 客户端添加自动重连的空方法"

**验证要点**:
- 两个项目独立注册、独立工作
- 各自 worktree 互不影响
- Dashboard 多项目视图正确显示

### 4.5 证据保留标准

每个真实执行场景保留：
1. Dashboard 截图（任务状态、看板、活动日志）
2. `git log --oneline -10` 输出
3. `git worktree list` 输出
4. `git diff HEAD~1` 输出（验证 merge 内容）
5. Activity Log API 响应 JSON
6. WebSocket 事件日志（如捕获到）

---

## 5. 需预先修复的 Bug

在开始集成测试前，需先修复以下已发现的问题：

| # | Bug | 修复方案 |
|---|-----|----------|
| 1 | `GetTaskResult` 返回 Task 而非 TaskResult | 修改 handler 查询 task_results 表 |
| 2 | Worker 状态误用 WorktreeStatusActive | 添加 WorkerStatus 常量或使用字符串 "idle" |
| 3 | CLAUDE.md Go 版本和依赖描述不准 | 更新文档 |

---

## 6. 文件清单

**Phase A 新建文件（纯测试，不修改源代码）**:
```
tests/e2e/helpers/git-helper.ts
tests/e2e/helpers/real-project-data.ts
tests/e2e/helpers/mock-agent.ts
tests/e2e/playwright.real-world.config.ts
tests/e2e/specs-real-world/R01-git-worktree.spec.ts       # 14 场景
tests/e2e/specs-real-world/R02-zero-trust-validation.spec.ts # 13 场景
tests/e2e/specs-real-world/R03-multi-agent-dependency.spec.ts # 14 场景
tests/e2e/specs-real-world/R04-multi-repo-realtime.spec.ts   # 11 场景
tests/e2e/specs-real-world/R05-mcp-protocol.spec.ts          # 8 场景
```

**Phase B 无新建文件** — 使用现有 Maestro MCP Server + Claude Code

---

## 7. 执行顺序

```
Phase 0: Bug 修复
  → 修复 GetTaskResult handler
  → 修复 Worker 状态常量
  → 更新 CLAUDE.md

Phase A (自动化模拟 Agent):
  Step 1: 创建基础设施 (git-helper, real-project-data, mock-agent, config)
  Step 2: x_blog git init
  Step 3: R01 mcp_test (14 场景) → 运行验证
  Step 4: R02 x_blog (13 场景) → 运行验证
  Step 5: R03 jcai (14 场景) → 运行验证
  Step 6: R04 jiuxi (11 场景) → 运行验证
  Step 7: R05 MCP 协议 (8 场景) → 运行验证
  Step 8: 全量运行 + 生成 HTML 报告 + 截图

Phase B (真实 Claude Code 执行):
  Step 9: 编译 + 启动 + 配置 MCP Server
  Step 10: B1 mcp_test 最小场景
  Step 11: B2 x_blog 前端场景
  Step 12: B3 jcai 多模块场景
  Step 13: B4 jiuxi 多项目并行
  Step 14: 收集全部证据，整理最终报告
```

**总计**: 60 个自动化场景 + 4 个真实执行场景

---

## 8. 功能覆盖交叉索引

以下表格确保 Maestro-MCP 每个 MCP Tool、REST 端点、Service 方法都有对应测试：

### 8.1 MCP Tools 覆盖

| Tool | Phase A 场景 | Phase B |
|------|-------------|---------|
| register_project | R01-R04 项目注册 | B1-B4 |
| list_projects | R05 Resource 测试 | B1 |
| create_feature | R01#14 feature auto-transition | B3 |
| split_task | R01-R04 任务创建 | B3 |
| update_task | R05#8 字段限制 | - |
| cancel_task | R01#13 blocker+cancel | - |
| resolve_blocker | R01#12 blocker 全流程 | - |
| resolve_merge_conflict | R01#7-9 reopen/cancel/followup | - |
| get_next_task | R01-R04 所有 claim 场景 | B1-B4 |
| submit_task_result | R02 所有 validation 场景 | B1-B4 |
| report_blocker | R01#12 blocker 全流程 | - |
| claim_batch | R03#8 claim_batch | - |
| release_worker | R03#9 release_worker | - |
| get_verification_task | R01-R04 验证流程 | B3 |
| submit_verification | R01-R04 approve/reject | B3 |
| merge_task | R01#5-6 成功/冲突 merge | B1-B4 |

### 8.2 Service 方法覆盖

| 方法 | 测试场景 |
|------|----------|
| ProjectService.CreateProject | R01-R04 项目注册 |
| ProjectService.GetProject | R01-R04 全部 |
| ProjectService.ListProjects | R05#1 |
| ProjectService.UpdateProject | - |
| ProjectService.ArchiveProject | R04#3 |
| ProjectService.RestoreProject | R04#3 |
| ProjectService.BindProject | R04#4 |
| TaskService.CreateTask | R01-R04 全部创建 |
| TaskService.GetTask | R01-R04 全部获取 |
| TaskService.ListTasks | R03 依赖链验证 |
| TaskService.UpdateTask | R05#8 |
| TaskService.ClaimTask | R01-R04 claim |
| TaskService.CancelTask | R01#13 |
| TaskService.ReportBlocker | R01#12 |
| TaskService.ResolveBlocker | R01#12 |
| TaskService.GetNextTask | R01-R04 所有 getNextTask |
| TaskService.SubmitTaskResult | R02 全部 + R01-R04 submit |
| TaskService.GetVerificationTask | R01-R04 verify |
| TaskService.SubmitVerification | R01-R04 approve/reject |
| TaskService.MergeTask | R01#5-6 |
| TaskService.ResolveMergeConflict | R01#7-9 |
| TaskService.ForceRollback | R04#7 |
| TaskService.GetTaskDiff | R01#3-4 |
| SessionService.* | R04#8-11 |
| FeatureService.* | R01#14 |
| ValidationService.SubmitAndValidate | R02 全部 |
| ValidationService.GetValidationHistory | R02#13 |
| ContextService.GetTaskContext | R03#2-5 |
| ContractService.* | R03#5 API 契约 |
| WorktreeService.GCWorktrees | R01#10 |

---

---

## 10. Agent 约束机制分析与增强方案

> 核心问题：每次检查都发现大量需要修复的问题，如何确保 MCP 任务在粒度和落地质量上达到预期？

### 10.1 现有约束机制（已实现）

| 约束 | 粒度 | 生效时机 | 能力 | 局限 |
|------|------|----------|------|------|
| `allowed_directories` | 目录前缀 | submit 时验证 | 限制可修改的目录 | 无法限制单文件内的修改范围 |
| `forbidden_patterns` | **文件名** glob | submit 时验证 | 阻止修改敏感文件 | **只匹配文件名不匹配路径**，`config/prod-*` 永远不匹配 |
| `role` 枚举 | 任务级 | claim 时 | 限制哪种 Agent 可领取 | 不控制 Agent 的具体行为 |
| `test_requirements` | 命令级 | submit 时验证 | 强制测试通过 | 无 lint 集成，测试通过 ≠ 代码质量好 |
| `min_coverage` | 百分比 | submit 时验证 | 强制覆盖率门槛 | 无法区分有效覆盖和冗余覆盖 |
| 循环依赖检测 | 依赖图 | 创建/更新时 | 防止循环等待 | 不验证依赖合理性 |
| 命令注入防护 | 字符串 | 创建时 | 阻止 `&&`, `||`, `;`, `\n` | 不覆盖所有注入向量 |
| 路径穿越防护 | 字符串 | 创建时 | 阻止 `..` | **无 symlink 防护** |
| 环境变量白名单 | 进程级 | 测试执行时 | 防止密钥泄露 | 白名单固定不可配置 |
| 串行化事务 | 数据库级 | 所有状态变更 | 防止并发冲突 | 不解决逻辑冲突 |
| 状态字段限制 | 状态级 | update 时 | in_progress 只能改 description/summary | 不阻止"合理但不正确"的修改 |

### 10.2 关键缺失（需要实现）

#### 缺失 1：变更规模控制 — CHANGE SCOPE LIMITS

**问题**：Agent 理论上可以在一个任务中改写整个项目，boundary checker 只验证文件路径，不验证变更量。

**影响**：一个"修复 typo"的任务可能包含数百行无关改动。

**方案**：在 Task 模型中增加变更规模约束字段：

```go
// model/model.go Task struct 新增
MaxFilesChanged  *int `json:"max_files_changed,omitempty"`  // 最多改几个文件
MaxLinesAdded    *int `json:"max_lines_added,omitempty"`    // 最多新增几行
MaxLinesRemoved  *int `json:"max_lines_removed,omitempty"`  // 最多删除几行
```

在 `SubmitAndValidate` 的 git diff 步骤后增加：

```
Step 4.5 -- Change scope validation
  git diff --stat <base_commit>  → 统计每个文件的 +- 行数
  如果 files_changed > MaxFilesChanged → SCOPE_EXCEEDED
  如果 lines_added > MaxLinesAdded → SCOPE_EXCEEDED  
  如果 lines_removed > MaxLinesRemoved → SCOPE_EXCEEDED
```

#### 缺失 2：实现计划门控 — PLAN GATE

**问题**：Agent claim 任务后直接开始编码，没有"方案评审"环节。错误方案到 submit 时才发现，返工成本高。

**方案**：新增任务状态 `planning`：

```
pending → planning (Agent 开始分析)
planning → in_progress (Coordinator 批准方案)
planning → pending (Coordinator 驳回方案)
```

Task struct 新增：
```go
ImplementationPlan *string `json:"implementation_plan,omitempty"` // Agent 提交的实现方案
PlanApprovedBy     *string `json:"plan_approved_by,omitempty"`    // 审批人
```

新增 MCP Tool：`submit_plan` — Agent 提交实现方案（包含：将修改的文件、实现思路、预计变更量）

#### 缺失 3：结构化验收标准 — ACCEPTANCE CRITERIA

**问题**：`description` 字段是自由文本，Agent 是否完成全靠人工判断。没有机器可校验的"完成定义"。

**方案**：Task struct 新增 `acceptance_criteria`：

```go
AcceptanceCriteria json.RawMessage `json:"acceptance_criteria,omitempty"`
// 格式：
// [
//   {"type": "file_exists", "path": "src/components/Footer.tsx"},
//   {"type": "file_not_exists", "path": "src/components/Footer.test.tsx"},
//   {"type": "pattern_in_file", "path": "src/components/Footer.tsx", "pattern": "export.*Footer"},
//   {"type": "command_exits_0", "command": "grep -r 'Footer' src/"},
//   {"type": "diff_line_count", "max_added": 50}
// ]
```

在 `SubmitAndValidate` 中，test execution 之后增加 acceptance criteria 自动校验步骤。

#### 缺失 4：Lint 集成

**问题**：只有 test command，没有独立的 lint command。测试通过 ≠ 代码风格正确。

**方案**：TestRequirements 新增字段：

```go
type TestRequirements struct {
    Command        string `json:"command"`
    LintCommand    string `json:"lint_command,omitempty"`    // 新增
    CoverageFormat string `json:"coverage_format"`
    CoveragePath   string `json:"coverage_path"`
    MinCoverage    int    `json:"min_coverage"`
    Timeout        int    `json:"timeout,omitempty"`
}
```

SubmitAndValidate 管道中，boundary check 之后、test execution 之前插入 lint execution。

#### 缺失 5：任务超时和重试限制

**问题**：
- 任务可以永远停留在 `in_progress` 状态
- 任务可以在 `in_progress` ↔ `submitted` 之间无限循环（验证反复失败）
- 没有死 Worker 自动检测

**方案**：

```go
// Task struct 新增
MaxAttempts   *int `json:"max_attempts,omitempty"`   // 最大提交次数，默认 5
TimeoutMinutes *int `json:"timeout_minutes,omitempty"` // in_progress 超时分钟数，默认 60

// 超时检测在 StaleSessionScanner 中扩展：
// 扫描 in_progress 且 assigned_at 超过 TimeoutMinutes 的任务
// 自动回退到 pending 并标记 worktree abandoned
```

#### 缺失 6：forbidden_patterns 路径匹配修复

**问题**：当前 `filepath.Base(file)` 只匹配文件名，`config/prod-*` 等路径模式永远不会匹配。

**方案**：修改 `boundary_checker.go`，对 forbidden_patterns 做双匹配：
1. `filepath.Match(pattern, filepath.Base(file))` — 兼容现有文件名 glob
2. `filepath.Match(pattern, file)` — 新增全路径匹配（仅当 pattern 含 `/` 时启用）

### 10.3 约束层级模型

从弱到强，定义 5 个约束层级：

```
L0 约束（现有）: allowed_directories + forbidden_patterns + role
  → 控制 WHERE（在哪改）

L1 约束（现有）: test_requirements + min_coverage
  → 控制 WHAT（改完必须通过什么验证）

L2 约束（新增）: max_files_changed + max_lines_added/removed
  → 控制 HOW MUCH（改多少）

L3 约束（新增）: lint_command + acceptance_criteria
  → 控制 HOW WELL（改成什么样才算好）

L4 约束（新增）: implementation_plan + plan_approved_by
  → 控制 HOW（用什么方案来改）
```

### 10.4 最佳实践：任务粒度控制

基于以上约束，定义任务拆分原则：

| 原则 | 说明 | 约束机制 |
|------|------|----------|
| **单一职责** | 一个任务只做一件事 | `max_files_changed ≤ 5` |
| **可验证** | 每个任务有明确的完成标准 | `acceptance_criteria` 必填 |
| **可回滚** | 单个任务的变更可以独立回滚 | Worktree 隔离 |
| **可度量** | 变更规模可量化 | `max_lines_added ≤ 100` |
| **有测试** | 必须有对应的测试覆盖 | `min_coverage > 0` |
| **有边界** | 明确的文件范围限制 | `allowed_directories` 必填 |
| **有超时** | 不能无限期执行 | `timeout_minutes ≤ 120` |

**推荐的任务创建模板**：

```json
{
  "title": "添加健康检查端点 GET /api/v1/health",
  "description": "在 controller/health.go 中添加 HealthCheck handler，返回 {\"status\": \"ok\"}",
  "role": "backend",
  "allowed_directories": "[\"controller/\", \"service/health/\"]",
  "forbidden_patterns": "[\"*.env*\", \"config/prod-*\"]",
  "test_requirements": {
    "command": "go test ./controller/... -run TestHealth",
    "lint_command": "golangci-lint run ./controller/",
    "coverage_format": "go-cover",
    "coverage_path": "coverage.out",
    "min_coverage": 80
  },
  "max_files_changed": 3,
  "max_lines_added": 50,
  "max_lines_removed": 10,
  "max_attempts": 3,
  "timeout_minutes": 30,
  "acceptance_criteria": [
    {"type": "file_exists", "path": "controller/health.go"},
    {"type": "pattern_in_file", "path": "controller/health.go", "pattern": "func.*HealthCheck"},
    {"type": "file_exists", "path": "controller/health_test.go"}
  ]
}
```

### 10.5 约束机制实现优先级

| 优先级 | 增强项 | 实现复杂度 | 价值 |
|--------|--------|-----------|------|
| **P0** | 变更规模控制 (max_files/max_lines) | 低（git diff --stat 解析） | 高 — 防止超大变更 |
| **P0** | forbidden_patterns 路径匹配修复 | 极低（改一行代码） | 高 — 修复现有 bug |
| **P0** | 任务超时 + 重试限制 | 低（扩展现有 scanner） | 高 — 防止任务卡死 |
| **P1** | 结构化验收标准 | 中（新增字段 + 校验逻辑） | 高 — 自动化完成验证 |
| **P1** | Lint 集成 | 低（新增 lint_command 字段） | 中 — 代码风格保障 |
| **P2** | 实现计划门控 | 高（新增状态 + tool + UI） | 中 — 前置设计审查 |
| **P2** | AllowedTestCommands 白名单执行 | 极低（已有字段，只需加校验） | 中 — 安全加固 |
| **P3** | Symlink 防护 | 中（resolve symlink 后再检查） | 低 — 边缘场景 |

---

## 11. 集成测试中的约束验证场景

在 Phase A 中增加以下约束验证场景：

### R01 中补充：

| # | 场景 | 验证点 |
|---|------|--------|
| 15 | **变更规模限制** | 创建任务 max_files_changed=1, max_lines_added=10 → Agent 改 3 个文件 → submit 返回 SCOPE_EXCEEDED |
| 16 | **超时回退** | 创建任务 timeout_minutes=1 → 等待超过 1 分钟 → stale scanner 自动回退到 pending |

### R02 中补充：

| # | 场景 | 验证点 |
|---|------|--------|
| 14 | **Lint 集成** | 任务设 lint_command="npm run lint" → lint 失败 → submit 拒绝 |
| 15 | **验收标准自动校验** | 任务设 acceptance_criteria=[file_exists "src/components/Footer.tsx"] → Agent 只改了其他文件 → submit 拒绝 |

### R03 中补充：

| # | 场景 | 验证点 |
|---|------|--------|
| 15 | **最大重试次数** | 创建任务 max_attempts=2 → Agent 连续 3 次 submit 失败 → 第 3 次后任务自动 cancelled |

### R05 中补充：

| # | 场景 | 验证点 |
|---|------|--------|
| 9 | **AllowedTestCommands 白名单** | 项目配置 allowed_test_commands=["go test"] → 任务 test_command="npm test" → 创建时拒绝 |

**总计更新**：Phase A 从 60 场景 → **64 场景**

---

## 12. 集成测试执行总表（最终版）

| Spec 文件 | 场景数 | 核心验证目标 |
|-----------|--------|-------------|
| R01-git-worktree | 16 | Git 操作 + 状态机 + 变更规模 + 超时 |
| R02-zero-trust-validation | 15 | 边界检查 + 测试执行 + 覆盖率 + lint + 验收标准 |
| R03-multi-agent-dependency | 15 | 依赖链 + 上下文 + claim_batch + 配置降级 + 重试限制 |
| R04-multi-repo-realtime | 11 | 多仓库 + 实时 + session 生命周期 |
| R05-mcp-protocol | 9 | MCP 协议 + 工具验证 + 安全约束 |
| **Phase A 总计** | **66** | |
| Phase B 真实执行 | 4 | 端到端可用性 |
| **全部总计** | **70** | |
