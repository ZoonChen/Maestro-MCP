# M3-P6 收敛审计记录（V3 仪式前置）

> 三项阶段特定审计的可执行化：每条审计线映射到 `internal/m3drill/p5_test.go`、`internal/agent/agent_test.go`、`internal/budget/budget_test.go` 中的真实断言（纯逻辑 + PG 门控行为），门禁 23 包全绿含 PG、lint 0。三项审计的执行日期：2026-09-05（PR #72 收敛剧本合入后）。

## 审计一：预算对账（记账 vs 真实 provider 用量）

| 审计线 | 证据 |
| --- | --- |
| 每次模型调用前预算 Gate（AGT-RULE-001） | `budget.ReserveCall` pre-call 拒绝：spent+reserved+asked 超过上限即 `ErrInsufficient`，条目不落账（`TestReserveCallGatesBeforeTheCall`） |
| 按 Provider usage 真记账（AGT-RULE-002） | `RecordUsage` 以 provider 自己的数值结算（release+spend 同事务逻辑）；usage 超预留 = `ErrUsageExceedsReservation` 记账违规 |
| usage 缺失按预留上限扣并转人工核对 | `RecordUsageMissing` 扣 ceiling + `OverdrawnCalls()` 标记对账债务（冻结规则"usage 缺失按预留上限扣并转人工核对"） |
| 持久台账同事务 | `postgres_budget.AppendEntry`：行锁下条目 + 累计 + 上限复查单事务；停止后拒绝一切（PG 门控 `TestBudgetLedgerLifecycle`） |
| 停止边界穷举 | `StopReasonIfExhausted`（预算上限/尝试上限/墙钟）+ 四 agent handoff 路径各自消费（`TestStopBoundary`、agent `TestHandoffsAreHonest`） |
| spend 只计真实用量 | 修正过的记账语义：release 永不冲销 spend（单测断言结算后 spent=420 reserved=0） |

## 审计二：Agent 轨迹抽查（工具面越界检测）

| 审计线 | 证据 |
| --- | --- |
| Agent 不得写 Workflow state（AGT-RULE-005） | 状态机为封闭纯转移表；非法边/自由文本状态结构性拒绝（`TestFrozenStateMachine` 全 11×11 边穷举） |
| 无任意命令串 | Ports 注入模式：Modify 走版本化 profile 端口；`ErrToolForbidden` 哨兵守卫冻结工具面（AGT-RULE-003） |
| 无 ground truth 不得声明修复（AGT-RULE-004） | `ErrNoGroundTruth` 哨兵；happy path 绿 CI 后 PARKS awaiting_human——defect 保持未 resolved（m3drill `TestAgentHonestyAnchors` 断言 `NotEqual("resolved")`） |
| resolved 属 Defect 生命周期非 Agent 状态 | Agent 状态枚举无 resolved（wire 一致）；m3drill 断言 agent 终态三选一 |
| 注入不扩权 | 枚举无自由文本状态；`TestAgentHonestyAnchors` 的注入纪律断言（CanTransition 拒绝） |
| 轨迹 durable | `postgres_agent.Settle` 版本守卫逐步落账（PG 门控 `TestAgentRunPersistence`：幂等重设/陈旧态冲突/未知 run miss） |

## 审计三：红队用例覆盖核对

| 覆盖项 | 证据 |
| --- | --- |
| 提示注入不能驱动状态机 | m3drill 注入纪律断言（终态不可离、非法边拒绝） |
| secret 适配器不泄漏凭据材料 | defect `TestAdaptersNormalizeAndFailClosed/secret`：适配器自行掩码凭据前缀（AKIA→AKIA[REDACTED]），漏的 producer 泄漏不过去 |
| 恶意契约注入 | CTR 引擎 fail-closed：畸形 JSON/YAML、非法路径、错误版本全拒绝（contract 包 17 golden cases + YAML fail-closed 测试） |
| 签名/权限面 | M2 已收敛面（授权四面、webhook 验签）经 m2drill P5 剧本回归保持 |
| 完整红队数据集 | **诚实缺口**：正式红队注入集（M3-P1 测试输入准备项）尚未建设——当前覆盖为组件级断言；登记为 M4 P1 输入（m4 评测/红队四层 harness 正式执行） |

## 审计期间发现并修复

**[真实缺口] 冻结机器的 MRCreated → CIVerifying 边无公共驱动**：`agent_runs` 状态机承载该边但编排器没有暴露驱动方法——收敛剧本首跑即命中（durable settle 序列在 mr_created 断裂）。修复：补 `MRTransitionStep` 公共步骤（PR #72 随剧本合入）。

## 收敛仪式（本 PR）

m3 任务书 implementation_status implemented / verification_status passed / last_verified_commit HEAD（自引用收口绑定，V0/V1/V2 先例）；矩阵 M3 六行（CTR/INT/DEF/DSP/AGT/BUD-001）→ implemented,passed,HEAD。
