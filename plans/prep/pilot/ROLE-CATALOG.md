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

| 角色域 | 产出制品（类型码） | 评审 Gate | Jira 对应物 | skill/MCP 工具（现状/待建） |
|---|---|---|---|---|
| **蓝图** | blueprint、research | tech+product | Epic 链 | 待建 MCP：`asset.register/submit-review`；ZCode skill：蓝图模板生成（待建，P5b 评估） |
| **产品** | prd、release-note、bom（合产） | product_owner | Story/需求 issue | 待建 MCP：登记/评审；mcp-atlassian 读写 issue |
| **设计** | hld（UI/UX 归此处） | technical_lead | 设计评审 issue | 待建：设计稿（图片类）以 digest+指针登记（同 legacy-intake 模式） |
| **技术** | hld、detailed-design、research | technical_lead | 技术方案 issue | 既有：WorkItem 领取/提交工具；待建：asset 工具 |
| **详设** | detailed-design（接口/OpenAPI 变更） | technical_lead+评审人 | — | 既有：契约引擎（OpenAPI 校验即机检的一部分）；待建：asset 工具 |
| **实现** | 代码（GitLab SoR）+ MR | 证据门禁（Pipeline） | — | 既有：全套（领取/沙箱/Profile/MR/证据）✅ |
| **测试** | test-plan、test-report | qa_owner | 测试任务 | 既有：评测 harness + pass^k；待建：test-report 登记工具 |
| **运维** | ops-runbook、incident | operations_owner | 运维工单 | 既有：四 Runbook 锚点、DLQ/SLO 视图 ✅；待建：runbook 扩类登记 |
| **网络** | deploy-plan（网络段）、hld（拓扑） | operations_owner | 变更单 | 待建：网络方案模板（P5b 定） |
| **部署/生产** | deploy-plan、release-note（放行） | operations_owner+qa_owner | 发布单 | 既有：pilot flags（shadow/gray/full）✅、放行=flags 变更（双人审计已交付）✅ |

**工具面分工原则**（对齐 SOLUTION-BLUEPRINT §1）：创作类工具留在现有环境（ZCode skill/办公工具），Maestro 只建**登记/评审/放行/查询**四类 MCP 工具（挂 J2c 钉子），Agent 侧过程工具直接用 mcp-atlassian 等。

## 3. 首演对照（PoC 两周，brief-P5b）

D1 路线对比 PoC 按本目录全角色走一遍：research（存量摄取）→ blueprint（PoC 决策卡）→ detailed-design（对比维度与压测方案）→ test-plan/report（直播压测）→ release-note（选型结论）→ 决策 Gate（product+technical 双签）。**这是 0→1 链路的首个完整样本**。

## 4. 度量

每角色工作流三指标（进遥测，PLAYBOOK 阶段审计消费）：制品机检一次通过率、评审周期时长、Gate 阻断原因分布。
