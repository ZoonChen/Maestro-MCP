# 制品标准与资产治理规范（ARTIFACT-STANDARDS）

> **定位**：工作层设计件，被 `brief-J2a`（台账存储语义）、`ROLE-CATALOG`（各角色制品实例）、`PLAYBOOK`（阶段审计）消费。目标：**每个设计与中间产物从诞生起就是可机检、可版本化、可审计的受治理资产**。
> 分级词汇与生命周期语义见 `SOLUTION-BLUEPRINT` §2.2；本文只定制品层。

## 1. 标准模式（所有制品统一）

每个制品 = 内容文件 + 机器可读 frontmatter：

```yaml
asset_id: ART-<类型码>-<序号>          # 全局唯一，发布后不改
asset_type: blueprint                   # §2 类型目录之一
title: 企业学堂平台建设蓝图 v1
version: 3                              # 从 1 起，每次修订 +1
status: draft | reviewed | approved | superseded
owner: <principal>                      # 产出责任人
reviewers: [<principal>, ...]           # 评审人（职能角色，依赖 J1）
sensitivity: public | internal | confidential
supersedes: ART-blueprint-001@2         # 版本替换链（可选）
source_digest: sha256:<64hex>           # 内容文件的规范摘要（注册时由台账计算）
locked_gate: <gate-id>                  # 该制品作为前置的 Gate（可选）
created_at / approved_at: <RFC3339>
```

**机检规则**（注册时校验，不合规则拒绝登记）：必填字段齐全；`asset_type` 在目录内；`status` 流转只允许 draft→reviewed→approved→superseded（回退=新版本）；`supersedes` 指向的版本必须存在且未被再替换；内容文件 digest 与登记一致。

## 2. 首轮制品类型目录（15 类，试点所需）

| 类型码 | 制品 | 产出角色 | 评审 Gate（职能） | 下游消费 |
|---|---|---|---|---|
| `blueprint` | 总体蓝图/路线 | 技术+产品 | technical_lead+product_owner | `hld`、Jira epic |
| `prd` | 产品需求文档 | 产品 | product_owner | `detailed-design` |
| `research` | 调研/选型报告 | 技术/产品 | technical_lead | `blueprint`（**存量：调研报告-开源选型**） |
| `bom` | BOM 清单（功能/技术/资源） | 产品+技术 | technical_lead | WorkItem 拆解输入（**存量：BOM xlsx**） |
| `hld` | 概要设计（架构/网络/部署拓扑） | 技术/网络 | technical_lead | `detailed-design`、`deploy-plan` |
| `detailed-design` | 详设（模块/接口/OpenAPI 变更） | 开发负责人 | technical_lead | 实现 WorkItem 的 `locked_gate`（详设未 approved 不得开工——PLAYBOOK 影子期起执行） |
| `test-plan` | 测试方案/用例集 | 测试 | qa_owner | 验收 Evidence |
| `test-report` | 测试/评测报告（含 pass^k） | 测试/评测 | qa_owner | `release-note`、放行 Gate |
| `sec-review` | 安全评审/威胁建模 | 安全 | security_owner | 放行 Gate 前置 |
| `ops-runbook` | 运维手册（四类 Runbook 之外的扩类） | 运维 | operations_owner | 生产操作 |
| `deploy-plan` | 部署/发布方案（含网络变更） | 运维/部署 | operations_owner | 发布 WorkItem `locked_gate` |
| `release-note` | 发布说明/验收报告 | 产品+测试 | product_owner+qa_owner | 试点验收（PILOT-001） |
| `incident` | 事故报告/演练记录 | 运维 | operations_owner | Runbook 迭代 |
| `retrospective` | 复盘报告（试点阶段复盘） | 各角色 | technical_lead | Maestro 待办反馈回路 |
| `legacy-intake` | 存量产物摄取登记（摘要+指针+digest） | 集成会话 | technical_lead | 首批资产（**会话记录等**） |

## 3. 生命周期与审计事件

```text
draft →（评审通过）reviewed →（职能审批）approved →（新版替换）superseded
```

台账必须为每次流转落审计事件（进既有只追加审计链）：`asset.registered / asset.reviewed / asset.approved / asset.superseded`，事件体携带 asset_id@version、操作主体、（审批时）对应授权书资产 ID。

## 4. Gate 绑定语义

`locked_gate` 制品是下游的**开工前置**：WorkItem 领取校验其消费的制品处于 `approved` 且 digest 与 Gate 记录一致；制品发布新版本（supersede）自动使下游 Evidence 标 stale（复用 Work Graph 的 stale 传播语义，ADR-009 §8）。

## 5. 存量摄取规则（首批资产清单）

| 存量物 | 类型 | sensitivity | 摄取方式 |
|---|---|---|---|
| `调研报告-企业培训平台开源选型.md` | research | internal | 原文入 `assets/` + 注册 |
| `企业学堂平台建设BOM清单规划.xlsx` | bom | internal | 原件 digest 入账（不改原件）+ 各 Sheet 摘要行 |
| `.zcode/plans/plan-sess_*.md`（会话记录） | legacy-intake | confidential | 仅摘要+文件指针入账，正文不入库 |

## 6. 存储与保留

- 内容文件统一存试点仓 `assets/<asset_id>/`（进 Git 版本控制）或 Maestro 资产存储（J2a 交付，二选一以 J2a 评审为准）；台账记录 digest 与位置指针。
- 保留期：与 audit_log_retention（365d）对齐；`superseded` 版本永不物理删除（审计链依赖）。
- 导出：随审计链导出（#88 端点）附资产清单与 digest，验证一次导出全量可验。
