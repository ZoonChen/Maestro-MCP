# V0 收敛手册：M0 Exit（目标提交 + CI Evidence + 角色签署）

> 执行者：I0 收口会话（F0 修复合入后启动）。权威判定：`docs/delivery/m0-foundation.md` 第 10 章 Exit Gate。
> 关键背景：本地门禁已全绿；唯一缺口是 v3 变更尚未成为"目标提交"。`docs-check.rb` 对 `passed` 状态有 git 强校验：`last_verified_commit` 必须可解析、且该文档自该提交后无 diff——因此**状态翻转必须与目标提交在同一提交链上自引用绑定**。

## 1. 入口条件

- F0 修复清单（`plans/stages/m0-closure.md` P4 表）全部合入
- `make release && make smoke` 本地全绿

## 2. 提交编舞（联调与合流）

```text
1. git checkout -b m0/target-candidate（基于最新 main）
2. 确认工作区包含：v2.1 归档移动、全部代码/CI/docs 变更、plans/ 管线文档、F0 修复
3. 本地预演全量门禁（见第 3 节），任何失败先修复再继续
4. 在同一分支上完成第 5 节的状态翻转编辑（先填占位，SHA 留空）
5. git add -A && git commit（单一收口提交，含代码+测试+文档翻转+矩阵翻转）
6. 推送分支并开 PR 到 main；记录 PR head SHA = 候选目标提交 C
7. 三个工作流（ci.yml / docs.yml / m0-runtime.yml）在 C 上全绿；
   m0-runtime 产出 artifact m0-runtime-<C>（binary + SBOM）
8. 将文档/矩阵中的 last_verified_commit 与 Verified Commit 填为 C：
   注意 docs-check 校验"该文件自 <commit> 后无 diff"，
   因此正确做法是 amend 该收口提交使 SHA 变为 C'，再推、CI 重跑、文档指向 C'；
   循环直至"文档指向的提交 == 最终通过 CI 的提交"（通常 1–2 轮）
9. squash/merge 到 main；目标提交 = main 上的最终提交
```

简化替代（推荐优先尝试）：把第 4 步的状态翻转直接指向 PR head 自身，PR 合并即闭环；若 docs-check 因自引用问题失败，再按第 8 步的 amend 循环处理。

## 3. 全量门禁（本地预演 = 远程 CI 镜像）

```bash
make release          # test+coverage+race+vet+lint+e2e+security-scan+docker-build+image-scan+sbom
make smoke            # 真实二进制集成测试（tests/m0 全量）
make compose-up && make smoke && make compose-down
ruby scripts/docs-check.rb
npx markdownlint-cli2
ruby scripts/schema-check.rb
ruby scripts/spec-consistency-check.rb
node scripts/asyncapi-check.mjs
node scripts/mermaid-check.mjs
```

干净 clone 复验一次（临时目录全新 `git clone` 该分支后重复上述命令），排除本机环境残留。

## 4. 查缺补漏审计（按 plans/QUALITY-AUDIT.md 执行 + 本阶段专项）

本阶段专项：
- [ ] m0 书第 6 节"仍未关闭"三列逐项关闭或有记录依据
- [ ] Exit Evidence 关联 Test ID 齐备（TC-MCP-001/003/004、TC-TASK-002/003/004、TC-VAL-001..005、TC-CTX-002/003、TC-DEP-001/002）
- [ ] artifact `m0-runtime-<sha>` 存在且可下载（30 天保留，另存关键日志到 `docs/retrospective/` 或 PR 描述）
- [ ] 工作区无未解释的残留文件（如 `bin/` 垃圾文件不入库）

## 5. Exit Gate 状态翻转

同一收口提交内完成：

1. `docs/delivery/m0-foundation.md`：`implementation_status: partial → implemented`、`verification_status: unverified → passed`、`last_verified_commit: <目标提交>`
2. `docs/README.md` 第 4.2 表 M0 行与第 10 章状态文字同步（"本地完整门禁通过，待目标提交/远程 CI/审批" → 实际状态）
3. `docs/governance/traceability-matrix.csv`：M0 七行同步翻转，`Verified Commit` 填目标提交
4. 复跑 `ruby scripts/docs-check.rb` 确认 passed 行校验通过（含 git 绑定）

## 6. 角色签署

按 m0 书 frontmatter `approver_roles`：product_owner、technical_lead、qa_owner、security_owner。签署方式：PR 评审批准记录（每人一条 approving review），并在复盘文档记录签署人/日期/范围。QA 逐项签署 Exit Evidence；technical/security 对各自负测试签署。

## 7. 复盘

新建 `docs/retrospective/v0-closure-retrospective.md`（retrospective 目录无 frontmatter 约束）：记录目标提交 SHA、CI Evidence 链接、签署记录、本轮发现的偏离项与根因、对 W1 的输入（如契约冻结 sprint 待办）。

## 8. 可选升级（不阻塞）

- 公司 VM 上的等价部署演练（用 `make compose-up` 的产物）
- 远程仓库保护规则设置：main 分支保护、required checks 勾选三个工作流（为 V0 之后"main 受保护"的分支协议做准备）
