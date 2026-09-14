# V4 收敛仪式预制备（PREP-V4）

> **定位**：`plans/prep/pilot/` 工作层预制备件。出口评估通过后按此清单一次性执行收口 PR。当前状态：等待 09-26 出口评估。
> **前提**：V4-1 ✅ 蓝图全关 / V4-2 ⬜ D1 入 BOM（owner）/ V4-3 ⬜ Jira 受限账号（owner）/ V4-4 🔶 出口评估（S2C）。

## 1. 矩阵新增行（M4 原六行 + W4.5 试点新增行）

### M4 原六行（现有 not_started → implemented+passed）

| Task ID | 交付载体 | Evidence 指针 |
|---|---|---|
| M4-UI-001 | #90 console gen1 + #97 gen2 + #96 auth backend | CI 全绿 + Playwright 33/33 |
| M4-EVAL-001 | #84 harness + #95 ingest + dataset skeleton | AJV+PassPowerK 测试 + round-trip |
| M4-OBS-001 | #77 audit chain + #88 endpoints + #94 telemetry producer | PG 门控 + 端到端 SLO 测试 |
| M4-REL-001 | #79 SLO core + #80 backup + #81 drill + #88 SLO endpoint | RPO/RTO 实测 144ms/142ms |
| M4-RBK-001 | #81 restore + #85 webhook + #86 runner + G1 replay | 四类演练锚点全绿 |
| M4-PILOT-001 | #93 flags API + #108 prep + #112 PoC 首演 | 全链路 0→1 首演证据 |

### W4.5 新增行（Work Graph + 试点能力，需新增 Task ID）

| Task ID | 内容 | 交付载体 |
|---|---|---|
| M4-WG-001 | Work Graph 模型与资产台账 | #100 职能角色 + #101 ADR-009 + 0017 迁移 |
| M4-WG-002 | 拆解协议与调度 | #103 封板/聚合/协议 |
| M4-WG-003 | 工具面与控制台 | #104 25 工具 + 控制台三视图 |
| M4-JR-001 | Jira 连接器 | #102 锚定/镜像/对账 |
| M4-PT-001 | 试点准备与首演 | #108 双仓/Profiles + #112 PoC |
| M4-SP-001 | 共享栈保护 | #126 三道防线 + 库恢复 |

## 2. 需翻状态的权威文档

| 文档 | 现状态 | 目标状态 |
|---|---|---|
| `docs/delivery/m4-governance-console.md` | not_started/unverified | implemented/passed/HEAD |
| `docs/governance/traceability-matrix.csv` | M4 六行 not_started | 12 行（原 6+新 6）implemented+passed |
| `docs/README.md` §4.2 | M4 "进行中" | implemented + passed + v4 复盘链接 |
| `docs/README.md` §10 | "M4 进行中、尚未收敛" | "M0–M4 已分别通过 V0–V4 收口提交" |
| `docs/retrospective/v4-pipeline-retrospective.md` | 不存在 | 新建（全管线五波次复盘） |

## 3. V4 复盘模板（收口 PR 内产出）

```markdown
# V4 收敛复盘（治理控制台、评测、审计、可靠性与生产准入）

## 1. 目标提交与 CI Evidence
- 收口 PR：本 PR（HEAD 自引用）
- 全部 CI 工作流在本 PR 全绿

## 2. 联调剧本执行统计
- 影子期周报 W1–W4
- 出口评估三标准逐项证据
- 四大演练（备份恢复/Runner/GitLab 中断/DLQ）全绿

## 3. 偏离项统计
- 目标偏离 / 契约偏离 / 实现偏离 + 根因与预防

## 4. 五波次管线回顾
- W4（M4-P4）：切片化推进 + 契约 PR
- W4.5（Work Graph）：ADR-009 提前激活
- P5a/P5b（试点地基 + PoC 首演）
- W5/W6/S2（契约清理 + 灰度前提 + 影子期开工）
- P3（共享栈保护）

## 5. 角色签署
- product_owner（试点验收）
- technical_lead（总架构）
- qa_owner（评测/演练）
- security_owner（安全 Gate）
- operations_owner（SLO/恢复/Runbook）

## 6. 项目级收尾
- 并行度收益评估
- 质量环有效性
- 对下一版本的建议
```

## 4. 收口 PR 内容清单（一次性原子合入）

1. 矩阵 12 行全部 → `implemented,passed,HEAD`
2. M4 任务书 → `implemented/passed/HEAD`
3. DOC-INDEX §4.2 M4 行 + §10 全 M0–M4 通过句
4. `v4-pipeline-retrospective.md`（按模板填充）
5. V4 手册 §5 的其余要求（已在 #89 修订时写明）

## 5. 执行前置确认清单（出口评估通过时逐项核对）

- [ ] V4-2 D1 入 BOM 已完成
- [ ] V4-3 Jira 受限账号已换
- [ ] 出口评估三标准全过（零干扰确认 + 四面数据完整 + 灰度就绪建议正面）
- [ ] 影子期 ≥2 周周报归档（W1 + W2 至少）
- [ ] 全量门禁在收口 head 上全绿
