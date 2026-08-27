# V3 收敛手册：M3 Exit（契约 / 跨仓集成 / Defect / Agent 修复闭环）

> 执行者：I3 集成会话。权威判定：`docs/delivery/m3-integration-defect-automation.md` Exit Gate。环境：两个试点仓库 + 本地 GitLab CE 真实 MR 通道（不再用 mock）。

## 1. 入口条件

- S5a/S5b 交接物齐备：红队注入用例执行记录、预算对账报告、Agent 轨迹样本
- S4 MR 通道、S3 沙箱执行面、S1 表迁移全部就绪

## 2. 联调剧本

| # | 场景 | 预期 |
|---|---|---|
| 1 | 跨仓 breaking change | 试点仓 A 改接口、仓 B 依赖断裂 → IntegrationRun 阻断 + 生成明确责任任务（含 fingerprint 去重） |
| 2 | compatible change | 向后兼容变更不阻断（golden cases 判定） |
| 3 | Pipeline 失败归一 | 同一根因的多处失败 → 唯一 Defect（occurrence 聚合）；不同根因不误合并 |
| 4 | Agent 闭环（正路径） | Defect 分派 → Agent 预算内复现 → 修复 → 测试 → MR → GitLab CI 复测通过 → Defect 关闭（关闭证据绑定 pipeline） |
| 5 | Agent 闭环（停止路径） | 预算耗尽 / 无法复现 → 停止并 handoff 人工，任务状态与审计明确；不输出无证据"已修复" |
| 6 | 注入不扩权 | 红队集：仓库文本/pipeline 日志/Defect 描述中的注入指令不能扩大工具与数据权限 |
| 7 | Agent 边界 | Agent 改状态机/权限/Gate 的任何尝试被拒；高危动作触发人检点 |
| 8 | 预算红线 | 每次 LLM 调用前有预算检查、调用后记真实用量；对账报告与 provider 记录一致 |
| 9 | M0–M2 回归 | 全量零回归 |

## 3. 全量门禁

V0 第 3 节命令集 + m1/m2 CI 工作流全绿；评测数据集先导跑一轮（S6 提供，为 W4 预热）。

## 4. 查缺补漏审计（专项）

- [ ] 六类 Finding 来源全部有真实摄取测试（不只 mock）
- [ ] Defect fingerprint 稳定性：同一问题跨运行不重复建单
- [ ] Agent 工具面白名单与 Command Profiles 引用一致（无任意命令串、无 token 透传）
- [ ] 预算台账与审计事件原子性
- [ ] core-coverage 清单扩展（contract/defect/budget 核心文件）

## 5. Exit Gate 状态翻转

m3 任务书 + 矩阵 M3 六行 + `docs/README.md` 第 4.2 表（模式同 V0）。

## 6. 角色签署

qa_owner（契约/集成/Defect 判定）、security_owner（Agent 边界与红队）、technical_lead 总签；product_owner 验收责任任务可读性。

## 7. 复盘与下波输入

`docs/retrospective/v3-retrospective.md`；输出 W4 开局清单：I4 契约冻结范围（控制台/评测）、四演练排期、试点仓库清单（2–5 个）与影子运行计划。

## 8. 可选升级（不阻塞）

公司 GitLab 上重跑剧本 1/4/5（真实 CI runner 与网络时延）。
