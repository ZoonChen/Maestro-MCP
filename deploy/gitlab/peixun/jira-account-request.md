# Jira 专用账号申请单（Maestro 控制面连接器——替换 rd03 共用管理员账号）

> 申请人：陈自恒（owner）｜提出：集成会话 2026-10-12｜用途背景：V4 欠账 V4-3（Jira 连接器现为 rd03 系统管理员凭据，违反最小权限，蓝图 §1 SoR 矩阵要求专用受限账号）。
> **只需 1 个账号**。开通后由集成会话完成凭据切换与回归验证，管理员随后可回收 rd03 的 PAT。

## 账号规格

| 项 | 值 |
|---|---|
| 账号名（建议） | `maestro-svc` |
| 类型 | Jira 服务账号（非个人，不挂单人邮箱登录用途） |
| 显示名 | Maestro 控制平面连接器 |
| 邮箱（建议） | maestro-svc@（运维组代管邮箱） |
| 加入组 | `jira-software-users`（如无则默认用户组） |
| 项目成员 | 仅 `PEIXUN` 项目 Members |

## 所需权限（全部项目级，无任何全局管理权）

| 权限 | 用途（连接器实际调用面，internal/jira/client.go） |
|---|---|
| Browse Projects | 对账搜索（POST /rest/api/2/search）与镜像读取（GET /rest/api/2/issue/{key}） |
| Create Issues | 工作项锚定建号（POST /rest/api/2/issue；PEIXUN-21…37 先例） |
| Edit Issues | labels 两步贴标/状态镜像/assignee 写回（PUT /rest/api/2/issue/{key}） |

**明确不需要**（当前 rd03 多余的权）：Administer Jira、Manage Users、全局配置、其他项目的任何权限、删除权限（Delete Issues 不需要——连接器只建不改删）。

## 交付物（开通后）

1. 该账号的 **API Token**（Jira Server：PAT，Profile→Personal Access Tokens 生成；权限勾 read/write API）
2. 交回方式：写入本机 `pilot-stack/jira-pat`（0600）或经运维渠道递交
3. 集成会话将执行：连接器凭据切换 → 全链回归（建号/镜像/对账三面）→ 回收建议（rd03 的 PAT 在验证通过后由管理员吊销）

## 为什么只有这一个账号

Maestro 对 Jira 的全部读写都经唯一连接器实例（镜像 worker + 对账 + 锚定推送共用一套凭据，蓝图 §1.2 单向锚定语义）；职能角色（product_owner/technical_lead 等）的签核发生在 **Maestro 台账**而非 Jira，不需要 Jira 侧账号。
