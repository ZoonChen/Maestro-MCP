# 任务书 J3：Jira 连接器（过程数据通路）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → `plans/prep/pilot/SOLUTION-BLUEPRINT.md` §1（SoR/同步语义/角色映射——本任务书的实现规范）→ `~/Works/yuandong/projects/mcp-atlassian`（README+docs，作为 Agent 侧工具面与协议参考，不 fork）→ 本任务书。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-j3jira -b j3/jira-connector origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-j3jira && make web-build
```

## 1. 使命

Maestro↔Jira 通路：WorkItem↔issue 锚定、单向镜像、对账 worker。**严格按蓝图 §1.2 同步语义**：Jira 状态禁止反向写 Maestro；分歧进对账清单。Agent 侧工具面由 mcp-atlassian 独立承载（部署约定写入交接物，不在本任务书实现）。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| J3-1 | 锚定存储：`jira_anchors` 表（迁移编号与 J1/J2a 协调）：work_item_id↔issue_key/project、锚定来源、镜像字段快照 | PG 门控 |
| J3-2 | Jira 客户端：Server/DC PAT（env 引用，同 Secret 纪律），JQL 查询与字段读写的最小面；**对不可达 fail-closed**（对账降级，不阻塞任务流） | 单测 + 沙箱模拟 |
| J3-3 | 镜像 worker：SoR→Jira 方向的标题/指派/迭代标签/状态标签镜像（Maestro 状态→Jira 标签，不改 Jira issue 状态字段）；issue 反向只读快照 | 双向语义测试（含禁止反向写的负测试） |
| J3-4 | 对账：字段分歧检测→对账清单→人工裁决后更新镜像；两周期未裁决升级事件 | PG 门控 |
| J3-5 | 控制台：锚点与对账清单视图（只读） | DOM 测试 |

## 3. 边界与 DoD

可改：`internal/jira/**`（新）、`internal/store/**`、`internal/handler/**`、`web/src/**`、配置/ schema。禁改：workgraph、矩阵。DoD：brief-E 全套 + `make e2e`。

## 4. 交接物

连接器 implemented 候选；mcp-atlassian 部署约定文档（PAT/范围/版本钉子）；内网 Jira Server 可用性实测结论（不可达则按蓝图回退：Jira Cloud 或手工锚定——**此项可在 P5a 实测**，本任务书以模拟沙箱交付）。
