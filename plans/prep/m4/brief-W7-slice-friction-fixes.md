# 任务书 W7：切片摩擦四修（F29 误领两例 / F32 SHA 校验 / F15 上报面 / F20 投影守卫）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（§7 各收口节）→ 本任务书；摩擦一手证据在切片工作区 `~/Works/yuandong/projects/peixun-s2b{4,8,10}/S2B*-FRICTION-REGISTER.md` 与同目录 `s2b*-evidence.json`（只读参考）。自包含。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-w7 -b w7/slice-friction-fixes origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-w7 && make web-build
```

本波全部落在咽喉点（router/store/spec）——**W7 本身就是契约 PR**，无需另行登记变更请求。

## 1. 使命

影子期开发切片 S2B3–S2B10 连续十条 done 的同时，累计出一批**每切片都在交税**的治理面摩擦。四项核心已有两例以上实证或被标记灰度硬前提，出口评估（09-26）与灰度期（阶段 3）前收口。灰度 Agent 不会手工跳这些舞——平台必须自己正确。

## 2. 切片

| # | 项（摩擦源） | 内容 | 验收 |
|---|---|---|---|
| W7-1 | F29（S2B8/S2B10 两例）claim 无定向/无归还 | ① `/api/v3` claim 端点增加可选定向参数（work_item_id）：命中 ≠ 目标 → 明确错误码（如 INVALID_STATE + 「已领取他项」），**绝不做静默误领**；② 新增归还面：execution running → 工单回 queued（**队首插回**，queue_version 递增），审计 `work_item.released` + outbox 同事务。B10-1 因误领两次悬挂、开工推迟累计约两天——两例同证 | PG 门控复现 B10-1 场景：定向 claim 错领被拒；release 后工单回队首可重领；审计/outbox 落链 |
| W7-2 | F32（S2B9 两次受理截断 SHA）complete 不校验 commit_sha | complete 的 `commit_sha` 服务端强制 40 位 hex + 与该 execution 绑定工单分支的平台已知头一致（不一致→明确拒绝）。当前仅靠客户端自觉（S2B9 后切片已内化传全 SHA，服务端缺口本身未修） | 截断/错分支 SHA 被拒（400 类错误码）；正确全 SHA 一次受理（回归现有用例） |
| W7-3 | F15（S2B3 起**每切片**手工回填）boundary 验证上报面缺失——**灰度硬前提** | `validation_runs` 的上报 API：`POST /api/v3/projects/{pid}/work-items/{wid}/validation-runs`，Body 含 profile_ref/base_commit/source_commit/changed_files/duration 等（列名以库内 `validation_runs` 现表为准）；Idempotency-Key 幂等；RBAC 优先复用既有冻结写串（pilot 写域），确需新串在本 PR 一并声明 | 同键二次上报幂等（200 同行）；权限负例（无写串 403）；切片手册后续把 psql 手工回填替换为本端点 |
| W7-4 | F20（S2B4 确诊，切片 4 起悬置）job 投影 last-write-wins 乱序覆写终态 | `UpsertJob` 单调守卫：终态（success/failed/canceled）不被后到的 pending/created/running 覆写（状态等级或完成时间戳裁决）。F26 八复现的根因一半在此：GitLab 事件乱序到达时投影冻结在中间态，切片靠 admin retry 手工解 | 乱序回放测试：success 后注入 late pending → 投影保持 success；现有 webhook 用例全绿 |
| W7-5（随行，时间允许） | F26 证据竞速 | job 终态事件先于 reconcile 元组建立时证据被丢弃（S2B10 实证：retry 一轮终态≈元组时刻→丢，二轮才落）。缓解：元组建立时回放缓冲事件，或事件侧 upsert-on-tuple。**时间不足则只交设计登记，不硬塞** | 竞速场景测试翻绿或设计文档入交接物 |
| W7-6（随行） | F22 reconcile 键 002≠006 | 对账键不对称：切片 reconcile 恒借道 002 映射面。对称化或至少文档化差异 | 测试或设计登记 |

## 3. 边界与 DoD

可改：`internal/handler/**`（新端点+校验）、`internal/store/**`（claim 定向/归还/守卫）、`internal/webhook/**`（W7-4 投影守卫）、`internal/model/**`、`docs/specs/openapi/**`（writes 36→+N 钉子随行）、`docs/specs/schemas/**`（如需）。禁改：冻结状态机语义（release 是 running→queued 的**新合法边**，须在状态机文档一并登记）、RBAC 既有映射不变（只增不改）、web 结构、`docs/governance/traceability-matrix.csv`。

DoD：brief-E 全套门禁（gofmt/build/vet/test 含 PG -p 1/lint 隔离缓存/docs 四检）+ `make e2e`（`MAESTRO_E2E_BROWSER_CHANNEL=chrome`）+ 上表各复现场景测试 + spec 钉子变更记录（OpenAPI writes、事件目录如新增 `work_item.released`）。

## 4. 常驻栈重建窗口（纪律）

W7 合入后需重建常驻镜像（8080）。**只在切片间隙执行**（s2b11 收口后）：先 pg_dump（`scripts/pilot/pg-backup.sh` 语义），按 `deploy/gitlab/peixun/resident-server.sh` 配方先迁移后换二进制，`pilot-stack/change-window.md` 登记窗口。切片会话运行中禁动。

## 5. 交接物

W7-1..W7-6 关闭声明（各附复现前后对比）；spec 钉子变更；常驻栈重建记录；残余项清单（F21/F27/F34 类流程项不含本波，列明去向）。冲突协调：本波与 peixun 切片会话不同仓，仅常驻栈窗口需错峰。
