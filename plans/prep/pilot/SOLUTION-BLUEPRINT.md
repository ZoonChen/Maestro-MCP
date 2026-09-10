# 试点解决方案蓝图（SOLUTION-BLUEPRINT）

> **定位**：工作层设计件（`plans/prep/pilot/`），统领企业学堂平台试点（`peixun` 双仓）的边界架构、合规、运行与组织四域；被 W4.5 任务书与 P5 试点体系消费。标注 **[待评审]** 的决策块需 owner/security_owner 正式评审后生效，其余为本会话按既有权威文档推导的写实决策。
> **决策输入**（2026-09-09 owner 确认）：Java+Vue 双仓为试点对象；PoC 从 0 纳入治理；GitLab 用本地沙箱栈（CE 1931）；提前激活 ADR-009 Work Graph；过程数据与 Jira 配合（经 `mcp-atlassian` 通路）。

## 1. 系统边界与数据主权（SoR）

### 1.1 SoR 矩阵（写实）

| 数据域 | SoR | 镜像方 | 说明 |
|---|---|---|---|
| 任务/工作图（WorkItem、父子拆解、状态） | **Maestro** | Jira（只读镜像） | Jira issue 是看板视图，不是任务事实 |
| 制品资产（蓝图/详设/方案/报告，含存量） | **Maestro 资产台账** | — | 版本/digest/生命周期只在 Maestro |
| 审计与门禁证据（Gate/Evidence/豁免/审批） | **Maestro** | — | 只追加审计链，导出可验（#88 已交付） |
| 代码与 CI 事实（SHA/Pipeline/MR） | **GitLab** | Maestro（投影+对账） | merged webhook 与 reconcile 双通道收敛 |
| 过程协同（评论、日常讨论、燃尽图） | **Jira** | Maestro（只读锚定，不入库正文） | 人味数据留 Jira，Maestro 只持锚点 |
| 人员与组织主数据 | **企业 HR/IAM** | 三系统各自同步 | 不在 Maestro 建主数据 |

### 1.2 同步语义（写实）

1. **单向优先**：状态流转以 SoR 方向为准——Jira→Maestro 只允许"镜像字段更新"（标题/指派人/迭代标签），**禁止** Jira 状态反向改写 Maestro WorkItem 状态（Maestro 状态只由领取/证据/合并 webhook 驱动）。
2. **冲突规则**：镜像字段与 SoR 分歧时，SoR 胜出，分歧项进对账清单（J3 的 reconcile worker），人工裁决后才更新镜像；连续两个周期未裁决的对账项升级 owner。
3. **锚点规则**：WorkItem↔issue 双向 ID 锚定在创建时建立；未锚定的 Jira issue 不产生 Maestro 副作用（不建任务、不触发门禁）。

### 1.3 账号与角色映射 **[待评审]**

| 企业岗位 | Jira 侧 | Maestro 项目角色 | Maestro 职能角色（J1 交付后） | GitLab |
|---|---|---|---|---|
| 产品经理 | project admin | project_admin | product_owner | Reporter |
| 技术负责人 | lead | project_admin | technical_lead | Maintainer |
| 后端/前端工程师 | developer | developer | — | Developer |
| 测试工程师 | QA | viewer | qa_owner | Reporter |
| 安全合规 | — | viewer | security_owner | — |
| 运维工程师 | — | viewer | operations_owner | — |

## 2. 安全与合规

### 2.1 等保 2.0 三级 ↔ Maestro 控制项映射（骨架，写实部分标注）

| 等保要求族 | Maestro 已有控制项 | 差距 **[待评审]** |
|---|---|---|
| 安全审计（留存≥6 个月） | 只追加审计链 + 导出/验证（#88）；audit_retention_days=365 冻结 | 审计归档的**异地/独立介质**存储策略；导出介质的保管规程 |
| 身份鉴别 | OIDC + BFF 会话（#96）；设备令牌；密钥仅 OS Keychain | **管理面双因子**；口令策略归属 IdP（企业 IAM 侧） |
| 访问控制 | RBAC 冻结权限表（60 项）+ 职能角色（J1 后） | 职能角色的授权书流程（见 §4.2） |
| 数据备份恢复 | 每日全备台账 + 恢复演练锚点（RPO 144ms/RTO 142ms 实测） | 季度演练的正式排期与签署 |
| 剩余信息保护/通信保密 | 全站 HTTPS 约定；Secret 仅 env 引用 + 白名单 | 试点产品的等保测评（三期）由外部机构执行，Maestro 提供证据包 |

### 2.2 数据分级全景（复用 evidence 冻结的 sensitivity 词汇）

