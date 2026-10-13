# datasets/ — 数据集分片、治理面域分组与覆盖契约

`tests/eval/datasets/` 是 M4-EVAL-001 数据集骨架 + 任务书 EV 的 120 场景 v1：**regression/**（固定回归，70%=84）与 **holdout/**（仅 QA/Security 可见，30%=36）两个分片目录，每个 `*.json` 是一个版本化数据集（格式见 [上级 README](../README.md) 的数据集格式节）。

## 两个正交分组轴（同 120 场景）

| 轴 | 来源 | 取值 | 配比 |
| --- | --- | --- | --- |
| `layer`（评测层，权威） | `docs/testing/agent-evaluation-redteam.md` §4 | quality / trajectory / security / capability | 40 / 30 / 40 / 10 |
| `governance_domain`（治理面域，EV-1） | 任务书 EV（M4-EVAL-001 场景骨架分组） | claim 领取 / gate 门禁 / merge 合并 / audit 审计 / rollback 回滚 | 各 24 |

分片文件按治理面域组织（`regression/claim.json` … `holdout/rollback.json` 共 10 个文件），每个 case 同时携带 `layer` 与 `governance_domain`——两者是同一 120 场景的两个划分，互不覆盖。

## `status`：implemented 与 planned（诚实口径）

每个分片 case 携带可选 `status`（缺省 `implemented`）：

- **implemented**（当前 16 个，自 H2 种子重新归域编号而来）：带 `initial_state.simulation`，mock 适配器可执行、产出合规 wire 记录；
- **planned**（当前 104 个）：**撰写骨架**——已带全部权威必填字段（输入 `inputs`、期望治理行为 `expected_outcome` + `trajectory_constraints`、判分标准 `scorer` + 约束），但不含 simulation、不可执行。runner 跳过 planned（不产 trial、不产记录、不污染 `evaluation_records`），loader 拒绝「planned 却带 simulation」的自相矛盾写法。

`dataset-check` 因此输出双层结论：**structure**（骨架 5×24 + 40/30/40/10 + 70/30，应全绿）与 **authoring**（implemented 进度，显式列缺口——不虚报完成）。

## 覆盖校验

```bash
npm --prefix tests/eval run dataset-check           # 人类可读 + 双层缺口清单（未达标时 exit 1）
npm --prefix tests/eval run dataset-check -- --json # 机器可读报告
```

校验器只数**实际落盘**的 case：骨架结构（域/层/分片配比、跨分片 `case_id` 唯一、`governance_domain` 齐全、空分片）与 implemented 进度（权威层配比 + 全量实现后的 70/30）。任一层不足时显式 FAIL 并列出每条缺口。

## 当前状态（v1 骨架，2026-10-13）

| 分片 | 域文件分布 | 每域合计（含 holdout） |
| --- | --- | --- |
| regression（84） | claim 17 / gate 17 / merge 15 / audit 16 / rollback 19 | 各 24 |
| holdout（36） | claim 7 / gate 7 / merge 9 / audit 8 / rollback 5 | — |
| **合计** | 10 文件，120 场景 | **implemented 16 / planned 104** |

implemented 的层分布（与 G3-3 记录一致）：quality 4 / trajectory 4 / security 4 / capability 4（各层 regression 3 + holdout 1）。`datasets/seed.json`（A 会话交付）保留为 harness 冒烟数据集，不计入分片。

## 正式内容的撰写工单（QA/Security 协作项，G3-E1 残余）

104 个 planned 场景就是精确工单：每个场景已固定身份（`case_id` 形如 `EV-<DOMAIN>-<LAYER>-<NN>`）、所属分片、层、面域、类别、风险、输入、期望治理行为与判分方向。撰写动作 = 为场景补 `initial_state.simulation`（mock 可执行脚本）或真实适配器执行细节，并翻 `status` 为 `implemented`；建议按各层 70/30 比例推进（40→28/12、30→21/9、10→7/3），全局 84 回归 + 36 holdout 达标即通过（`dataset-check` 以 implemented 口径判定）。撰写要求（权威 §7）：

- case 必填字段齐全（加载器 fail-closed 校验），`case_id` 全分片唯一；
- 安全场景覆盖直接/间接注入、工具滥用、数据外泄、capability enumeration、循环/资源耗尽、Agent-to-Agent 污染（v1 骨架已按此铺类目）；
- 关键场景 `budget.trials >= 3`（pass^3 门的输入）；
- holdout 内容不进入公开回归集与 Agent 可读仓库（EVAL-RULE-005）。

缺口精确数字以 `npm --prefix tests/eval run dataset-check` 输出为准。骨架再生成/调整时用 `python3 scripts/ev1-generate.py`（生成器内嵌的场景表即撰写底稿；改动表后重跑并 review 产出的 JSON）。
