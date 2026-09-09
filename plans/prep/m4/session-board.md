# M4 收敛调度板（W4 多会话并行总控）

> **定位**：工作层文档（`plans/prep/`），非权威真源；冲突以 `docs/README.md` 权威顺序为准。本文是 M4-P4 收尾阶段的多会话调度入口：任务书分发、分支依赖、工作区约定与协作纪律。执行编排查阅 `plans/PIPELINE.md`。
> **分发方式**：用户手动开新 ZCode 会话，开场输入对应任务书路径（见第 6 节分发清单）。

## 1. 现状快照（2026-09-08 第二次更新）

- M0–M3 已收敛（矩阵 25/31 行 `implemented+passed`，V0–V3 仪式与复盘齐备）。
- **Phase 0 已完成**：GitHub 认证解锁，六分支 PR #78–#83 全部合入（各自远程 CI 全绿）；本地与远端工作分支已清理。
- **P4 并行进度**：A（S6-eval harness）✅ PR #84 合入；C（S3-runbooks 两锚点）✅ PR #86 合入；D（S4-webhook-drill 锚点）✅ PR #85 合入；**B（S6-console）进行中**；`internal/m4drill` 四锚点齐全，`tests/eval` 已落地。
- 会话 I 当前使命：契约 PR **已合入（#88）**；剩余裁决项见第 2.5 节（遥测生产者、UI-1 端点、EVAL-1）。

## 2. 已合入 PR 台账（Phase 0 + 第一波并行）

| PR | 分支 | 内容 |
|---|---|---|
| #78 | i4/doc-drift-alignment | 文档漂移对齐 + v2/v3 复盘重建 |
| #79 | s1/m4-slo-core | M4-REL-001① SLO 评估核心（internal/slo） |
| #80 | s1/m4-backup-metadata | M4-REL-001② backup_runs + 0013 + Reliability 存储 |
| #81 | s1/m4-drill-restore | M4-RBK-001① 数据库恢复演练锚点 |
| #82 | s1/m4-eval-store | M4-EVAL-001① internal/eval + 0014 + Evaluation 存储（rebase 解 README 冲突后合入） |
| #83 | i4/m4-session-board | 本调度板 + 5 份任务书 |
| #84 | s6/m4-eval-harness | M4-EVAL-001 harness 第一代（tests/eval） |
| #85 | s4/m4-webhook-drill | M4-RBK-001 webhook 侧锚点（D1–D3 + 实测 Evidence） |
| #86 | s3/m4-runbooks | M4-RBK-001 runner 侧两类 Runbook + 两锚点 |
| #88 | i4/m4-contract-wiring | 契约 PR：审计导出/验证端点、SLO 快照端点、G1 双人审批重放（含两处 CI 修复：幽灵 500 引用、daemon 测试安装竞态） |

## 2.6 第一波停止位综合检查（2026-09-09，会话 I）

- main 本地全量门禁全绿：全量 Go 套件含 PG（-p 1）、test-hygiene、lint 0、docs 四检、真实二进制 smoke、`make coverage`（四核门槛 PASS，state-machine 100%）、A 的 eval harness 49/49。
- 观察项：`make coverage` 曾有一次 m2drill 包瞬时失败，两次复现均通过（覆盖率插桩时序 flake，未定位）——复发再追。
- 缺口修复：m4drill 未计入 store 覆盖率包清单（Makefile 已补）；#90 的 CI 需要真实浏览器二进制（workflow 已修两轮：装包顺序 + lockfile 版本对齐）。
- worktree 盘点：s3rb/s4wh/s6eval 干净无未提交变更，#90 合并后统一清理；s6ui 待合并。

### B 会话登记（#90，待会话 I 裁决）

| 编号 | 内容 | 影响 |
|---|---|---|
| UI-AUTH | `/auth` 协议端点未实装（IdentityMount.RegisterRoutes 恒 nil）：OIDC 授权码流 + HttpOnly cookie 会话 + Authenticate 接受 cookie——浏览器登录态无法真实建立，B 的登录 UI 仅过 stub 测试 | M4-UI-001 "implemented" 的硬前提 |
| UI-2 | GET waivers 列表端点缺失（store 已有 ListWaiversForWorkItem 未暴露）：HITL 队列无法列待审豁免 | M4-UI-001 队列页的硬前提 |
| UI-3 | ES256 验签用 DER 编码偏离 RFC 7515（raw R\|\|S）——真实 IdP 互操作会失败 | M1-AUTH 修正项 |
| UI-4 | waiver.approve 无角色可达（冻结矩阵只授 security_owner/qa_owner，身份层未建模职能角色）——已知的 M2 遗留 | 与 V2 复盘遗留同源 |
| UI-5 | 审计导出/SLO 快照视图：契约 PR #88 端点已上线，剩前端消费 | 小切片 |
| UI-6 | v1/v3 双存储过渡桥（overview 读 SQLite、治理在 PG，UI 手输项目 ID） | 工作项切换后移除 |

