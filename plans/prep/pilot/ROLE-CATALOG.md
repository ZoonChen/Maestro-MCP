# 角色工作流目录（ROLE-CATALOG）

> **定位**：工作层设计件。"所有角色工作流接入"的具体机制：十角色域 ×（标准制品 × 评审 Gate × Jira 对应物 × skill/MCP 工具）。消费 `ARTIFACT-STANDARDS`（制品类型）与 `SOLUTION-BLUEPRINT` §1（SoR/角色映射）。

## 1. 通用工作流骨架（每个角色同构）

```text
领取角色任务（WorkItem，类型=该角色产出物）
  → 用该角色的工具生产标准制品（frontmatter 合规）
  → 提交登记（asset.registered，机检拒绝不合格）
  → 评审 Gate（职能角色审批 → asset.approved，事件入审计链）
  → 下游消费（Work Graph consumes/produces 或 WorkItem 的 locked_gate）
  → Jira 锚点状态同步（SoR：Maestro→Jira 镜像）
```

## 2. 十角色域目录

> 2026-09-12 P5b 首演实测勘误：J2c/J4 已交付六件 MCP 工具（`asset_register/asset_review/asset_approve/asset_query/worktree_graph_query/decomposition_propose`）与控制台 Work Graph/资产视图，下表"待建"按实测更新；实测摩擦与缺口（职能审批在 MCP 委派上下文不可达、双签=两次流转表达等）记入 ART-retrospective-001。

| 角色域 | 产出制品（类型码） | 评审 Gate | Jira 对应物 | skill/MCP 工具（2026-09-12 实测） |
|---|---|---|---|---|
| **蓝图** | blueprint、research | tech+product | Epic 链 | ✅ asset 四件（register/review/approve/query）；蓝图模板 skill 待建（实测：内容文件+frontmatter 人工可产，模板化推迟到影子期） |
| **产品** | prd、release-note、bom（合产） | product_owner | Story/需求 issue | ✅ asset 四件；mcp-atlassian（内网不可达期按蓝图手工锚定，实测回退通路可行） |
| **设计** | hld（UI/UX 归此处） | technical_lead | 设计评审 issue | 待建：设计稿（图片类）digest+指针登记（同 legacy-intake 模式） |
| **技术** | hld、detailed-design、research | technical_lead | 技术方案 issue | ✅ WorkItem 工具 + asset 四件 + `decomposition_propose`（实测 applied/rejected 决议码与违规码齐全） |
| **详设** | detailed-design（接口/OpenAPI 变更） | technical_lead+评审人 | — | ✅ 契约引擎；✅ locked_gate 全生命周期实测（draft 拒绑→approved 可绑→supersede 全量转 stale→claim 阻断→重绑愈合，ADR-009 §8） |
| **实现** | 代码（GitLab SoR）+ MR | 证据门禁（Pipeline） | — | ✅ 全套；首演 playedu-eval 仓以 MR+人工 merge+管线绿交付 |
| **测试** | test-plan、test-report | qa_owner | 测试任务 | ✅ asset 四件（test-plan/report 首演走通）；评测 harness+pass^k 既有 |
| **运维** | ops-runbook、incident | operations_owner | 运维工单 | ✅ 四 Runbook 锚点/DLQ/SLO；incident 类型首演走通（ART-incident-002）；runbook 扩类登记待建 |
| **网络** | deploy-plan（网络段）、hld（拓扑） | operations_owner | 变更单 | 待建（本期无网络变更未演练——影子期补，模板随首例产） |
| **部署/生产** | deploy-plan、release-note（放行） | operations_owner+qa_owner | 发布单 | pilot flags 既有；shadow 置位待 J5（#111）合入后操作 |

**工具面分工原则**（对齐 SOLUTION-BLUEPRINT §1）：创作类工具留在现有环境（ZCode skill/办公工具），Maestro 只建**登记/评审/放行/查询**四类 MCP 工具（已按此交付），Agent 侧过程工具直接用 mcp-atlassian 等。
**首演实测补充（通路口径）**：MCP 写路径=stdio runner 委派上下文（HTTP /mcp 无 TransportBinding，六件工具 fail-closed）；职能面强制在 REST（冻结权限串经 J1 resolver 合并 functional_principals，seal 403→授权→200 实证）。

## 3. 首演对照（PoC 两周，brief-P5b）

D1 路线对比 PoC 按本目录全角色走一遍：research（存量摄取）→ blueprint（PoC 决策卡）→ detailed-design（对比维度与压测方案）→ test-plan/report（直播压测）→ release-note（选型结论）→ 决策 Gate（product+technical 双签）。**这是 0→1 链路的首个完整样本**。

## 4. 度量

每角色工作流三指标（进遥测，PLAYBOOK 阶段审计消费）：制品机检一次通过率、评审周期时长、Gate 阻断原因分布。
