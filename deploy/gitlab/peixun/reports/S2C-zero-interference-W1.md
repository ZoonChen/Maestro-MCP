# S2C 零干扰确认清单核验（W1，2026-09-13）—— brief-S2C S2C-2

> **定位**：影子期阶段 2 出口标准之一「零干扰确认」的分类底稿。
> 输入一：S2B 摩擦登记（`~/Works/yuandong/projects/peixun-s2b/S2B-FRICTION-REGISTER.md`，F1–F7）；
> 输入二：S2C-1 首份周报采集过程的新发现（O-1/O-2，见 `shadow-report-W1-20260913.md`）。
> 分类桶：**治理面误伤**（治理行为/缺陷损害了它本应服务的流转——进 Maestro 待办）/
> **合理阻断**（fail-closed 按冻结契约正确拒绝——记录不修）/ **环境问题**
>（部署/账号/网络配套缺位——进运维或试点侧待办）。

## 1. S2B 摩擦逐条分类

| 编号 | 摩擦（概） | 分类 | 依据 | 去向 |
|---|---|---|---|---|
| F1 结构·高 | v3 work_items `validating → ready_for_human_merge` 无生产写者，done 不可达 | **治理面误伤** | 治理面自身闭环断裂：合并已发生、Jira 已 merged，治理状态永卡 validating——旁路记录失真，正是影子期要守护的完整性。W1 周报 done=0 / validating×2 为直接证据 | Maestro 待办 S2C-A1 |
| F2 结构·高 | MR→WorkItem 绑定按映射项目解析，与治理域独立项目冲突（FK 23503，投影 defer 循环） | **治理面误伤** | 分支命名契约自带 project-key 却被忽略；MR 投影缺位 → 证据永不 complete → 第二重 done 阻断。周报「evidence=0」口径因它失效（勘误 E1） | Maestro 待办 S2C-A2 |
| F3 通路·中 | claim / gate 绑定 / 多签 approve 无 REST/MCP 面，store 面是唯一通路 | **治理面误伤** | brief 预设的「认证主体=开发会话」通路不存在，三动作均需进程内组合根=事实上只能脚本驱动；职能签核被迫回落 store 面（与 F4 复合） | Maestro 待办 S2C-A3 |
| F4 通路·中 | `/mcp` 对多项目会籍主体 fail-closed 且报 INTERNAL_ERROR | **治理面误伤** | 双缺陷：错误码（应 INVALID_PARAMETER+明确 message）+ 单会籍前提在单操作员多项目试点天然不成立；W5-1 通路对试点唯一操作员不可用 | Maestro 待办 S2C-A4 |
| F5 权限·低 | gates/evidence 读面 403（quality.read 无 project_admin 持有者） | **合理阻断**（冻结矩阵正确行为）+ 环境配套缺口 | RBAC fail-closed 按契约工作；缺的是 verifier/viewer 账号开通（S2 缺口④同族） | 试点侧待办（账号开通），不修 Maestro |
| F6 环境·中 | 沙箱 GitLab runner 持久 egress 代理缺位，CI 大量下载死在 squid 黑洞 | **环境问题** | 网络配套缺位，非治理行为；历史管线绿靠暖缓存掩盖。已处置：常驻 egress（squid 白名单）起在 maestro-gitlab 网络，p41/p42 复绿 | 配方待办：并入 resident-server 配方或 peixun-provision.sh |
| F7 文档·低 | asset_id 命名规则（`^ART-[a-z-]+-[0-9]{3,}$`）未写进团队手册，实踩 INVALID_PARAMETER | **合理阻断** + 文档缺口 | store 校验正确拒绝；团队不可知=文档问题，非行为问题 | 试点仓 MAESTRO-GUIDE.md 增补（S2C 会话不建议直接改试点仓——登记，随下次团队 MR 带入） |

## 2. S2C W1 采集过程新发现

| 编号 | 发现 | 分类 | 依据 | 去向 |
|---|---|---|---|---|
| O-1 | outbox 域事件（asset.\*/workgraph.\*，187 条）无 sink，dispatcher 每分钟空转重试（最老 22.6h）；仅 `gitlab.webhook.received`（102 条）有消费者 | **环境问题** + Maestro 改进项 | 常驻栈未配置域事件 sink（W5 只登记了事件类型契约）；空转重试无退避上限属产品语义缺口 | 配方/部署侧：sink 接线裁决；Maestro 待办 S2C-A5（无消费者时的退避或停发语义） |
| O-2 | SLO availability 样本饥饿：低流量项目窗口内个位数请求，单次 5xx 即翻 breached；SLO 端点自身 503 `SLO_AVAILABILITY_UNMEASURED` 疑似计入可用性分母（观察者效应） | **治理面误伤**（度量语义缺陷） | 实测 …006 治理域外项目一度 50% breached（伪信号），随后窗口内新成功请求又翻回 healthy——抖动即证据 | Maestro 待办 S2C-A6（UNMEASURED 不计入分母或最小样本门槛） |
| O-3 | 遥测历史断点：telemetry_aggregates 最早窗口=2026-09-13T01:13Z（S2A 库恢复事故 ART-incident-003 候选） | **环境问题**（已在案） | rolling_7d 跨事故周为部分窗口 | 已有登记（ART-incident-003 候选/交接项），周报勘误 E3 持续标注 |
| （勘误 E4） | MR reconcile 操作不在审计动作目录（12 枚举无 reconcile） | 契约观察项 | 对账操作数自动面不可见 | 随 W5 系契约清理后续裁决（非阻塞） |

## 3. 零干扰确认结论（W1，如实）

- **未检出治理面对团队正常开发的直接干扰**：W1 窗口内团队自然 MR 流量 = 0（团队手册 9/12 刚合入，日常开发尚未进场）；观测到的全部 MR（S2 演练 !3、S2B 治理 !5/!6）均为治理内流量。
- **「零干扰确认」在 W1 不可下达**：确认的前置=非零的团队自然流量样本（目标：连续 ≥2 周周报中团队 MR 非零且零误伤新增）。当前只能记录为「未检出干扰，样本=0」。
- **误伤项合计 6 项**（F1/F2/F3/F4/O-2 + O-1 的产品侧半项）已登记 Maestro 待办（见调度板 §7 S2C 段）；其中 F1/F2/F3 直接构成灰度期（阶段 3）硬前提——Agent 接手缺陷流转依赖 done 链与领取通路闭合。

## 4. 与出口评估的接线

本表是出口评估包「标准① 零干扰确认」的逐周累积底稿：W2 起每周周报的「异常与摩擦」自动检出 + 本表追加行，≥2 周后由出口评估包（`exit-assessment-SKELETON.md`）汇总裁决。
