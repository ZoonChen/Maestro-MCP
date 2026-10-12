# W8 定向 claim 语义放宽：F58 关闭声明（2026-10-12，会话 W8）

> **定位**：brief-W8 交接物。F58（10-10 并行波实证）的关闭声明、前后对比与 spec 钉子记录。
> **一手证据**：`~/Works/yuandong/projects/peixun-s2b19/S2B19-STATE.md`（F58 阻塞现场：091 无主居队首 3h，三片 claim 409 空转）与 `peixun-s2b19/S2B19-FRICTION-REGISTER.md`。
> **服务版本**：本 PR 分支 `w8/targeted-claim-semantics`（基线 main=011c90f）。

## 1. 关闭声明与实现要点

**F58（高·结构）已关——显式定向即意图，定向领取不再要求队首位置。**

W7 把「防误领」（F29 本意：AI 拿错项零副作用拒绝）实现成了「队首串行调度」：带 `work_item_id` 定向时守卫先选队首，队首≠目标一律 409 `CLAIM_TARGET_MISMATCH`。串行切片下两者等价；10-10 并行切片/灰度多 Agent 下立即分化——**无主队首卡死所有定向领取**（运营解=先领队首让位级联，损失有效并行度，s2b19 三片 409 空转 3h 实录）。owner 裁决（10-12 随里程碑确认）：显式定向即意图。

| 切片 | 实现 | 测试锚点 |
|---|---|---|
| W8-1 定向直取 | `claimWorkItem` 的 targetWorkItemID ≠ "" 分支不再走队首选择，改为**直取目标项**（FOR UPDATE 行锁串行化同目标并发）：queued 态 + 无活跃租约（`idx_leases_one_active` 部分唯一索引为结构底线）+ runner 绑定其项目（**路由项目限定查找**——路由外工单按不存在处理，L4 资源隐藏）+ locked-gate 可消费（沿用队首选择的同一可消费性谓词）→ 租约/execution/queue token 同事务推进。**不定向路径逐字未动**（队首 + 优先级 + band 内 FIFO + release 回队首） | `TestW7FrictionTargetedClaimAndRelease` 第一子例（语义反转，见 §2）|
| W8-2 失败语义 | 四个拒绝面各自明确、**绝不回退领取其他项**：目标不存在/非 queued → **404 `TASK_NOT_FOUND`**（`ErrTaskNotFound`，publicerror 既有 wire 码）；存在活跃租约 → **409 `CLAIM_TARGET_HELD`**（新哨兵 `ErrClaimTargetHeld` + 携带型错误 `ClaimTargetHeld`，应答带 `held_by_runner`/`lease_id`/`lease_expires_at`——并行车道知道等谁，而非盲目重试）；gate 不可消费 → **409 `ASSET_GATE_NOT_SATISFIED`**（既有原因码复用；检查序=持有者先于状态、状态先于 token，保证并发败者的错误是最有信息量的那个）；queue token 过期 → **409 `CONCURRENCY_CONFLICT`**（定向面保持 queue CAS 契约；此负例为覆盖门画像核对补齐——W7 教训「新增分支须看画像」） | `TestW8TargetedClaimFailureSemantics` 四负例 |
| W8-3 测试翻新 | W7 第一子例语义反转 + 新增负例与并发争抢 | 全绿（含 `TestW8TargetedClaimConcurrentRace`） |
| W8-4 文档与钉子 | PRD §8 claim 语义段 + OpenAPI claim 操作/work_item_id 描述行；`MAESTRO-GUIDE.md` 查过**无队首纪律表述，无需勘误**（试点仓 backend/MAESTRO-GUIDE.md，grep 队首/queue head/CLAIM_TARGET 零命中） | docs 四检 PASS |

**边界遵守**：无迁移（纯逻辑变更）；不定向领取语义、租约/状态机结构、RBAC 零新串（claim 面 `work_item.claim` 不变）；`ErrClaimTargetMismatch` 哨兵随语义退役删除（全仓无残留引用）。

## 2. F58 场景前后对比（DoD 验收）

