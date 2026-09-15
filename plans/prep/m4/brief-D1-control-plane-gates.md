# 任务书 D1：三门控制面自证（F14 结构解·其一）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（F14 段）→ `docs/quality/quality-policy.md` §7（producer 模型）→ `docs/quality/gates-and-evidence.md` §3 → 本任务书。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-d1 -b d1/control-plane-gates origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-d1 && make web-build
```

## 1. 使命

把 `policy_integrity`/`baseline_freshness`/`boundary` 三门从"全系统无生产者的永久 pending"改为**控制面评估器自证**——每次 Evaluate 时顺带产出这三门的 GateRun evidence（`authority=control_plane`），让 done 链对全司打开。

**设计依据**（RDOS 原则映射）：
- 这三门的自然 oracle 不是 CI 作业，是评估引擎自身的确定性判定
- `policy_integrity`：effective policy 链装载完整（company⊕project digest 匹配）——评估器已解析这些数据，只差"把它当可自证的判定对象"这半步
- `baseline_freshness`：元组的 policy_version == 当前 active 版本（无评估中途策略漂移）——纯函数判断
- `boundary`：该工作项执行经注册 runner + 批准 profile 完成——从 execution 记录机械推导

## 2. 切片

| # | 内容 | 验收 |
|---|---|---|
| D1-1 | **Spec 先行**：`quality-policy.schema.json` 的 producer 模型扩为种类声明（`pipeline_job \| control_plane`）；`gates-and-evidence.md` §3 补 control_plane 语义（判定权归引擎、append-only、audit 可查） | docs 四检全绿 |
| D1-2 | **评估器自证**：`internal/evidence` 的 Evaluate 流程内——对三门各做机械判定并产出 Gate snapshot（`authority=control_plane`，evidence 表写入，append-only 不破坏）；判定逻辑：policy_integrity=链 digest 匹配、baseline_freshness=版本一致性、boundary=runner+profile 注册事实 | PG 门控：三门自证正负例（正常→passed；策略链断裂→failed；未注册 runner→failed） |
| D1-3 | **与现有 evidence 的合成**：三门自证 evidence 与 CI evidence（build/unit 等）在 gate_snapshots 合成同一 verdict——自证门与 CI 门平权，不互相覆盖 | 集成测试：一个同时有 CI evidence + 自证门的元组，verdict 合成正确 |
| D1-4 | **历史元组回补**：评估器支持对已有元组重跑（按元组的 source/target SHA + 当时 policy version 重演），A1-4/A1-6 的历史元组能随下次评估补齐三门 | PG 门控：用 A1-4 的元组重跑→三门 passed→validating 中的该元组 Gate 全绿 |

## 3. 边界与 DoD

可改：`docs/specs/schemas/quality-policy.schema.json`、`docs/quality/gates-and-evidence.md`、`internal/evidence/**`、`internal/store/**`（evidence 写入面）、e2e。禁改：评估器的既有 CI evidence 判定语义、web 结构、矩阵。DoD：brief-E 全套 + PG 门控测试含三门自证正负例 + **A1-4 元组重跑翻绿**。

## 4. 交接物

三门自证声明 + Evidence 指针；spec 变更记录（producer 种类声明）；D2 的输入（schema 已扩展，capability_gates 字段留待 D2 填充）。
