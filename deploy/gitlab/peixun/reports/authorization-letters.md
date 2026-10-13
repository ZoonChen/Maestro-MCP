# 职能角色授权书（V4-7 正式化）

> 蓝图 §4.2 模板一句话版。签发人：陈自恒（owner）。
> 本文件登记为 `internal` 敏感度资产入台账，audit 事件 `asset.approved`。

## 授权清单（当前活跃职能主体）

| 主体 | 职能 | 授权范围 | 有效期 | 撤销条件 |
|---|---|---|---|---|
| pilot-admin（Keycloak 用户） | technical_lead | peixun BOM 治理域（018fb5b0-…-006）：详设审批、gate 绑定、reconcile、合并裁决 | 长期（试点存续期） | owner 书面撤销或试点终止 |
| pilot-admin（同上） | product_owner | 同上：出口评估双签、灰度决策、flags 置位 | 同上 | 同上 |
| pilot-admin（同上） | qa_owner | 同上：waiver 审批、质量门禁 | 同上 | 同上 |
| pilot-admin（同上） | security_owner | 同上：安全审查、Secret 纪律 | 同上 | 同上 |
| pilot-admin（同上） | operations_owner | 同上：运维操作、备份/恢复、变更窗口 | 同上 | 同上 |
| pilot-qa（Keycloak 用户） | qa_owner | 同上（独立主体，用于 SoD 分离场景） | 同上 | 同上 |

## 授权依据

- R1 裁决（2026-09-14）：技术负责人兼任 technical_lead 职能成立（owner 批准）
- R4 裁决（2026-09-14）：授权签发人=陈自恒（owner）
- 蓝图 §4.2：一句话版+台账登记+audit 事件

## 备注

- pilot-admin 同时持有五职能是试点期特殊安排（owner 身份合并）——推广期应拆分为独立主体
- pilot-qa 为 D3 创建的独立 qa_owner 主体（SoD 备用）
- 授权书本身作为 legacy-intake 类资产入账
