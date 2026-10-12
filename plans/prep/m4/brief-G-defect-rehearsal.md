# 任务书 G（灰度首演·缺陷修复）：Agent 缺陷闭环首演（PLAYBOOK 阶段 3）

> **用法**：新会话依次读 `CLAUDE.md` → `docs/README.md` → `plans/prep/m4/session-board.md`（灰度启动段）→ 本任务书。与另一缺陷片（G1/G2）**并行执行**——本片是并发领取实战的一部分。工作区自最新已收官切片（peixun-s2b17/s2b20 任一）复制。

## 0. 使命

灰度期首演：**缺陷下发→定向领取→沙箱修复→MR→管线→人工合并→done** 全链在治理下走通。这是"Agent 修复人工合并率 100%"推广门槛指标的起点。缺陷为**测试补齐类低风险**（出口评估选型）：目标 Rules 类零测试覆盖——补齐测试，**若测试发现真实缺陷则如实修复**（发现即诚实记录，不隐瞒不虚构）。

## 1. 缺陷指派（开场语给出工项 ID）

- 工项已由 owner 侧下发入队（审计 `gray-rehearsal-20261013`）；**定向 claim 直取本项**（W8 新语义：显式定向即领，无队首约束——与并行片并发领取正是本演目的）。
- 领取前现场核验工单存在与 queued 态；误领即 release。

## 2. 执行流（与开发切片同构，两处差异如实标注）

1. **定向 claim**：`POST /api/v3/projects/018fb5b0-0000-7000-8000-000000000006/work-items/claim`，body 带 `work_item_id`（全 UUID）+ runner_id（`s2a-claim-runner` 先例）+ `connection_generation`（fresh UUID）+ `lease_ttl_seconds: 86400`；dev 面 token（`fetch-token-dev.sh`）。
2. **无详设门**：缺陷项不携带 locked-gate 资产绑定（首例）——**不需要**资产四步，直接进实现。**观察点 A**：六门评估对无绑定缺陷项的适应性（core 六门应照常工作），如实记录。
3. **实现**：分支 `maestro/peixun/<work-item-id>`（base=origin/main）；通读目标 Rules 类全部分支→写 Test（覆盖每个公共方法+边界+fail-closed 路径；风格对齐既有 `*RulesTest`）→发现真实缺陷即修（修 = 生产代码变更，测试先行证明）。
4. **本地四面**：F25 跑法（钉镜像+egress 代理）unit/build/smoke/secret_scan 全绿（测试数增量如实）。
5. **MR→管线**：`先开 MR 再触管线`（F57）；GitLab API 建 MR（bot PAT 在 pilot-stack/gitlab-bot-pat）；管线四 job 绿。
6. **validation-runs**：`POST …/work-items/<id>/validation-runs`，producer 报 **local 类**（`g<N>-local`），coverage_ok 诚实 false。
7. **complete**：全 40 位分支头 SHA（F32）。六门 6/6（reconcile 走 002 映射面先例，admin 面，四要素）。
8. **SHA 钉定人工合并**（owner 先例）→ merged webhook → done。
9. **Jira**：raw REST 建号（issuetype **id 10101**）+ labels 两步（edit 204 加 maestro:done）+ manual 锚行（F42 psql 先例）+ 对账零未决。
10. **交接物**：`peixun-g<N>/` 下 FR-REGISTER + evidence.json（发现的真实缺陷逐条记录：现象/根因/修复/测试证明）。

## 3. 并行纪律

- 与并行片各占各工单各分支；**合并前若 main 前移先 rebase 进基再合并**；complete 用进基后分支头。
- GitLab 单 runner：管线排队属预期。
- 卡壳（claim 409 带 CLAIM_TARGET_HELD 等）按错误面语义处置并登记，不绕过。

## 4. 边界与观察点

禁改：Maestro 仓库（治理工具只读）；peixun 生产代码仅限目标缺陷修复所需最小面。**观察点汇报**（G3 观察台的输入）：A=无绑定项六门适应性；B=W8 并发领取实战时序（与并行片的 claim 时间差/错误面）；C=Mimosa ask 触发情况（如有，确认弹窗由 owner 点）。
