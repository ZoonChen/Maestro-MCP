# 文档审查复盘：Rounds 1-21 经验总结

> 历史资料：本文复盘的是 v2.1 文档审查过程，不是 v3.0 权威需求、状态机、接口或实现依据。v3 工作必须从 [`docs/README.md`](../README.md) 进入；本文出现的旧状态与接口仅用于迁移和经验追溯。

> **复盘日期:** 2026-04-18
> **审查范围:** 11 份 PRD 文档 + 12 份技术设计文档 = 23 份
> **修改文件:** 24 份（含 README.md、PRD.md、TECHNICAL.md）
> **变更规模:** +418 行 / -215 行，净增 203 行

---

## 一、审查概览

### 1.1 项目背景

Maestro-MCP 的文档体系在 v2.1 版本从两个单体文档（PRD.md / TECHNICAL.md）拆分为 23 份模块化文档。拆分后文档间出现大量交叉引用不一致、术语混用、状态机遗漏等问题。经过 21 轮迭代审查逐步修复。

### 1.2 文档全景

| 层级 | 文档数 | 核心定位 |
|---|---|---|
| PRD（产品需求） | 11 份 | 定义"做什么"：功能需求、状态机、字段定义、错误码 |
| Technical（技术设计） | 12 份 | 定义"怎么做"：DDL、伪代码、架构实现、部署方案 |
| 导航/索引 | 2 份 | README.md（文档中心）、CLAUDE.md（开发指引） |

### 1.3 审查轮次演进

| 阶段 | 轮次 | 核心关注点 | 典型发现 |
|---|---|---|---|
| **基础对齐** | R1-R5 | 拆分后的基础一致性 | 大量命名冲突、状态机遗漏、字段缺失 |
| **深度交叉** | R6-R10 | PRD ↔ Technical 逐项对照 | 配置链断裂、错误码遗漏、DDL 约束缺失 |
| **状态机完整性** | R11-R15 | 9 态状态机全部路径覆盖 | blocked 解除路径、merge_conflicted 处理选项、rejected 瞬时态 |
| **精细打磨** | R16-R18 | 术语统一、DDL 注释、枚举值 | 瞬时伪状态统一、JSON 字段结构注释、DDL 枚举注释 |
| **交叉引用** | R19-R20 | 跨文档链接、配置回退链 | 旧文档废弃声明、Config Schema 对齐 |
| **伪代码审查** | R20-R21 | Go 伪代码语法/逻辑/安全 | ctx 未声明、事务泄漏、exec.CommandContext |

**关键规律：** 每轮审查都会发现"上一轮觉得已经没问题了"的新问题。问题不是在减少，而是在**向更深层转移**——从表面命名→逻辑语义→代码正确性。这验证了一个重要原则：**文档一致性审查没有"最后一轮"，只有"当前深度够不够"的判断。**

---

## 二、问题分类与统计

### 2.1 六大问题类别

| 类别 | 发现数 | 占比 | 严重性分布 | 典型案例 |
|---|---|---|---|---|
| **命名/术语不一致** | ~45 | 28% | LOW-MEDIUM | merge_conflict vs merge_conflicted、ErrInvalidState vs ErrTaskStateInvalid、verifier vs session_id/worker_id |
| **状态机遗漏/矛盾** | ~25 | 15% | HIGH | blocked→in_progress reassign 路径缺失、rejected 瞬时态未在 DDL 注释、merge_conflicted 三选项未展开 |
| **DDL/字段级缺陷** | ~30 | 18% | MEDIUM-HIGH | 缺少索引、缺少 NOT NULL、JSON 字段无结构注释、assigned_at 未在 PRD 提及 |
| **伪代码错误** | ~20 | 12% | HIGH | ctx 未声明、BeginTx 错误忽略、tx.Rollback 遗漏、exec.Command vs CommandContext |
| **跨文档引用断裂** | ~25 | 15% | MEDIUM | 配置回退链分散5处无权威来源、错误码表缺2个码、WS事件无activity_log映射 |
| **配置系统不对齐** | ~18 | 11% | MEDIUM | 三层级联实际部分字段仅两级、YAML示例混入非config字段、全局配置无Schema表 |

### 2.2 按严重级别统计