## 2.5 契约 PR 待办与缺口登记（会话 I 裁决队列）

| 编号 | 来源 | 内容 | 归属 |
|---|---|---|---|
| ~~G1~~ | D 交接物 | **已关闭（#88）**：`ReplayDeadLetter` 双人审批 + 原子审计行（`ReplayApproval` 契约） | ✅ |
| G2 | D 交接物 | webhook DLQ 深度无常驻指标/告警面（Runbook §3/§11）——SLO 快照端点已上线（#88），剩遥测生产者采集 DLQ/inbox 深度 | 遥测生产者切片 |
| UI-1 | D 交接物 | 控制台 DLQ 人工清单视图 + 带审批人身份的重放动作——存储契约已就绪（G1），**剩 HTTP 端点与冻结权限** | 小切片（含 spec） |
| EVAL-1 | A 交接物 | JSONL 记录 → `evaluation_records` 入库接线；正式 120 场景数据集；LLM judge 校准 | 会话 I 后续 / QA 协作 |
| REL-2 | 会话 I | 遥测生产者 worker（请求路径埋点设计）——M4-OBS-001 收尾最后一块；备份调度 worker 已按 Runbook 诚实砍掉（运维工具链职责） | 会话 I 后续 |

## 3. 会话-任务书矩阵

| 会话 | 任务书 | 任务（矩阵行） | 分支命名 | 开工前提 |
|---|---|---|---|---|
| I（集成） | [brief-I-integration.md](brief-I-integration.md) | Phase 0 解锁合入；契约 PR：OBS/REL 接线（M4-OBS-001 剩余 + M4-REL-001 接线面） | `i4/m4-contract-wiring` | 用户完成 `gh auth login` |
| A（S6-eval） | [brief-A-s6-eval.md](brief-A-s6-eval.md) | M4-EVAL-001 harness（tests/eval 从零建） | `s6/m4-eval-harness` | 无（可与 Phase 0 并行） |
| B（S6-console） | [brief-B-s6-console.md](brief-B-s6-console.md) | M4-UI-001 控制台（OIDC 接线/HITL 队列/写操作/DOM 测试） | `s6/m4-console` | 无（可与 Phase 0 并行） |
| C（S3-runbook） | [brief-C-s3-runbooks.md](brief-C-s3-runbooks.md) | M4-RBK-001 runner 侧两类 Runbook + 两锚点 | `s3/m4-runbooks` | **#3+#4 合入后**（需要 m4drill 包在基线上） |
| D（S4-webhook-drill） | [brief-D-s4-webhook-drill.md](brief-D-s4-webhook-drill.md) | M4-RBK-001 webhook 侧演练锚点 | `s4/m4-webhook-drill` | 同 C |

## 4. Phase 0：解锁合入（任务书 I 的第 0 步）

1. 用户在本机执行 `gh auth login`（或配置 SSH key 并切换 remote）。
2. 会话 I 从主 checkout 推送全部 6 个分支 → 逐个开 PR → 按第 2 节顺序合并 → 拉回 main。
3. 并行会话（A/B 已开工的）在各自 worktree 里 `git fetch origin && git rebase origin/main` 同步基线。

## 5. 多会话工作区与环境约定

- **独立 worktree**：每个并行会话一个 git worktree，避免共享 checkout 的分支冲突。主 checkout 中执行：
  ```bash
  git worktree add ~/Works/yuandong/projects/Maestro-MCP-<会话名> -b <分支命名> i4/m4-session-board
  ```
  （C/D 在 Phase 0 后改用 `-b <分支命名> origin/main`。）会话结束后 `git worktree remove` 清理。
- **PG 门控栈全局一份**：compose 的 `maestro-postgres` 只在**主 checkout** 启停一次，端口固定 5434：
  ```bash
  MAESTRO_POSTGRES_PORT=5434 docker compose up -d maestro-postgres   # 开
  docker compose stop maestro-postgres                               # 关
  ```
  各会话测试 DSN 统一：`postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable`。**不要在自己的 worktree 里再起 compose**（端口冲突）。本机 5433 被无关项目占用。
- **Go 包构建依赖 web/dist**：worktree 内首次跑 `make web-build`（或 `npm --prefix web ci && npm --prefix web run build`）生成嵌入资源。
- **门禁口径**（各任务书 DoD 引用）：`gofmt -l <改动包>`、`go build/vet`、`go test -count=1`（PG 套件带 DSN 且 `-p 1`）、`ruby scripts/test-hygiene-check.rb`、`go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run`；涉及 docs/specs 的变更另跑 `ruby scripts/docs-check.rb` 与 `ruby scripts/spec-consistency-check.rb`。

