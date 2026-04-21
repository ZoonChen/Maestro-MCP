# Claude Code MCP 测试指南

> **版本:** v1.0 | **更新日期:** 2026-04-19
> **前置条件:** 已完成 REST API Playwright E2E 测试 (86/86 通过)

---

## 概述

Maestro-MCP 提供 16 个 MCP Tools、6 个 Resources 和 3 个 Prompts，通过 stdio（Claude Code）和 SSE（OpenClaw/远程）两种传输模式对外服务。本文档描述如何对 MCP 协议层进行完整的功能测试。

**与 REST API E2E 测试的关系：** REST API 和 MCP 共享同一个 Service 层，REST 测试已覆盖基本 CRUD 和状态流转。MCP 测试重点在于：
1. **协议级集成** — stdio/SSE 传输通道正确性
2. **MCP 特有逻辑** — 隐式 Session 注册、角色校验、Worktree 创建、上下文组装
3. **零信任验证流程** — `submit_task_result` 触发的 git diff + 测试执行 + 覆盖率解析
4. **资源/提示词** — Resource 查询和 Prompt 模板返回

---

## 1. 测试架构

### 1.1 测试分层策略

| 层级 | 测试方式 | 覆盖范围 | 当前状态 |
|---|---|---|---|
| L1: REST API | Playwright (`request` fixture) | CRUD + 状态流转 + Session/Worker | 86 tests, 100% pass |
| L2: MCP 协议集成 | Go test + mcp-go client | Tool 调用、Resource 读取、Prompt 注入 | 待实现 |
| L3: 零信任验证 | Go test + 真实 Git repo | submit_task_result → git diff + test + coverage | 待实现 |

### 1.2 为什么需要独立的 MCP 测试

REST API 和 MCP 虽然共享 Service 层，但有以下差异需要独立覆盖：

| 差异点 | REST API | MCP |
|---|---|---|
| Session 注册 | 显式 `POST /sessions` | `get_next_task` 隐式注册 |
| submit 行为 | 简化状态流转 (in_progress → submitted) | 完整零信任验证 (git diff + test + coverage) |
| 角色校验 | 无（REST 无 session 绑定） | `get_next_task` 校验 role 与 session role 一致 |
| Worktree | 不创建 | `get_next_task` 自动创建 worktree |
| 上下文组装 | 不涉及 | `get_next_task` 返回 dependency_summaries + api_contracts |

---

## 2. 环境准备

### 2.1 构建服务器二进制

```bash
cd /path/to/Maestro-MCP
go build -o maestro.exe ./cmd/maestro/main.go
```

### 2.2 准备测试 Git 仓库

MCP 的零信任验证流程需要真实 Git 仓库。测试前需准备：

```bash
# 创建测试用 Git 仓库
TEST_REPO=$(mktemp -d)
cd $TEST_REPO
git init
echo "package main" > main.go
echo 'func main() {}' >> main.go
git add .
git commit -m "initial commit"
```

### 2.3 启动 MCP SSE 服务器

```bash
maestro serve --db :memory: --http :19080 --sse :19000
# 或仅启动 MCP
maestro mcp --transport sse --sse-addr :19000
```

---

## 3. MCP Tool 测试场景

### 3.1 项目管理工具

#### 3.1.1 `register_project`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 基本注册 | `{name: "test", workspace_path: "/tmp/test"}` | `{project_id: "P-xxx", status: "active"}` |
| 2 | 含配置注册 | `{name: "test", workspace_path: "/tmp/test", config: '{"default_test_command":"go test ./..."}'}` | project_id + config 被存储 |
| 3 | 重复 workspace_path | 两次相同 workspace_path | 第二次返回错误 |
| 4 | 缺少必填字段 | `{name: "test"}` (缺 workspace_path) | 返回参数错误 |

#### 3.1.2 `list_projects`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 空列表 | (无项目) | `[]` |
| 2 | 列出活跃项目 | include_archived=false | 仅 active 项目 |
| 3 | 包含归档项目 | include_archived=true | active + archived |

### 3.2 协调者工具

#### 3.2.1 `create_feature`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 基本创建 | `{project_id, title, description}` | `{feature_id: "F-xxx", status: "planning"}` |
| 2 | 含参考 URL | `{..., reference_urls: '["https://example.com"]'}` | feature_id + urls 存储 |

