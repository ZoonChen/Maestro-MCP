# V1 预审：M0.5 十项阻断清单逐项销号状态

> 工作层预审记录。权威清单：`docs/retrospective/codex-session-sync-2026-08-25_26.md` §三-E（V1 收敛手册 §4 引用）。取证基线：`origin/main @ 8e32cba`（含 #8–#19 全部合入）。判定"闭环"要求代码证据，不以文档状态代替。

## 1. 逐项状态

| # | 阻断项（2026-08-26 评估原文摘要） | 判定 | 证据（main@8e32cba） |
|---|---|---|---|
| 1 | `MAESTRO_REMOTE_WRITE=false` 阻断远程写 | **已转化** | 旗标仍在 config.go:307，但语义已从"身份旁路"变为本地开发显式开关（默认关）；身份已由 OIDC + authorize PDP 承担。与 V1 审计项"无残余静态 token 路径（除本地开发显式开关且默认关闭）"口径一致 |
| 2 | 无 Zcode Adapter/推送能力 | **开放 → M1-WGS-001** | `internal/` 无 Provider Adapter 实现；B0.1 仅本地 app-server worker（未持久化为 SessionBinding）；SPI 契约在 `docs/technical/work-graph-scheduler.md`（capabilities/start/resume/…/release） |
| 3 | MCP 无 Session/Worker 注册、恢复协议 | **基本闭环** | Runner enroll/approve/revoke + 设备令牌（#17）；MCP 侧 `TransportBinding` 组合期服务端绑定，"Request payloads can never override"，未绑定 fail-closed（mcp/tools/binding.go）；claim 强制 REGISTERED 会话（worker_tools.go）；心跳工具在册。**会话续接/恢复的收口随 claim/lease 实装完成**（I1 待办） |
| 4 | 领取结果不返回精确 worktree 路径 | **✅ 闭环** | `claimOutcome` 含 `WorktreePath` + Lease 身份/fencing + `QueueVersion`（worker_tools.go:62-70）；Runner 协议含 `WorkspaceGeneration`（runner/protocol.go:49,103） |
| 5 | 领取接口缺幂等键与队列版本 | **✅ 闭环** | v3 `get_next_task` 强制 `idempotency_key`（16–128 字符模式校验）+ `queue_version` CAS；stale token 冲突拒绝（worker_tools.go:77-130） |
| 6 | 无可信 Codex/Zcode 会话-任务绑定 | **开放 → M1-WGM-001** | `SessionBinding`/`ExecutionAttempt` 在 `internal/` 零命中 |
| 7 | 父子任务无法建立、无聚合规则 | **开放 → M1-WGP/WGM/WGS** | `WorkPlan`/`plan_revision` 在 `internal/` 零命中；规划/调度/聚合均未实装 |
| 8 | 提交验证要求 Command Profile/Policy 隔离 | **✅ 闭环（下发面）** | Runner 协议携带 digest 钉扎、不可变的 `CommandProfileRef`（runner/protocol.go:52-56 + daemon 契约测试）；任意命令串红线自 M0 保持 |
| 9 | M0 无法产生真实 GitLab CI merge-gate Evidence | **归属 M2（V2 销号）** | 非本波销号项；S4 GitLab CE 验证报告（#14）已为 I2/M2 备好偏差输入 |
| 10 | 静态 Bearer Token + 参数自报身份 | **✅ 闭环** | 自报 `project_id/role/session_id` 已从 v3 工具面消失（worker_tools.go 全文无自报入参）；OIDC 验签 + 401/403/404 中间件 + RBAC 决策点 + PG identity 面（#9/#10/#17/#18） |

## 2. 汇总与 V1 前置

- **已闭环 4 项**（#4/#5/#8/#10）+ **基本闭环 1 项**（#3，余 claim/lease 会话续接）+ **转化 1 项**（#1）+ **归属 M2 1 项**（#9）。
- **仍开放 3 项**（#2/#6/#7），全部挂 M1-WGP/WGM/WGS 三任务——即 I1 队列中的 claim/lease 实装与 Work Graph 实装本身就是最后三块拼图；V1 收敛前无法提前销号，本预审给出的是"哪些已可勾、哪些必须等实装"的准确底账。
- 与 core-coverage 预审（#19）交叉印证：planning/scheduler/provider 文件尚不存在 → 覆盖率组与销号项同步空缺，属同一等待点。

## 3. 建议

1. V1 审计引用本表时，闭环项按证据链接勾销；#1 按审计项原文口径（本地开发显式开关）核对默认值与文档一致性。
2. #3 的收口动作 = claim/lease 实装后的恢复演练（收敛手册剧本 #3/#10 已覆盖），不需要新任务。
3. #9 在 V1 复盘中登记为"移交 V2/I2"，附 #14 报告链接，避免重复评估。
