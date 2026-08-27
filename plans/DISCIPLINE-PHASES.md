# 纪律主轴 P1–P6 环节定义卡

> 适用于每个里程碑（M0 收尾、M1–M4）。规则：严格正向递进、环节间 fail-closed、产物逐环节传递。本文件是工作层定义；权威规则以 `docs/delivery/README.md`（统一 DoD、状态机）与 `docs/governance/traceability-guide.md`（追踪链）为准。

## 总则

1. **递进不可跳环**：P(n) 出口 Gate 未过不得进入 P(n+1)。唯一的灵活性是"无产出"环节的显式记录（如 P3 无 schema 变更时，必须写下"本阶段无数据模型影响"及理由，而不是跳过）。
2. **产物链**：P1 需求锚定卡 → P2 冻结契约 + 技术设计 → P3 schema/迁移/导入方案 → P4 代码 + 单元测试 → P5 测试 Evidence → P6 审计报告 + 状态翻转。每个环节的输入是上一环节的产出。
3. **责任角色**：每个环节指定主责会话/流与审批角色（映射治理签署矩阵：PRD=product+technical；安全=security+technical；Gate/测试=QA+technical；运维=operations+technical）。
4. **命令可执行**：所有出口 Gate 绑定本地可执行命令，禁止"人工看一眼就算过"。

## P1 文档规划（目标正确性：锁定"做什么/不做什么"）

| 项 | 内容 |
|---|---|
| 入口条件 | 上一里程碑对应任务书存在；本环节开始前列出受影响的全部领域文档与机器规范 |
| 执行内容 | ① 逐任务提取需求锚定卡；② 盘点文档缺口（draft/review 缺 approved 的清单）并给出推进 owner 与时限；③ 确认非目标边界，防止范围蔓延 |
| 核心产出 | ① **需求锚定卡**（每任务：Task ID、Requirement/Rule/Gate ID、验收标准、非目标、Test ID）；② 文档缺口清单与推进计划 |
| 出口 Gate | 任务书 `spec_status ≥ review` 且缺口文档有 owner 与时限；锚定卡全部 ID 可在权威文本解析：`ruby scripts/docs-check.rb` 通过 |
| 防偏离检查 | 锚定卡只引用权威文本不得改写目标；目标/验收标准变更必须走文档 MR；`docs-check.rb` 的 ID 解析与断链校验兜底 |
| 主责/审批 | 集成会话 I(n) 主导；product_owner + technical_lead 签署锚定卡 |

## P2 实现方案（方案正确性：锁定"怎么做"）

| 项 | 内容 |
|---|---|
| 入口条件 | P1 出口通过 |
| 执行内容 | ① 技术设计：模块边界、时序、故障与恢复路径（落 `docs/technical/` 对应文档或 ADR）；② 冻结契约：store 接口、OpenAPI、`docs/specs/mcp/tools.schema.json`、AsyncAPI 事件目录、咽喉点文件变更；③ 明确各流文件边界 |
| 核心产出 | 技术设计文档修订 + **契约 PR**（机器规范与咽喉点代码一次合入）+ 各流文件边界表 |
| 出口 Gate | 契约 PR 经 technical_lead 评审合入；设计覆盖全部锚定卡任务、无未解释偏差；`ruby scripts/spec-consistency-check.rb`、`node scripts/asyncapi-check.mjs`、`node scripts/mermaid-check.mjs` 通过 |
| 防偏离检查 | 契约变更唯一入口 = 集成会话；流内发现契约问题只能登记变更请求；设计偏离锚定卡必须显式记录理由并回溯 P1 |
| 主责/审批 | 集成会话 I(n) 主导；technical_lead 审批（安全相关加 security_owner） |

## P3 数据模型建设（数据正确性：锁定"存什么"）

| 项 | 内容 |
|---|---|
| 入口条件 | P2 契约冻结 |
| 执行内容 | ① schema 迁移设计（expand/migrate/contract）；② SQLite→PostgreSQL 导入映射（如适用）；③ 触发器/不变量/迁移锁方案；④ 无 schema 变更时显式记录 |
| 核心产出 | 迁移 DDL（评审版）、导入映射表、回滚方案、（或）"无数据模型影响"声明 |
| 出口 Gate | 迁移在本地 Compose PostgreSQL 上演练通过（含前向迁移、导入 dry-run、回滚）；schema 评审通过；`ruby scripts/schema-check.rb`、`node scripts/asyncapi-check.mjs` 通过 |
| 防偏离检查 | DDL 单 owner 串行合入（治理规则）；每个新表/列必须能追溯到锚定卡 Requirement；禁止"先写代码再补表" |
| 主责/审批 | S1（数据与平台流）主责；technical_lead 审批 |

