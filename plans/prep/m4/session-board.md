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

### W4.5 收口（2026-09-10，会话 I）

J1（#100 职能角色）/ J2a（#101 ADR-009 批准+模型+资产台账）/ J2b（#103 拆解协议+调度）/ J3（#102 Jira 连接器）/ J2c（#104 工具面+控制台）全部合入，main=be3628b，spec 钉子 25 工具/14 mutating/32 writes/60 permissions。**W4.5 能力波次关闭**；V4 收敛仪式需补登矩阵三行（J2a/J2b/J2c 的登记建议见 #104 描述）。

会话 I 裁决（J2c 三项登记）：
1. **J2c-DEC-1 接受**：任务书"19→24"为笔误，六件具名工具为准，钉子=25（现实优先，钉随契约走）
2. **J2c-CR-1 立项**：`asset.*`/`workgraph.*` 权限族 + product/technical/operations 职能授权——排为 **J4 小切片**，与 P5a 并行，**P5b 角色工作流首演前必须合入**（当前单一 qa_owner 审批是 fail-closed 窄面，多角色 Gate 会被卡）
3. **J2c-DEC-2 接受**：seal 过渡映射 `project_policy.strengthen`，终态随 J4（J4 已于 #107 合入）
4. **OPS-1 裁决**（B2 登记）：运维面板维持 deep-link 可达；DLQ 重放权限终态与 outbox 事件类型登记合并为 **W5 契约清理小切片**，P5b 前不阻塞
5. **蓝图评审启动**：五块 [待评审] 打包为 `plans/prep/pilot/REVIEW-REQUEST.md`（含推荐与批注方式），owner/security/operations 逐块批注，P5b 前完成

### P5a 收口与 J5（2026-09-11，会话 I）

- **P5a 已合入（#108，五项 CI 全绿）**：权威 MR（Java/Vue 栈范围，owner 批准随 PR）、peixun 双仓（RuoYi 底座+首条管线全绿）、三 Command Profile 真实执行（maven 96s/npm/playwright + 白名单出网 CI 级测试）、存量三资产入台账（asset.registered 审计落链）、Jira 实测不可达按蓝图回退（VPN 复测为 P5b 观察项）。两处 M1 遗留真 bug 随修（image_digest 口径断裂、workdir 归一）。集成会话追加两处 CI 修复（跨引擎网络可见性、podman 输出断言）。
- **CR-P5a-1 确诊为 P5b 硬阻塞并裁决开 J5**：平台级权限串（pilot.write/gitlab_instance.configure 等）在 PG 部署下无合法授予路径（memberships CHECK 禁 platform_admin、J1 职能主体只覆盖职能串）——**brief-J5-platform-grants.md**（平台授权小切片）下发，与 P5b 并行，**门控 P5b 的 flags 步骤**（shadow 置位/灰度推进）。
- **P5b 可开工**：非 flags 切片（拆解提案/制品生产/角色工作流）不依赖 J5；flags 相关步骤等 J5 合入。

### P5b 收口（2026-09-12，会话 P5b）

- **Stage One/Two harness 全绿**（`tests/p5b/`，tag p5b）：基线重铺（事故恢复）→ WorkPattern v1 封板 + decomposition_propose 父子图（4 子任务+D1 Gate）→ seal J1 通路首用（8081 当前代码实例 403→授权→200 实证；常驻 8080 仍为 J4 前镜像，**J5 #111 合入后重建常驻镜像并退役 8081**）→ 制品台账七类走通（blueprint/详设 v1v2/test-plan/test-report/release-note/retrospective/incident/research 重登记；SoD 拒绝实证）→ locked_gate 全生命周期（draft 拒绑/supersede 转 stale/claim 阻断/重绑愈合）→ Jira 复测不可达按蓝图回退手工锚定（5 锚点，对账清单空）→ 审计链导出+验证（verify 200）。
- **工程链路**：三仓六 MR 人工合并全绿（web CI 修复、playedu 基线+两次契约对齐、backend 制品两批）；maven-build Profile A/B 双轮实测：B 线 RuoYi 141.9/154.3s、A 线 playedu 163.8/188.4s（环境契约对齐成本一次性）；终审计链 46 条（registered×12/reviewed×9/approved×9/gate.bound×5/superseded×1/plan.sealed×4/proposal.applied×1+rejected×1），导出+验证 200。
- **事故**：P5a 种子数据被 J5 并行会话重建共享库抹除（ART-incident-002，根因=共享栈无隔离+重建动作无登记）；行动项三条（库隔离/最小备份 pg_dump 已起/审计异地导出规程）。
- **flags=shadow 未执行**：等 J5 #111 合入（观察项）；P5b 非 flags 切片全部完成。
- **Maestro 待办登记**（详见 PR 描述与 ART-retrospective-001）：职能审批 MCP 通路缺口、asset 多签 Gate 缺口、proposal 幂等键全局命名空间、summary 参数口径（schema 字符串 vs store JSON）、常驻服务器镜像随 main 重建机制。

