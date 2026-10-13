# 任务书 EV：评测回归数据集（G3-E1/E2/E3——推广前必补）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `~/Works/yuandong/projects/peixun-g3/G3-3-eval-readiness.md`（就绪判定与缺口一手分析）→ `docs/technical/evaluation-dataset.md`（如存在）→ 本任务书。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-ev -b ev/dataset origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-ev && make web-build
```

## 1. 使命

G3 判定：评测 harness ✅ 就绪但**数据集缺失**——评测回归在数据集补齐前不可执行（出口评估标准③待填项的残余）。补齐 120 场景数据集骨架 + 读面缺口（G3-E2）+ 契约观察（G3-E3）。

## 2. 切片

| # | 项 | 内容 | 验收 |
|---|---|---|---|
| EV-1 | 场景数据集 v1 | `tests/eval/datasets/` 下 120 场景骨架（M4-EVAL-001 原设）：按治理面域分组（领取/门禁/合并/审计/回滚各 24），每场景=输入+期望治理行为+判分标准；首版可含 `planned` 状态占位（分批充实） | JSONL 齐全+schema 校验过+README 口径 |
| EV-2 | 评测读面（G3-E2） | evaluation_records 的读 API 面（`GET /api/v3/projects/{pid}/evaluations`），RBAC 映射 quality.read 族 | 端点+负例+spec 钉子 |
| EV-3 | 契约观察（G3-E3） | G3 指出的契约不一致点核实与文档化（eval-import CLI 与 API 面的字段差异等） | 勘误或修复（小则修，大则登记） |

## 3. 边界与 DoD

可改：`tests/eval/**`、`internal/handler/**`（EV-2 端点）、`docs/specs/**`、`cmd/eval-import/**`。禁改：harness 四层结构（M4-EVAL 交付面）。DoD：brief-E 全套 + eval-import roundtrip 门禁绿 + 新端点 spec 一致性过 + docs 四检。

## 4. 交接物

数据集 v1 + 场景分布说明（多少 implemented vs planned——诚实口径）+ G3-E2/E3 关闭声明；灰度期 Agent 质量评估的执行路径说明（数据集充实节奏建议）。
