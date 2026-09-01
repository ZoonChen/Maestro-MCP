# V2 收敛就绪预审（证据索引 + 缺口地图）

> 工作层预审（离线本地完成），供 I2/V2 收敛仪式消费。基线：`origin/main @ 4ff6b21` + 已推分支（s4b/evidence-engine）。权威判定：`docs/delivery/m2-gitlab-quality-loop.md` Exit Gate + `plans/convergence/v2-gitlab-quality.md`。

## 1. 剧本 #1–#11 × 当前证据

| # | 场景 | 已有证据 | 缺口 |
|---|---|---|---|
| 1 | Webhook 签名 | **#44** 手工矩阵（401 TOKEN_INVALID 稳定码）+ **#45** 真沙箱投递 | S4a 落地后补"签名事件无业务效果"端到端断言 |
| 2 | 重复/乱序/重放 | **#44**：同 Event-UUID 重放 202 EVENT_DUPLICATE 幂等 | 乱序（版本序）路径待 dispatch 消费面实装后测 |
| 3 | SHA 漂移 | **S4b-2 引擎测试**（stale 重评在 PG 套件覆盖） | 端到端（webhook→gate stale）待 S4a |
| 4 | Required Gate 缺失 | 引擎测试（missing→blocked；diagnostic 永不满足） | 同上，端到端归 S4a/P5 |
| 5 | 不可豁免四类 | waiver 引擎测试（生命周期在库） | **负测试待 S4b-3 REST 接线后**（waiver 越权） |
| 6 | 豁免流程 | TestQualityWaiverLifecycle（绑定/到期语义） | 申请人不能自批——需 OIDC 面实测（Keycloak 栈） |
| 7 | 保护分支 | **#14 沙箱实测**（Bot scope 拒绝/纵深拒绝/GL-GATE-003 证据） | maestro/* 仅 broker 可推——待 S3 git broker 实装 |
| 8 | merged 真相 | 状态机 done 入边契约已冻结（#40） | **核心缺口**：S4a MR 同步落地后端到端首验 |
| 9 | Evidence 权威分离 | 引擎测试 + **#42 触发器不可变实测** | — |
| 10 | GitLab 中断 | #14 对账字段清单 | 降级/恢复演练待 S4a |
| 11 | M0/M1 回归 | CI 每次全量 | — |

## 2. §4 审计专项 × 现状

| 审计项 | 现状 |
|---|---|
| Evidence append-only 触发器级测试 | ✅ 引擎套件 + #42 实测（UPDATE/DELETE 拒绝） |
| 策略层级只增强 | ✅ TestResolveEffectiveRejectsWeakening + scope 负测试 |
| Webhook Secret 生命周期 | 部分：env-ref 逐请求解析/轮换免重启/白名单实测（#44）；生成与存储句柄清单待 S4a onboarding |
| 供应链（SBOM/签名/provenance） | CI 已有 build-test-sbom 工作流；镜像签名/provenance **未启动** |
| core-coverage 扩展（evidence/gate） | 待 S4b 合入后按 #19 模式实测基线 |

## 3. 关键路径结论

V2 剩余工作高度集中在 **S4a（连接器+MR 同步+onboarding）与 S3 git broker**——剧本 #1/#2/#7/#8/#10 的端到端形态都等它们；S4b 线（收件箱+引擎）已具备收敛级证据。**建议 V2 前置排序：S4a 优先于 S4b-3 收尾**（REST 接线可并行），使 merged→done 主链尽早首验。

## 4. 已备弹药索引

#14（GitLab 实证+三偏差）、#42（0004 钻孔+触发器）、#44（收件箱七用例+豁免修复）、#45（真投递+静态加密实测）、S4b-2 独立验证（本地待推）；沙箱容器与钻孔 PG 保持待命，回网即可复跑。
