# 任务书 DG：第五道防线——DSN 连接串断言（四次库事故根因根治）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `deploy/gitlab/peixun/reports/P3-shared-stack-guard-closure.md`（三道防线）→ `plans/prep/m4/session-board.md`（第四次事故段）→ 本任务书。

## 0. 工作区

```bash
git worktree add ~/Works/yuandong/projects/Maestro-MCP-dg -b dg/dsn-guard origin/main
cd ~/Works/yuandong/projects/Maestro-MCP-dg && make web-build
```

## 1. 使命

四次库事故（2026-09-12 → 2026-10-13）根因完全相同：**测试 DSN 指向主库（5434）而非隔离测试库（5435）**。P3 三道防线（物理隔离/角色对调/自动备份）修的是后果，本片修**原因**——在 `DROP`/`CREATE`/`TRUNCATE` 等破坏性 DDL 执行前检测目标 DSN，命中保护名单即拒绝。

## 2. 设计

### 断言函数（pure function）

```go
// internal/testguard/dsn_assert.go
// AssertTestDSN checks that a DSN targets the isolated test database,
// not the resident production database. It is called by test fixtures
// before any destructive DDL (DROP DATABASE/SCHEMA/TABLE, TRUNCATE).
func AssertTestDSN(dsn string) error {
    // Rules (fail-closed — any parse failure blocks):
    // 1. Must NOT contain port 5434 (resident)
    // 2. Must NOT contain database name "maestro" without suffix "_test"
    // 3. Must contain port 5435 OR database name containing "_test"
    // 4. Environment override: MAESTRO_ALLOW_ANY_DSN=1 (for rare CI
    //    scenarios where the test DB is ephemeral and unnamed)
}
```

### 接入面

| 文件 | 变更 |
|---|---|
| `internal/testguard/dsn_assert.go` | 断言函数 + 保护名单常量 |
| `internal/testguard/dsn_assert_test.go` | 正负例：5434 拒 / 5435 过 / `maestro_test` 过 / `maestro` 拒 / 环境变量覆盖 |
| `internal/store/postgres_store_test.go` 等所有 `DROP SCHEMA` 夹具 | 在 `DROP` 前调用 `AssertTestDSN(dsn)` |

### 错误面

```
ERR_DSN_GUARD_BLOCKED = "dsn targets the resident database (port 5434 or db 'maestro'); use the isolated test PG (port 5435, db 'maestro_test' or 'maestro_handler_test'). Set MAESTRO_ALLOW_ANY_DSN=1 only in CI where the test DB is ephemeral."
```

## 3. 边界与 DoD

可改：`internal/testguard/`（新包）、所有调用 `DROP SCHEMA`/`DROP DATABASE` 的测试文件（加一行断言调用）。禁改：生产代码路径、P3 既有三道防线。

DoD：brief-E 全套 + 断言正负例全绿 + **现有全量测试套件在正确 DSN 下不受影响**（零行为变更——只加防护不加逻辑）。

## 4. 常驻栈

合并后无需重建常驻栈（纯测试侧变更，不影响运行时二进制）。

## 5. 交接物

关闭声明：四次事故时间线回顾 + 断言覆盖面说明（哪些 DDL 前有断言）+ 一行总结（"第五道防线=在伤害发生前喊停的哨兵"）。
