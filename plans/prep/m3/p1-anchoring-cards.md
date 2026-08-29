# M3 需求锚定卡（P1 预备，供 I3 签署）

> 提取依据：`docs/delivery/m3-integration-defect-automation.md`（第 6/7/10/12 章）。锚定卡只引用权威文本 ID，不改写目标；验收标准变更必须走文档 MR。签署角色：product_owner + technical_lead（Agent 人机分工另需 security_owner 会签 AGT 卡）。

## 卡 1：M3-CTR-001 OpenAPI 契约检查

- 依赖：M2 Exit Gate
- 权威文档与 ID：`docs/prd/context-filtering.md`（`CTX-REQ-001`、`CTX-REQ-002`、`CTX-RULE-001..004`）、`docs/technical/contract-engine.md`（`CTR-REQ-001..003`、`CTR-INV-001..003`、`CTR-GATE-001..003`）
- 验收标准（任务书 §7）：解析 OpenAPI 3 JSON/YAML；完整 request/response/security/schema validation；canonical normalize/hash；breaking/non-breaking diff 规则版本化；无契约/解析错误 fail-closed；owner mapping。
- 非目标：不自动改写契约；不把"可兼容"推断为"已验证"。
- Test ID：`TC-CTR-001..005`、`TC-CTX-001..004`

## 卡 2：M3-INT-001 跨仓 IntegrationRun

- 依赖：M3-CTR-001
- 权威文档与 ID：`docs/prd/end-to-end-workflows.md`（`E2E-REQ-001`、`E2E-REQ-002`、`E2E-RULE-001..004`）
- 验收标准：manifest 固定前后端 SHA、artifact digest、contract/suite/fixture/profile versions；环境 Lease/TTL/teardown；状态 `waiting/running/pass/fail/cancel/expired`；Evidence 与责任输出。
- 非目标：不做模糊版本组合；环境失败不冒充测试失败。
- Test ID：`TC-E2E-001..004`

## 卡 3：M3-DEF-001 六类 Finding 归一

- 依赖：M3-INT-001
- 权威文档与 ID：`docs/prd/defect-and-test-issues.md`（`DEF-REQ-001`、`DEF-REQ-002`、`DEF-RULE-001..004`）、`docs/technical/defect-ingestion.md`（`DEFING-REQ-001..003`、`DEF-INV-001..004`、`DEF-GATE-001..004`）
- 验收标准：Pipeline/JUnit/contract/SAST/Secret/manual QA 六类 adapters；保留 source/severity/environment/repro/evidence/Task-MR-Pipeline refs；状态转换与独立验证。
- 非目标：Finding 层不做去重判定（归 DSP）；不自动关闭。
- Test ID：`TC-DEF-001..005`、`TC-DEFING-001..005`

## 卡 4：M3-DSP-001 Defect 唯一化与分派

- 依赖：M3-DEF-001
- 权威文档与 ID：`docs/technical/defect-ingestion.md`（同上 ID）
- 验收标准：版本化 fingerprint（project/branch/test-or-rule/error signature）；事务 upsert occurrence；resolved 后复发 reopen；severity SLA、owner routing、quarantine/replay。
- 非目标：fingerprint 不用于自动合并语义不同的失败；无 Evidence 不关闭。
- Test ID：`TC-DEF-001..005`（去重/责任子集）、`TC-DEFING-001..005`

## 卡 5：M3-AGT-001 Agent 修复闭环

- 依赖：M3-DSP-001
- 权威文档与 ID：`docs/prd/agent-remediation.md`（`AGT-REQ-001`、`AGT-REQ-002`、`AGT-RULE-001..005`）、`docs/technical/workflow-engine.md`（`WF-REQ-001..003`、`WF-INV-001..005`、`WF-GATE-001..004`）
- 验收标准：eligibility 判定；scoped ContextSet/Tool profiles；ground-truth loop；allowed path diff；本地诊断→task branch/MR→CI；计划/Tool/证据透明；停止/接管路径。
- 非目标（红线）：Agent 不改状态机/权限/Gate；无法复现即停止 handoff，不输出"已修复"；最终合并必须人检；同 Defect/SHA 单 active remediation。
- Test ID：`TC-AGT-001..005`、`TC-WF-001..005`、`TC-EVAL-001..005`（红队侧）

## 卡 6：M3-BUD-001 预算台账

- 依赖：M3-AGT-001（记账贯穿 AGT，见任务书 §6 依赖链）
- 权威文档与 ID：`docs/technical/workflow-engine.md`（`WF-REQ-003`、`WF-INV-001..005`、`WF-GATE-001..004`）
- 验收标准：每次 LLM 调用前 reserve/check；记录 Provider real usage（并行/流式全计）；默认 3 attempts/30m；checkpoint；budget/context/repro/security 四类停止边界；fallback 只产诊断不宣称修复。
- 非目标：预算不是软建议——耗尽即停；不回填历史无 usage 记录。
- Test ID：`TC-WF-001..005`、`TC-AGT-001..005`（预算拒绝子集）、`TC-NFR-002`

## 文档缺口清单（P1 出口条件：缺口有 owner 与时限）

| 文档 | 现状 | 目标 | 动作 |
|---|---|---|---|
| `docs/delivery/m3-integration-defect-automation.md` | review | approved | I3 P1 时按本锚定卡复核后签署 |
| `docs/prd/agent-remediation.md`、`docs/prd/defect-and-test-issues.md`、`docs/prd/end-to-end-workflows.md` | not_started（实现态） | spec approved | I3 P1 走文档 MR；如与锚定卡冲突先回溯本卡 |
| `docs/technical/contract-engine.md`、`defect-ingestion.md`、`workflow-engine.md` | review/partial | 实施级细节补齐 | S5a/S5b 在 P4 前提交修订 MR |
| ADR-007 | review | 落地核对 | I3 核对 Workflow-Agent 边界与 M1 落地（Provider SPI 无自动修复）一致 |

## 测试输入准备清单（任务书 §3 前置）

- [ ] 两个试点仓库（Go + TypeScript），含可触发 breaking/compatible 的 OpenAPI 样例
- [ ] OpenAPI golden cases：compatible/breaking 判定集（进 `tests/fixtures/`，版本化 digest）
- [ ] 红队注入集：直接/间接注入、恶意 OpenAPI/仓库文本/日志、工具滥用、跨项目、Secret 外泄、命令/路径/网络逃逸、资源耗尽（对接 `docs/testing/agent-evaluation-redteam.md`）
- [ ] CI/JUnit/SAST/Secret/QA fixtures 六类 Finding 样本（含重复与复发场景，供 DSP 去重/reopen 用例）
- [ ] 失败责任映射表与 human handoff owner 指派（M3 §3 要求的先行输入）