### P5b 收口：PLAYBOOK 阶段 1 出口达成（2026-09-12，会话 I）

- **J5（#111）合入**：CR-P5a-1 关闭（platform_grants/0020 + 常驻栈 403→200 复现 + 部署红线：先迁移后换二进制）。pilot flags 通路就绪。
- **P5b（#112）合入**：0→1 全链路首演五切片全过——WorkPattern 封板与真 MCP 提案、七类制品角色工作流（**locked_gate 首次全量生效**：draft 拒绑→approved 可绑→supersede 全量 stale→claim 阻断→重绑愈合）、三仓六 MR 管线绿 + Profile A/B 双轮、D1 决策双签（建议：路线 B 主线+PlayEdu 对照，进 BOM 决策）、Jira 回退手工锚定。终审计 46 条。事故 ART-incident-002 如实登记（共享栈无会话隔离根因，行动项三条）。
- **W5 契约清理切片立项**（P5b 复盘七项 + 既有登记聚类）：职能审批 MCP 通路、资产多签 Gate、proposal 幂等键全局命名空间、summary 参数口径（500）、locked_gate 下游等待面、OPS-1 终态（DLQ 重放权限）、outbox 事件类型登记 events.yaml。**P5b 首演 bug 修复经验随切片带回归**。
- **常驻栈行动项（未单方执行，登记）**：按 p5b-server.sh 从 main（d1ccb2e）重建常驻镜像并退役 8081 对照实例——共享栈操作需协调执行（ART-incident-002 根因即共享栈无隔离变更）。
- **阶段 2（影子期）就绪**：J5 已通→flags=shadow 可置位；首演项=webhook 接线（MAESTRO_WEBHOOK_PAYLOAD_KEY + host.docker.internal 回调）+ 影子期观察（PLAYBOOK §阶段 2 出口）。

### W5 + S2 下发（2026-09-12，双会话并行）

| 会话 | 任务书 | 内容 | 备注 |
|---|---|---|---|
| W5 | brief-W5-contract-cleanup.md | 契约清理：P5b 复盘七项 + OPS-1 终态 + outbox 事件登记 | 灰度期前落定；契约面集中变更 |
| S2 | brief-S2-shadow-phase.md | 影子期开工：常驻栈重建（登记窗口+备份纪律）/flags=shadow/webhook 首演/观察面 | 含共享栈变更，执行前登记窗口 |

### S2 共享栈变更窗口（2026-09-12，会话 S2 登记并执行）

- **窗口 WIN-20260912-S2-01**（ART-incident-002 纪律，brief-S2 切片 S2-1）：常驻栈 `maestro-pilot-server`（8080）换镜像 `maestro-main:local`（自 main 8c8d82e 构建）；**先迁移后换二进制**（J5 红线，库已在 0020，`migrate up` 预期 no-op 并留记录）；退役 8081 对照实例（`p5b-server.sh down`）；换容器时补 `MAESTRO_WEBHOOK_PAYLOAD_KEY`/`MAESTRO_PILOT_WEBHOOK_KEY`（S2-3 webhook 首演前提，一次重启内完成，最小扰动）。
- 前后 pg_dump 快照存 `maestro-p5a-bases/pilot-backups/`；回滚点=旧镜像 `maestro-j5:local` 保留。实时标记：`pilot-stack/change-window.md`（并行会话可见）。
- **结果（全窗口收口）**：WIN-01 ✅（换镜像+退役 8081+smoke：health/readyz/console 302/webhook 面 401/MCP 20 工具）；WIN-02 ✅（补 `MAESTRO_PILOT_GITLAB_PAT`（实例 bot 凭据）后重启，mid 快照；root PAT 经 rails console 重建——原文件随 P5b worktree 清理丢失）；WIN-03 ✅（挂 `resident-config.yaml`：telemetry 生产者+SLO 快照激活，mid2 快照）。配方=**`deploy/gitlab/peixun/resident-server.sh`**（build|migrate|up|down，可重复拉起经两次连续重建+doctor 配置校验证实）。快照四份：pre-183916 / post-185621 / mid-191643 / mid2-193350。

### S2 影子期开工收口（2026-09-12，会话 S2）

