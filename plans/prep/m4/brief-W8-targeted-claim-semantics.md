# 任务书 W8：F58 定向 claim 语义放宽（显式定向即领——灰度前结构修复）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（一期 P0 里程碑段 + F58 事件）→ 本任务书。一手证据：`peixun-s2b19/S2B19-STATE.md`（F58 阻塞现场）与 `internal/handler/workflow_actions_w7_test.go`（现行语义测试）。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-w8 -b w8/targeted-claim-semantics origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-w8 && make web-build
```

咽喉点变更（store/handler/spec）——W8 本身就是契约 PR。

## 1. 使命与背景

F58（10-10 并行波实证）：W7 的定向 claim 守卫把「防误领」（F29 本意——AI 拿错项零副作用拒绝）实现成了「队首串行调度」——带 `work_item_id` 定向时守卫先选队首，队首≠目标一律 409 `CLAIM_TARGET_MISMATCH`。串行切片下两者等价；并行切片/灰度多 Agent 下立即分化：**无主队首会卡死所有定向领取**（10-10 三车道实证，运营解=先领队首让位级联，损失有效并行度）。

裁决（owner，10-12 随里程碑确认）：**显式定向即意图**——定向领取不再要求队首位置。

## 2. 切片

| # | 项 | 内容 | 验收 |
|---|---|---|---|
| W8-1 | 定向路径直取目标项 | `claimWorkItem` 的 targetWorkItemID ≠ "" 分支改为**直取该项**：`queued` 态 + 无活跃租约（部分唯一索引语义）+ runner 绑定其项目 + locked-gate 可消费（沿用队首选择的同一可消费性检查）→ 直接领取（租约/execution/queue token 同事务推进）。**不定向路径行为零变更**（队首+FIFO+release 回队首语义不动） | 复现 F58 场景：无主队首 091 在队时定向领 094 成功；091 状态不受扰 |
| W8-2 | 定向失败语义 | 目标不存在/非 queued → 404 类 `TASK_NOT_FOUND`；存在活跃租约 → 409 携当前持有信息；gate 不可消费 → 409 沿用既有原因（不跳到下一项——定向即指定） | 三负例各一测；错误码与 message 明确 |
| W8-3 | 测试翻新 | W7 的 `TestW7FrictionTargetedClaimAndRelease` 第一子例「targeted claim of the non-head item is refused」**语义反转**：非队首合格项定向领取成功。保留/新增：定向 executing 项拒绝、定向不存在 404、不定向仍队首、release 回队首不变、并发两 runner 定向同一项一胜一败（租约唯一索引实证） | 全绿 |
| W8-4 | 文档与钉子 | PRD `task-management.md` claim 语义段更新（定向=显式意图，无队首约束）；OpenAPI claim 描述行同步；`deploy/gitlab/peixun/MAESTRO-GUIDE.md` 若述及队首纪律则勘误 | docs 四检过 |

## 3. 边界与 DoD

可改：`internal/store/postgres_workitems.go`（claimWorkItem）、`internal/handler/workflow_actions_endpoint.go`（错误映射）、对应测试、`docs/specs/**` 描述、PRD。禁改：不定向领取语义、租约/状态机结构、RBAC。**无迁移预期**（纯逻辑变更；若实现中发现需要 schema 变更，停下登记）。

DoD：brief-E 全套 + **pg-store 覆盖门必查**（`MAESTRO_TEST_POSTGRES_DSN=…5435… go test -p 1 -covermode=atomic -coverpkg=./internal/store -coverprofile=… store/handler/identity/gitlab/jira/m2drill/m4drill/webhook` 后跑 `ruby scripts/core-coverage-check.rb`——W7 教训：门只算 12 白名单文件，新增分支须同时看画像）+ `make e2e`（chrome channel）。

## 4. 常驻栈与收尾

合并后常驻栈重建（**切片间隙**，pg_dump 前置，`resident-server.sh` 配方先迁移后换二进制——本波预期 no-op 迁移）；窗口登记 change-window.md。交接物：F58 场景前后对比（旧语义 409 复现 → 新语义成功）、语义反转的测试 diff 说明、W8 关闭声明。
