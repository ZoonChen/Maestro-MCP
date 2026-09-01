# M2-P4 预演：质量环首段（job→evidence→门禁评估）真实贯通（s4b/quality-loop @ 76fc05d，PR #54）

> 工作层预演记录。环境：宿主 server + 钻孔 PG（迁移 0001–0009）；投递为契约形状的 Job/Pipeline Hook（header/字段与 #14 实测捕获一致）。

## 1. 演练链路（全部命中）

```text
Pipeline Hook（id=77, success）→ pipeline 投影
Job Hook ×14（gate 命名, success, 任务分支 ref, 精确 SHA）
  → 收件箱验签 accepted → outbox 信封 → GitLabSync 消费
  → pipeline_jobs 行 + evidence 12 条（authority=merge_gate, producer=gitlab_job, 绑定 MR SHA 元组）
  → 每事实变化触发确定性重评 → gate_snapshots 12 项 passed
  → work_item 保持 validating（2 个内算门禁 pending 正确阻断）
```

## 2. 语义验证要点（全部实测）

1. **乱序容忍**：job 先到、pipeline 未投影 → 信封 deferral 退避重试，pipeline 到位后自动收敛（"pipeline not projected yet, job deferred"→delivered）。
2. **门禁名白名单**：`unit-test`/连字符名不产证据（unknown producers never become evidence）；仅规范 ID（`unit`、`lint_typecheck`、`secret_scan`…）生效。
3. **SHA 精确绑定（EVIDENCE-RULE-002）**：jobSHA ≠ 元组 sourceSHA 即弃——合成链中实测该路径。
4. **内算门禁正确拒判合成数据**：`policy_integrity`/`baseline_freshness` 保持 pending（假 baseline `000…` 不满足新鲜度）——失败即设计，阻断合成 ready。
5. 证据稳定 ID（pipeline+job 派生）重投收敛为单条。

## 3. 前置条件（P5 复跑清单）

mapping 种子（S4a onboarding REST 落地后替代）；MR 投影需含 `work_item_id`（真实链路由 MR 事件投影自动推导，#51 已证；SQL 种子需自带）；job 事件需 pipeline 先投影。

## 4. 结论与剩余

质量环首段（事实→证据→门禁→ready 判定）在真实组件上贯通：12/12 CI 门禁路径验证，2 个内算门禁的满足条件留给真实 baseline（P5 级真 MR + 注册 runner 的全真跑）。加上 #51 的 merged→done 末段，**整个 V2 质量闭环的两半均已各自真实验证**。
