# 质量工程程序（P6 完整展开：查缺补漏与状态翻转）

> 适用：每个收敛点 V0–V4 的第六步（收敛仪式的 P6 环节）。执行者：集成会话 I(n)。目标：在状态翻转之前证明"交付可信"，任何未闭环项要么修复要么显式登记为带条件遗留。

## 1. 三层质量环

| 层 | 时点 | 口径 |
|---|---|---|
| 任务级 DoD | 每个任务 PR | `docs/delivery/README.md` 第 10 章统一 DoD（代码/迁移/配置/文档、机器规范与示例、按风险的单测/集成/安全/恢复测试、可观察审计点、追踪完整、目标环境运行、无 Required Gate 缺失） |
| 流内门禁 | 每流 P4 出口 | `make build test vet lint` + test-hygiene + 流专项（见各流 brief 第 5 节） |
| 收敛点审计 | 每 V 点 P6 | 本文件第 2–5 节 |

## 2. 收敛点审计清单（七类，逐条核对并留证）

### 2.1 目标对齐
- [ ] 本阶段全部锚定卡任务的验收标准逐条有 Evidence 指针
- [ ] 未立项工作（PR/分支/代码）无越界实现锚定卡之外的目标
- [ ] 非目标清单未被悄悄突破（回读任务书"非目标"段逐条核对）

### 2.2 契约一致
- [ ] 代码接口与 `docs/specs/**`（OpenAPI/tools.schema/events.yaml/permissions.yaml）逐字段一致
- [ ] 咽喉点文件变更全部发生在契约 PR 内（`git log --stat` 复核）
- [ ] 无未决契约变更请求

### 2.3 实现完整
- [ ] grep 无 TODO/FIXME/XXX/stub 残留；有意的 disabled 桩有任务书依据 + DENIED 审计 + 注释指向依据
- [ ] 无 fail-open 路径（ DrainGuard 类模式仅限无生命周期单测且有注释）
- [ ] 错误/取消/超时/恢复路径均有代码与测试（不只 happy path）

### 2.4 测试证据
- [ ] 本阶段 Test ID 全部执行且绑定目标提交；`missing/skipped/error/stale/unverified` 均视为未过
- [ ] MCP 协议行为用真实 MCP 测试；无 REST 等价替代（test-hygiene 兜底 + 人工抽查）
- [ ] 断言精确（状态码 + 错误码 + 关键字段），无双态可过

### 2.5 安全红线（见第 5 节核对表）
- [ ] 逐条过红线核对表，记录通过/例外（例外须 security_owner 签署）

### 2.6 可观测与审计
- [ ] 本阶段新增 SLI/告警/Runbook 已验证（delivery README 第 11 章要求）
- [ ] deny、身份/成员/Runner/配置变化、Lease 操作审计事件齐全

### 2.7 追踪闭环
- [ ] 矩阵本阶段行 16 列完整（Verified Commit 除外，翻转时填）
- [ ] frontmatter 三状态与矩阵行一致；`ruby scripts/docs-check.rb` 全绿

## 3. 偏离分级与处置

| 维度 | 定义 | 处置 |
|---|---|---|
| 目标偏离 | 实现与锚定卡目标/验收不符（做错方向、漏做、多做） | Critical：阻断收敛；High：本轮必须修复或回退范围；Medium/Low：登记遗留 |
| 契约偏离 | 代码与冻结契约不一致 | 一律 High 起步：要么代码回归契约，要么走契约变更流程（含 spec MR + 评审） |
| 实现偏离 | 符合目标与契约但违反工程红线/质量要求 | 按红线等级（见第 5 节） |

Severity 叠加规则：目标偏离 × Critical = 阶段回退到 P1 重锚定；其余进补丁冲刺。

## 4. 补丁冲刺流程

1. 审计输出问题清单（编号、类别、严重度、责任流、修复方案）
2. 责任流在集成分支修复（小修复直接改；大修复回流内分支走 PR）
3. 回归：受影响 Test ID 重跑 + 全量门禁（PIPELINE 各收敛手册第 3 节命令集）
4. 复审：集成会话复核关闭状态，更新审计报告
5. 无法当轮关闭的 Critical/High：**不得翻转状态**，收敛延期；Medium/Low 登记遗留（owner + 期限 + 补偿措施）

## 5. 安全红线核对表（CLAUDE.md 工程不变量逐条化）

| # | 红线 | 核对方式 |
|---|---|---|
| 1 | Default deny；身份/角色/项目/Session 来自服务端授权上下文 | 授权矩阵实测 + 代码走查无自报路径 |
| 2 | REST/MCP/WS/后台同一授权与审计策略 | 一致性测试（V1 剧本 2） |
| 3 | Control Plane 不挂载不读仓库源码 | 部署检查 + 静态断言 |
| 4 | 无任意命令串；任务引用版本化 Command Profiles | 工具面白名单审查 + profile 引用校验 |
| 5 | 缺失/跳过/无效/过期必需 Evidence 阻断推进 | Evidence 状态机负测试 |
| 6 | 本地 Runner Evidence 仅 diagnostic；GitLab CI 是权威 | TC-EVIDENCE-004 语义测试 |
| 7 | Evidence 绑 source SHA/target SHA/pipeline/job/policy 版本 | 字段完备性测试 |
| 8 | 不推不合并保护分支 | 推送边界实测 + merge API 调用审计为零 |
| 9 | `maestro/*` 分支仅 Runner host git broker 以 Keychain 凭据推送；中央 Bot 无源码推送 | 边界实测 |
| 10 | `done` 仅由 merged webhook 或对账确认 | 状态机入边审查 |
| 11 | 状态变更/审计事件/Outbox 写原子 | 同事务测试 |
| 12 | 每次 LLM 调用前查预算、调用后记真实用量 | pre-call gate 测试 + 对账报告 |
| 13 | 高危动作与最终合并人检点 | HITL 流程走查 |
| 14 | 无隐藏旁路/宽松回退/通配权限/裸主机执行/token 透传/意外状态提前返回的测试 | 代码走查 + test-hygiene |

## 6. 复盘模板（每收敛点一份，`docs/retrospective/v{n}-*.md`）

```markdown
# V{n} 收敛复盘（<阶段名>）
- 目标提交 / CI Evidence / 签署记录（人、日期、范围）
- 联调剧本执行统计（通过/失败/重跑）
- 偏离项统计：目标偏离 X 项、契约偏离 Y 项、实现偏离 Z 项；每项根因与预防措施
- 补丁冲刺记录（问题编号 → 修复 → 回归结果）
- 遗留登记（级别、owner、期限、补偿措施）
- 对下一波次的输入（契约冻结待办、分工调整、风险预警）
```

## 7. 度量（随复盘更新，供交付看板）

任务完成率、blocked age、变更 lead time、回滚率、缺陷逃逸、Gate 失败率、追踪覆盖率、偏离项分类计数（目标/契约/实现）。连续两次回归或安全关键失败 → 暂停下一波次启动，先根因复盘（delivery README 第 11 章规则）。