#### 3.2.2 `split_task`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 基本拆分 | `{project_id, feature_id, role:"backend", title, description, allowed_directories:'["src/"]'}` | `{task_id: "T-xxx", status: "pending"}` |
| 2 | 含依赖 | `{..., dependencies:'[{"task_id":"T-001","require_state":"done"}]'}` | task_id，依赖被存储 |
| 3 | 含测试要求 | `{..., test_requirements:'{"command":"go test ./...","coverage_format":"go-cover","min_coverage":80}'}` | task_id |
| 4 | 无效 role | `{..., role:"invalid"}` | INVALID_PARAMETER |
| 5 | 循环依赖 | A→B, B→C, C→A | CIRCULAR_DEPENDENCY |
| 6 | feature 不存在 | `{..., feature_id:"nonexist"}` | FEATURE_NOT_FOUND |
| 7 | allowed_directories 为空 | `{..., allowed_directories:'[]'}` | INVALID_PARAMETER |
| 8 | 优先级校验 | `{..., priority:"urgent"}` | task_id with priority=urgent |
| 9 | 无效优先级 | `{..., priority:"critical"}` | INVALID_PARAMETER |

#### 3.2.3 `update_task`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | pending 状态全字段修改 | `{task_id, title:"new", description:"new", ...}` | 更新成功 |
| 2 | in_progress 仅改 description | 任务 in_progress，修改 description | 更新成功 |
| 3 | in_progress 改 allowed_directories | 任务 in_progress，修改 allowed_directories | TASK_STATE_INVALID |
| 4 | submitted 状态不可修改 | 任务 submitted | TASK_STATE_INVALID |
| 5 | cancelled 状态不可修改 | 任务 cancelled | TASK_ALREADY_CANCELLED |

#### 3.2.4 `cancel_task`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 取消 pending 任务 | `{task_id, reason:"不需要了"}` | `{status: "cancelled"}` |
| 2 | 取消 in_progress 任务 | `{task_id, reason:"..."}` | cancelled, Worker 被释放 |
| 3 | 取消已取消任务 | 重复取消 | TASK_ALREADY_CANCELLED |

#### 3.2.5 `resolve_blocker`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 默认解除 | `{task_id, resolution:"已解决依赖"}` | `{status: "pending"}`，Worker/Worktree 清空 |
| 2 | reassign 且 session 在线 | `{..., reassign:true}` | `{status: "in_progress"}`，保留原绑定 |
| 3 | reassign 但 session 离线 | `{..., reassign:true}` | `{status: "pending"}`，清空绑定 |

#### 3.2.6 `resolve_merge_conflict`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | reopen | `{task_id, action:"reopen"}` | `{status: "in_progress"}` |
| 2 | cancel | `{task_id, action:"cancel"}` | `{status: "cancelled"}` |
| 3 | followup | `{task_id, action:"followup"}` | 原 task 保持 merge_conflicted，新 task 创建 |

### 3.3 执行者工具

#### 3.3.1 `get_next_task`（核心工具，需重点测试）

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 基本领取 | `{project_id, role:"backend"}` | 完整任务上下文 + worktree_path |
| 2 | 隐式 Session 注册 | 首次调用，session_id 不存在 | session 自动创建，worker 自动注册 |
| 3 | 隐式 Worker 注册 | session 存在但 worker_id 不存在 | worker 自动注册 |
| 4 | 角色不匹配 | session role=frontend, 请求 role=backend | TASK_STATE_INVALID |
| 5 | 协调者不可领取 | session role=coordinator | TASK_STATE_INVALID |
| 6 | 无可用任务 | 所有任务已完成/已认领 | NO_AVAILABLE_TASK |
| 7 | 依赖阻塞 | 任务依赖 T-001(done) 和 T-002(pending) | 仅返回依赖已满足的任务 |
| 8 | 已取消依赖视为满足 | 任务依赖 T-001(cancelled) | 任务可被领取 |
| 9 | 返回上下文包含 dependency_summaries | 前置任务有 summary | 返回中包含 dependency_summaries |
| 10 | 返回上下文包含 api_contracts | 项目有 API 契约 | 返回中包含 api_contracts |
| 11 | Worktree 自动创建 | 任务认领成功 | worktree_path 非空，Git worktree 已创建 |

