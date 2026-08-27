---
doc_id: REF-MCP-GO
spec_version: 3.0
spec_status: review
implementation_status: partial
verification_status: unverified
owner_role: technical_lead
approver_roles: [technical_lead]
introduced_in: M0
authority_for: []
related_adrs: [ADR-004]
related_specs: [../specs/mcp/tools.schema.json]
related_tests: [../testing/mcp-test-guide.md]
last_verified_commit: f24bdf7
---

# mcp-go API 参考速查

> 本文仅是锁定依赖 `github.com/mark3labs/mcp-go v0.48.0` 的 SDK 速查，不是 Maestro v3 的协议、认证或 Tool 语义权威文档。实现必须以 `docs/prd/mcp-protocol.md`、`docs/technical/api-spec.md` 和机器 Schema 为准；升级 mcp-go 后必须重新验证本文并更新 `last_verified_commit`。

> 调研日期: 2026-04-18 | 库: `github.com/mark3labs/mcp-go`

## 核心包导入
```go
import (
    "github.com/mark3labs/mcp-go/mcp"
    "github.com/mark3labs/mcp-go/server"
)
```

## Server 创建
```go
s := server.NewMCPServer("name", "version",
    server.WithToolCapabilities(true),
    server.WithResourceCapabilities(true, true),
    server.WithPromptCapabilities(true),
    server.WithHooks(hooks),
    server.WithToolHandlerMiddleware(middleware),
    server.WithToolFilter(filterFunc),
)
```

## 传输启动
```go
// Stdio (单连接，sessionID="stdio")
server.ServeStdio(s)

// Legacy SSE API reference only; Maestro v3 MUST NOT use this transport.
sse := server.NewSSEServer(s, server.WithSSEContextFunc(injectCtx))
sse.Start(":3000")

// Streamable HTTP
http := server.NewStreamableHTTPServer(s, server.WithHTTPContextFunc(injectCtx))
http.Start(":8080")
```

## Tool 定义 + Handler
```go
tool := mcp.NewTool("name",
    mcp.WithDescription("desc"),
    mcp.WithString("param", mcp.Required(), mcp.Description("...")),
)
s.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
    val, err := req.RequireString("param")
    if err != nil { return mcp.NewToolResultError(err.Error()), nil }
    return mcp.NewToolResultText("ok"), nil
    // 或: return mcp.NewToolResultJSON(data), nil
})
```

## Resource + Template
```go
s.AddResource(mcp.NewResource("static://uri", "Name",
    mcp.WithResourceDescription("..."), mcp.WithMIMEType("application/json"),
), staticHandler)

s.AddResourceTemplate(mcp.NewResourceTemplate("dynamic:///{id}", "Name",
    mcp.WithTemplateDescription("..."),
), func(ctx context.Context, req mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
    id, _ := req.Params.Arguments["id"].(string)
    return []mcp.ResourceContents{mcp.TextResourceContents{URI: req.Params.URI, MIMEType: "application/json", Text: data}}, nil
})
```

## Prompt
```go
s.AddPrompt(mcp.NewPrompt("name",
    mcp.WithArgument("arg1", mcp.RequiredArgument()),
), func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
    arg1 := req.Params.Arguments["arg1"]
    return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{{Role: mcp.RoleUser, Content: mcp.TextContent{Type:"text", Text: arg1}}}}, nil
})
```

## Session 上下文
```go
session := server.ClientSessionFromContext(ctx)
sessionID := session.SessionID()
mcpServer := server.ServerFromContext(ctx)
```

## Hooks (全局拦截)
```go
hooks := &server.Hooks{}
hooks.AddOnRegisterSession(func(ctx context.Context, s server.ClientSession) { ... })
hooks.AddAfterInitialize(func(ctx context.Context, id any, msg *mcp.InitializeRequest, result *mcp.InitializeResult) { ... })
hooks.AddBeforeCallTool(func(ctx context.Context, id any, msg *mcp.CallToolRequest) { ... })
hooks.AddAfterCallTool(func(ctx context.Context, id any, msg *mcp.CallToolRequest, result any) { ... })
```

## 关键注意事项
1. 工具业务错误: `return mcp.NewToolResultError("msg"), nil` (error 返回 nil)
2. 协议错误: `return nil, fmt.Errorf("msg")`
3. Stdio 只有单 session, sessionID 固定 "stdio"
4. Tool name 冲突会 panic
5. Resource Template 参数在 `req.Params.Arguments` (map[string]any)