## 6. 协作纪律（防冲突/防跑偏）

1. **咽喉点唯一入口**：`internal/handler/router.go`、`internal/model/model.go`、`internal/config/config.go`、`internal/store/interfaces.go`、`docs/specs/**` 的变更只进会话 I 的契约 PR；其他会话发现契约需求 → 在交接物中登记变更请求（原因/影响/方案），等待集成会话裁决。
2. **矩阵单写者**：`docs/governance/traceability-matrix.csv` 只在 V4 收敛仪式翻转；流会话在交接物报告 implemented 候选与 Evidence 指针。
3. **m4drill 按锚点文件拆分**：C 占 `runner_offline_test.go`/`emergency_stop_test.go`，D 占 `webhook_failure_test.go`，互不触碰对方文件。
4. **文件边界互斥**：各任务书第 4 节的可改清单无交集；越界前先登记偏离项。
5. **分支短生命周期**：合入即弃；长期不合的分支每 2–3 天 rebase 一次 main。
6. **PR 落后会被 strict 保护 BLOCK**：main 前移后先 `gh pr update-branch <PR号>`（把 main 并入 PR 分支、等检查重跑）再合并；属主 UI 直合亦可（enforce_admins=false）。
7. **诚实状态**：`partial/unverified` 是常态；只有 P6 收敛（远程 CI Evidence + 签署 + commit 绑定）才翻转。

## 7. 分发清单

### 第一波（已完成，存档）

A（#84）/ B（#90）/ C（#86）/ D（#85）/ I-契约（#88）全部合入；Phase 0 六分支（#78–#83）与调度板更新（#87/#89/#91）合入。综合检查结论见 §2.6。

### 第二波（2026-09-09 下发，已完成）

#93 G（pilot 后端）、#94 F（遥测生产者，G2/REL-2 关闭）、#95 H（eval 入库+数据集骨架）、#96 E（auth 后端：0015 会话双表/ES256 raw/waivers 列表/DLQ 重放端点，冲突双保留+钉子 31/60）、#97 B2（控制台二代：真登录/HITL 接真/四治理视图/过渡桥决策=保留手输+会话范围注入）。**M4-P4 于 2026-09-09 收口**：六任务全部具备首代实现。

B2 交接登记（待裁决）：**OPS-1** 运维面板 IA 可见性（platform/ops/security）与 DLQ 重放权限持有者（project_admin）错位，现需 deep-link `#/operations`，是否扩可见性待集成会话；**UI-4**（职能角色不可建模→审计导出恒 403）维持已知状态。工程备注：Playwright 拦截带 query 的 URL 需 glob 尾 `*`；表单原生 minlength 挡 submit，长度校验放 JS 层。

### 第二波（原分发表，存档）

| 顺序 | 会话 | 任务书 | 开场输入（粘贴给新会话的第一句） | 前提 |
|---|---|---|---|---|
| 1 | E | [brief-E-auth-backend.md](brief-E-auth-backend.md) | `读 plans/prep/m4/brief-E-auth-backend.md，从第 0 节 worktree 准备开始，按任务书执行。` | 无 |
| 2 | F | [brief-F-telemetry.md](brief-F-telemetry.md) | `读 plans/prep/m4/brief-F-telemetry.md，从第 0 节 worktree 准备开始，按任务书执行。` | 无 |
| 3 | G | [brief-G-pilot.md](brief-G-pilot.md) | `读 plans/prep/m4/brief-G-pilot.md，从第 0 节 worktree 准备开始，按任务书执行。` | 无 |
| 4 | H | [brief-H-eval-ingest.md](brief-H-eval-ingest.md) | `读 plans/prep/m4/brief-H-eval-ingest.md，从第 0 节 worktree 准备开始，按任务书执行。` | 无 |
| 5 | B2 | [brief-B2-console-gen2.md](brief-B2-console-gen2.md) | `读 plans/prep/m4/brief-B2-console-gen2.md，从第 0 节 worktree 准备开始，按任务书执行。` | **E 与 G 合入后** |

E/F/G/H 无共享可改文件可四会话并行（E 占 handler/identity/store-auth，F 占 app/埋点/config-telemetry，G 占 pilot 存储/权限，H 占 cmd/tests-eval）。唯一触点：`docs/specs/schemas/config.schema.json` 为 E/F/G 共享（各加各段，后合并者解一行冲突，两条都保留）。

## 8. P4 收齐后的路径（预告，非本板范围）

P5：V4 剧本全量演练（Runner compromise、GitLab 中断、DLQ replay；备份恢复锚点已在 #4）+ 2–5 试点仓库影子运行。P6：V4 收敛仪式（矩阵 M4 六行 + 任务书 + DOC-INDEX M4 行 + `docs/retrospective/v4-retrospective.md` **同一收口 PR**，手册 §5 已修订为此要求）+ 生产准入彩排。
