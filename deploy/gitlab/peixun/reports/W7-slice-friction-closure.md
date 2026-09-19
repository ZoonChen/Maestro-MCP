# W7 切片摩擦四修 + 随行两项：F29/F32/F15/F20(+F26/F22) 关闭声明（2026-09-18，会话 W7）

> **定位**：brief-W7 交接物。S2B3–S2B10 连续十条 done 期间累积的治理面摩擦中，两项以上实证或灰度硬前提的六项的关闭声明、复现前后对比与 spec 钉子变更记录。
> **输入**：切片工作区一手证据 `~/Works/yuandong/projects/peixun-s2b{3..11}/S2B*-FRICTION-REGISTER.md` 与同目录 evidence.json（只读）；任务书 `plans/prep/m4/brief-W7-slice-friction-fixes.md`。
> **服务版本**：本 PR 分支 `w7/slice-friction-fixes`（基线 main=0a440c7）。

## 1. 关闭声明与实现要点

| # | 摩擦（实证来源） | 关闭面 | 实现 | 测试锚点 |
|---|---|---|---|---|
| W7-1（F29，S2B8/S2B10 两例，B10-1 悬挂≈两天） | claim FIFO 派发与会话切片指派错位时无定向、无归还；唯一安全解=保留租约等 TTL | **已关（定向+归还双面）** | ① 定向守卫：claim body 可选 `work_item_id`，命中 ≠ 目标 → **409 CLAIM_TARGET_MISMATCH，任何租约副作用之前拒绝**（store 在头选 SELECT 后、INSERT 前判定 `ErrClaimTargetMismatch`）。定向是守卫不是插队：派发序不变，错领绝不静默发生。② 归还面：`POST …/executions/:id/release`（work_item.claim，与 claim 同授权面）——lease 持有代际校验后 execution→`released`、lease→`released`、工单 executing→**queued 队首插回**（新列 `queue_requeued_at`，同优先级带内最新归还排最前）、queue CAS token 递增；审计 `work_item.released` + outbox `work_item.state.changed`(executing→queued) **同事务**（recordGovernanceEvent 复用） | `TestW7FrictionTargetedClaimAndRelease`（B10-1 场景全链：非队首定向被拒且零副作用+token 不动 / 队首定向受理 / release 后工单 queued+审计+outbox+token 递增 / **再 claim 首领即归还项**；含 release 403 负例） |
| W7-2（F32，S2B9 两次受理错误 SHA：8 位截断 + S2B8 错分支 40 位） | complete 的 commit_sha 是四端点契约里唯一无服务端校验的标量 | **已关（双栅栏）** | ① 形态栅栏（handler）：40 位 hex 正则，截断 SHA 400 INVALID_PARAMETER；② 分支头栅栏（store，complete 事务内）：与该工单绑定 MR 投影的 `source_sha`（平台已知分支头）比对，不一致 400 COMMIT_SHA_MISMATCH、execution 不动。**无投影时仅形态栅栏**（无可矛盾物——如实注记，非静默放行：merged webhook/reconcile 的 SoR 语义不变） | `TestW7FrictionCommitSHAFences`（截断 8 位被拒 / 错分支 40 位被拒且 execution 保持 running / 与投影头一致的 40 位一次受理 → validating）；既有 `TestWorkflowActionsS2BEquivalence` 回归（无投影场景受理不变） |
| W7-3（F15，S2B3 起**每切片** psql 手工回填；灰度硬前提） | validation_runs 在常驻 PG 唯一写者是 legacy SQLite 导入；judgeBoundary 读最新非空 profile_ref | **已关（上报面）** | `POST …/work-items/:wid/validation-runs`（work_item.submit——提单包语义，冻结串复用）：**Idempotency-Key 头必填**，同键二次上报**坍缩回同一行**（200 replayed=true，新列 partial unique (project, work_item, idempotency_key)）；新键=下一 attempt（work_items 行锁串行化，无 attempt 竞态）；body 覆盖 profile_ref（必填）/base_commit/source_commit/changed_files/duration_ms/boundary_ok/test_ok/coverage_ok/result/producer | `TestW7FrictionValidationRunReporting`（无键 400 / 无写串 403 / 首报 201 attempt=1 / **同键异体 200 同行同 attempt** / 新键 201 attempt=2 / judgeBoundary 读到的 profile_ref 即上报值） |
| W7-4（F20，S2B4 确诊，F17/F26 族的投影面根因；切片 4 起悬置） | `UpsertJob` ON CONFLICT last-write-wins：延迟重放的陈旧 created/running 覆写终态 success → 终态-only 证据铸造器永不再触发 | **已关（单调守卫）** | DO UPDATE 加 WHERE：已投影终态（success/failed/canceled）**拒绝回退**到后到的非终态（陈旧重放坍缩为 no-op）；非终态间迁移与终态写入不变 | `TestW7JobProjectionTerminalIsMonotonic`（created→running→success 正常推进；**success 后注入 late pending+running 双事件，投影保持 success**）；`TestPipelineAndJobProjections`/`TestJobEvidenceDrivesGateEvaluation` 既有 webhook 用例回归绿 |
| W7-5（F26 随行，S2B10 实证：终态事件≈元组时刻→证据丢弃，二轮 retry 才落） | 终态 job 事件先于 reconcile 元组建立时 BranchTuple 不完整 → IngestJob 静默丢弃证据 | **已关（事件侧 deferral，非仅设计登记）** | `applyJob` 前置检查：**终态+门类 job+marker 分支（maestro/&lt;key&gt;/&lt;item&gt;）+元组不完整** → 返回 deferral 错误，复用 pipeline-deferral 同款 outbox 重试路径；reconcile 补齐元组后重放自动铸证，无需人工 job retry。**marker 条件把 deferral 限定在治理分支**——团队自然分支不进该路径、无 DLQ 噪音；outbox 重试无上限（dead-letter 仅显式），流程断裂时事件可持续可见 | `TestW7TerminalJobEvidenceDeferredUntilTuple`（终态事件到达时元组不完整 → 证据为 0 且事件停 `retry_wait`（**非丢弃非 delivered**）→ MR 补齐元组 → 重放后 unit 门 merge_gate 证据 passed，**零人工 retry**） |
| W7-6（F22 随行） | reconcile 对账键不对称：切片恒借道 002 映射面（006 首调 MAPPING_NOT_FOUND） | **文档化（未对称化，如实）** | 差异登记于 control-plane.yaml reconcile 端点 description：路由按 ROUTE 项目解析映射、MR 投影落映射项目——治理项目须借道映射项目调 reconcile（切片现行 workaround 的规格化）。对称化（BranchTuple 式分支契约解析）登记为后续设计，本波不改对账契约 | spec 文档面（reconcile description）+ 本表 |

