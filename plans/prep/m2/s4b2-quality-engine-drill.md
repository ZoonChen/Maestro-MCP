# M2-P4 预演：S4b-2 质量引擎独立环境验证（s4b/evidence-engine @ f25e901）

> 工作层预演记录（离线本地完成）。对象：I1 的质量引擎分支（PR 待网络恢复开启）。

## 1. 独立环境测试（钻孔 PG 16）

- `internal/evidence`（纯引擎：policy 解析/门禁评估/waiver）+ `internal/store`（含 PG 门控 quality 套件）全量 **PASS**。
- PG 套件已覆盖完整组合流：`CompanyPolicy → ResolveEffective → Evaluate → 快照持久化 → SHA 漂移 stale 重评 → waiver 生命周期`——引擎与存储的链路在真实 PG 上闭环。

## 2. 迁移 0006 往返（真实二进制）

- `migrate up` → `applied=6`，28 → **29 表**（`quality_policies`）；
- `migrate revert --steps 1` → 精确回 28 表；再 up 零漂移。

## 3. 语义要点（测试名即证据）

- 全过 → ready；缺 Evidence → blocked；**diagnostic 证据永不满足 merge_gate**；上游漂移使既有快照 stale。
- 项目 overlay 只能单调增强（削弱被拒）、scope 不符被拒（QG-RULE-002 canonical digest + fail-closed 结构校验含未知字段）。

## 4. 结论

S4b-2 出口质量充足：纯引擎 759 行测试 + 组合流 PG 套件 + 迁移往返全部独立复验通过。S4b-3（REST 接线）落地后，waiver 越权负测试（brief 要求）成为下一个验证点。
