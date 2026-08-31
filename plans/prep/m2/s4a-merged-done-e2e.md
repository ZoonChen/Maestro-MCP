# M2-P4 预演：merged→done 端到端首次真实贯通（s4b/quality-endpoints @ 4858064）

> 工作层预演记录（离线本地完成）。**V2 剧本 #8（merged 真相）的首次真实运行**，早于 P5 正式联调。

## 1. 演练链路（全真实组件，零 mock）

```text
本地 git push maestro/e2e-proj/<work-item-id>（冻结命名绑定）
  → GitLab CE 17.9.1 沙箱建 MR !#3
  → 人工合并（root，模拟 HITL 终审）state=merged sha=ce73e2d48c3d
  → merge_request webhook（真实投递）
  → 收件箱验签 accepted（共享 token + Event-UUID 幂等）
  → GitLabSync 投影：merge_requests 行 mr_iid=3 state=merged has_sha=t
  → merged 事实驱动：work_item ready_for_human_merge → done
     merged_at=2026-08-31T14:09:24Z merge_sha=ce73e2d48c3d（与 GitLab 精确一致）
```

GL-INV-003 语义实测：**merged 事实是唯一可达 done 的写者**，SHA 双侧一致绑定。

## 2. 前置与坑（P5 复跑手册）

1. `gitlab_project_mappings` 种子（SQL 直插；S4a-1 的 onboarding REST 落地后替代）。
2. 沙箱 `maestro/*` 通配保护（#14 演习残留 push_access_level=0）需删除或调整为 broker 授权形态——**生产语义即 #14 结论：maestro/* 仅 broker 可推**，本次演练删保护仅为本地模拟，已记录。
3. MR 源分支必须先存在（首次 MR 建在未推送分支上会 opened-but-unmergeable）。
4. 环境：`MAESTRO_WEBHOOK_PAYLOAD_KEY` + `MAESTRO_GITLAB_WEBHOOK_TOKEN`（与沙箱 hook token 一致）+ remote_write=true。

## 3. 结论

M2 的灵魂用例（merged 真相 → done 首度开启）在沙箱端到端验证通过，早于 I1 剩余计划（S4a-1 onboarding REST、S3 broker、evidence 摄取触发）。P5 剧本 #8 执行时可直接引用本文拓扑与坑清单。