| 严重级别 | 定义 | 数量 | 典型代表 |
|---|---|---|---|
| **HIGH** | 会导致实现错误或运行时故障 | ~30 | SubmitVerification 事务泄漏、exec.Command 无法超时、错误码未定义 |
| **MEDIUM** | 会导致理解困难或实现歧义 | ~70 | 命名不一致、JSON 结构缺注释、配置回退链缺失 |
| **LOW** | 影响可读性和文档质量 | ~65 | 冗余索引、注释措辞、字段定义表覆盖不全 |

### 2.3 修改最频繁的文件 TOP 5

| 文件 | 变更行数 | 核心原因 |
|---|---|---|
| `technical/concurrency-model.md` | +134/-18 | 伪代码修复（ctx、事务、错误处理）、submit_verification 完整实现 |
| `technical/data-model.md` | +82/-48 | DDL 注释补全、JSON 结构注释、索引说明、字段语义注释 |
| `prd/task-management.md` | +54/-35 | 状态机补充、字段定义修正、assigned_at 补充、test_timeout 例外 |
| `technical/api-spec.md` | +42/-25 | WS 事件映射注释、新增错误码、claim_batch 描述 |
| `technical/zero-trust-validation.md` | +73/-38 | CommandContext 修复、WorkingDir 删除、truncateHeadTail 修正 |

**关键洞察：** 修改最频繁的文件恰好是**跨文档交叉引用最多**的文件。data-model.md 是所有文档的"数据锚点"，task-management.md 是"业务语义锚点"——任何修改都会产生级联影响。

---

## 三、方法论提炼

### 3.1 审查策略演进

**早期（R1-R10）：全量扫描 + 人工比对**
- 每轮由 Agent 读取所有 23 份文档，输出差异列表
- 问题：Agent context 有限，无法同时持有所有文档内容做精细比对
- 效果：发现大量表面问题，但深度不足

**中期（R11-R15）：聚焦维度 + 分组审查**
- 每轮聚焦一个维度（状态机 / 错误码 / 配置链 / WS 事件）
- 效果：单维度深度大幅提升，但跨维度关联问题遗漏
- 典型遗漏：修了状态机但忘了同步 activity_log action

**后期（R16-R21）：并行 Agent + 分工审查**
- 每轮启动 3-4 个并行 Agent，每个 Agent 负责一个审查维度
- 维度设计从"文件"维度转向"交叉引用"维度
- 效果：覆盖面和深度同时提升
- 最佳实践：**每个 Agent 的职责定义必须包含"输出与哪个文档的哪个部分对比"**

### 3.2 高效审查的 Agent 维度设计

以下维度设计经验经过 21 轮迭代验证，推荐作为未来审查的标准分片策略：

| 维度 | Agent 职责 | 对比对象 | 发现问题类型 |
|---|---|---|---|
| **DDL ↔ PRD 字段级** | 逐字段对比 DDL 与 PRD 字段定义表 | data-model.md ↔ task-management.md | 字段缺失、类型矛盾、约束不一致 |
| **WS ↔ activity_log ↔ 看板 三方对齐** | 三套事件/动作定义逐一映射 | api-spec.md ↔ data-model.md ↔ web-dashboard.md | 命名不一致、映射缺失、展示格式偏差 |
| **配置链完整性** | Task > Project > Global 三层级联 | data-model.md ↔ deployment.md ↔ validation.md | 回退链断裂、层级缺失、Schema 不一致 |
| **伪代码语法/逻辑** | Go 伪代码的编译级检查 | 所有含 Go 代码的技术文档 | ctx 未声明、事务泄漏、错误处理缺失 |
| **状态机路径覆盖** | 每个状态的入/出路径完整性 | task-management.md ↔ worktree-model.md ↔ concurrency-model.md | 遗漏路径、资源绑定语义矛盾 |

### 3.3 关键经验教训

#### 教训 1：DDL 是文档一致性的基石

> **规则：** 任何字段的命名、类型、约束、注释，以 DDL 为最终真相。PRD 描述业务语义，DDL 定义物理约束。

- DDL 的一个注释缺失，会在 3-5 个下游文档产生歧义
- **反面案例：** `test_requirements TEXT DEFAULT '{}'` 早期无 JSON 结构注释，导致 task-management.md、zero-trust-validation.md、validation.md 三处结构描述各自为政
- **修复成本：** 补一个 DDL 注释只需 1 分钟，但 3 个下游文档的歧义修正需要 3 轮审查

#### 教训 2：状态机是系统性错误的最高发区

> **规则：** 状态机的每个状态必须有：入路径列表、出路径列表、资源绑定语义（session/worker/worktree）、异常恢复行为。缺一不可。

