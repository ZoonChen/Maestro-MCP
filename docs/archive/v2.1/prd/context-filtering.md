# 3.3 M3: 动态上下文降噪

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 功能需求 > 动态上下文降噪
> **相关文档:** [任务管理](task-management.md) | [契约解析引擎](../technical/contract-engine.md)

---

## 降噪策略

Agent 领取任务时，Maestro 返回**最小必要上下文**，而非全量文档：

| 上下文类型 | 降噪规则 |
|---|---|
| **API 契约** | 仅含 `required_apis` 指定的接口（方法、路径、请求/响应 Schema），其余全部丢弃 |
| **文件树** | 仅列出 Worktree 中 `allowed_directories` 内的文件，跳过 node_modules、.git 等 |
| **依赖摘要** | 前置任务的输出仅返回 `summary` 字段，不返回全量变更文件列表 |

## 契约源 Provider

| Provider | 格式 | 支持阶段 |
|---|---|---|
| `openapi` | OpenAPI 3.x YAML/JSON | v0.2（首版） |
| `manual_json` | 手动录入的 JSON 契约 | v0.2 |
| `graphql` | GraphQL Schema | 规划中 |
| `protobuf` | Proto 文件 | 规划中 |

未配置契约源的项目，上下文降级为纯 description + allowed_directories + 文件列表。

### manual_json 契约格式

手动录入的 JSON 契约必须遵循以下最小 schema：

```json
[
  {
    "method": "GET",
    "path": "/api/v1/orders",
    "description": "获取订单列表",
    "request_schema": {
      "type": "object",
      "properties": {
        "page": { "type": "integer" },
        "size": { "type": "integer" }
      }
    },
    "response_schema": {
      "type": "object",
      "properties": {
        "data": { "type": "array" },
        "total": { "type": "integer" }
      }
    }
  }
]
```

**必填字段：**

| 字段 | 类型 | 说明 |
|---|---|---|
| `method` | string | HTTP 方法: GET / POST / PUT / DELETE / PATCH |
| `path` | string | API 路径，以 `/` 开头 |
| `description` | string | 接口描述 |

**可选字段：**

| 字段 | 类型 | 说明 |
|---|---|---|
| `request_schema` | object | 请求体 JSON Schema（可简化） |
| `response_schema` | object | 响应体 JSON Schema（可简化） |
| `tags` | string[] | 接口标签（如 `["user", "read"]`） |
| `examples` | object[] | 请求/响应示例 |
| `source` | string | 来源说明（如 "手动录入 by 张三"） |

`manual_json` 契约文件放在项目配置的 `contract_paths` 目录下，后缀为 `.json`。

## 数据源与降级

- 项目可配置 API 契约文件路径（如 OpenAPI 文档）
- Maestro 启动时解析契约文件，构建索引，领取任务时按需提取
- 契约文件变更时自动重新解析
- **降级策略**: 未配置契约文件时，`required_apis` 字段失效，上下文仅包含任务描述 + 目录边界 + 文件列表。不影响边界控制和测试验证

## 配置继承

优先级：`Task.test_requirements` > `Project.config` > `全局默认配置`。各字段的具体回退规则（含例外项如 `test_timeout` 不在 Task 级覆盖）详见 [边界控制与验证](validation.md) 的测试要求配置回退链表。

## get_next_task 返回上下文标准结构

Agent 调用 `get_next_task` 后，返回的上下文遵循以下标准结构：

```json
{
  "task": {
    "id": "T-00042",
    "title": "实现订单查询 API",
    "description": "...",
    "role": "backend",
    "priority": "normal",
    "allowed_directories": ["src/api/orders/"],
    "forbidden_patterns": ["*.md"],
    "test_requirements": {
      "command": "go test ./src/api/orders/... -coverprofile=coverage/cover.out",
      "coverage_format": "go-cover",
      "coverage_path": "coverage/cover.out",
      "min_coverage": 80.0
    },
    "dependencies": []
  },
  "workspace": {
    "root": "/path/to/project/.maestro/worktrees/T-00042",
    "allowed_directories": ["src/api/orders/"],
    "forbidden_patterns": ["*.md"]
  },
  "context": {
    "api_contracts": [
      { "method": "GET", "path": "/api/v1/orders", "request_schema": {}, "response_schema": {} }
    ],
    "related_files": [
      "src/api/orders/controller.go",
      "src/api/orders/model.go"
    ],
    "dependency_summaries": [
      { "task_id": "T-00001", "summary": "完成用户模型", "outputs": [...] }
    ]
  },
  "limits": {
    "max_related_files": 50,
    "max_dependency_summary_chars": 2000
  }
}
```

### 裁剪上限规则

| 维度 | 上限 | 超出处理 |
|---|---|---|
| `api_contracts` 数量 | 无硬限制 | 按 `required_apis` 精确匹配，不会超量 |
| `related_files` 数量 | 50 | 超出按修改时间排序，截断保留最近修改的 50 个 |
| `dependency_summaries` 单条长度 | 2000 字符 | 超出截断并追加 `[TRUNCATED]` |
| `related_files` 目录深度 | 不限制 | 但跳过 node_modules、.git、vendor 等 |
| 文件内容 | 不返回 | 仅返回路径列表，Agent 按需自行读取 |

### 降级处理

| 场景 | 处理方式 |
|---|---|
| `required_apis` 中的接口在契约索引中找不到 | 从 `context.api_contracts` 中排除该项，不报错 |
| 多个契约源对同一接口有不同定义 | 以 `openapi` > `manual_json` 的优先级选取 |
| 前置任务无 summary | `dependency_summaries` 中该项仅包含 `task_id` 和 `title` |
| 项目未配置契约源 | `api_contracts` 返回空数组，其他字段正常返回 |
| 前置任务无 outputs 字段 | `dependency_summaries` 中省略 outputs，仅包含 task_id、title、summary |
| 前置任务无 summary 也无 outputs | `dependency_summaries` 中仅包含 task_id 和 title |
