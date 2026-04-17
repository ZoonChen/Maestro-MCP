# 3.0 Service 层边界

> **文档版本:** v2.1 | **更新日期:** 2026-04-17
> **所属:** 技术设计文档 > 核心机制 > Service 层边界
> **相关文档:** [项目隔离](project-isolation.md) | [项目结构](project-structure.md)

---

## 架构原则: Service 层为唯一真源

```
请求流转:
MCP Tool ──┐
REST API ──┼──► Service Layer ──► Store Layer ──► SQLite
WebSocket ──┘     (唯一状态机入口)   (强制 project_id)

规则:
- Handler / MCP Tool 禁止直接操作 Store
- 所有状态流转只能通过 Service 层统一方法
- Service 层负责: 状态校验、权限校验、审计日志、事件推送
```

**设计约束：**

1. **Handler 禁令**: MCP Tool Handler 和 REST API Handler **禁止**直接调用 Store 层
2. **唯一入口**: 所有状态变更只能通过 Service 层的统一方法
3. **Service 职责**: 状态校验 + 权限校验 + 审计日志 + 事件推送 + 调用 Store
4. **Store 职责**: 纯数据读写，所有查询强制携带 `project_id`

这一原则确保了：
- 项目隔离在 Service 层统一执行，不会因 Handler 遗漏校验而出现安全漏洞
- 审计日志在 Service 层统一记录，不会遗漏
- 状态机流转在 Service 层统一控制，不会出现非法状态转换
