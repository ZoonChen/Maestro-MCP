# 任务书 D2：capability-gated 基线 3.1.0 + 常驻栈重建（F14 结构解·其二）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（F14 段 + D1 交接物）→ `docs/quality/quality-policy.md` §7 → `internal/evidence/company_policy.json` → 本任务书。**前提：D1(#136) 已合入**。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-d2 -b d2/capability-gates origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-d2 && make web-build
```

## 1. 使命

把公司基线从"12 门无条件必达（其中 10 门无生产者）"改为**两档制**：core（有生产者的无条件门）+ capability_gates（项目声明该能力才必达的门）。消解 F14 病根的制度性防御：**声明能力 = 声明生产者**，不允许再出现"无生产者的必达门"。

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| D2-1 | **schema 扩展**：`quality-policy.schema.json` 增 `capability_gates` 数组（每项：`gate_id`/`capability_key`/`producer_kind`）+ 项目 overlay 增 `capabilities` 数组（每项：`capability` 键名 + `producer` 锚——**必带** repo/job 引用，只写键名 schema 拒绝） | docs 四检 + schema-check |
| D2-2 | **effective 解析**：`internal/evidence/effective.go`——`required = core ∪ {g ∈ capability_gates : g.capability ∈ project.capabilities}`。棘轮保持：core 不可被 overlay 移除（现状不变）；capability 声明只能加。**未声明的 capability 门不进 gate_snapshots**（不再是永不消的 pending 噪音） | 单测：core∩capability 并集正确、未声明门不出现 |
| D2-3 | **company_policy.json 3.1.0**：`required_gates` 拆两档——**core** 6 门（build/unit/secret_scan/policy_integrity/baseline_freshness/boundary——后三门 D1 已有控制面自证生产者）；**capability_gates** 8 门（coverage→quality.coverage / lint_typecheck→quality.lint / license→supply-chain.license / sast→security.sast / dependency→supply-chain.dependency / image→supply-chain.image / integration→integration.enabled / contract→contract.openapi）。integration/contract 从文档特例**收编**进统一机制 | 基线 digest 变更 + 评估器用 3.1.0 跑通 |
| D2-4 | **试点验证**：peixun 零声明 → effective = core 6 门 → 用 A1-4 元组跑完整评估 → **6/6 门全绿** → done 链闭合 | PG 门控：A1-4 六门 passed + ready_for_human_merge |
| D2-5 | **常驻栈重建窗口**：从 main 构建新镜像 → `resident-server.sh migrate`（迁移 0022）→ 换二进制 → 常驻库 A1-4 重跑验证（6/6 门 passed） | 常驻栈 A1-4 翻绿截图/查询结果 |

## 3. 边界与 DoD

可改：`docs/specs/schemas/quality-policy.schema.json`、`docs/quality/quality-policy.md`、`internal/evidence/**`（effective.go/company_policy.json/effective_test.go）、`deploy/gitlab/peixun/resident-server.sh`、e2e。禁改：D1 已合入的控制面自证代码（只消费）、评估器的 CI evidence 判定语义、web 结构、矩阵。DoD：brief-E 全套 + **PG 门控含 A1-4 六门全绿** + 常驻栈重建后 A1-4 翻绿。

## 4. 交接物

基线 3.1.0 轮换记录；capability 声明与生产者锚对照表；A1-4/A1-6 → done 的 Evidence 指针；校准回路建议（周报加每门拦截率栏）。