- **S2-1 常驻栈重建** ✅（见上窗口段）；smoke 口径修正：MCP 面 live=20 工具，冻结目录钉 25——差的 5 件（get_agent_run/get_defect/get_integration_run/list_defects/report_agent_progress）自 M3 3.1 目录冻结起从未有 Go 实现（先存缺口，登记见下）。
- **S2-2 flags=shadow** ✅：backend/web 双项目 `rollout=shadow`（PUT 201×2，经 platform_admin 授权通路）；审计 #50/#51 `pilot.decision.recorded`（authority=platform:platform_admin）。playedu-eval/D1 治理域不置位（口径见 s2-evidence.json）。
- **S2-3 webhook 首演** ✅：双仓 hook（四契约事件）+ host.docker.internal 通路；真实 MR !3（s2/shadow-kickoff）全链：收件 18/18 processed（DLQ 0，负对照 401 TOKEN_MISMATCH）→投影（pipelines 2/2 success、MR merged 含 merge_commit）→对账（reconcile 202，If-Match v1）。
- **S2-4 观察面** ✅：四面 API spot check（work-graph 200/SLO 200 healthy 100%/jira 对账空=零未决/证据面=evidence 0 行属灰度前预期）；度量清单+周报骨架+首周基线=`deploy/gitlab/peixun/shadow-observation.md`；PLAYBOOK 阶段 2 行已勘误增补。
- **S2-5 团队开工** ✅：试点仓 `MAESTRO-GUIDE.md`（MR !4 合并，2f94df38）+ 台账登记 ART-opsrunbook-001@1（ops-runbook/internal/digest；审计 #52）。
- **行动项落地**：ART-incident-002 行动项①（库隔离）部分落地——同 PG 实例新建 `maestro_test` 库供门禁测试（`DROP SCHEMA` 不再触试点库）；**建议后续会话统一改用 `postgres://…:5434/maestro_test`**。
- **S2 新登记缺口（W5 裁决队列外）**：①MCP 目录 25 vs 实现 20（五件缺陷/Agent 域工具）；②config.schema.json 目标段（server/security/gitlab/observability）运行时不收（KnownFields 拒绝，resident-config 按 Go 形状编写）；③控制台浏览器登录通路（IdP 未发布宿主端口+无 hosts）待运维裁决；④证据面 quality.read 无 project_admin 持有者（冻结矩阵正确行为，verifier/viewer 账号未开）。

### 第一波（已完成，存档）

A（#84）/ B（#90）/ C（#86）/ D（#85）/ I-契约（#88）全部合入；Phase 0 六分支（#78–#83）与调度板更新（#87/#89/#91）合入。综合检查结论见 §2.6。

### W4.5 能力波次 + P5 试点（2026-09-09 规划，试点方案见 plans/prep/pilot/ 四件）

owner 三决策（2026-09-09）：Java+Vue 双仓为试点对象（权威 MR 随 P5a）；PoC 从 0 纳入治理；沙箱 GitLab（CE 1931）。架构决策：提前激活 ADR-009 Work Graph（里程碑级，原 M1-WGP/WGM/WGS 移交项回收）；过程数据与 Jira 配合（mcp-atlassian 通路，SoR 见 SOLUTION-BLUEPRINT §1）。V4 相应顺延约 3–4 周。

| 顺序 | 会话 | 任务书 | 前提 |
|---|---|---|---|
| 1 | J1 | brief-J1-functional-roles.md（职能角色，G-α/UI-4 关闭） | 无 |
| 2 | J2a | brief-J2a-workgraph-model.md（ADR-009 评审+模型+资产台账，含存量摄取命令） | 无（迁移编号与 J1 协调） |
| 3 | J3 | brief-J3-jira-connector.md（锚定/镜像/对账，按蓝图 §1.2） | 无 |
| 4 | J2b | brief-J2b-workgraph-protocol.md（拆解协议/封板/聚合） | **J2a 合入后** |
| 5 | J2c | brief-J2c-workgraph-surface.md（MCP 工具 19→24 + 控制台） | **J2a/J2b 合入后** |
| 6 | J4 | brief-J4-permission-families.md（asset/workgraph 权限族+三职能授权+seal 终态；任务书随 #106 入库） | 与 P5a 并行，**P5b 前必须合入** |
| 7 | P5a | brief-P5a-pilot-prep.md（权威 MR/双仓/Profiles/存量摄取/flags=shadow） | **W4.5 全合入后**（已满足） |
| 8 | P5b | brief-P5b-poc-governed.md（PoC 0→1 全链路首演） | **P5a + J4 后** |

试点全程按 `plans/prep/pilot/PLAYBOOK.md` 七阶段执行；设计与中间产物的标准化资产治理见 `ARTIFACT-STANDARDS.md`（J2a 台账语义依据）；十角色工作流见 `ROLE-CATALOG.md`；企业架构四域（SoR/等保/运行/组织）见 `SOLUTION-BLUEPRINT.md`（含 [待评审] 块，评审人已标注）。

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