#### 3.3.2 `submit_task_result`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 提交并触发验证 | `{project_id, task_id, summary:"完成"}` | `{status: "submitted", validation: "pending"}` |
| 2 | 非 in_progress 状态提交 | 任务 pending | TASK_STATE_INVALID |
| 3 | 零信任：git diff 取证 | 任务有文件变更 | validation_runs 包含 changed_files |
| 4 | 零信任：测试执行 | 项目配置了 test_command | validation_runs 包含 test_output + coverage |
| 5 | 零信任：覆盖率不足 | min_coverage=80, 实际 60 | VALIDATION_FAILED，任务回到 in_progress |

#### 3.3.3 `report_blocker`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 基本上报 | `{project_id, task_id, reason:"缺少权限"}` | `{status: "blocked"}` |
| 2 | 非 in_progress 上报 | 任务 pending | TASK_STATE_INVALID |

#### 3.3.4 `claim_batch`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 批量认领 3 个 | `{project_id, role:"backend", count:3}` | `{claimed: [T1, T2, T3], failed: []}` |
| 2 | 部分成功（仅 2 个可用） | count=3, 但只有 2 个 pending | `{claimed: [T1, T2], failed: []}` |
| 3 | count 超限 | count=25 (>20) | INVALID_PARAMETER |

#### 3.3.5 `release_worker`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 释放有任务的 Worker | worker 有 current_task_id | `{status: "released"}`, 任务回 pending |
| 2 | 释放空闲 Worker | worker 无 current_task_id | `{status: "released"}` |

### 3.4 验证者工具

#### 3.4.1 `get_verification_task`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 领取 submitted 任务 | 存在 submitted 任务 | `{task: {...}, validation_history: [...]}` |
| 2 | 无 submitted 任务 | 无 | NO_AVAILABLE_TASK |
| 3 | 自动设置为 verifying | 领取成功 | 任务状态变为 verifying |

#### 3.4.2 `submit_verification`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 通过验证 | `{passed: true}` | `{status: "ready_to_merge"}` |
| 2 | 验证不通过 | `{passed: false, notes:"代码质量问题"}` | `{status: "in_progress"}`, 通知原 Worker |
| 3 | 非 verifying 状态 | 任务 submitted（未被领取验证） | TASK_STATE_INVALID |

#### 3.4.3 `merge_task`

| # | 场景 | 输入 | 预期输出 |
|---|---|---|---|
| 1 | 合并成功 | ready_to_merge 任务，无冲突 | `{status: "done"}` |
| 2 | 合并冲突 | ready_to_merge 任务，有冲突 | `{status: "merge_conflicted"}` |
| 3 | 非 ready_to_merge | 任务 submitted | TASK_STATE_INVALID |

---

## 4. MCP Resource 测试场景

| # | Resource URI | 场景 | 预期 |
|---|---|---|---|
| 1 | `project://list` | 有 2 个项目 | JSON 数组，含 2 项 |
| 2 | `project://{id}` | 查询存在的项目 | 项目详情 + 进度统计 |
| 3 | `project://{id}` | 查询不存在的 ID | 错误响应 |
| 4 | `board://active` | 绑定项目有任务 | 看板摘要 (按状态计数) |
| 5 | `board://all` | 多项目 | 全局看板 |
| 6 | `task://{id}/context` | 任务有依赖+契约 | 完整上下文 (task + summaries + contracts) |
| 7 | `task://{id}/context` | 任务无依赖无契约 | 仅 task 本身 |
| 8 | `feature://{id}/summary` | Feature 有子任务 | 进度 (done/total) |

---

## 5. MCP Prompt 测试场景

| # | Prompt | 场景 | 预期 |
|---|---|---|---|
| 1 | `start-coordinator` | 无参数 | 返回协调者角色注入提示词 |
| 2 | `start-worker` | `{role: "backend"}` | 返回执行者提示词，含 backend 角色 |
| 3 | `start-worker` | `{role: "invalid"}` | 错误或忽略无效角色 |
| 4 | `start-verifier` | 无参数 | 返回验证者角色注入提示词 |

---

## 6. 实现方案

### 6.1 推荐方案：Go 集成测试 + mcp-go Client

