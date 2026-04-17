# 6-7. 部署方案与风险缓解

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 部署方案与风险缓解
> **相关文档:** [架构](architecture.md) | [配置与部署 PRD](../prd/deployment.md) | [恢复与灾难处理](recovery.md)

---

## 6.1 Dockerfile

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

## 6.2 docker-compose.yaml

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

## 6.3 本地部署

```bash
# 下载单二进制即可
curl -L https://github.com/xxx/maestro-mcp/releases/latest/download/maestro -o maestro && chmod +x maestro
./maestro serve --config maestro.yaml          # 启动全部服务
./maestro mcp --transport stdio                # 仅 MCP stdio (Claude Code)
```

## 6.4 maestro.yaml 配置示例

```yaml
server: { host: "0.0.0.0", port: 8080, ws_port: 8080 }
mcp: { sse_port: 3000 }
storage: { type: "sqlite", path: "./data/maestro.db" }
validation: { default_min_coverage: 0, default_test_timeout: 120, default_test_command: "" }
agents: { heartbeat_timeout: 300, max_connections: 50 }
logging: { level: "info", format: "json" }

# 项目级配置 (projects.config，覆盖全局默认值)
projects:
  user-service:
    workspace_path: "~/projects/user-service"
    contract_paths: ["docs/openapi.yaml", "docs/api-contracts/"]
    contract_watch: true
    contract_provider: "openapi"              # openapi / manual_json
    default_test_command: "go test ./..."     # 覆盖全局 default_test_command
    default_coverage_format: "go-cover"       # go-cover / cobertura / jacoco / istanbul
    default_coverage_path: "coverage/cover.out"
    default_min_coverage: 80                  # 覆盖全局 default_min_coverage (0)
    default_test_timeout: 180                 # 覆盖全局 default_test_timeout (120s)
    max_worktrees: 10                         # 项目最大并行 Worktree 数
    merge_target_branch: "main"               # 默认自动检测 main/master
    allowed_test_commands: ["go test ./...", "npm test"]  # 测试命令白名单，空=不限制
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