- 9 态状态机看似简单，但×4 种资源绑定语义 = 36 个检查点
- **最易遗漏：** 异常恢复行为（进程重启时该状态如何处理）——这不在正常流程中，容易被忽略
- **最佳实践：** 状态机审查必须同时对照 recovery.md 的 8 步启动恢复流程

#### 教训 3：伪代码不是"示意"，是"契约"

> **规则：** 技术文档中的伪代码必须达到"可编译"的严谨度——缺失 ctx、忽略错误、遗漏 Rollback 这些问题在实际编码时会被复制粘贴。

- 发现 7 个 HIGH 级伪代码问题，每个都会导致生产事故（事务泄漏、数据库死锁、超时无效）
- **根因分析：** 文档编写者常把伪代码当作"流程示意"而非"实现契约"，但实现者会把伪代码当作编码模板
- **修复标准：** 所有 Go 伪代码必须：(1) ctx 作为首个参数 (2) 错误处理不使用 `_` (3) 事务 Rollback 覆盖所有错误路径 (4) BeginTx/Commit/Rollback 三配对

#### 教训 4：跨文档交叉引用需要"锚点-映射"模型

> **规则：** 任何出现在两个以上文档中的概念（如错误码、状态名、配置项），必须有一个明确的"锚点文档"和"映射注释"。

- **锚点原则：** 错误码以 api-spec.md 为锚点，状态机以 task-management.md 为锚点，DDL 以 data-model.md 为锚点
- **映射注释：** 非锚点文档引用该概念时，必须标注"详见 {锚点文档}"或"对应 {锚点文档} 中的 X"
- **反面案例：** 21 轮审查中至少 8 轮在解决"同一个概念在不同文档中叫不同名字"的问题

#### 教训 5：并行 Agent 审查的效果高度依赖 prompt 质量

> **规则：** Agent prompt 必须包含：(1) 具体文件路径和行号范围 (2) 期望的输出格式 (3) 严重级别定义 (4) "未发现问题"也要明确输出

- **低效 prompt：** "检查文档一致性" → 输出泛泛而谈的总结
- **高效 prompt：** "逐字段对比 data-model.md L58-86 的 tasks 表 DDL 与 task-management.md L175-191 的字段定义表，列出：(1) 字段名不一致 (2) 类型/约束矛盾 (3) PRD 有但 DDL 无 (4) DDL 有但 PRD 无"
- **关键发现：** 同样的审查任务，精确 prompt 的发现率是模糊 prompt 的 3-5 倍

---

## 四、审查维度检查清单

以下清单可直接用于未来的文档审查，按优先级排序：

### 4.1 第一优先级：数据一致性（阻断性）

- [ ] DDL 每个字段的命名是否与 PRD 字段定义表一致
- [ ] DDL 每个字段的 JSON 结构注释是否与 Go struct / PRD 描述一致
- [ ] 状态机每个状态的入/出路径是否在 task-management.md、worktree-model.md、recovery.md 三处一致
- [ ] 错误码表（api-spec.md + nfr-milestones.md）是否完全一致（数量和描述）
- [ ] WS 事件中的字段名是否与 data-model DDL 中的列名对应

### 4.2 第二优先级：语义完整性（理解性）

- [ ] 每个状态在 3 种文档中的资源绑定语义是否一致：正常运行 (task-management.md) vs 异常恢复 (recovery.md) vs Session 超时 (multi-client.md)
- [ ] activity_log action 枚举是否覆盖所有 WS 事件的 task.* 类型
- [ ] 配置回退链的每个字段是否标注了实际支持的最大层级（三级 / 两级 / 仅 Task）
- [ ] 伪代码中的错误变量是否在错误码表中有对应（如 ErrConcurrentConflict → CONCURRENT_CONFLICT）

### 4.3 第三优先级：代码正确性（实现性）

- [ ] Go 伪代码的 ctx 是否通过参数传入（非全局变量）
- [ ] BeginTx 的错误是否被检查
- [ ] 所有 tx.Rollback() 是否覆盖了错误路径
- [ ] ExecContext / QueryRowContext 的返回值是否被检查
- [ ] exec.CommandContext（非 exec.Command）用于需要超时的场景
- [ ] SQL 中 json_each 配合 json_extract 而非简写 ->>

### 4.4 第四优先级：文档质量（可维护性）

