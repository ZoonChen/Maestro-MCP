# 5. 项目结构

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 项目结构
> **相关文档:** [架构](architecture.md) | [部署方案](deployment.md)

---

## 目录结构

```
Maestro-MCP/
├── cmd/maestro/main.go              # 入口：serve / mcp / project 子命令
├── internal/
│   ├── config/config.go             # maestro.yaml 加载
│   ├── mcp/
│   │   ├── server.go                # MCP Server 注册 (mcp-go)
│   │   ├── transport.go             # stdio + SSE 双传输
│   │   ├── tools/                   # { project, feature, task, submit, blocker, verifier, merge }.go
│   │   ├── resources/               # { board, task_context }.go
│   │   └── prompts/                 # { coordinator, worker, verifier }.go
│   ├── handler/                     # HTTP: { feature, task, session, board, websocket }.go
│   ├── service/                     # 业务逻辑: { feature, task, session, context_filter,
│   │                                #   boundary_guard, test_runner, coverage_parser,
│   │                                #   contract_parser, worktree, merge }.go
│   ├── store/                       # 数据层: { sqlite, project, feature, task, session, worker,
│   │                                #   worktree, result, contract, activity, audit }_store.go
│   ├── model/model.go
│   └── ws/hub.go
├── web/                             # Preact + Vite → go:embed
│   └── src/components/              # { Board, TaskCard, SessionList, ActivityLog }.tsx
├── data/                            # SQLite (gitignore)
├── Dockerfile / docker-compose.yaml / maestro.yaml / Makefile
```

## 分层职责

| 层 | 目录 | 职责 |
|---|---|---|
| **入口层** | `cmd/maestro/` | CLI 命令解析、子命令路由 |
| **协议层** | `internal/mcp/` | MCP 协议实现（Tools/Resources/Prompts 注册） |
| **接口层** | `internal/handler/` | REST API Handler、WebSocket Hub |
| **服务层** | `internal/service/` | 业务逻辑、状态机、校验（**唯一真源**） |
| **存储层** | `internal/store/` | 数据读写、SQL 操作（强制 project_id） |
| **模型层** | `internal/model/` | 数据结构定义 |
| **前端** | `web/` | Preact + Vite 构建的 Web 看板 |
