# M0 收尾执行计划（波次 W0 → 收敛点 V0）

> 主轴位置：M0 的 P1–P3 已在既有建设与文档中完成，本计划聚焦 **P4 修复 → P5 全量验证 → P6 Exit 翻转**。
> 权威依据：`docs/delivery/m0-foundation.md`（第 6 节"当前执行状态"、第 10 章 Exit Gate）。

## P1 文档规划（已完成，仅核对）

锚定卡 = m0-foundation.md 第 6 节"仍未关闭"列，无需重写：

| 任务 | 仍未关闭项（= 本阶段目标） | 明确非目标（划入后续） |
|---|---|---|
| M0-DOC-001 | 目标提交、Owner 审批和远程 CI Evidence | — |
| M0-BLD-001 | 目标提交远程 CI | 签名/provenance（发布环境） |
| M0-RUN-001 | —（本地候选完整） | v3 远程 MCP 身份上下文（M1） |
| M0-STATE-001 | —（本地候选完整） | PostgreSQL/Outbox/跨 Runner fencing（M1） |
| M0-VAL-001 | —（本地候选完整） | GitLab CI merge_gate 聚合（M2） |
| M0-SEC-001 | —（本地候选完整） | rootless OCI/设备身份（M1） |
| M0-TST-001 | 目标提交全量远程 CI 与规定角色签署 | — |

Exit Evidence 至少关联：`TC-MCP-001/003/004`、`TC-TASK-002/003/004`、`TC-VAL-001..005`、`TC-CTX-002/003`、`TC-DEP-001/002`。

## P2 实现方案（无新契约，仅提交编舞）

无接口/schema 变更。提交编舞（详细步骤见 [convergence/v0-m0-closure.md](../convergence/v0-m0-closure.md)）：

1. 工作区当前有大量未提交 v3 变更（v2.1 归档移动 + 代码 + CI + 本轮 plans/），先由 F0 完成修复再定稿。
2. 目标提交采用"单 PR 收口"：代码 + 测试 + 文档状态翻转 + 矩阵翻转在同一个合入 main 的提交里，`last_verified_commit` 指向该提交本身（自引用绑定，满足 `docs-check.rb` 的 `git diff --quiet` 校验）。

## P3 数据模型建设（显式记录）

**本阶段无数据模型变更**：schema 维持 v5（14 表 + 隔离触发器），不新增迁移。理由：M0 收尾只做修复与验证闭环。

## P4 代码工程建设（F0 修复会话）

来源：`docs/retrospective/codex-session-sync-2026-08-25_26.md` 前端 6 项风险 + 遗留物清理：

| # | 修复项 | 落点 | 验收 |
|---|---|---|---|
| 1 | Vite dev proxy Token 误发风险收紧（默认 127.0.0.1:8080 指向的注入策略） | `web/vite.config.js`、`tests/e2e/` 相关配置 | dev 代理仅对显式配置的后端地址注入 token；无明文 token 出现在产物 |
| 2 | ActivityLog / FeatureView 不随 WebSocket 事件刷新 | `web/src/`（App.jsx 事件分发） | `feature.created` 等事件后两组件刷新（复用既有 WS 测试模式） |
| 3 | TaskDetail `Promise.all` → `Promise.allSettled`（局部失败不白屏） | `web/src/components/TaskDetail.jsx` | 单请求 404 时仍渲染可用部分并显示 ErrorNotice |
| 4 | 未知 Task 状态静默消失 → 显式 unknown 徽标 | TaskBoard/TaskDetail 状态映射 | 未知状态渲染为 `unknown` 而非过滤掉 |
| 5 | a11y 基础缺口（aria-label、键盘可达、对比度低处） | `web/src/` 组件 | 关键交互元素可键盘操作、有可访问名 |
| 6 | 清理遗留 `playwright.real-world.config.ts`（指向已删除目录，防误用） | `tests/e2e/` | 目录中无指向不存在 testDir 的配置 |
| 7 | CLAUDE.md Current State 过时描述修正 + 管线入口 | `CLAUDE.md` | 与实际代码一致（cmd/maestro、transports、测试均已存在） |

明确非目标：写操作 UI（M4）、真实浏览器 DOM 测试套件（W1+ 增强）、Playwright 之外的框架引入。

流内门禁：`make build test vet lint` + `make e2e` 全绿。

## P5 测试验证（I0 收口会话）

1. **干净 clone 本地全量**：临时目录 `git clone` → `make release`（test/coverage/test-race/vet/lint/e2e/security-scan/docker-build/image-scan/sbom）→ `make smoke`（真实二进制集成测试）→ `make compose-up && make smoke && make compose-down`。
2. **远程 CI**：推 PR 触发 `ci.yml` + `docs.yml` + `m0-runtime.yml` 三工作流在 PR head SHA 全绿；确认 artifact `m0-runtime-<sha>`（binary + SBOM）生成。
3. 证据口径：`m0-runtime` artifact = M0 远程 CI Evidence；PR 链接与 SHA 记入收口报告。

## P6 质量工程（V0 收敛仪式）

按 [convergence/v0-m0-closure.md](../convergence/v0-m0-closure.md) 执行六步：合流 → 全量门禁 → 审计 → 补丁 → Exit 翻转 → 复盘。翻转范围：

- `docs/delivery/m0-foundation.md`：implementation_status `partial→implemented`、verification_status `unverified→passed`、`last_verified_commit=<目标提交>`
- `docs/README.md` 第 4.2/10 节的 M0 状态文字同步
- `docs/governance/traceability-matrix.csv`：M0 七行同步翻转，`Verified Commit` 列填目标提交
- 四角色签署（product_owner、technical_lead、qa_owner、security_owner）
- 复盘：`docs/retrospective/v0-closure-retrospective.md`

## 会话编排与估算

- **F0 修复会话**（1 个）：读 `plans/stages/m0-closure.md` P4 节执行修复，1–2 天
- **I0 收口会话**（1 个）：修复合入后启动，执行 P5/P6 与收敛手册，1–2 天
- 粗估总时长：2–4 天