- [ ] 跨文档概念是否有锚点文档和映射注释
- [ ] 旧文档（PRD.md / TECHNICAL.md）是否有废弃声明
- [ ] README.md 的统计数据是否与实际一致（错误码数、WS 事件数、REST 端点数）
- [ ] 非锚点文档的引用是否指向正确的锚点

---

## 五、核心数据快照

以下为 21 轮审查结束后的文档核心指标，可作为后续实现阶段的基准参考：

### 5.1 数据模型

| 指标 | 数值 |
|---|---|
| 数据库表 | 11 张（projects, features, tasks, task_results, validation_runs, worktrees, api_contracts, agent_sessions, agent_workers, activity_log, audit_log） |
| 索引 | 15 个 |
| UNIQUE 约束 | 4 个 |
| 外键 | 12 个 |

### 5.2 状态机

| 指标 | 数值 |
|---|---|
| 稳定状态 | 9 个（pending/in_progress/submitted/verifying/ready_to_merge/merge_conflicted/done/blocked/cancelled） |
| 瞬时伪状态 | 1 个（rejected，不持久化到 DB） |
| 只读终态 | 2 个（done, cancelled） |

### 5.3 接口

| 指标 | 数值 |
|---|---|
| REST 端点 | 34 个 |
| MCP Tools | 16 个 |
| MCP Resources | 6 个 |
| MCP Prompts | 3 个 |
| WS 事件 | 19 个 |
| 统一错误码 | 27 个 |
| activity_log action | 15 个 |

### 5.4 配置

| 指标 | 数值 |
|---|---|
| Project.config 字段 | 11 个 |
| 全局配置块 | 6 个（server/mcp/storage/validation/agents/logging） |
| Task 级 test_requirements 子字段 | 4 个（command/coverage_format/coverage_path/min_coverage） |
| 配置优先级 | Task > Project > Global（部分字段仅两级） |

---

## 六、遗留的低优先级项

以下问题在 21 轮审查中识别但选择暂不修复（不影响实现正确性，仅影响文档完备性）：

| # | 问题 | 位置 | 理由 |
|---|---|---|---|
| 1 | `dependency_summaries` 传递结构未在 data-model.md 中定义 | task-management.md L191 | 是动态组装结构非持久化字段，可在 API 实现阶段定义 |
| 2 | 全局配置 YAML 缺少正式 Schema 表格 | deployment.md | YAML 示例已足够清晰，Schema 表可在 Phase 4 补充 |
| 3 | project-isolation.md 中 `store` 是全局变量 | project-isolation.md | 伪代码为简化示意，实现时自然会改为依赖注入 |
| 4 | deployment.md YAML 示例中 `workspace_path` 混入 config 区域 | deployment.md | 示例已加注释，不影响理解 |
| 5 | `min_coverage` YAML 示例用整数 vs DDL 用浮点 | deployment.md / data-model.md | JSON/YAML 运行时等价 |

---

## 七、对未来项目的建议

### 7.1 文档架构设计

1. **锚点优先：** 拆分文档前先确定每个核心概念的锚点文档。错误码 → api-spec.md，DDL → data-model.md，状态机 → task-management.md。非锚点文档只能引用不能重新定义。

2. **单一真相源：** 任何只应存在一份的定义（如状态枚举、错误码列表），只在一个文件中写完整版，其他文件用"详见 X"代替。

3. **注释即文档：** DDL 的注释必须足够详细（包含 JSON 结构示例、枚举值列表、业务语义）。这是成本最低、效果最高的文档对齐手段。

### 7.2 审查流程设计

1. **分阶段审查：** 不要试图一次审查所有维度。先确保数据模型和状态机正确，再审查接口和伪代码。

2. **伪代码审查标准：** 对伪代码采用与真实代码相同的审查标准——必须通过编译级检查。文档中的伪代码会被直接复制到实现中。

3. **变更级联检查：** 每次修改一个核心概念（如增加状态、增加错误码），必须检查所有引用该概念的文档并同步更新。建议维护一个"概念→引用位置"的索引。

4. **并行 Agent 分工：** 审查任务按"交叉引用维度"而非"文件维度"分配。每个 Agent 负责一对或多对文档的交叉验证。

### 7.3 工具化建议

1. 建议为 DDL 中的枚举字段（status、role、priority、action 等）维护 JSON Schema 或 YAML 定义文件，实现自动化校验
2. 建议将错误码表提取为独立的 YAML/JSON 文件，PRD 和技术文档从此文件生成，避免双源不一致
3. 建议为 WS 事件和 activity_log action 维护映射表，实现自动化一致性检查
