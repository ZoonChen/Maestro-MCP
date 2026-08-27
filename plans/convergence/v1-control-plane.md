# V1 收敛手册：M1 Exit（Control Plane / OIDC / PostgreSQL / Runner）

> 执行者：I1 集成会话（S1/S2/S3 的 P4 合入集成分支后）。权威判定：`docs/delivery/m1-control-plane-runner.md` 第 10 章 M1 Exit Gate。

## 1. 入口条件

- S1/S2/S3 交接物齐备：implemented 候选清单、授权矩阵实测记录、M0.5 漂移修复对照、沙箱逃逸测试记录、Runner 支持矩阵
- 契约无未决变更请求；集成分支上 `make build test vet lint` 全绿

## 2. 联调剧本（逐条通过并留证）

| # | 场景 | 步骤 | 预期 |
|---|---|---|---|
| 1 | 认证矩阵 | 匿名/错 token/过期/错 audience/伪造 scope 逐一请求 REST、MCP（stdio+HTTP）、WS | 未认证 401、无权限 403、无项目可见性 404；全部产生 deny 审计 |
| 2 | 授权一致性 | 同一 principal 对同一资源分别走 REST / MCP Tool / MCP Resource / WebSocket / background | 五面授权决定完全一致（同一 authorize 辳出） |
| 3 | Runner 生命周期 | 注册码申请→批准→出站连接→心跳→撤销 | 撤销后旧 generation 连接被 fencing 拒绝；审计完整 |
| 4 | 沙箱逃逸 | 路径逃逸/敏感环境变量/默认网络/进程与容器逃逸用例 | 全部被阻；仅在支持平台真实执行 |
| 5 | CP 无源码 | 检查 Control Plane 容器/挂载/进程 | 无项目源码接触点（静态断言 + 部署检查） |
| 6 | PG 备份与恢复 | 每日备份/WAL 基线 → 破坏性演练 → 恢复 | readiness 恢复、数据完整 |
| 7 | SQLite 导入 | 既有 SQLite 样本 → dry-run → import → reconcile → 回滚 | 四段式全通、幂等、失败整体回滚 |
| 8 | M0.5 验收 | MCP 领取带幂等键重放、队列版本冲突、会话-任务绑定断开尝试 | 重放幂等、版本冲突拒绝、未绑定会话不能领取/提交；接口形状与 tools.schema.json 逐字段一致 |
| 9 | M0 回归 | tests/m0 全量 + e2e smoke + 重启恢复 | 零回归 |

## 3. 全量门禁

与 V0 第 3 节同一命令集 + 新增：`m1-runtime.yml`（本波新增 CI 工作流，含 postgres service）在目标 SHA 全绿。

## 4. 查缺补漏审计（QUALITY-AUDIT 全清单 + 专项）

- [ ] core-coverage 清单扩展评审：identity/authorize、PG store 关键路径纳入 80% 门禁（改 `scripts/core-coverage-check.rb` 文件清单，属契约级变更，随收口 PR）
- [ ] M0.5 十项阻断清单逐项销号（`docs/retrospective/codex-session-sync-2026-08-25_26.md` §三-E）
- [ ] 授权缓存撤销传播实测（P99 <60s）
- [ ] 无残余静态 token 路径（除本地开发显式开关且默认关闭）

## 5. Exit Gate 状态翻转

同 V0 模式（单收口提交自引用绑定）：m1 任务书三状态翻转 + `last_verified_commit`；矩阵 M1 六行翻转；`docs/README.md` 第 4.2 表 M1 行同步。

## 6. 角色签署

security_owner 签署身份/Runner Evidence；operations_owner 签署部署/恢复；qa_owner 签署跨 adapter 一致性；technical_lead 总签。

## 7. 复盘与下波输入

`docs/retrospective/v1-retrospective.md`；输出 W2 开局清单：I2 契约冻结范围（Webhook/Evidence/done 入边）、S4a/S4b 分工确认、S5 mock MR 客户端接口确认。

## 8. 可选升级（不阻塞）

公司 OIDC（替换本地 Keycloak/Dex）、公司 VM 部署演练、真实 Runner 主机接入一台做带外验证。
