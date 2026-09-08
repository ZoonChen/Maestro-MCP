# 任务书 I：集成会话（Phase 0 解锁合入 + M4 契约 PR 接线）

> **用法**：新会话第一步依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（调度板）→ 本任务书。做任务书之外的事之前先登记偏离项。本任务书自包含：不需要其他会话的上下文。

## 1. 使命与所有权

两个相继使命：

1. **Phase 0 解锁**：用户完成 `gh auth login` 后，把 6 个本地分支推送到 origin、开 PR、按依赖顺序合并，让所有并行会话获得干净基线（详见调度板第 2、4 节，命令见下）。
2. **M4 契约 PR**：把已实现但未接线的 M4 store 能力接到 HTTP 面与后台 worker——这是本波次**咽喉点变更的唯一入口**。

背景事实（已勘察确认）：`internal/store` 的 M4 能力已实现且有 PG 门控测试，但完全未消费——handler 层无任何 audit/telemetry/SLO/backup 端点；组合根（`internal/app/application.go`，618 行）没有对应 worker。

## 2. Phase 0 命令序列（认证完成后）

```bash
git push -u origin i4/doc-drift-alignment s1/m4-slo-core s1/m4-backup-metadata s1/m4-eval-store i4/m4-session-board
# s1/m4-drill-restore 与 s1/m4-backup-metadata 同名依赖：backup 合并后再推/开 PR
```

逐个开 PR（标题引用 Stage Task ID），按调度板第 2 节顺序合并：doc-drift-alignment → slo-core → backup-metadata → drill-restore → eval-store（合并 eval-store 时解决迁移 README 第 11/12 条的一行冲突，两条都保留）→ session-board。全部合并后 `git checkout main && git pull`。

## 3. 契约 PR 范围（M4-OBS-001 剩余 + M4-REL-001 接线面）

冻结契约：`docs/specs/schemas/audit-export.schema.json`、`slo-status.schema.json`；事件目录 `docs/specs/asyncapi/events.yaml`。**字段以冻结 schema 为准，本 PR 不改语义，只做接线。**

| 切片 | 内容 | 参照模式 |
|---|---|---|
| 审计导出端点 | GET 审计导出（range 参数 → `pgObservabilityStore.AuditExport`，返回行+digest+chain）与链验证端点（claimed digests → `AuditChainVerify`） | 挂载照抄 `internal/handler/controlplane.go` 的 `RegisterControlPlane`（Options + nil=不暴露） |
| SLO 快照端点 | GET SLO 快照：telemetry 聚合（`LatestMetricWindows`）+ 可用性输入 → `internal/slo.Evaluate` → 冻结 wire JSON | 同上；策略阈值来自配置（显式，不自创默认值——见 `internal/slo` 包文档） |
| 遥测生产者 worker | 周期聚合内部指标（经脱敏策略版本标记）→ `RecordTelemetry` upsert | worker 照抄 `application.go` 的 `startDataGC`/`startWebhookDispatch`（`backgroundWG.Add(1)` + `go` + `Options` nil=不启动的诚实降级惯例） |
| 备份调度 worker | 每日全备触发壳：`RecordBackupStart` → 外部备份动作（可插拔接口）→ `CompleteBackup`；恢复验证入口留给演练 | 同上 |
| spec 同步 | `docs/specs/openapi/control-plane.yaml` 增补端点；`tests/fixtures/openapi-golden/` golden 三元组；`docs/specs/schemas/config.schema.json` 增配置段 | 契约变更与代码同一 PR（咽喉点纪律） |

认证边界：审计导出/SLO 端点走既有 AuthMiddleware；只读 GET；公开错误码遵循 publicerror 既有稳定码风格。

## 4. 文件边界

- **可改**：`internal/handler/**`（新文件 + router.go 挂载）、`internal/app/application.go`、`internal/config/config.go`、`internal/model/model.go`（仅必要时）、`internal/store/interfaces.go`（仅必要时）、`docs/specs/**`（openapi/config schema/事件目录）、`tests/fixtures/openapi-golden/**`、`cmd/maestro/main.go`（PG 侧接线，需协调项）
- **禁改**：`docs/governance/traceability-matrix.csv`（收敛仪式单写者）、`docs/retrospective/**`、其他流的文件（web/src、tests/eval、internal/runner、internal/m4drill 的他人锚点文件）

## 5. DoD 与验收命令

- 端点契约与冻结 schema 一致（spec-consistency 通过）；golden fixtures 覆盖新端点
- worker 在无配置时不启动（诚实降级）；配置错误 fail-closed 拒绝启动
- 全量门禁（在主 checkout、PG 栈已起）：
  ```bash
  make web-build
  MAESTRO_TEST_POSTGRES_DSN='postgres://maestro:maestro-local-dev@127.0.0.1:5434/maestro?sslmode=disable' \
    go test -count=1 -p 1 ./internal/... ./tests/m0
  ruby scripts/test-hygiene-check.rb
  go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run
  ruby scripts/docs-check.rb && ruby scripts/spec-consistency-check.rb
  ```

## 6. 交接物（回调度板/收敛仪式）

1. 契约 PR 链接与合并记录；各流 PR 的审阅意见汇总
2. M4-OBS-001/M4-REL-001 的 implemented 候选声明与 Evidence 指针（端点 PG 测试、worker 行为测试）
3. 各流登记的契约变更请求裁决记录
4. 偏离项清单（如有）

## 7. 与其他会话的接口

- A/B/C/D 的契约需求统一登记到本会话裁决；本 PR 合并前通知各会话 rebase。
- B（控制台）消费本 PR 的端点前，可先用既有 `/api/v3` waiver 端点开发，不阻塞。
