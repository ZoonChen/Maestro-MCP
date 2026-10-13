# 任务书 EV 关闭声明：评测数据集骨架 + 读面 + 契约观察（2026-10-13，会话 EV）

> **定位**：brief-EV 交接物。G3-E1（骨架侧）/G3-E2/G3-E3 的关闭声明、正交设计裁定记录与灰度期执行路径建议。
> **一手证据**：`~/Works/yuandong/projects/peixun-g3/G3-3-eval-readiness.md`（缺口登记原稿）；本 PR 的 dataset-check 输出与测试。
> **服务版本**：本 PR 分支 `ev/dataset`（基线 main=27346ec）。

## 1. 关闭声明

| G3 编号 | 内容 | 本 PR 后状态 |
|---|---|---|
| G3-E1 | 正式数据集 deficit 104（16/120） | **骨架侧关闭**：120 场景骨架落盘（`tests/eval/datasets/` 10 个域别分片文件，structure 校验全绿）；**内容侧仍是试点侧待办**——104 个 planned 场景即精确工单（撰写主体=QA/Security Owner 不变，见 §4 执行路径） |
| G3-E2 | 评测面无 HTTP API（观察/裁决面缺位） | **关闭**：`GET /api/v3/projects/{pid}/evaluations` 上线（RBAC `quality.read`，openapi `Evaluation` tag + `EvaluationRunSummary` schema；钉子 38 writes/68 RBAC/25 MCP 全部不动） |
| G3-E3 | eval-import CLI 与 API 面契约差异未核实 | **关闭**：三条差异核实并登记为语义差异（见 §3），openapi operation description 钉住 trial 消歧投影与 digest null 语义；wire/schema 零改动 |

## 2. EV-1：120 场景骨架 v1（正交设计裁定）

**裁定**：任务书的治理面域分组（领取/门禁/合并/审计/回滚各 24）与评测权威 §4 的四层分布（40/30/40/10）是**同一 120 场景的两个正交分组轴**——`layer` 保持权威不变，新增 harness 本地字段 `governance_domain` 承载 5×24 分组。此为设计实现裁定（任务书原文未说明两轴关系），记录于 `tests/eval/datasets/README.md`。

| 交付 | 内容 |
|---|---|
| 骨架 | 10 分片文件（regression/holdout × 5 域），120 场景全部带权威必填字段（输入/期望治理行为/判分标准），`case_id` 形如 `EV-<DOMAIN>-<LAYER>-<NN>` |
| 诚实口径 | implemented 16（H2 种子重新归域编号，模拟与断言原样保留）+ planned 104（撰写骨架，无 simulation 不可执行）；runner 跳过 planned（零 trial、零记录、不污染 evaluation_records），loader 拒绝「planned 带 simulation」 |
| 覆盖校验 v2 | `dataset-check` 双层输出：**structure**（5×24 + 40/30/40/10 + 70/30，本 PR 后全绿）与 **authoring**（implemented 16/120，deficit 104 显式列出=QA/Security 工单）；`ok = structure ∧ authoring`——骨架完成不等于数据集完成，不虚报 |
| 生成器 | `tests/eval/scripts/ev1-generate.py`（内嵌场景表=撰写底稿，可再生成） |

分布核对（与 G3-3 数字对齐）：implemented 层分布 quality 4 / trajectory 4 / security 4 / capability 4（各层 3 回归 + 1 holdout）；分片 84 回归 + 36 holdout。

## 3. EV-3：契约观察（三条差异，均裁定为语义差异不修）

| # | 差异 | 裁定依据 |
|---|---|---|
| 1 | wire 记录无 `project_id`：项目归属是入库时属性（CLI `--project`），API 由路由作用域承载 | 服务端作用域不可由载荷自报（默认拒绝不变式） |
| 2 | wire 记录无 trial 字段：入库投影为 `<case_id>#t<k>`；API `records` 计数=trial 行数（含消歧行） | 投影确定性且幂等，`#t` 前缀可还原场景；openapi 描述已钉住 |
| 3 | CLI `--json` 的 `runs[].verdict_counts` 只含非零键；API 固定五键补零 | CLI 是运维摘要非契约面；补零契约只对 API 生效，CLI 统一登记为后续增强项 |

另：API `dataset_digest`/`model_config_digest` 在 run 内混值时返回 `null`（权威 §3 run 冻结预期），不静默取 min。

## 4. 灰度期 Agent 质量评估执行路径（数据集充实节奏建议）

