# 任务书 C：S3-runbook 会话（M4-RBK-001 runner 侧两类 Runbook）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（调度板）→ 本任务书。做任务书之外的事之前先登记偏离项。本任务书自包含。

## 0. 工作区准备与开工前提

**前提**：`s1/m4-backup-metadata` 与 `s1/m4-drill-restore` 已合入 main（`internal/m4drill` 包在基线上）。在主 checkout 执行：

```bash
git checkout main && git pull
git worktree add ~/Works/yuandong/projects/Maestro-MCP-s3rb -b s3/m4-runbooks origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-s3rb
make web-build   # 首次生成嵌入资源
```

## 1. 使命与所有权

实装 M4-RBK-001 中归 S3 的两类 Runbook——`runner-offline` 与 `emergency-stop-credential-revoke`——的可执行面与演练锚点：故障路径真实可触发、动作可审计（`runner.revoke`、`security.emergency_stop` 事件）、演练在 `internal/m4drill` 有可执行断言。其余两类 Runbook 归 S1（数据库，锚点已合入）与 S4（webhook）。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | 默认拒绝、凭据红线（Keychain-only） |
| 2 | `docs/operations/runbooks/runner-offline.md` | Runbook 权威：90s 内识别、Lease 到期重派、恢复路径 |
| 3 | `docs/operations/runbooks/emergency-stop-credential-revoke.md` | Runbook 权威：停止面、吊销传播、保全证据 |
| 4 | `docs/security/runner-security.md` + ADR-001 | Runner 安全边界 |
| 5 | `internal/runner/` 现状 + `plans/streams/s3-runner-sandbox.md` §4 | 本流文件边界 |
| 6 | `internal/m4drill/db_restore_test.go` | 锚点模式：runbook 章节 → 可执行断言 → 实测证据 |
| 7 | `docs/delivery/m4-governance-console.md` 的 M4-RBK-001 行 | Test ID：TC-RBKRUN-001、TC-SEC-STOP-001 |
| 8 | `plans/prep/m4/session-board.md` | 环境与协作纪律（PG 栈在主 checkout 5434） |

## 3. 任务切片

| 切片 | 内容 | 验收 |
|---|---|---|
| C1 runner-offline 检测 | 心跳超时/Lease 到期的离线判定与重派路径接实（`internal/runner` + 需协调的 lease service）；90s 识别目标 | PG 门控测试：模拟心跳停止 → 离线判定 + 重派恰好一次 |
| C2 runner.revoke 面 | 吊销一个 runner：后续注册/领取被拒（401/403 稳定码）、进行中 Lease 的处置、审计事件落库 | 负测试：吊销后一切动作被拒且可审计 |
| C3 emergency-stop 面 | 紧急停止路径：停止新工作下发、在途请求的 fail-closed 行为、`security.emergency_stop` 审计事件、凭据吊销联动（Keychain 引用不落明文） | 停止后拒绝新写、审计链完整 |
| C4 演练锚点 | `internal/m4drill/runner_offline_test.go` 与 `emergency_stop_test.go`：按 runbook 章节组织子测试，PG 门控，断言动作+审计+恢复路径；输出实测时延证据（识别时延 vs 90s 目标） | 锚点测试绿 + 证据日志 |

## 4. 文件边界（沿 S3 任务书 §4）

- **可改**：`internal/runner/**`、`internal/sandbox/**`、`internal/service/command_profile.go`、`internal/service/git_helper.go`、`internal/service/worktree_service.go`、`internal/m4drill/runner_offline_test.go`、`internal/m4drill/emergency_stop_test.go`
- **需协调**（登记变更请求，不直接改）：`cmd/maestro/main.go`、`internal/config/config.go`、`internal/service/task_lease_service.go`
- **禁改**：咽喉点（`internal/handler/router.go`、`internal/model/model.go`、`internal/store/interfaces.go`、`docs/specs/**`）、`web/**`、`tests/eval/**`、`internal/m4drill/webhook_failure_test.go`（D 会话所有）、`db_restore_test.go`（已合入，不改）

## 5. DoD 与验收命令

```bash
make web-build
MAESTRO_TEST_POSTGRES_DSN='postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable' \
  go test -count=1 -p 1 ./internal/... 
ruby scripts/test-hygiene-check.rb
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
```

沙箱/凭据专项负测试若涉及（逃逸、Keychain），按 S3 任务书 §5 的既有口径执行。

## 6. 交接物（回集成会话）

1. M4-RBK-001 runner 侧 implemented 候选声明 + 两锚点 Evidence 指针（含识别时延实测）
2. 登记项：需协调文件的实际变更请求（config/lease service）、恢复演练的季度排期输入
3. 偏离项清单（如有）
