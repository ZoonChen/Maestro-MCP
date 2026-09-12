# 任务书 W5：契约清理切片（P5b 复盘七项 + OPS-1 终态 + outbox 事件登记）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（P5b 收口节的 W5 立项）→ `deploy/gitlab/peixun/READINESS.md` §P5b 复盘引用（试点仓 ART-retrospective-001 的七项实单）→ 本任务书。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-w5 -b w5/contract-cleanup origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-w5 && make web-build
```

## 1. 使命

修平 PoC 首演暴露的契约毛刺，灰度期（阶段 3）前落定。七项实单来自 ART-retrospective-001（含复现路径），加上两项既有登记。

## 2. 切片

| # | 项 | 内容 | 验收 |
|---|---|---|---|
| W5-1 | 职能审批 MCP 通路 | `asset_review/asset_approve` 当前无职能主体可达的 MCP 调用面（首演走了 REST+授权基础）——补职能主体在 MCP 面的认证与授权通路 | 真 MCP 协议测试：职能主体 review/approve 走通 |
| W5-2 | 资产多签 Gate | release-note 类 Gate 需 product+technical 双签语义（现在单 approve 即过）——Gate 绑定多审批角色，全签才 approved | PG 门控：单签不翻、双签翻 |
| W5-3 | proposal 幂等键命名空间 | `decomposition_propose` 幂等键当前项目内唯一，跨项目撞键——改全局命名空间（含 project 前缀或全局表） | 复现撞键用例翻绿 |
| W5-4 | summary 参数口径 | summary 长度超限返回 500 应为 400（参数校验先行） | 单测 |
| W5-5 | locked_gate 下游等待面 | 制品 supersede 后下游 claim 阻断时的等待/通知面（控制台显示"等待哪版制品"） | 控制台字段 + 测试 |
| W5-6 | OPS-1 终态 | DLQ 重放端点权限从 `gitlab.reconcile` 换运维族权限（新增冻结串 `webhook.deadletter.replay`，运维职能持有） | RBAC+e2e 更新 |
| W5-7 | outbox 事件登记 | `workgraph.proposal.*`/`node.claimed`/`attempt.*`/`asset.*` 事件类型补进 `docs/specs/asyncapi/events.yaml`（J2a/J2b 先例欠账） | asyncapi-check + docs 四检 |

## 3. 边界与 DoD

可改：`internal/mcp/**`、`internal/workgraph/**`（口径修正不动协议语义）、`internal/handler/**`、`internal/store/**`（W5-3 如需）、`docs/specs/**`（events.yaml + RBAC）、e2e。禁改：web 结构（W5-5 仅增量字段）、矩阵。DoD：brief-E 全套 + `make e2e` + asyncapi 检查。

## 4. 交接物

七项+两项关闭声明（各附复现翻绿证据）；events.yaml 登记的事件目录清单；灰度期前的遗留项核对（应为零）。
