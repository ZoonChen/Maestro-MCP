# V1 core-coverage 清单扩展预审（供 I1 收敛审计直接消费）

> 工作层测量报告，不是权威真源。对应 `plans/convergence/v1-control-plane.md` §4 审计项"core-coverage 清单扩展评审：identity/authorize、PG/graph store、planning/scheduler/session/provider 关键路径纳入 80% 门禁"。**门禁脚本未修改**——是否扩展 `scripts/core-coverage-check.rb` 及分组口径由 I1 在 V1 审计中决定。

## 1. 测量方法

- 基线：`origin/main @ 8669493`（含 #17 v3 Runner 端点）。
- `go test -count=1 -coverprofile` 按包实测；store 与 handler 的 PG 门控套件使用真实 PostgreSQL 16（`MAESTRO_TEST_POSTGRES_DSN`，隔离钻孔实例 127.0.0.1:15432）。
- 局限：Go 按包 profile 只计入**本包测试**的覆盖；identity 文件被 handler 测试覆盖的部分未计入（跨包口径会用 `-coverpkg` 重算，I1 定稿口径时可要求）。

## 2. 实测结果（候选清单按 V1 手册措辞圈定）

| 提案组 | 文件 | 实测 | 80% 达标 |
|---|---|---|---|
| identity（authorize） | authorize.go 92.9%、policy.go 95.0%、devicetoken.go 81.0%、token.go 72.6%、resolver.go 0%（6 stmts 无测试接线） | **77.6%**（190/245） | ✗ |
| handler 认证/v3（含 PG DSN） | auth.go 85.7%、identity_middleware.go 84.6%、v3runner.go 63.6%（无 DSN 时 0%——PG 门控套件必须带 DSN 才计入） | **75.0%**（159/212） | ✗ |
| runner 协议 | client.go 81.5%、daemon.go 83.6%、protocol.go 纯类型 0 stmts | **82.5%**（127/154） | ✓ |
| PG store + 导入 | postgres_events 80.6%、postgres_identity 83.8%；**缺口**：import_sqlite_pg_plan.go 64.6%（288 stmts）、postgres_runner.go 63.3%（139）、postgres_migrations.go 70.7%（157）、import_sqlite_pg.go 73.8%、postgres_store.go 65.0%、postgres.go 66.7%、postgres_idempotency.go 76.2% | **70.5%**（716/1016） | ✗ |
| contract 引擎 | document.go 84.0%、diff.go 81.2% | **82.2%**（213/259） | ✓ |
| planning / scheduler / session / provider | **文件尚不存在**（M1-WGP/WGS 未实装） | — | 待 V1 末复测 |

注：handler 组无 DSN 时 v3runner.go 计 0%（整组 45.3%）——CI 的 postgres-integration 工作流必须承担该组测量，且按 #17 的先例**每组用独立数据库**避免死锁。

## 3. 给 I1 的建议

1. **分组建议**（扩展 `scripts/core-coverage-check.rb` 的 groups，命名沿用现有风格）：`identity-authorize`（identity 5 文件）、`cp-auth-v3`（handler 3 文件，profile 由 postgres-integration 产出）、`runner-protocol`、`pg-store-import`（store 9 文件）、`contract-engine`。
2. **先补测再上门禁**：直接把当前清单挂 80% 门禁会红——按缺口排序补测：
   - v3runner.go 63.6%：enroll 批准/拒绝分支、设备令牌吊销后 fencing、错误路径负测试；
   - token.go 72.6%：签名/时间/audience 拒绝分支；
   - import_sqlite_pg_plan.go 64.6%：quarantine 判定边界、deferrable 变体、对账差异分支；
   - postgres_runner.go 63.3%：注册码重复消费、审批状态机边沿；
   - resolver.go 0%：最小的构造/解析接线测试（仅 6 stmts，性价比最高）。
3. **graph/planning 组**在 WGM/WGP 实装后补充，勿提前挂空组（脚本对 profile 缺文件会 FAIL，fail-closed 语义保持）。

## 4. 复现命令

```bash
for pkg in identity runner contract; do go test -count=1 -coverprofile=$pkg.out ./internal/$pkg/; done
MAESTRO_TEST_POSTGRES_DSN=... go test -count=1 -coverprofile=store.out ./internal/store/
MAESTRO_TEST_POSTGRES_DSN=... go test -count=1 -coverprofile=handler.out ./internal/handler/  # 需先 make web-build
```
