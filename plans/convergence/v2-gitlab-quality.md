# V2 收敛手册：M2 Exit（GitLab / MR / Pipeline / 权威质量闭环）

> 执行者：I2 集成会话。权威判定：`docs/delivery/m2-gitlab-quality-loop.md` Exit Gate。环境：本地 GitLab CE 容器（S1 Compose 提供）；公司 GitLab 接入为可选升级。

## 1. 入口条件

- S4a/S4b/S3 交接物齐备：Webhook 恰好一次实测、Gate/豁免演示脚本、Secret 管理清单、git broker 推送边界实测
- `done` 入边契约 PR 已合入（状态机咽喉点变更）

## 2. 联调剧本

| # | 场景 | 预期 |
|---|---|---|
| 1 | Webhook 签名 | 无效/缺失签名事件无业务效果，仅入审计与 DLQ 统计 |
| 2 | 重复/乱序/重放 | 去重键 + 版本序：恰好一次正确状态变化 |
| 3 | SHA 漂移 | source/target SHA 变化 → ready 立即阻断、全部旧 Evidence stale |
| 4 | Required Gate 缺失 | 缺任一 Required Gate（build/unit/lint/coverage/secret_scan/sast/dependency/image/license…）不得 ready；`missing/skipped/unknown` 均阻断 |
| 5 | 不可豁免项 | 身份隔离/SHA 完整性/策略完整性/Webhook 真实性四类无豁免路径（负测试） |
| 6 | 豁免流程 | 有效 waiver：不同主体审批、≤7 天、绑定 SHA、到期失效；申请人/作者/Agent 不能自批 |
| 7 | 保护分支 | 尝试以任何组件推送/合并保护分支、调用 merge API：双重防护全拒并审计；`maestro/*` 分支仅 Runner host git broker 可推 |
| 8 | merged 真相 | GitLab 侧人工合并 → webhook → `ready_for_human_merge → done`（首度开启） |
| 9 | Evidence 权威分离 | 本地 diagnostic Evidence 无法满足 merge_gate（TC-EVIDENCE-004 语义） |
| 10 | GitLab 中断 | 停容器 → 只读缓存、不新授权、不标完成；恢复 → 对账无丢失/重复 |
| 11 | M0/M1 回归 | 全量零回归 |

## 3. 全量门禁

V0 第 3 节命令集 + m1-runtime 全绿；SLO 抽查：事件到达 60s 收敛、Evidence 齐备 30s 出 Gate 结论。

## 4. 查缺补漏审计（专项）

- [ ] Evidence append-only + supersedes 链有 DB 触发器级测试
- [ ] 策略层级（company → project → task）只增强不放宽有负测试
- [ ] Webhook Secret/Token 全生命周期（生成/存储句柄/轮换）审计
- [ ] 供应链：SBOM 进 CI、镜像签名/provenance 在发布路径可用
- [ ] core-coverage 清单扩展（evidence/gate 引擎核心文件）

## 5. Exit Gate 状态翻转

m2 任务书 + 矩阵 M2 六行 + `docs/README.md` 第 4.2 表（模式同 V0）。

## 6. 角色签署

security_owner（Webhook/Secret/供应链）、qa_owner（Gate/Evidence/Waiver）、technical_lead 总签；operations_owner 签署 GitLab 中断降级与对账。

## 7. 复盘与下波输入

`docs/retrospective/v2-retrospective.md`；输出 W3 开局清单：I3 契约冻结范围（Finding/Defect/预算/Agent 工具面）、S5a/S5b 分工、试点仓库与红队集到位确认。

## 8. 可选升级（不阻塞）

公司自建 GitLab 接入（host/onboarding 在真实实例重演第 2 节剧本 1–8）。
