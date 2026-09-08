# 交接物 C（S3-runbook 会话 → 集成会话）

> 会话：brief-C-s3-runbooks（M4-RBK-001 runner 侧）｜分支 `s3/m4-runbooks`｜基线 fd731c7（#80/#81 合入后）
> 工作区：`~/Works/yuandong/projects/Maestro-MCP-s3rb`（worktree）

## 1. implemented 候选声明（M4-RBK-001 runner 侧）

两类 Runbook 的可执行面与演练锚点已落地，PG 门控测试全绿：

| 切片 | 实现面 | 测试 |
|---|---|---|
| C1 runner-offline 检测 | `internal/runner/monitor.go`（45/90s 冻结窗口判定 + Lease TTL 到期清扫 + 恰好一次重派）、`internal/runner/ops.go`（`Touch` 设备心跳观测面） | `internal/runner/runbooks_test.go::TestRunbookOfflineDeterminationAndRedispatch`；锚点 `internal/m4drill/runner_offline_test.go::TestRunbookRunnerOffline` |
| C2 runner.revoke 面 | `internal/runner/revoke.go`（终态吊销 + 在途 Lease 取消 + 工作项重派 + `runner.revoke` 审计 + 幂等） | `TestRunbookRevocationDisposesLeasesAndRefusesEverything` |
| C3 emergency-stop 面 | `internal/runner/emergency.go`（双人批准触发/解除、全局/项目冻结格、在途 Lease 立即失效、凭据吊销联动、`security.emergency_stop`/`security.emergency_recover` 审计、重启从审计链重放冻结态） | `TestRunbookEmergencyStopFreezeAndRecovery`、`TestRunbookEmergencyScopeHierarchy`；锚点 `internal/m4drill/emergency_stop_test.go::TestRunbookEmergencyStop` |
| C4 演练锚点 | 两锚点按 runbook 章节组织子测试：§3 触发窗口、§4.1/4.2 止损与恢复、§6 状态机/重启、§9/§10 审计链与红线 | 见下 Evidence |

### Evidence 指针（2026-09-08，本机 compose PG 5434）

- **识别时延实测（90s 目标）**：`TestRunbookRunnerOffline` 输出
  `drill evidence: suspect at 46.0s after silence (window 45s)`、
  `drill evidence: identification_latency=1m31.02s (objective <= 90s + tick)`——
  1s tick 引擎节奏下实测 91.02s，满足 90s+tick 界。
- **重派恰好一次**：同锚点断言重派后 active lease 唯一、新 epoch=2、静默世代迟到结果 410 LEASE_EXPIRED 拒绝。
- **审计链**：两锚点均断言 `audit.Verify` + `AuditChainVerify` 通过、篡改 UPDATE 被不可变触发器拒绝、审计行不含设备公钥/Token 明文（Keychain 红线，仅哈希引用）。
- **重启持久**：`LoadFreezeState` 从 append-only 审计链重放全局冻结（§6 子测试）。
- 门禁：`go build/vet`、`gofmt`、`ruby scripts/test-hygiene-check.rb`、golangci-lint v2.12.2 均 0 issue；PG 全量 `-p 1` 见会话记录。

## 2. 变更请求登记（需协调，本会话未改）

1. **`internal/handler/v3runner.go`**（咽喉点相邻，集成会话裁决）：
   - claim/heartbeat/complete 处理器在设备鉴权后调用 `Ops.Touch(runnerID)`：当前 v3 端点只续 Lease，不落 `runners.last_heartbeat_at`，离线判定在生产装配下没有输入源（演练中由驱动显式 Touch 模拟 daemon 心跳通道）。
   - claim 前调用 `EmergencyController.GuardClaim(projectID)`：`ErrWriteFrozen` → 建议 503 `SECURITY_FROZEN`。当前缺口：冻结期间对**新排队项**的领取仍会成功（恢复面 redispatch/requeue 已 fail-closed，演练避免制造该场景并已在锚点注释登记）。
   - revoke 端点改走 `Ops.RevokeRunner`（incident + 审计 + Lease 处置）：现 handler 直调 `store.RevokeRunner`，无审计、无在途处置。
   - 新增 emergency stop/lift 管理端点（双人批准参数）。
2. **`cmd/maestro/main.go`**：装配 `OfflineMonitor.Run`（后台 sweep）与启动时 `EmergencyController.LoadFreezeState`。
3. **`internal/config/config.go`**：monitor tick 与 lease TTL 配置化（现 tick 为包内默认 5s，lease TTL 在 handler 内固定 90s）。
4. **`internal/service/task_lease_service.go`**：经查服务 M0 SQLite legacy 路径，与 v3 PG 路径无交集——**无需变更**，登记确认结案。
5. **`internal/m4drill/db_restore_test.go` 包注释**（禁改）：仍写"剩余锚点随所属流落地"，现已落地 runner-offline 与 emergency stop 两枚，注释待 V4 收敛时顺带更新。

## 3. 恢复演练季度排期输入（M4-RBK-001）

- runner-offline 锚点即季度隔离演练的可重复内核：注入心跳静默（真实验证 45/90s 窗口）→ 离线判定 → Lease 到期 → 安全重派 → 旧世代回传拒绝。单轮 ~95s，可纳入季度窗口自动化。
- 建议季度轮换补充（当前锚点未覆盖、留给 P5 剧本）：真网络分区注入（替代心跳静默）、smoke Profile 恢复前置（runbook §4.2.4）、多 Runner 同池 >20% 离线的 P2 升级路径。
- emergency-stop 锚点覆盖触发/遏制/重启持久/分阶段恢复全链；双人桌面演练（每季度）与隔离实做（每半年）可直接复用 `TriggerEmergencyStop`/`LiftEmergencyStop` 的 incident/approver 参数化入口。

## 4. 偏离项清单

1. **控制面组件落在 `internal/runner`**：任务书 C1 行明确"（`internal/runner` + 需协调的 lease service）"，语义上监视器/吊销/紧急停止是服务端面，与既有成员侧 daemon/client 同包——文件头注释已标明；若集成会话倾向拆 `internal/runnerops`，属机械移动。
2. **演练中 monitor tick 压缩**（1s/250ms，生产默认 5s）：tick 是引擎节奏非协议常量，45/90s 阈值取自 `internal/config` 冻结常量未动；锚点注释已说明。
3. **offline 锚点 rescuer 以 10s 间隔 Touch**（真实 daemon 15s 心跳）：为在 91s 等待期保持健康侧在线；顺带覆盖 suspect→online 重联路径。
4. **store 层经 `PostgresStore.DB()` 直查**：监视器清扫/紧急失效是集合型 SQL，未新增 store 接口方法（`interfaces.go` 为咽喉点）；若集成会话要求收进 store 契约，登记为契约 PR 内容。
