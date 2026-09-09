# datasets/ — 数据集分片与覆盖契约

`tests/eval/datasets/` 是 M4-EVAL-001 数据集骨架：**regression/**（固定回归，70%）与 **holdout/**（仅 QA/Security 可见，30%）两个分片目录，每个 `*.json` 是一个版本化数据集（格式见 [上级 README](../README.md) 的数据集格式节），层别标记是 case 的 `layer` 字段。

权威依据：`docs/testing/agent-evaluation-redteam.md` §4 —— 正式数据集 120 场景 = 质量 40 / 轨迹 30 / 安全 40 / 能力 10，70% 回归 + 30% holdout。

## 覆盖校验

```bash
npm --prefix tests/eval run dataset-check           # 人类可读 + 缺口清单（未达标时 exit 1）
npm --prefix tests/eval run dataset-check -- --json # 机器可读报告
```

校验器只数**实际落盘**的 case：层配比、全局 70/30、跨分片 `case_id` 重复、空分片。不足时显式 FAIL 并列出每条缺口，不虚报完成。

## 当前状态（种子骨架）

| 分片 | 文件 | 质量层 | 轨迹层 | 安全层 | 能力层 |
|---|---|---|---|---|---|
| regression | `regression/seed.json` | 3 | 3 | 3 | 3 |
| holdout | `holdout/seed.json` | 1 | 1 | 1 | 1 |
| **合计** | | **4/40** | **4/30** | **4/40** | **4/10** |

每层 ≥3 个可执行示范场景（mock 适配器可跑、产出合规 wire 记录）；`seed.json`（A 会话交付）保留为 harness 冒烟数据集，不计入分片。

## 正式内容的撰写工单（QA/Security 协作项）

正式 120 场景由 QA Owner（质量/轨迹/能力）与 Security Owner（安全/红队）撰写，建议按各层 70/30 比例分配（40→28/12、30→21/9、10→7/3）；全局 84 回归 + 36 holdout 达标即通过（`dataset-check` 以全局口径判定）。撰写要求（权威 §7）：

- case 必填字段齐全（加载器 fail-closed 校验），`case_id` 全分片唯一；
- 安全场景覆盖直接/间接注入、工具滥用、数据外泄、capability enumeration、循环/资源耗尽、Agent-to-Agent 污染；
- 关键场景 `budget.trials >= 3`（pass^3 门的输入）；
- holdout 内容不进入公开回归集与 Agent 可读仓库（EVAL-RULE-005）。

缺口精确数字以 `npm --prefix tests/eval run dataset-check` 输出为准。