**边界遵守**：release 是 executing→queued 的**新合法边**，已在 PRD 状态机（task-management.md §6 mermaid）登记——工作项状态枚举未变（queued/executing 本就在册）；RBAC **零新串**（release→work_item.claim、validation-runs→work_item.submit，均冻结串复用）；web 结构未改；`docs/governance/traceability-matrix.csv` 未动（V4 仪式）。

## 2. 复现前后对比（DoD 验收）

| 摩擦场景 | 修前（切片实录） | 修后（本 PR 测试） |
|---|---|---|
| 定向领取错领（B10-1 型） | claim 静默领到队首他项，租约悬挂至 24h TTL，开工推迟累计≈2 天 | **409 CLAIM_TARGET_MISMATCH，零租约副作用，queue token 不动**（TestW7FrictionTargetedClaimAndRelease 第 1 段） |
| 误领归还 | 无归还面（blocked/failed/cancelled 均终态） | release 202：工单回 queued **队首**、execution/lease=released、审计+outbox 同事务落链、**再 claim 首领即该工单**（第 2–4 段） |
| 截断 SHA（650f5ff9，S2B9 B5-2 实录） | 202 受理，靠 merged webhook 回填侥幸纠正 | **400 INVALID_PARAMETER**（形态栅栏） |
| 错分支 40 位 SHA（S2B8 B2-3 实录） | 202 受理，错误值停驻 work_items 上游面 | **400 COMMIT_SHA_MISMATCH，execution 保持 running**（分支头栅栏） |
| 正确全 SHA | 受理（依赖客户端自觉） | **一次受理 → validating**（回归不变） |
| boundary 门 profile_ref 回填（F15） | 每切片 psql 手工 INSERT（灰度 Agent 无法手工） | API 上报：首报 201 / 同键 200 同行 / 新键 attempt+1 / judgeBoundary 直接可读 |
| job 终态被乱序覆写（F20） | pipeline_jobs 冻结 pending/created，五门 pending，靠 admin retry 手工解 | late pending/running 落笔为 no-op，**投影保持 success**，证据链不断 |
| 终态事件竞速元组（F26） | 证据静默丢弃，二轮 retry 才落（S2B10 每事故损失一轮） | 事件停 retry_wait 待元组，元组补齐后**重放自动铸证**，零人工干预 |

