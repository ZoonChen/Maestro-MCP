# 1. 系统架构

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 系统架构
> **相关文档:** [数据模型](data-model.md) | [项目结构](project-structure.md) | [部署方案](deployment.md)

---

## 1.1 架构图

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

## 1.2 技术栈

| 组件 | 技术 | 说明 |
|---|---|---|
| **语言** | Go 1.22+ | 单二进制，零运行时依赖 |
| **MCP SDK** | `github.com/mark3labs/mcp-go` | Go 原生 MCP 实现，支持 stdio + SSE 双传输 |
| **HTTP 框架** | Gin | REST API + WebSocket + 静态文件托管 |
| **数据库** | SQLite (modernc.org/sqlite) | 纯 Go 实现，无 CGO 依赖，跨平台编译 |
| **前端** | Preact + Vite | 构建后通过 `go:embed` 嵌入二进制 |
| **Git 操作** | `go-git` 或命令行 git | Worktree 管理、diff 比对 |
| **覆盖率解析** | Go 标准库 XML 解析 | 读取 Cobertura/gocover 格式 |

## 1.3 进程模型

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
