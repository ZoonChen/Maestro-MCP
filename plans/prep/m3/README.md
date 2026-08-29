# M3 预备工作包（S5 流，W1 后半提前量）

> 定位：本目录是**工作层预备产物**，不是权威真源。目标是把 M3 的 P1（需求锚定卡）与 P2/P3 的**提案输入**提前备好，供 I3 契约冻结 sprint 直接消费。任何规则冲突以 `docs/README.md` 权威顺序为准；权威 ID 均引用自 `docs/` 现行文本，本文不复制或改写规则正文。

## 文件清单

| 文件 | 环节 | 内容 |
|---|---|---|
| [p1-anchoring-cards.md](p1-anchoring-cards.md) | P1 | 六任务需求锚定卡（M3-CTR/INT/DEF/DSP/AGT/BUD）、文档缺口清单、测试输入准备清单 |
| [p2-contract-freeze-proposal.md](p2-contract-freeze-proposal.md) | P2 提案 | 契约变更请求清单（咽喉点只提案不改）、Agent 边界红线核对表、S5a/S5b 拆分 |
| [p3-data-model-design.md](p3-data-model-design.md) | P3 评审版 | 八张新表 DDL 草案、迁移/回滚方案、与 PG baseline 的衔接 |

## 边界声明

- 本包**不修改** `docs/specs/**`、`internal/handler/router.go`、`internal/store/interfaces.go`、`internal/model/model.go`、状态机——咽喉点变更只以提案形式登记（见 p2 文件），由 I3 契约 PR 落地。
- 本包不推进任何 Task 的 implemented/passed 状态；`M3-*` 六任务在追踪矩阵中保持 `not_started`。
- 基线：分支 `s5/m3-prep-contracts`，基于 `fe47859`（含 M1-P1/P2/P3 已提交链）。