使用 `mcp-go` 库提供的 client 端直接连接 MCP 服务器（stdio 或 in-process），避免依赖外部工具。

```go
package mcp_test

import (
    "context"
    "testing"

    "github.com/mark3labs/mcp-go/client"
    "github.com/mark3labs/mcp-go/mcp"
)

func TestMCPRegisterProject(t *testing.T) {
    // 1. 创建 in-process MCP client (stdio transport)
    c := client.NewStdioMCPClient("maestro", []string{"mcp", "--db", ":memory:", "--transport", "stdio"})

    // 2. 初始化连接
    ctx := context.Background()
    initResult, err := c.Initialize(ctx)
    if err != nil {
        t.Fatalf("Initialize failed: %v", err)
    }
    t.Logf("Server: %s v%s", initResult.ServerInfo.Name, initResult.ServerInfo.Version)

    // 3. 调用 register_project tool
    result, err := c.CallTool(ctx, "register_project", map[string]interface{}{
        "name":          "test-project",
        "workspace_path": "/tmp/test",
    })
    if err != nil {
        t.Fatalf("CallTool failed: %v", err)
    }

    // 4. 验证返回
    if len(result.Content) == 0 {
        t.Fatal("Expected content in result")
    }
    // 解析 JSON 验证 project_id 和 status
}
```

### 6.2 测试目录结构

```
tests/
  mcp/
    mcp_test.go           # 测试入口 + helper
    project_tools_test.go # register_project, list_projects
    coordinator_test.go   # create_feature, split_task, update_task, cancel_task, resolve_*
    worker_test.go        # get_next_task, submit_task_result, report_blocker, claim_batch
    verifier_test.go      # get_verification_task, submit_verification, merge_task
    resources_test.go     # 6 个 Resource 查询
    prompts_test.go       # 3 个 Prompt 注入
    testutil/
      fixture.go          # 测试固件：创建项目/Feature/任务的标准流程
      git_helpers.go      # Git 仓库辅助：创建临时仓库、提交变更
```

### 6.3 零信任验证测试策略

零信任验证是 MCP 层最关键的差异点。测试需要真实 Git 环境：

```go
func TestSubmitTaskResult_ZeroTrustValidation(t *testing.T) {
    // 1. 创建真实 Git 仓库
    repoDir := createTestGitRepo(t)
    // 2. 注册项目，workspace_path 指向该仓库
    // 3. split_task，配置 test_command
    // 4. get_next_task（自动创建 worktree）
    // 5. 在 worktree 中创建/修改文件
    // 6. submit_task_result
    // 7. 验证 validation_runs 包含：
    //    - changed_files（来自 git diff）
    //    - test_output（来自测试执行）
    //    - coverage（来自覆盖率文件解析）
}
```

**覆盖率格式支持矩阵（需逐一测试）：**

| 格式 | 测试文件内容 | 解析器 |
|---|---|---|
| go-cover | Go coverage profile | `go tool cover` 输出解析 |
| cobertura | XML 格式 | XML parser |
| jacoco | XML 格式 | XML parser |
| istanbul | JSON 格式 | JSON parser |

---

## 7. 测试优先级

### P0: 必须通过（阻塞发布）

| ID | 测试场景 | 原因 |
|---|---|---|
| T-01 | `register_project` 基本注册 | 项目创建是所有操作的前提 |
| T-02 | `split_task` 基本拆分 | 任务创建是核心流程 |
| T-03 | `get_next_task` 隐式注册 | MCP 核心差异：隐式 session/worker |
| T-04 | `get_next_task` 角色校验 | 安全边界 |
| T-05 | `submit_task_result` 零信任验证 | 零信任是项目核心价值 |
| T-06 | `submit_verification` 通过/拒绝 | 验证闭环 |
| T-07 | `merge_task` 成功/冲突 | 完整生命周期 |

### P1: 应该通过（影响质量）

| ID | 测试场景 |
|---|---|
| T-08 ~ T-14 | `split_task` 参数校验（7 个场景） |
| T-15 ~ T-19 | `update_task` 状态限制（5 个场景） |
| T-20 ~ T-24 | Resources 查询（主要场景） |
| T-25 ~ T-27 | Prompts 注入 |

### P2: 锦上添花

