// Shared governance-console constants (M4-UI-001 first generation).
// The PRD (docs/prd/web-dashboard.md §4) owns the eight scenario
// journeys; this module only projects them into navigation shape. The
// console renders exactly these ids, so DOM tests assert against them.

export const ROUTES = {
  overview: '#/',
  waivers: '#/waivers',
  mrs: '#/mrs',
  scenarios: '#/scenarios',
  pilot: '#/pilot',
  jira: '#/jira',
  workgraph: '#/workgraph',
  assets: '#/assets',
  proposals: '#/proposals',
};

export function viewFromHash(hash) {
  if (hash === '#/waivers') return 'waivers';
  if (hash === '#/mrs') return 'mrs';
  if (hash === '#/scenarios') return 'scenarios';
  if (hash === '#/pilot') return 'pilot';
  if (hash === '#/jira') return 'jira';
  if (hash === '#/workgraph') return 'workgraph';
  if (hash === '#/assets') return 'assets';
  if (hash === '#/proposals') return 'proposals';
  if (hash === '#/admin') return 'admin';
  if (hash === '#/operations') return 'operations';
  if (hash.startsWith('#/project/')) return 'project';
  return 'overview';
}

export function projectFromHash(hash) {
  if (hash.startsWith('#/project/')) {
    const id = hash.slice('#/project/'.length);
    return id || null;
  }
  return null;
}

// The eight PRD §4 journeys. `linked` scenarios already have a console
// view this generation; `skeleton` ones render the honest empty state
// with the journey outline until their backend surfaces land.
export const SCENARIOS = [
  {
    id: 'scenario-oidc-mcp',
    title: 'OIDC 登录与远程 MCP',
    prd: '§4.1',
    roles: ['全角色'],
    status: 'linked',
    linkTo: null,
    summary: 'Authorization Code + PKCE 登录，建立 HttpOnly 会话；远程 MCP 客户端以同一身份初始化。',
    steps: ['登录 / 连接', '授权码 + PKCE', 'IdP 校验 claims', '建立安全会话', '载入控制台能力'],
  },
  {
    id: 'scenario-runner-lifecycle',
    title: 'Runner 注册、批准与吊销',
    prd: '§4.2',
    roles: ['project admin'],
    status: 'skeleton',
    linkTo: null,
    summary: '一次性注册码、设备证明、项目范围批准与带因吊销；吊销立即拒绝新租约。',
    steps: ['创建注册码', 'Runner 注册 + 设备证明', '影响摘要待批', '批准项目范围', '吊销（含原因）'],
  },
  {
    id: 'scenario-lease-heartbeat',
    title: 'Lease、心跳、离线与恢复',
    prd: '§4.3',
    roles: ['coordinator', 'operations'],
    status: 'skeleton',
    linkTo: null,
    summary: '长轮询租约与心跳；失联标记 suspect/offline，超时过期后旧 epoch 拒绝并重新入队。',
    steps: ['认领租约', '心跳上报', '失联告警', '租约过期', '旧 epoch 拒绝 + 重新派发'],
  },
  {
    id: 'scenario-gitlab-onboarding',
    title: 'GitLab 项目接入与 Webhook',
    prd: '§4.4',
    roles: ['platform admin', 'project admin'],
    status: 'skeleton',
    linkTo: null,
    summary: '受批实例与仓库映射、TLS/机器人范围校验、签名 Webhook 持久化入站与对账。',
    steps: ['配置受批实例', '校验 TLS 与范围', '展示范围与 Webhook', '签名事件入站', '同步状态/对账'],
  },
  {
    id: 'scenario-mr-pipeline',
    title: '任务分支、MR、Pipeline 与人工合并',
    prd: '§4.5',
    roles: ['developer', 'qa', 'technical lead'],
    status: 'linked',
    linkTo: ROUTES.mrs,
    summary: '服务器生成任务分支、机器人建 MR；GitLab CI 证据回流，最终合并只能由人完成。',
    steps: ['基线 + 任务分支', '主机代理推送', '机器人建/更 MR', 'Pipeline 证据回流', '人工评审合并', 'done + 审计'],
  },
  {
    id: 'scenario-cross-repo-e2e',
    title: '前后端跨仓联调',
    prd: '§4.6',
    roles: ['coordinator', 'qa'],
    status: 'skeleton',
    linkTo: null,
    summary: '选定前后端 SHA 组合，契约引擎校验兼容性，部署精确制品并产出联合 E2E 证据。',
    steps: ['选择前后端 SHA', '契约哈希/差异校验', '部署精确制品', '联合 E2E', '组合结果与责任'],
  },
  {
    id: 'scenario-defect-remediation',
    title: 'Defect 到 Agent 修复',
    prd: '§4.7',
    roles: ['coordinator', 'qa'],
    status: 'skeleton',
    linkTo: null,
    summary: '缺陷分派与资格评估、预算门控的修复执行、证据回流并创建 MR 供人工评审。',
    steps: ['批准可修复缺陷', '范围与预算门控', '复现/修改/测试', 'Diff 与证据回流', '建 MR 请求 CI', '人工评审'],
  },
  {
    id: 'scenario-gate-waiver',
    title: 'Gate 豁免、撤销与应急恢复',
    prd: '§4.8',
    roles: ['security owner', 'qa owner', 'project admin'],
    status: 'linked',
    linkTo: ROUTES.waivers,
    summary: '限时豁免绑定 MR/SHA/检查项；独立审批人批准；应急撤销即时生效并失效相关决定。',
    steps: ['申请豁免（原因/期限）', '影响与证据展示', '独立批准/拒绝', '限时生效 + 审计', '应急撤销/恢复复批'],
  },
];

// Copy shown next to write actions so operators know which frozen
// permission each action needs; absence of a hint never implies access.
export const ACTION_PERMISSION_HINTS = {
  'waiver.request': 'project_admin / platform_admin',
  'waiver.approve': 'security_owner / qa_owner（且不得为请求人）',
  'waiver.revoke': 'project_admin / security_owner',
  'gitlab.reconcile': 'project_admin / platform_admin',
  'audit.export': 'platform_admin / security_owner（职能角色）',
  'webhook.dead_letter.replay': 'gitlab.reconcile（project_admin / platform_admin），审批人必须不同于请求人',
  'pilot.write': 'platform_admin（本代控制台只读展示）',
  'project_policy.strengthen': 'project_admin',
  'workgraph.seal': 'technical_lead（职能角色，J4 终态）',
  'asset.read': '全部项目角色（viewer 级读）',
  'asset.register': 'developer / coordinator',
  'asset.review': 'technical_lead / qa_owner（职能角色）',
  'asset.approve': 'product_owner / technical_lead / qa_owner / operations_owner（职能角色；按制品类型的分工由 reviewers 名单约束）',
};

// AppWithAuth flattens the auth session into { status, principal, roles,
// logout }; only authenticated sessions carry roles.
export function rolesOfAuth(auth) {
  if (auth && auth.status === 'authenticated') {
    return Array.isArray(auth.roles) ? auth.roles : [];
  }
  return [];
}
