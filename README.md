# Maestro-MCP

**多 AI Agent 协作的本地任务调度与验证闭环总线 (Based on Model Context Protocol)**

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8.svg)](https://go.dev/)
[![MCP](https://img.shields.io/badge/Protocol-MCP-blue.svg)](https://modelcontextprotocol.io/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

## 为什么需要 Maestro

多 AI Agent（Claude Code、OpenClaw 等）并行开发同一项目时，传统"基于 Markdown 文件"的协作方式面临：

| 痛点 | 后果 |
|---|---|
| 上下文噪音 | Agent 读取全量 OpenAPI 文档，Token 浪费 + 幻觉 |
| 边界失控 | 后端 Agent "顺手"改前端代码，工作区冲突 |
| 验证缺失 | Agent 跳过测试就宣称完成 |
| 状态不透明 | 人类只能逐个终端查看进度 |

## Maestro 的解决方式

单 Go 二进制文件，通过 MCP 协议为 Agent 舰队提供：

- **动态上下文降噪** — 按任务裁剪 API 契约和代码路径，只下发最小必要上下文
- **零信任验证** — 不信任 Agent 汇报的结果。服务端主动执行 `git diff` + 测试命令 + 读取覆盖率文件
- **物理隔离** — 每个 Task 分配独立 Git Worktree，从文件系统层面杜绝并发冲突
- **四层项目隔离** — 连接绑定 → API 校验 → 业务逻辑 → 数据存储，防止 Agent "窜台"
- **Web 看板** — 实时展示多项目、多 Agent 的工作状态和任务流转

## 工作流

```
协调者                    Maestro                     执行者              验证者
  │                         │                           │                   │
  ├─ create_feature ──────► │                           │                   │
  ├─ split_task ──────────► │                           │                   │
  │                         │◄── get_next_task ─────────┤                   │
  │                         │─── 降噪上下文+Worktree ──►│                   │
  │                         │                           ├── 编码+测试 ──►   │
  │                         │◄── submit_task_result ────┤                   │
  │                         ├── git diff + 测试 + 覆盖率 │                   │
  │                         │─── submitted ◄────────────┤                   │
  │                         │◄── get_verification_task ─────────────────────┤
  │                         │─── TaskResult + Worktree ────────────────────►│
  │                         │◄── submit_verification ──────────────────────┤
  │                         │─── ready_to_merge ───────►│                   │
  │                         │◄── merge_task ───────────────────────────────┤
  │                         ├─── done ─────────────────►│                   │
```

## MCP 接口

Maestro 通过 MCP 协议暴露 Tools、Resources、Prompts 三类能力：

### Tools

| 角色 | 工具 | 说明 |
|---|---|---|
| 协调者 | `create_feature` | 创建 Feature（含标题、描述、关联文档 URL） |
| 协调者 | `split_task` | 拆分子任务（角色、边界、禁止模式、依赖、测试要求、优先级） |
| 协调者 | `update_task` / `cancel_task` | 修改或取消任务 |
| 协调者 | `resolve_blocker` / `resolve_merge_conflict` | 解除阻塞或处理合并冲突 |
| 执行者 | `get_next_task` | 领取任务（返回降噪上下文 + Worktree 路径） |
| 执行者 | `submit_task_result` | 声明完成（服务端自动取证，不接受 Agent 数据） |
| 执行者 | `report_blocker` | 上报阻塞 |
| 执行者 | `claim_batch` / `release_worker` | 批量认领 / 释放子 Worker |
| 验证者 | `get_verification_task` | 领取已提交任务（返回变更文件、测试结果、覆盖率） |
| 验证者 | `submit_verification` | 提交验证结果（通过/打回） |
| 验证者 | `merge_task` | 执行 git merge |

### Resources

| URI | 内容 |
|---|---|
| `project://list` | 所有项目及状态概览 |
| `project://{id}` | 单项目详情 |
| `board://active` | 当前项目看板 |
| `board://all` | 跨项目全局看板 |
| `task://{id}/context` | 任务纯净上下文 |
| `feature://{id}/summary` | Feature 级进度 |

### Prompts

| Prompt | 说明 |
|---|---|
| `start-coordinator` | 注入协调者角色 |
| `start-worker` | 注入执行者角色 |
| `start-verifier` | 注入验证者角色 |

## 多客户端支持

| 客户端 | 传输方式 | 配置 |
|---|---|---|
| Claude Code | stdio | `.claude/settings.json` 中配置命令 |
| OpenClaw | SSE | 配置 MCP Server URL `http://localhost:3000/sse` |
| 自定义 MCP 客户端 | SSE | 同上 |

## 快速开始

### 构建

```bash
make build
# 或直接:
go build -ldflags="-X main.Version=$(git describe --tags --always)" -o maestro ./cmd/maestro
```

### 启动服务

```bash
# 启动 HTTP + MCP SSE 服务
maestro serve

# 仅启动 MCP stdio 模式（Claude Code 用）
maestro mcp --transport stdio
```

### 注册项目

通过 MCP Tool（在 Claude Code 中）或 REST API 注册：

```bash
# Via REST API
curl -X POST http://localhost:8080/api/v1/projects \
  -H "Content-Type: application/json" \
  -d '{"name": "my-project", "workspace_path": "/path/to/project"}'

# Via MCP Tool (在 Claude Code 中自动调用)
# register_project(name="my-project", workspace_path="/path/to/project")
```

### 在 Claude Code 中配置

```json
{
  "mcpServers": {
    "maestro": {
      "command": "maestro",
      "args": ["mcp", "--transport", "stdio"]
    }
  }
}
```

## 技术栈

- **Go 1.22+** — 单二进制，零外部运行时依赖
- **SQLite** (modernc.org/sqlite) — 纯 Go 实现，无 CGO
- **MCP** ([mcp-go](https://github.com/mark3labs/mcp-go)) — 协议实现
- **Gin** — HTTP 框架（REST API + WebSocket + 静态资源）
- **Preact + Vite** — Web 看板，通过 `go:embed` 嵌入二进制

## 项目结构

```
cmd/maestro/main.go          # 入口: serve / mcp / version 子命令
internal/
  mcp/                        # MCP 协议层: tools, resources, prompts
  handler/                    # Gin HTTP handlers + WebSocket
  service/                    # 业务逻辑: 状态机, worktree, 测试, 边界校验
  store/                      # SQLite 数据访问（所有查询按 project_id 隔离）
  model/model.go              # 数据模型
  ws/hub.go                   # WebSocket 事件推送
web/                          # Preact + Vite 前端 → go:embed
docs/                         # 产品需求 + 技术设计文档
```

## 里程碑

| Phase | 版本 | 目标 |
|---|---|---|
| Phase 1 | v0.1 | 核心骨架 + 多项目基础。服务可启动，Task 状态机可运转，MCP 可连通 |
| Phase 2 | v0.2 | 验证闭环 + 上下文降噪。提交必须过测试，API 契约按需裁剪 |
| Phase 3 | v0.3 | Web 看板。实时多项目看板 + Agent 监控 + WebSocket 事件推送 |
| Phase 4 | v0.4 | 生产就绪。Docker 部署 + 认证 + 限流 + 日志配置 + 健康检查 |
| Phase 5 | v1.0 | 增强特性。跨项目依赖、任务模板、看板手动干预 |

## 文档

完整的产品需求和技术设计文档在 [`docs/`](docs/) 目录：

- **PRD:** `docs/prd/` — 11 个模块化文档
- **技术设计:** `docs/technical/` — 12 个模块化文档
- **导航索引:** [`docs/README.md`](docs/README.md)

## 贡献

欢迎提交 Issue 和 Pull Request。
