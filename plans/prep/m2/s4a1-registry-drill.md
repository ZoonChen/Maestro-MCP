# M2-P4 预演：S4a-1 /api/v3 对齐 + GitLab 注册表验证（s4a/connector-rest @ 397c8fc，PR #52）

> 工作层预演记录（离线本地完成，栈式于 #49 之后）。

## 1. 验证结果

| 项 | 结果 |
|---|---|
| 套件（ControlPlane/GitLab/Quality） | ✅ 全绿 |
| 迁移 0008 往返 | ✅ applied=8；列级迁移（表数 29 不变），revert+re-apply 零漂移 |
| webhook 接收器路径迁移 | ✅ 实测：新路径 `/api/v3/webhooks/gitlab/{id}` 稳定 `INSTANCE_UNKNOWN`；旧根路径 404 已移除 |
| 人类面鉴权设计 | ✅ 复核：`controlPlaneActions` 冻结权限表 + 同验证器再鉴权——/api/v3 引擎级豁免不构成人类面裸奔 |

## 2. 关键设计确认（契约路径对齐）

质量端点与 GitLab 注册表从首片落点（/api/v1、根路径）**迁回冻结契约声明的 /api/v3 基准**；设备面（runner）、共享令牌面（webhook）、人类面（注册表/质量/豁免）三种 scheme 在同一前缀下各司其职。`gitlab_instance.configure`（平台管理员）/`project.repository.manage`（映射）/`waiver.approve` 等权限映射与冻结 x-maestro-permission 一致。

## 3. 演练衔接备忘

- 后续沙箱演练的 hook URL 需更新为新路径（含 instance_id）。
- 静态组合下人类 /api/v3 面（注册表/质量）仍为 OIDC 专属（同 #50 结论口径）；onboarding REST 的组合级演练归 Keycloak 栈时段。

## 4. 增量补录（分支头 8661abb：API client + MR reconcile）

- `internal/gitlab`（含 client 与对账）+ handler 套件全绿。
- **演练约束发现**：`gitlab_instances.base_url` 的 `CHECK (base_url ~ '^https://')` 使本地 http 沙箱（127.0.0.1:8181）无法注册为实例——reconcile 的真机演练需要 TLS 终接代理，或归入 Keycloak/HTTPS 组合时段。该 CHECK 是正确设计（SSRF 姿态），仅记录本地演练路径。
