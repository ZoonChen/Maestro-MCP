# M2-P4 预演：S3 git broker 真实沙箱边界演练（s3/git-broker @ e2a44d6，PR #55）

> 工作层预演记录。探针测试（`live_sandbox_probe_test.go`，env 门控跳过）对真实 GitLab CE 沙箱执行七用例，全部 PASS。

## 1. 七用例实录

| # | 用例 | 结果 |
|---|---|---|
| a | 路径逃逸 project key（`../evil`） | ✅ 结构性拒绝（严格语法 `^[a-z][a-z0-9-]{0,62}$`） |
| b | 未允许的 scheme（生产 Broker 不含 http） | ✅ 拒绝（生产默认 https/ssh/git） |
| c | 空租约新建任务分支 | ✅ 推送成功 `maestro/e2e-proj/<task-id>` → `7992049d`（空租约原子断言不存在） |
| d | 已存在分支无租约 | ✅ 拒绝（`exists but no lease was given`——用 #51 演练遗留分支实测） |
| e | 错误 base 的租约 | ✅ 拒绝（`remote branch moved, lease failed`，force-with-lease CAS） |
| f | 正确 base 的租约更新 | ✅ `7992049d → 5c280242` 精确 CAS 快进 |
| g | 生成名恰等于 default branch | ✅ 守卫拒绝（纵深：不依赖生成器永远正确） |

## 2. 语义确认（GL-INV-005 / M2-GIT-001）

- **生成式 refspec**：推送目标由 `TaskBranchName` 唯一构造，自由 refspec 结构性不可能；
- **租约 CAS**：`--force-with-lease`（含空租约变体）实现原子占位与防移动覆写；
- **推后验证**：远端重读 SHA 必须等于新头；
- 与 #14 沙箱保护实测互补：broker 是 Maestro 侧唯一推源，GitLab 侧 `maestro/*` 保护是纵深（生产配置 broker 专用授权）。

## 3. 至此 M2-P4 全部实现切片均有真实演练

收件箱（#44/#45）→ 质量引擎（#46）→ REST 端点（#50）→ merged→done（#51）→ 注册表/对齐（#53）→ 质量环首段（#56）→ **git broker（本文）**。P5 剧本的全真跑（注册 runner 的真 CI）与 V2 收敛是最后两步。