| 级别 | 覆盖数据 | 规则 |
|---|---|---|
| `public` | 对外宣传类制品 | 无限制 |
| `internal` | 任务/工作图/蓝图/详设/报告、脱敏遥测、Jira 锚点 | 团队内可见；导出走审计 |
| `confidential` | 会话记录（Agent 轨迹）、评测数据集、审计链正文 | 默认级；台账登记时必须显式降级才可改；Agent 轨迹不进父任务上下文（ADR-009 §9） |
| `secret` | 凭据/Token/密钥 | **永不入台账与审计正文**；仅 env/Keychain 引用（既有纪律） |

存量摄取执行：调研报告/BOM xlsx/会话记录 = `internal`；xlsx 以 digest+元数据入账不改原件；会话记录按 `confidential` 登记且仅存摘要与指针。

### 2.3 外部组件引入评审（写实，输入 = BOM 02 技术选型表）

RuoYi-Vue3(MIT)/SRS(MIT)/Centrifugo(Apache)/kkFileView(Apache)/SurveyKing(MIT) 全为宽松 License，无传染风险（BOM 已核）；**mcp-atlassian(MIT)** 新增引入走同一评审口径；MinIO(AGPL) 维持"独立服务使用"边界（BOM 结论沿用）。信创适配（达梦/麒麟）为三期决策点 D2，不阻塞试点。

## 3. 平台运行架构

### 3.1 部署形态演进（写实）

- **试点期（当前）**：单实例 + 每日全备台账 + 季度恢复演练（锚点已实测）；SLO 快照端点已上线，按项目维度观察。
- **推广期门槛（触发才评估，不预先建设）**：连续两个月 控制面可用性 <99.5% 或 并发活跃项目 >3 时，启动双节点评估（会话/任务队列的主从语义需要一次 ADR）。

### 3.2 运维责任矩阵 **[待评审]**

| 事项 | 试点期责任方 | 推广期目标 |
|---|---|---|
| Maestro 控制面运维（沙箱栈） | 本项目 S1 会话/平台组 | 平台运维组 + SLO 值班 |
| GitLab 沙箱（CE 1931） | 平台组 | 公司 GitLab 迁移决策（试点后） |
| Jira Server | 企业既有运维（只读 PAT） | 不变 |
| 备份介质与恢复演练 | operations_owner 签署 | 同左 + 异地介质 |

## 4. 组织与推广

### 4.1 角色工作流接入总则

十角色域（蓝图/设计/产品/技术/详设/实现/测试/运维/网络/部署/生产）的工作流统一表达为：**角色 → 产出标准制品（ARTIFACT-STANDARDS）→ 制品评审 Gate（职能角色 J1 审批）→ 资产入账（J2a 台账）→ 下游消费（Work Graph consumes/produces）**。细化见 ROLE-CATALOG。

### 4.2 职能审批授权书 **[待评审]**

`我授权 <主体> 在 <项目/范围> 行使 <职能角色>（security_owner/qa_owner/…），有效期至 <日期>，撤销条件 <…>`——登记为 `internal` 资产并产生 `asset.approved` 类审计事件；J1 的职能角色绑定必须能追溯到一份有效授权书。

### 4.3 推广门槛（写实指标，阈值 [待评审]）

开放第二个项目前须全部达成：Required Gate 阻断率与误报复盘 ≥1 轮；审计导出完整率 100%（抽查验证）；Agent 修复 MR 人工合并率 100% 且预算超限率 <2%；角色工作流覆盖度（ROLE-CATALOG 已运转角色 ≥8/10）；试点复盘 ≥2 份（影子期、灰度期各一）。

### 4.4 变革管理

试点期陪跑节奏：影子期每周一次控制台操作培训（按角色）；灰度期缺陷闭环演示双周一次；所有培训材料本身按标准制品入账（学以致用）。

## 5. 路线图（蓝图视角）

```text
W4.5 能力波次（~2–3 周）→ P5 试点（七阶段，见 PLAYBOOK）→ E1 企业化 [远景]
E1 依赖 §4.3 推广门槛；含多项目推广、等保测评（外部机构）、双节点评估（§3.1 门槛）。
```

## 6. 评审记录

| 决策块 | 状态 | 评审人 |
|---|---|---|
| §1.1 SoR 矩阵 / §1.2 同步语义 / §2.2 数据分级 / §2.3 组件评审 / §3.1 部署演进 / §4.3 推广门槛（指标名） | 本会话写实（推导自冻结权威文档与 owner 三决策） | 集成会话 |
| §1.3 角色映射 / §2.1 差距清单 / §3.2 运维矩阵 / §4.2 授权书 / §4.3 门槛阈值 | **[待评审]** | owner + security_owner + operations_owner |