场景（s2b19 实录形态）：无主队首 091 在队，会话被指派其后方的 094，定向领取 094。

| 面 | 旧语义（main=011c90f 及之前，W7 守卫） | 新语义（本 PR） |
|---|---|---|
| 定向领 094 | **409 `CLAIM_TARGET_MISMATCH`**，094 领不到；三车道全部空转直到第五会话领走 091 让位（3h 实录） | **200 直取 094**（execution/租约落在 094），**091 状态不受扰**（仍 queued、零租约副作用、queue token 恰好 +1）——`TestW7FrictionTargetedClaimAndRelease/targeted_claim_of_the_non-head_item_succeeds_(F58_reversal)` |
| 定向领不存在项 | 409 `CLAIM_TARGET_MISMATCH`（错误码语义失真：目标根本不在队） | **404 `TASK_NOT_FOUND`**，零副作用、token 不动 |
| 定向领他 runner 执行中项 | 409 `CLAIM_TARGET_MISMATCH`（不告知持有者） | **409 `CLAIM_TARGET_HELD`** 携 `held_by_runner`/`lease_id`/`lease_expires_at` |
| 定向领 gate 不可消费项 | 409 `CLAIM_TARGET_MISMATCH` | **409 `ASSET_GATE_NOT_SATISFIED`**，不跳到下一项（定向即指定）；不定向面对同一项仍按既有语义**跳过**（负对照：available=false） |
| 定向领携过期 queue token | （守卫先拒，不触及） | **409 `CONCURRENCY_CONFLICT`**，token 不动（queue CAS 契约保持） |
| 不定向领取 | 队首 + FIFO + release 回队首 | **逐字不变**（同一测试函数第 4–5 子例回归：release 回队首 + 再 claim 首领即归还项） |
| 两 runner 并发定向同一项 | （W7 无此路径——守卫先行拒绝） | **一胜一败**：胜者 200，败者 409 `CLAIM_TARGET_HELD` 且 `held_by_runner`=胜者；`idx_leases_one_active` 实证仅一条活跃租约——`TestW8TargetedClaimConcurrentRace` |

## 3. 语义反转的测试 diff 说明（W8-3）

- `TestW7FrictionTargetedClaimAndRelease` 第一子例 `targeted claim of the non-head item is refused`（断言 409 + 零租约 + token 不动）**反转为** `targeted claim of the non-head item succeeds (F58 reversal)`（断言 200 + tail 转 executing + **head 不受扰** + token 恰 +1）。旧断言的「零副作用」意图由新负例继承（404/gate 两负例仍断言零租约、token 不动）。
- 函数级文档注释同步改写（W8 语义）；第 2–5 子例（队首定向受理 / release 权限面 / release 回队首+审计+outbox 同事务 / 归还项为下一派发）**原样保留全绿**——这四段就是「不定向零变更 + release 语义不动」的回归证明。
- 新增 `workflow_actions_w8_test.go`：三负例 + 并发争抢（第二 runner 由 W8 fixture 自行插入，W7 fixture 未动）。

## 4. spec 钉子变更记录

| 钉子 | 旧 | 新 | 备注 |
|---|---|---|---|
| OpenAPI 写操作数 | 38 | **38（不变）** | 无新路径/schema；claim 操作与 `work_item_id` 描述行改写（定向=直取 + 三拒绝面） |
| RBAC 权限数 | 68 | 68（不变） | claim 面仍 `work_item.claim` |
| MCP 工具目录 | 25 | 25（不变） | 无 MCP 面变更 |
| outbox 事件类型 | 23 | 23（不变） | 定向领取不新增事件（与不定向领取同路径） |
| 迁移 | 0023 | **无新迁移** | 纯逻辑变更（任务书预期内） |

## 5. 常驻栈与后续

合并后常驻栈重建（切片间隙执行，pg_dump 前置，`resident-server.sh` 配方**先迁移后换二进制**——本波预期 no-op 迁移）；窗口登记 change-window.md。灰度并行度前提（F58 关闭）落定，W5 周报后的出口评估不再受队首死锁拖累。