## P4 代码工程建设（实现完整性）

| 项 | 内容 |
|---|---|
| 入口条件 | P3 出口通过；契约未漂移 |
| 执行内容 | 各流按文件边界并行实现 + 单元测试 + 流内自测集成；下阶段的 P4 提前量只允许在预备分支 |
| 核心产出 | 流内代码 + 单元测试 + PR（含变更说明、Test ID 引用、交接物报告） |
| 出口 Gate | 每流：`make build`、`make test`、`make vet`、`make lint` 全绿；`ruby scripts/test-hygiene-check.rb` 通过；无咽喉点文件变更；代码评审通过；grep 无 TODO/FIXME/stub/fail-open 残留（有意的 disabled 桩必须带审计写入且有任务书依据） |
| 防偏离检查 | 文件边界表强制（brief 第 4 节"可改/需协调/禁改"）；实现必须引用锚定卡 Test ID；隐藏旁路、宽松回退、通配权限、裸主机执行、token 透传一律禁止 |
| 主责/审批 | 各流会话主责；流内互审 + 集成会话合流评审 |

## P5 测试验证（行为正确性）

| 项 | 内容 |
|---|---|
| 入口条件 | P4 各流合入集成分支 |
| 执行内容 | ① 单元/集成/协议/安全/恢复测试补全；② 跨流联调剧本执行（见对应收敛手册）；③ CI 工作流扩展（service 容器等）；④ 目标提交上远程 CI |
| 核心产出 | 测试代码 + 联调剧本执行记录 + 远程 CI Evidence（artifact 绑定 SHA） |
| 出口 Gate | 本地 `make release` + `make e2e` 全绿；联调剧本逐条通过；远程 CI 三个工作流在目标 SHA 全绿且 artifact 存在；MCP 协议行为用真实 MCP 测试（禁止 REST 等价替代） |
| 防偏离检查 | Evidence 绑 commit/config/policy/test profile，重跑不覆盖；`missing/skipped/error/stale/unverified` 不算通过；测试断言精确（状态码+错误码），禁止双态可过 |
| 主责/审批 | 集成会话 I(n) 主导；qa_owner 签署（安全类加 security_owner） |

## P6 质量工程（交付可信性）

| 项 | 内容 |
|---|---|
| 入口条件 | P5 Evidence 齐备 |
| 执行内容 | 按 [QUALITY-AUDIT.md](QUALITY-AUDIT.md) 执行收敛点审计 → 补丁冲刺 → 状态翻转 → 签署 → 复盘（即"收敛仪式六步"） |
| 核心产出 | 审计报告、补丁记录、traceability 该阶段行翻转（`Verified Commit` 非空）、`last_verified_commit` 绑定、角色签署、阶段复盘文档 |
| 出口 Gate | 审计零 Critical/High 遗留；矩阵该阶段行 `implemented + passed`；`ruby scripts/docs-check.rb` 对 passed 行的 git 绑定校验通过；规定角色签署完成 |
| 防偏离检查 | 统一 DoD（delivery README 第 10 章）逐项核对；红线核对表逐条过；状态翻转只能发生在 Evidence 真实存在之后 |
| 主责/审批 | 集成会话 I(n) 主导；按签署矩阵：qa_owner + technical_lead（+security_owner/operations_owner 按范围） |

## 环节 × 既有治理机制对应表

| 环节 | 对应治理机制 |
|---|---|
| P1 | `docs/delivery/m*.md` 任务书、frontmatter 三状态、docs-check ID/断链校验 |
| P2 | ADR、`docs/technical/` 设计文档、`docs/specs/**` 机器规范、spec-consistency-check |
| P3 | `docs/technical/data-model.md`、schema-check、DDL 单 owner 串行规则 |
| P4 | 文件边界 + PR 流程、test-hygiene-check、Makefile 门禁 |
| P5 | `docs/testing/*`、CI 工作流（ci/docs/m0-runtime 及后续扩展）、Evidence 绑定 |
| P6 | `docs/governance/traceability-matrix.csv`、统一 DoD、角色签署矩阵、retrospective |
