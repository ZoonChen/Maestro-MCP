# V2 收敛复盘（GitLab / MR / Pipeline / 权威质量闭环）

> 本目录不受权威文档模板/frontmatter 约束，作为 Evidence 记录载体。本复盘对应收敛点 V2（M2 Exit Gate）；仪式程序见 `plans/convergence/v2-gitlab-quality.md`，就绪底账见 `plans/prep/v2/readiness-preaudit.md`。
>
> **诚实声明**：V2 收敛当时未按手册第 7 节产出本复盘文件（偏离登记见第 4 节）。本文于 2026-09-07 由文档对齐审计**重建**，全部内容取自 git 史实、收敛提交 `bb85cfa` 的记录与 `plans/prep/m2/p6-convergence-audit.md`；未补写任何当时不存在的运行结果。

## 1. 目标提交与 CI Evidence

- 收敛 PR：#62（合流提交 `3859381`）；状态翻转提交 `bb85cfa`，`last_verified_commit: HEAD` 自引用绑定（M0/V1 先例：翻转提交本身即被验证目标，合流后 main 头上对 HEAD 字面量的校验成立）。
- 翻转范围：m2 任务书 → `implemented / passed / HEAD`；矩阵 M2 六行（M2-GL/WHK/GIT/MR/QG/SEC-001）→ `implemented,passed,HEAD`；四份 M2 权威规范（gitlab-integration / gates-and-evidence / quality-policy / secrets-webhooks-supply-chain）`implementation_status` → `partial`——接收/同步/质量面已实装并验证，artifact 解析与剩余 SEC 控制诚实保持 partial。
- Evidence（收敛提交记录）：P5 收敛剧本（`internal/m2drill`，全部冻结出口锚点端到端）；三项阶段审计可执行化（`plans/prep/m2/p6-convergence-audit.md` → `internal/m2drill/p6_audit_test.go` 真实断言，18 包全绿含 PG、lint 0）；真实演练（真实 binary + PG16 迁移循环、两种 Compose 开机冒烟）；四核心覆盖率强制门禁（enforce 模式下全部 ≥80%）。

## 2. 波次执行统计（W2：PR #47–#62，共 16 个 PR）

readiness 预审 #47 → s4b3 剧本脚手架 #48 → 质量端点 #49 → 端点钻孔 #50 → merged→done 端到端 #51 → 连接器 REST 对齐 #52 → 注册表钻孔 #53 → 质量环 #54 → git broker #55 → 质量环钻孔 #56 → broker 钻孔 #57 → P5 收敛剧本 #58 → 合流后评审修复 #59 → Keychain 源推送 #60 → P6 审计 #61 → P6 收敛 #62。

## 3. 审计项处置（手册第 4 节）

三线审计全部可执行化并映射到真实断言（详见 `plans/prep/m2/p6-convergence-audit.md`）：

| 审计线 | 处置 |
|---|---|
| Evidence 权威性 | merge_gate 证据必带 provider 谱系（pipeline/job 数字 ID + 完整 SHA 元组）；diagnostic PASS 永不满足 Required Gate；证据写面仅 `EvidenceIngestor`/`AppendEvidence`，HTTP 面无证据写端点 |
| 豁免流程 | 自审批在 SQL WHERE 结构性拒绝；期限 ≤7 天；豁免绑定 gate 快照精确 SHA；不可豁免四原则负测试 |
| Webhook Secret | 仅 `env:MAESTRO_*` 引用（两列正则不变量）；脱敏注册表视图无 secret 字段；拒签不回显凭据、不留业务行；常量时间比较 + 验签前置任何解析 |
| 供应链 | SBOM 已进 CI；镜像签名/provenance 未启动 → 遗留登记（第 6 节） |
| core-coverage 扩展 | evidence/gate 引擎核心纳入，四核心 enforce 模式 ≥80% |

**审计捕获的真实缺陷（PR #61 修复，提交 `63585bc`）**：DB 装载的豁免永不生效——`waivers` 表无 check 列，读回豁免的 gate 身份 + check 双重校验永不匹配，任何后续评估都无法 waive 其 gate。修复：`ListWaiversForWorkItem`/`WaiverByID` 以 LEFT JOIN `gate_snapshots` 回填 check；豁免绑定的 gate 行消失时 check 为空 → 永不适用（fail-closed）。

## 4. 过程偏离与教训

1. **手册第 5 节规定的 `docs/README.md` 4.2 表更新与第 7 节规定的本复盘文件在收敛时均未执行**（4.2 行与本文件由 2026-09-07 对齐审计补齐）。根因：收口提交聚焦任务书 + 矩阵两处硬翻转，导航索引与复盘被当作"软步骤"遗漏。预防：V4 手册第 5 节已修订为收口 PR 内一并完成索引更新与复盘产出。
2. **职能审批人矩阵留为后续切片**：`waiver.approve` 的 security_owner/qa_owner 职能角色在身份层建模延后——当日对人类主体行为正确（非职能主体 403），登记遗留。
3. **SLO 抽查数字未单独留档**：手册第 3 节的"事件 60s 收敛 / Evidence 30s 出结论"抽查未出现在收口提交记录中（剧本 #10 中断降级的行为面由 m2drill 覆盖）；数字抽查留档缺失登记为遗留。
4. 就绪预审的排序建议（S4a 优先于 S4b-3 收尾）被采纳，merged→done 主链在 P5 剧本前完成端到端首验。

## 5. 角色签署

按 m2 书 frontmatter `approver_roles` 与手册第 6 节，签署以收口 PR #62 评审批准承载（V0/V1 先例）：security_owner（Webhook/Secret/供应链）、qa_owner（Gate/Evidence/Waiver）、operations_owner（GitLab 中断降级与对账）、technical_lead（总签）。

## 6. 遗留登记

| 项 | 级别 | owner | 期限 | 补偿措施 |
|---|---|---|---|---|
| 镜像签名/provenance 未启动 | Medium | security_owner | M4/W4 | 需发布环境受保护凭据（README 明确不由本地目标伪造）；SBOM 已在 CI 全量产出 |
| waiver 职能审批人身份层建模 | Medium | technical_lead | M4 | 当日对人类主体已正确 403；职能主体接入前豁免面不变宽 |
| SLO 抽查数字留档缺失 | Low | operations_owner | M4 | 行为面已有 m2drill 断言；M4-OBS-001 落地后以遥测数据补齐 |

## 7. W3 开局清单输出（实际执行对照）

1. I3 契约冻结范围（Finding/Defect/预算/Agent 工具面）→ 已执行（PR #64）。
2. S5a/S5b 分工确认 → 已执行（PR #65–#71）。
3. 试点仓库与红队集到位确认 → 红队正式注入集未建设，按 V3 审计口径登记为 M4 P1 输入（四层评测 harness 正式执行）。
