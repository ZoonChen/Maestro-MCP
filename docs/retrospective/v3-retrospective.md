# V3 收敛复盘（契约 / 跨仓集成 / Defect / Agent 修复闭环）

> 本目录不受权威文档模板/frontmatter 约束，作为 Evidence 记录载体。本复盘对应收敛点 V3（M3 Exit Gate）；仪式程序见 `plans/convergence/v3-defect-agent.md`，审计底账见 `plans/prep/m3/p6-convergence-audit.md`（三项审计执行日期 2026-09-05，PR #72 剧本合入后）。
>
> **诚实声明**：V3 收敛当时未按手册第 7 节产出本复盘文件（偏离登记见第 4 节）。本文于 2026-09-07 由文档对齐审计**重建**，全部内容取自 git 史实、收敛提交 `e355676` 的记录与上述审计底账；未补写任何当时不存在的运行结果。

## 1. 目标提交与 CI Evidence

- 收敛 PR：#73（合流提交 `566c0c2`）；状态翻转提交 `e355676`，`last_verified_commit: HEAD` 自引用绑定（M0/V1/V2 先例）。
- 翻转范围：m3 任务书 → `implemented / passed / HEAD`；矩阵 M3 六行（M3-CTR/INT/DEF/DSP/AGT/BUD-001）→ `implemented,passed,HEAD`；三份 M3 PRD（agent-remediation / defect-and-test-issues / end-to-end-workflows）`implementation_status` → `partial`——归一化/编排/生命周期面已实装并验证，数据集与工具缺口诚实保持 partial。
- Evidence（收敛提交记录）：P5 收敛剧本（`internal/m3drill`，V3 出口锚点端到端，提交 `b4538d1`）；三项阶段审计（预算对账 / Agent 轨迹抽查 / 红队覆盖核对）每条审计线映射到 `internal/m3drill/p5_test.go`、`internal/agent/agent_test.go`、`internal/budget/budget_test.go` 的真实断言，23 包全绿含 PG、lint 0。

## 2. 波次执行统计（W3：PR #63–#73，共 11 个 PR）

I3 P1 签署 #63 → I3 契约冻结 #64 → m3 schema（P3）#65 → CTR 契约引擎 #66 → 缺陷摄取归一 #67 → IntegrationRun #68 → 预算台账 #69 →（S3）GitLab 沙箱栈 #70 → Agent 修复编排 #71 → P5 收敛剧本 #72 → P6 收敛 #73。

## 3. 审计项处置（手册第 4 节）

| 审计线 | 处置 |
|---|---|
| 预算对账 | pre-call Gate（超限条目不落账）；按 provider 真实用量结算（usage 超预留 = 记账违规）；usage 缺失按预留上限扣 + 对账债务；PG 台账行锁单事务；停止边界穷举（预算/尝试/墙钟 × 四类 handoff）；release 永不冲销 spend |
| Agent 轨迹抽查 | 封闭状态机 11×11 边穷举；无 ground truth 不得声明修复（PARKS awaiting_human，Defect 保持未 resolved）；durable settle 版本守卫（幂等重设/陈旧冲突/未知 miss）；注入不扩权 |
| 红队覆盖核对 | secret 适配器自行掩码（AKIA→AKIA[REDACTED]）；契约引擎 fail-closed（畸形 JSON/YAML/非法路径/错误版本全拒，17 golden cases）；M2 已收敛面（授权四面、webhook 验签）经回归保持 |
| Finding 六类来源真实摄取 | 适配器归一 + fail-closed 测试（`TestAdaptersNormalizeAndFailClosed`）；正式红队注入集缺口见下 |
| fingerprint 稳定性 | 同根因 occurrence 聚合、跨运行不重复建单（指纹去重断言） |
| core-coverage 扩展 | contract/defect/budget 核心纳入门禁 |

**诚实缺口（登记不降级）**：正式红队注入数据集（M3-P1 测试输入准备项）未建设——当前覆盖为组件级断言；登记为 M4 P1 输入（m4 评测/红队四层 harness 正式执行）。

**审计捕获的真实缺口（PR #72 修复）**：冻结状态机的 `MRCreated → CIVerifying` 边无公共驱动方法，收敛剧本首跑即命中（durable settle 序列在 mr_created 断裂）；补 `MRTransitionStep` 公共步骤随剧本合入。

## 4. 过程偏离与教训

1. **手册第 5 节规定的 `docs/README.md` 4.2 表更新与第 7 节规定的本复盘文件在收敛时均未执行**（4.2 行与本文件由 2026-09-07 对齐审计补齐；与 V2 同型偏离，根因与预防措施见 v2 复盘第 4 节）。
2. **红队数据集缺口以登记而非降级方式处置**：组件级断言先行、正式数据集移交 M4 P1——符合"缺口诚实登记优于虚假通过"的纪律，未放宽任何 Gate。
3. **评审/门禁在合流前拦截三处实现缺陷**（PR #71 内修复后合入）：agent-run 测试夹具 ID 非法 hex、预算台账创建缺上限校验、parent defect 未播种——均为流内门禁捕获，未进入 main。

## 5. 角色签署

按 m3 书 frontmatter `approver_roles` 与手册第 6 节，签署以收口 PR #73 评审批准承载（V0/V1/V2 先例）：qa_owner（契约/集成/Defect 判定）、security_owner（Agent 边界与红队）、product_owner（责任任务可读性验收）、technical_lead（总签）。

## 6. 遗留登记

| 项 | 级别 | owner | 期限 | 补偿措施 |
|---|---|---|---|---|
| 正式红队注入数据集未建设 | High（阻断 M4-EVAL-001 出口） | qa_owner | M4 P1 | 组件级红队断言已就位（状态机注入免疫、secret 掩码、契约 fail-closed）；四层 harness 落地前不得宣称评测通过 |
| 三份 M3 PRD 数据集/工具缺口 | Medium | technical_lead | M4 | 面已实装验证，PRD 保持 partial 诚实状态 |

## 7. W4 开局清单输出（实际执行对照）

1. I4 契约冻结范围（控制台/评测/审计/SLO）→ 已执行（PR #75，eval/audit/SLO schemas）。
2. 四类演练排期 → 未启动（M4 P5 范围）。
3. 试点仓库清单（2–5 个）与影子运行计划 → 未启动（M4 P5 范围）。
4. M4 主轴进度（对照）：P1 文档签署 #74、P2 契约冻结 #75、P3 迁移 0012 #76、P4 首片（审计链 + 遥测存储）#77 已合入；P5/P6 待执行。
