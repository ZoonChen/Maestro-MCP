# Maestro-MCP 文档中心

> 版本: v2.1 | 更新日期: 2026-04-17

本文档中心包含 Maestro-MCP 的完整产品设计和技术设计文档，按模块拆分为独立文件，便于独立迭代和查阅。

---

## 产品需求文档 (PRD)

| 文档 | 内容 | 核心要点 |
|---|---|---|
| [项目概述](prd/overview.md) | 定位、核心问题、设计原则 | 零信任 Agent、物理隔离、双模部署 |
| [角色与场景](prd/roles-and-scenarios.md) | 用户角色、五大典型场景 | 协调者/执行者/验证者/人类开发者 |
| [M1: 多项目管理](prd/project-management.md) | 注册、绑定、生命周期、防窜台 | 四层隔离 (L1-L4)、审计告警 |
| [M2: Feature & Task 管理](prd/task-management.md) | 状态机、字段定义、依赖满足、权限矩阵 | 10 态状态机、require_state 策略 |
| [M3: 动态上下文降噪](prd/context-filtering.md) | 降噪策略、契约源 Provider、降级方案 | OpenAPI/manual_json、按需裁剪 |
| [M4: 边界控制与验证闭环](prd/validation.md) | 零信任原则、服务端验证流程、测试安全 | Git Diff 取证、测试执行安全边界 |
| [M5: MCP 协议层](prd/mcp-protocol.md) | Tools/Resources/Prompts 定义 | 协调者/执行者/验证者工具集 |
| [M6: Web 看板](prd/web-dashboard.md) | 页面布局、功能清单、人工运维规划 | Kanban 视图、WebSocket 实时推送 |
| [M7: 配置与部署](prd/deployment.md) | 部署模式、配置层次 | Docker 容器 / 本地单进程 |
| [M8: 多客户端支持](prd/multi-client.md) | Session+Worker 模型、并行级别 | 三层并行、隐式注册、生命周期 |
| [非功能需求与里程碑](prd/nfr-milestones.md) | NFR、Phase 1-5 里程碑、ID 规范、术语表 | 冷启动 < 500ms、27 统一错误码 |

---

## 技术设计文档

| 文档 | 内容 | 核心要点 |
|---|---|---|
| [系统架构](technical/architecture.md) | 架构图、技术栈、进程模型 | Go 单二进制、mcp-go + Gin + SQLite |
| [数据模型](technical/data-model.md) | ER 关系、11 张表 SQL 建表语句 | projects/tasks/worktrees/sessions/workers |
| [Service 层边界](technical/service-boundary.md) | 架构原则、请求流转规则 | Handler 禁止直接操作 Store |
| [项目访问隔离](technical/project-isolation.md) | 四层防御实现、代码示例 | BindProject、ProjectGuard、Store Scoping |
| [Worktree 模型](technical/worktree-model.md) | 生命周期、状态机、GC 策略 | allocated→active→submitted→merged→GC 删除行 |
| [零信任验证与测试安全](technical/zero-trust-validation.md) | 取证流程、TestRunner 实现、安全约束 | 6 步验证流程、进程树 kill |
| [契约解析引擎](technical/contract-engine.md) | 解析流程、Provider、无契约降级 | OpenAPI 解析→SQLite 索引→毫秒查询 |
| [Session+Worker 并发模型](technical/concurrency-model.md) | 数据模型、原子认领、隐式注册 | SQLite 事务、Serializable 隔离 |
| [恢复与灾难处理](technical/recovery.md) | 进程重启恢复、不一致状态处理 | 8 步启动恢复、9 种异常场景 |
| [接口规范](technical/api-spec.md) | REST API、MCP Tools/Resources/Prompts、错误码 | 34 REST 端点 + 19 WS 事件、27 统一错误码 |
| [项目结构](technical/project-structure.md) | 目录结构、分层职责 | cmd/mcp/handler/service/store 五层 |
| [部署方案与风险缓解](technical/deployment.md) | Dockerfile、docker-compose、配置示例、风险矩阵 | 8 项风险识别与缓解 |

---

## 文档交叉引用

PRD 文档与 Technical 文档之间存在以下主要对应关系：

| PRD 模块 | Technical 对应 |
|---|---|
| M1 多项目管理 | 项目访问隔离 (四层防御) |
| M2 Task 状态机 | 数据模型 (tasks 表)、并发模型 (原子认领) |
| M3 上下文降噪 | 契约解析引擎 |
| M4 边界控制与验证 | 零信任验证、Worktree 模型、测试安全 |
| M5 MCP 协议层 | 接口规范 |
| M6 Web 看板 | 接口规范 (Board 端点 + WebSocket 事件) |
| M7 配置与部署 | 部署方案、项目结构 |
| M8 多客户端支持 | Session+Worker 并发模型 |

---

## 旧文档

- `docs/PRD.md` — 合并前的单体 PRD 文档 (v2.1)
- `docs/TECHNICAL.md` — 合并前的单体技术文档 (v2.1)
