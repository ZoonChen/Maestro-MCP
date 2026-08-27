# 3.4 API 契约解析引擎

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 核心机制 > 契约解析引擎
> **相关文档:** [上下文降噪 PRD](../prd/context-filtering.md) | [数据模型](data-model.md)

---

## 解析流程

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

## 契约源 Provider

| Provider | 格式 | 支持阶段 |
|---|---|---|
| `openapi` | OpenAPI 3.x YAML/JSON | v0.2（首版） |
| `manual_json` | 手动录入的 JSON 契约 | v0.2 |
| `graphql` | GraphQL Schema | 规划中 |
| `protobuf` | Proto 文件 | 规划中 |

**Provider 优先级规则:** 当多种 provider 的契约存在重叠（相同 method + path）时，按优先级取用：`openapi` > `manual_json`。更高优先级的 provider 覆盖低优先级的同路径记录。

**manual_json 解析规格:**

```json
[
  {
    "method": "GET",
    "path": "/api/v1/users/{id}",
    "description": "获取用户信息",
    "request_schema": { ... },
    "response_schema": { ... }
  }
]
```

`manual_json` 的每个 endpoint 解析后写入 `api_contracts` 表，`source_file` 记录来源文件路径。

## 无契约降级

未配置 `contract_paths` 时，`required_apis` 失效，上下文降级为 `description` + `allowed_directories` + 文件列表，其他功能不受影响。

## 契约索引查询

`api_contracts` 表建表语句见 [数据模型](data-model.md)。查询接口按 `(project_id, method, path)` 唯一索引，毫秒级响应。

## 契约变更检测

- Maestro 启动时自动扫描并解析契约文件
- 支持 `contract_watch: true` 配置，文件变更时自动重新解析
- 解析失败时降级为纯 description 模式，不影响其他功能
