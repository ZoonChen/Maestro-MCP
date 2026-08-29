# S6 控制台 IA 设计与登录态底座（W1 后半预备，供 M4-P1/I4 消费）

> 工作层预备产物，不是权威真源。权威依据：`docs/prd/web-dashboard.md`（`UI-REQ-001`、`UI-REQ-002`、`UI-RULE-001..004`）、`docs/prd/roles-and-scenarios.md`（`ROLE-REQ-001`、`ROLE-REQ-002`、`ROLE-RULE-001..004`）、`docs/security/identity-rbac.md`（Authorization Code + PKCE、服务端授权上下文）。冲突时以上述文档为准。

## 1. 控制台 IA（角色化信息架构）

在既有只读 Dashboard（Overview / ProjectBoard / FeatureView / TaskDetail / SessionPanel / ActivityLog）之上按角色演进，导航结构一次定型、能力分波次点亮：

| 区域 | 一级导航 | 主要角色 | 点亮波次 |
|---|---|---|---|
| 态势 | Overview（现状基线与计数） | 全角色 | 已有（M0） |
| 执行 | Work Items（看板/列表切换）、Task Detail、Sessions/Workers | coordinator、developer、verifier | 已有；W4 补写操作 |
| 质量 | MR / Pipeline 视图、Quality Gates、Evidence 链 | qa_owner、technical_lead | W2 起 mock 数据 |
| 缺陷 | Defects（含 HITL 分派队列）、Findings 溯源 | coordinator、qa_owner | W3 接 S5 数据契约 |
| 治理 | HITL 审批队列、豁免申请、审计链检索/导出 | 各 owner 角色 | W4（M4-UI-001） |
| 管理 | 项目/成员/Runner 注册、策略版本 | project admin | W4 |

IA 规则：角色只影响**可见区域与操作**，不影响数据契约（同一 API 按服务端授权裁剪）；未知角色进入只读态势视图并在页面明示权限边界；每个视图保持"错误/降级永不为空"（`UI-RULE` 系列）。

## 2. 登录态组件底座（本轮交付：`web/src/auth/`）

### 2.1 认证流契约假设（待 I1 OIDC 实装后冻结进 control-plane.yaml）

| 端点（提案名） | 语义 |
|---|---|
| `GET /auth/session` | 返回当前 principal/角色/项目范围；未认证返回 401；auth 未启用返回 404 |
| `GET /auth/authorize` | 重定向到 IdP（Authorization Code + PKCE S256，state + nonce） |
| `GET /auth/callback` | IdP 回跳，后端完成 code 交换，建立会话（HttpOnly Cookie） |
| `POST /auth/logout` | 终止会话并吊销 |

最终命名/形状以冻结后的机器规范为准；`authClient.js` 的路径集中在 `createAuthConfig()` 一处，契约变更只改一处。

### 2.2 组件与职责

- `authClient.js`：`createAuthConfig()` 集中端点路径；`fetchSession` / `buildLoginURL`（携带 return-state）/ `logout`。PKCE 的 code_verifier 由后端持有（verifier 不出服务端）；前端仅生成一次性 return-state 存 sessionStorage，用于回跳一致性校验。会话凭证不进入 JS 可读存储。
- `useAuthSession.js`：状态机 `probing → authenticated | unauthenticated | auth-disabled | error`；探测 `/auth/session`（404 = auth-disabled，维持现状只读路径）。
- `LoginGate.jsx`：未认证时渲染登录视图（登录按钮触发 `buildLoginURL` 跳转）；回跳 state 不一致/会话过期展示稳定错误与重试；aria-live 播报状态。
- `AppWithAuth.jsx`：`LoginGate` 包裹现有 `App`。**默认不接线**（`main.jsx` 不变），M4-UI-001 落地时切换入口——保证 M0 回归与现有 e2e 不受影响。

### 2.3 红线落实

- Token 不入 localStorage/URL/bundle/日志；会话凭证只存在于 HttpOnly Cookie，前端仅持 principal 元数据。
- state/nonce 校验失败、code 重放、401 会话过期一律回到登录视图并给出稳定错误码，不静默重试 Authorization Code 交换。
- 前端不解析/不缓存权限决定——角色仅用于 IA 展示，授权判定全部以服务端 `authorize()` 为准（`UI-RULE` 与 identity-rbac 口径）。

## 3. 演进路线（对齐 m4 阶段计划）

1. 本轮（W1 尾）：IA 设计 + auth 底座组件（不接线、不加依赖、不破坏现有门禁）。
2. W2：MR/pipeline 视图（mock 数据）+ HITL 队列原型（消费 S4 契约提案）。
3. W3：agent_run 轨迹接入评测数据集准备。
4. W4：M4-UI-001 接线真实 OIDC、八场景 + a11y + 真实浏览器 DOM 测试（Playwright page 用例）。