1. **现状（骨架刚落）**：评测回归仍不可执行——`dataset-check` authoring 层 FAIL 是诚实状态；灰度期 Agent 质量评估继续以「逐片人工合并率」为主指标（本就是推广门槛硬指标）。
2. **充实节奏（建议）**：QA/Security Owner 按 planned 场景逐域推进（建议顺序 claim→gate→merge→audit→rollback——与切片流治理面出现频率一致）；每完成一批即翻 `status` 并跑 `npm --prefix tests/eval run dataset-check` 看 authoring 缺口收窄；层配比达标（40/30/40/10 全 implemented）后 70/30 比例自动参与判定。
3. **首个可执行回归门**：authoring 层转绿后，灰度缺陷片推广（flags=gray→full）可挂「回归分片全绿」作为 Agent 质量证据（G3-E3 原始问题「首演缺陷片是否要求评测回归通过才可推广」的裁决输入）；此前以管线四 job + 六门 + 人工合并兜底。
4. **入库与读面已就绪**：`maestro eval-import` roundtrip 门禁绿（本 PR 验收）；`GET /api/v3/projects/{pid}/evaluations` 可随时观察已入库 run——灰度期每轮评测跑完即可导入并读取聚合。

## 5. 偏离项清单

| # | 偏离 | 说明 |
|---|---|---|
| 1 | 可改面新增 `internal/store/postgres_evaluation.go`（只读辅助 `RunsByProject`） | brief-EV 可改清单未列 store；沿 brief-H 先例「除新增只读辅助」，未动任何既有存储语义 |
| 2 | 测试夹具 `quality_handler_test.go` 挂载 Evaluation handler（+1 行） | 同包夹具复用，现有 quality 测试断言零改动全绿 |
| 3 | 验收「JSONL 齐全」按「数据文件齐全 + fail-closed 校验过」解读 | 数据集格式是版本化 JSON（JSONL 是记录 wire），README 口径已写明 |
| 4 | 环境修正：resident dev 栈（5434）`maestro` 角色补 CREATEDB + `maestro` 库 public/maestro_meta schema 属主改为 maestro | 栈预置属主为 maestro-dba 导致 store 全量测试无法建库/删 schema（与本次改动无关的既有环境问题）；roundtrip.sh 与 brief-E DoD 均要求该栈可建库 |
| 5 | 环境修正：本机 e2e 以 `MAESTRO_E2E_BROWSER_CHANNEL=chrome` 跑（Playwright CDN 浏览器二进制缺失，配置文件自带该逃生门）；刷新过期镜像 `golang:1.26-alpine`（本地为 1.26.8，go.mod 要求 1.26.9——console-governance spec 的容器编译因此起不来）并预热其命名缓存卷 | 均为本机环境问题非代码回归；修复后 e2e 33/33 全绿 |
| 6 | roundtrip.sh 容器回退名修正（`maestro-postgres` → 优先 compose 规范名 `maestro-resident-postgres`，兼容旧名） | 规范栈上原回退名永远匹配不到（compose 的 container_name 就是 maestro-resident-postgres）——EV-3 精神下的小则修 |

## 6. DoD 证据（命令与结果，2026-10-13 实跑）

| 门 | 结果 |
|---|---|
| `npm --prefix tests/eval ci && npm --prefix tests/eval test` | 66/66 pass（含 planned 语义、覆盖 v2、骨架 120 断言） |
| `npm --prefix tests/eval run dataset-check` | structure COMPLETE / authoring INCOMPLETE（诚实缺口 104）——exit 1 属预期 |
| `MAESTRO_TEST_POSTGRES_DSN=… go test -count=1 -p 1 ./internal/... ./tests/m0` | exit 0，29 包全绿、零 FAIL（含新 `TestProjectEvaluations*`：正例 + 未认证 401 + 无权限 403 + 未知项目 404 + 空项目 `[]` + 混摘要 null） |
| `ruby scripts/test-hygiene-check.rb` | PASS files=7 |
| `golangci-lint@v2.12.2 run` | 0 issues |
| `ruby scripts/docs-check.rb && spec-consistency-check.rb && schema-check.rb` | 全 PASS（writes=38 / RBAC=68 / MCP=25 钉子不动） |
| `./tests/eval/scripts/roundtrip.sh` | ROUND-TRIP PASS（24 trial 本地汇总 = 存储读回逐 verdict 相等） |
| `make e2e` | 33/33 passed（Chrome channel，见偏离项 #5） |