## 3. spec 钉子变更记录

| 钉子 | 旧 | 新 | 备注 |
|---|---|---|---|
| OpenAPI 写操作数 | 36 | **38** | +executions/:id/release、work-items/:wid/validation-runs（两路径+四 schema：WorkflowReleaseRequest/ValidationRunReportRequest/Result；claim 增可选 work_item_id；complete.commit_sha 加 40-hex pattern+双栅栏语义）；spec-consistency-check 同步 |
| RBAC 权限数 | 68 | 68（不变） | 两新端点全部映射既有冻结串（work_item.claim / work_item.submit） |
| MCP 工具目录 | 3.4 | 3.4（不变） | 本波无 MCP 面变更 |
| outbox 事件类型 | 23（events.yaml） | 23（不变） | `work_item.state.changed` 在册，release 是其新生产者（payload from=executing/to=queued 本就在枚举内） |
| 审计动作 | — | +`work_item.released` / `validation_run.reported` | 经 OpenAPI x-maestro-audit-event 声明（审计动作目录即该面） |
| 迁移 | 0022 | **0023** | work_items.queue_requeued_at + executions 枚举 +released + validation_runs.idempotency_key（partial unique）；down 迁移含 released→interrupted 停靠 |

## 4. 切片手册接替指引（下一切片会话执行）

- **F15 手工回填退役**：S2B 模板的 boundary 回填步骤自下一切片起改为
  `POST /api/v3/projects/006…/work-items/<wid>/validation-runs`（头 `Idempotency-Key: <切片号>-<工单>-vr1`；body 以 judgeBoundary 所需 profile_ref 为必填）——psql 手工 INSERT 不再使用。试点仓 MAESTRO-GUIDE 增补由切片会话随切片 MR 带（本波不动 peixun 双仓）。
- **claim 语义升级**：会话按任务书指派领取时**一律带 work_item_id 定向**；错领即刻 `release`（reason ≥8 字符落审计），不再等待 TTL。
- **complete 纪律不变**：40 位全 SHA（服务端现已强制；截断/错分支会被 400 拒绝——被拒后核对 MR 投影头再重发）。

## 5. 常驻栈重建窗口（纪律，本会话未执行）

W7 合入后需重建常驻镜像（8080）：**只在切片间隙执行**（s2b11 已收官、下一切片未开时）；先 pg_dump（`scripts/pilot/pg-backup.sh` 语义），按 `deploy/gitlab/peixun/resident-server.sh` 配方**先迁移后换二进制**（0023 迁移随新镜像落地），`pilot-stack/change-window.md` 登记窗口。切片会话运行中禁动。

## 6. 残余项清单（不含本波，列明去向）

| 项 | 去向 |
|---|---|
| F21（merged webhook 缺 diff_refs，target_sha 靠 reconcile 补） | 平台侧缓解已由 W7-5 部分覆盖（终态证据不再因元组竞速丢失）；payload 补拉的根治方案留后续契约波 |
| F27（inbox 持久化缺位） | W5 裁决队列外，登记在案 |
| F34（详设 v1 池序误报） | 已在 S2B10 经 supersede 自纠，无需代码动作 |
| F22 对称化（本波仅文档化） | 后续设计：reconcile 走 BranchTuple 式分支契约解析（control-plane.yaml reconcile description 已注） |
| UpsertPipeline 同型 last-write-wins | 无事故实录（F20 证据全在 job 面）；如管线级投影出现同症再立项，本波按任务书范围只守 UpsertJob |
| F26 完整根治（元组建立时回放缓冲） | 本波交付事件侧 deferral（等价缓解，测试绿）；元组侧回放为结构性方案，如影子期再复现证据丢失再评估 |