| ID | 测试场景 |
|---|---|
| T-28 | `claim_batch` 批量认领 |
| T-29 | `resolve_merge_conflict` followup 创建新任务 |
| T-30 | 覆盖率格式兼容性（4 种格式） |

---

## 8. 与 REST API 测试的映射

以下 MCP 场景已被 REST API E2E 测试覆盖（Service 层逻辑已验证），MCP 测试可跳过或简化：

| MCP 场景 | REST E2E 对应 | 覆盖文件 |
|---|---|---|
| `register_project` CRUD | `01-project.spec.ts` | 完整 |
| `create_feature` CRUD | `02-feature.spec.ts` | 完整 |
| `split_task` 创建 + 依赖 | `03-task-create.spec.ts` | 部分（不含 role 校验） |
| `cancel_task` | `05-task-lifecycle.spec.ts` | 完整 |
| `get_next_task` 基本领取 | `04-task-claim.spec.ts` | 部分（不含隐式注册） |
| 状态流转完整链路 | `05-task-lifecycle.spec.ts` | 完整 |
| 验证 + 合并 | `06-verification-merge.spec.ts` | 部分（REST 简化） |
| blocker/merge_conflict | `07-blocker-conflict.spec.ts` | 完整 |
| Session/Worker 管理 | `08-session-worker.spec.ts` | 完整 |

**MCP 测试应专注于 REST 无法覆盖的差异点**（见第 2.2 节）。

---

## 9. CI 集成建议

```yaml
# .github/workflows/mcp-test.yml
name: MCP Integration Tests
on: [push, pull_request]
jobs:
  mcp-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.22' }
      - name: Run REST E2E
        run: cd tests/e2e && npx playwright test
      - name: Run MCP Integration
        run: go test ./tests/mcp/ -v -count=1
```

---

## 附录 A: MCP Tool 完整参数参考

### register_project
```
必填: name (string), workspace_path (string)
可选: description (string), config (string, JSON object)
返回: {project_id, status}
```

### list_projects
```
可选: include_archived (boolean, default false)
返回: Project[]
```

### create_feature
```
必填: project_id, title, description
可选: reference_urls (string, JSON array)
返回: {feature_id, status}
```

### split_task
```
必填: project_id, feature_id, role (backend|frontend|devops|verifier),
      title, description, allowed_directories (JSON array)
可选: forbidden_patterns (JSON array), required_apis (JSON array),
      dependencies (JSON array), test_requirements (JSON object),
      priority (low|normal|high|urgent)
返回: {task_id, status}
```

### update_task
```
必填: project_id, task_id
可选: title, description, summary, allowed_directories,
      forbidden_patterns, required_apis, test_requirements
返回: {task_id, status}
状态限制: pending=全部, in_progress=description/summary, 其余=禁止
```

### cancel_task
```
必填: project_id, task_id, reason
返回: {task_id, status: "cancelled"}
```

### resolve_blocker
```
必填: project_id, task_id, resolution
可选: reassign (boolean, default false)
返回: {task_id, status, resolution}
```

### resolve_merge_conflict
```
必填: project_id, task_id, action (reopen|cancel|followup)
可选: reason
返回: {task_id, action, resolved}
```

### get_next_task
```
必填: project_id, role
可选: session_id (default auto), worker_id (default "default")
返回: {task + dependency_summaries + api_contracts + worktree_path}
```

### submit_task_result
```
必填: project_id, task_id
可选: session_id, worker_id, summary
返回: {task_id, status: "submitted", validation: "pending"}
```

### report_blocker
```
必填: project_id, task_id, reason
可选: session_id
返回: {task_id, status: "blocked"}
```

### claim_batch
```
必填: project_id, role, count (1-20)
可选: session_id, worker_id
返回: {claimed: [{task_id, worktree_path}], failed: [{task_id, error}]}
```

### release_worker
```
必填: project_id, session_id, worker_id
返回: {worker_id, status: "released"}
```

### get_verification_task
```
必填: project_id
可选: verifier_session_id, verifier_worker_id
返回: {task, validation_history}
```

### submit_verification
```
必填: project_id, task_id, passed (boolean)
可选: verifier_session_id, verifier_worker_id, notes
返回: {task_id, passed, status}
```

### merge_task
```
必填: project_id, task_id
可选: session_id (default "coordinator")
返回: {task_id, status: "done"}
```
