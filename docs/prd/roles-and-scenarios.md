# 2. 用户角色与场景

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 产品需求文档 > 用户角色与场景
> **相关文档:** [项目概述](overview.md) | [多客户端支持](multi-client.md) | [任务管理](task-management.md) | [边界控制与验证](validation.md)

---

## 2.1 角色

| 角色 | 职责 | 典型代表 |
|---|---|---|
| **协调者 (Coordinator)** | 需求分析、任务拆分、阻塞解除、跨项目协调 | Claude Code（协调者模式） |
| **后端执行者 (Backend Worker)** | 执行后端任务，修改服务端代码 | Claude Code（后端角色） |
| **前端执行者 (Frontend Worker)** | 执行前端任务，修改客户端代码 | Claude Code（前端角色） |
| **验证者 (Verifier)** | 审核已提交任务的代码质量，决定合并或打回 | Claude Code（验证者角色） |
| **运维执行者 (DevOps Worker)** | 执行基础设施变更、CI/CD 配置、部署脚本等运维类任务 | Claude Code（devops 角色） |
| **人类开发者 (Human Developer)** | 通过 Web 看板监控全局进度，无直接操作任务权限 | 浏览器 |

## 2.2 典型场景

### 场景 A: 单项目多模块并行

同一项目中，后端 Agent 与前端 Agent 分别处理不同模块的任务，各自拥有独立的工作目录边界，互不干扰。

```
Project: user-service
├── Agent backend-01   → T-001: 用户注册 API (allowed: ["src/api/user/"])
├── Agent backend-02   → T-002: 订单查询 API (allowed: ["src/api/order/"])
└── Agent frontend-01  → T-003: 用户列表页   (allowed: ["web/src/pages/user/"])
```

### 场景 B: 同模块多实例并行

同一项目中，多个相同角色的 Agent 并行处理不同的后端任务，通过原子认领机制避免冲突。

```
Project: user-service
├── Agent backend-01  → T-005: 支付接口
└── Agent backend-02  → T-008: 订单查询
```

### 场景 C: 单实例子 Agent 并行

单个 Claude Code 终端中，父 Agent 派出多个子 Agent 并行领取不同任务，通过同一 MCP 连接协调工作。

```
Session: cc-backend-01 (capacity=5)
├── Worker: default → T-005: 支付接口
├── Worker: sub-1   → T-008: 订单查询
└── Worker: sub-2   → T-009: 退款接口
```

### 场景 D: 跨项目协调

一个协调者 Agent 管理多个相关微服务项目，查看全局进度。

```
协调者 Agent (全局视角)
├── 查看 user-service 的进度
├── 查看 order-service 的进度
└── 发现 order-service T-010 依赖 user-service T-001，记录人工跟进
```

**注:** 跨项目 Task 依赖关系（Phase 5 规划）当前版本不支持。协调者发现跨项目依赖时，需在任务描述中记录依赖信息，人工协调执行顺序。

### 场景 E: Monorepo 子项目

单一 Monorepo 注册为一个 Project，通过 Task 的目录边界实现子模块隔离。

```
Project: my-monorepo
├── T-001 (backend, allowed: ["services/user/"])
├── T-002 (frontend, allowed: ["apps/web/src/user/"])
├── T-003 (backend, allowed: ["services/order/"])
└── T-004 (frontend, allowed: ["apps/web/src/order/"])
```
