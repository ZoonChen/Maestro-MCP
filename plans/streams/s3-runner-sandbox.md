# S3 Runner 与沙箱流任务书（会话输入）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/PIPELINE.md` → 本文件 → 当前任务对应的 `plans/stages/m*.md` 与其权威文档。做 brief 之外的事之前先登记偏离项。

## 1. 使命与所有权

端到端拥有**成员侧 Runner 与执行安全**：runner daemon（出站连接、注册/批准/心跳/恢复）、rootless OCI 沙箱、Command Profile 执行、Runner host Git broker（W2）、四类 Runbook 中 Runner 相关两类的实装（W4）。当前 `maestro runner` 是本地 stdio MCP Runner——本流把它演进为 v3 目标形态，过程保持旧形态可用直至切换。

## 2. 必读清单

| 顺序 | 文档 | 为什么 |
|---|---|---|
| 1 | `CLAUDE.md` | Profile-only 命令、沙箱、凭据红线 |
| 2 | `docs/README.md`、`plans/PIPELINE.md`、`plans/DISCIPLINE-PHASES.md` | 治理与管线位置 |
| 3 | `docs/security/runner-security.md`、ADR-001（CP/Runner 分离） | Runner 安全权威 |
| 4 | `docs/prd/multi-client.md` | 成员侧多客户端语义 |
| 5 | `docs/technical/worktree-model.md`、`docs/technical/gitlab-integration.md`（W2 起） | worktree 与远端分支模型 |
| 6 | ADR-005（人审合并；bot 无源码推送） | Git broker 推送边界 |
| 7 | `docs/delivery/m1|m2|m4-*.md` 对应任务行 | 任务范围 |

## 3. 任务序列

| 波次 | 任务 | 主轴位置 | 收敛点 |
|---|---|---|---|
| W1 | M1-RUN-001（daemon/注册码/Keychain 设备 key/generation+epoch fencing/rootless OCI/cap drop/no-new-privileges/默认无网/资源硬限；**含 M0.5：Command Profile 实例配置下发**） | M1-P4 | V1 |
| W2 | M2-GIT-001（Runner host Git broker：远端 SHA 获取、`maestro/*` 命名分支推送、保护分支防护、成员凭据仅 OS Keychain、中央 Bot 无源码推送） | M2-P4 | V2 |
| W4 | M4-RBK-001（`runner-offline` 与 `emergency-stop-credential-revoke` 两类 Runbook 实装 + 演练记录；另两类与 S1/S4 协作） | M4-P4 | V4 |

## 4. 文件边界

- **可改**：`internal/runner/`（新：daemon、注册、心跳、恢复、git broker）、`internal/sandbox/`（新：rootless OCI 执行器）、`internal/service/command_profile.go`（配置下发）、`internal/service/git_helper.go`（远端操作扩展）、`internal/service/worktree_service.go`（远端重绑定）
- **需协调**：`cmd/maestro/main.go`（runner 子命令改造）、`internal/config/config.go`（runner/sandbox 配置段）、`internal/service/task_lease_service.go`（claim 与 worktree 路径返回）
- **禁改**（只随契约 PR）：`internal/handler/router.go`、`internal/model/model.go`、`internal/store/interfaces.go`、`docs/specs/**`

## 5. DoD 与本地验收命令

- 流内门禁：`make build && make test && make vet && make lint && ruby scripts/test-hygiene-check.rb`
- 沙箱专项负测试（Exit Gate 要求）：文件（路径逃逸）、环境（敏感变量不可见）、网络（默认无网、白名单才通）、进程（无 host PID namespace）、容器逃逸（无 host socket/device/SSH/cloud creds）；沙箱测试须在支持平台真实运行
- 凭据专项：设备 key/成员凭据仅存 OS Keychain，测试断言不落盘明文

## 6. 交接物契约（向集成会话）

1. implemented 候选 Task ID 与 Evidence 指针（含沙箱逃逸测试记录）
2. Runner 支持矩阵（OS/容器运行时版本）
3. Git broker 推送边界实测记录（保护分支推送被拒、merge API 未调用）
4. Runbook 草稿与演练记录（W4）

## 7. 与其他流的接口

- **S2**：注册/心跳/恢复协议 client 侧实现（协议以 `runner.yaml` 为准，争议提 S2）
- **S1**：部署打包（runner 分发方式）与 lease 数据
- **S4**：分支推送与 MR 创建的边界（S3 推 `maestro/*`，S4 管 MR 元数据）
- **S5**：Agent 执行经 Command Profile 与沙箱（W3 起提供执行面）

## 8. 内部拆分点

W1 可拆两会话：**S3a**（daemon + 注册 + 心跳 + 恢复）/ **S3b**（rootless OCI 沙箱 + Command Profile 配置化）。W2 的 git broker 归 S3b 延续。
