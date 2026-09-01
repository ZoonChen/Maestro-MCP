# M2-P5 预演：注册 runner 的真 CI 全真跑（s4b/quality-loop @ 76fc05d）

> 工作层预演记录。沙箱注册真实 gitlab-runner（shell executor，项目级 token），11 个门禁 job 由**真实 CI** 执行并全绿；真实事件链首次全量进入收件箱与投影。中途钻孔库被外部会话重置两次，证据挂接的最后一格未及完成——已获发现比闭环本身更有价值。

## 1. 已验证（全真实组件）

| 环节 | 结果 |
|---|---|
| runner 注册（项目 registration token，新式 verify 流程） | ✅ alive |
| 11 个门禁 job 真实执行（shell executor） | ✅ pipeline success |
| 真实事件进箱（push/MR/pipeline/job 全订阅） | ✅ 44 job + 3 pipeline + MR + push 全 accepted |
| 投影同步 | ✅ pipelines/jobs/mrs 行齐；**work_item_id 由任务分支命名自动推导**（真实 MR 事件驱动，非种子） |

## 2. 关键发现（P5 必读）

1. **`image` 门禁名与 GitLab 根级保留字冲突**：.gitlab-ci.yml 不可能存在名为 `image` 的 job（被解析为全局 image 配置，lint 报 `unknown keys: script`）——该门禁在真实 GitLab CI **结构性不可满足**。需 I1 处置：门禁更名（如 image_scan）或摄取器接受别名。本次以 11 门禁推进。
2. **元组时序依赖（最重要）**：MR webhook 不携带 target SHA → 投影 target_sha 为 NULL → **元组完备依赖 reconcile（#52 API client）回填**；而 IngestJob 对元组未完备的 job **永久跳过（无重排队）**——即 reconcile 必须在管线完成前补齐 target_sha，否则该管线的 job 证据全部丢失。建议：job 摄取对 incomplete tuple 走信封 deferral（与 job-before-pipeline 同机制），或 OnTupleComplete 时对已落 pipeline_jobs 的终态 job 补偿摄取。
3. **hook 运维坑**：PUT 更新 hook URL 不带 token 参数会重置 token → TOKEN_MISMATCH。
4. 演练环境：共享钻孔 PG 会被并行会话的测试套件重置——P5 正式跑需独立实例。

## 3. 与既有证据的拼图

- 首段合成链（#56）证明 job→evidence→门禁→判定；本预演证明**真实 CI 事件流+投影+分支绑定**全通；
- 两者相差的唯一一格（真实 job 证据挂接）被发现 2 的时序语义解释——这不是缺陷验证失败，而是把该语义从代码注释变成了实测事实。
- P5 正式跑清单：独立 PG + reconcile 周期先于管线完成 + image 门禁处置 + hook 带全事件订阅与 token。
