# 任务书 B2：控制台第二代（消费 E/G 的后端面，关闭 UI-5 与 UI-1 视图）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md` → 本任务书。**开工前提：任务书 E（auth/waivers/replay 端点）与任务书 G（pilot 端点）已合入 main。** 自包含。

## 0. 工作区准备

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-b2ui -b b2/m4-console-gen2 origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-b2ui
npm --prefix web ci
```

## 1. 使命与所有权

在 B 会话首代控制台之上：真实登录态替换 stub、HITL 队列接真数据、审计导出/SLO/DLQ/试点四个治理视图上线。**仅改 `web/src/**` 与 `tests/e2e/specs-m0/**`；禁改一切后端 Go。**

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `web/src/`（B 首代全部）+ `tests/e2e/specs-m0/console-*.spec.ts` | 现有结构、stub 位置（文件内逐处标注）、治理拓扑复用 |
| 2 | `docs/specs/openapi/control-plane.yaml` 的 auth/waivers/audit-export/slo/pilot 段 | E/G/#88 端点的精确契约 |
| 3 | `internal/webhook/store.go` 的 `ReplayApproval` | 重放表单语义：requested_by/approved_by 不得相同、理由≥16 字符 |
| 4 | `docs/prd/web-dashboard.md` + `plans/prep/m4/s6-console-ia-design.md` | 八场景 IA 与降级展示 |
| 5 | `plans/prep/m4/session-board.md` | 纪律 |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| B2-1 真实登录 | 去掉 /auth stub：走 E 的真实授权码流（e2e 拓扑已有 HTTPS IdP）；会话过期/撤销的 UI 降级 | 浏览器 e2e 真流 |
| B2-2 HITL 队列接真 | waivers GET 列表 + 既有三端点；审批人不可达时明示（职能角色遗留） | e2e |
| B2-3 治理视图 | 审计导出（链摘要/digest 呈现 + verify 动作反馈）、SLO 快照（可用性预算/目标态/告警着色）、DLQ 人工清单 + 带审批人身份的重放动作、试点 flags 视图（读为主） | e2e + 缺数据态诚实呈现 |
| B2-4 移除过渡桥 | v1/v3 双存储的手输项目 ID 桥：评估移除或保留，登记决策 | 决策记录 |

## 4. 文件边界

- **可改**：`web/src/**`、`tests/e2e/specs-m0/**`；需协调：`web/package.json`、`tests/e2e/playwright.config.ts`
- **禁改**：`internal/**`、`cmd/**`、`tests/eval/**`、`docs/specs/**`

## 5. DoD 与验收命令

```bash
npm --prefix web ci && npm --prefix web run build
make e2e && make build && make smoke
ruby scripts/test-hygiene-check.rb
```

## 6. 交接物

1. M4-UI-001 implemented 候选声明（二代范围：真登录 + 四治理视图）
2. 剩余 UI 缺口清单（如有）+ 过渡桥决策
3. 偏离项清单
