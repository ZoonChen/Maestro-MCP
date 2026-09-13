# 任务书 S2B：开发切片执行（真实业务工作在治理下流动，影子期核心）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/pilot/PLAYBOOK.md` 阶段 2 → `plans/prep/pilot/ROLE-CATALOG.md`（角色工作流）→ S2A 交接物（图就位）→ 本任务书。**前提：S2A 合入执行完毕**。本任务书是**可重复模板**：每个开发切片一个会话，按同构流程执行。

## 0. 工作区

每切片会话直接在常驻栈上工作（runner 模式，无需 Maestro worktree）：
```bash
# 领取（经 MCP claim 工具或控制台，认证主体=开发会话的 OIDC principal）
# worktree 由 Runner 在沙箱内创建（profile: maven-build / npm-build）
```

## 1. 使命

**真实开发 peixun 一期功能**，且每一步走 Maestro 治理面：领取→详设（制品）→实现（沙箱 profile 执行）→MR→Pipeline 证据→人工合并→done 确认。影子期语义：Maestro 记录不打断——若治理面出现摩擦（403/阻断/等待），如实登记不绕过，这正是影子期要采集的数据。

## 2. 切片执行流程（每个 WorkItem 同构，首切片清单见 §3）

| 步 | 动作 | 治理面 |
|---|---|---|
| 1 | claim WorkItem | lease/幂等；locked_gate 校验（无详设→阻断=S2B-详设先行） |
| 2 | 详设制品（detailed-design，接口/表结构/OpenAPI 增量） | asset register→technical_lead review→approve（W5-1 MCP 通路）；多签类按 W5-2 |
| 3 | 实现（沙箱 maven/npm profile 本地验证；本地 Evidence=diagnostic） | Command Profile 白名单出网 |
| 4 | MR（引用 BOM 编号与 WorkItem ID；GitLab 保护分支） | webhook→收件箱→投影（S2 已首演） |
| 5 | Pipeline 绿 → 合并 Gate 评估（merge_gate 权威） | 控制台 Gate 视图 |
| 6 | 人工合并（owner/Maintainer） | merged webhook→done |
| 7 | Jira 锚点状态同步（手工/镜像） | 对账零未决 |

## 3. 首切片清单（A1 域·组织与人员，建议 2 条并行）

`A1-1 多级组织架构`（8人日）+ `A1-2 学员档案`（5人日）——BOM 首两条 P0，RuoYi 部门管理打底（参考设计来源列）。完成后按同模板接力：A1-4/A1-6 → A2 课程域 → …

## 4. 边界与 DoD

- 可改：peixun 双仓业务代码、详设制品、Jira issue 状态
- 禁改：Maestro 仓库任何文件（摩擦走登记）；治理面拒绝不绕过不抱怨，记录后继续能做的部分
- DoD（每 WorkItem）：Pipeline 绿证据指针 + Gate 评估记录 + merged webhook 的 done 确认 + 摩擦登记（如有）

## 5. 交接物（每切片）

完成 WorkItem 清单与证据指针；摩擦登记（治理面每处阻断的复现路径——影子期最有价值的数据）；详设制品资产清单；下一切片建议。
