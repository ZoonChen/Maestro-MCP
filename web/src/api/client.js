export class APIError extends Error {
  constructor(message, { status = 0, code = 'REQUEST_FAILED', correlationId = '' } = {}) {
    super(message);
    this.name = 'APIError';
    this.status = status;
    this.code = code;
    this.correlationId = correlationId;
  }
}

// A rejected session surfaces as a window event so the auth shell can
// re-probe and route back to the login view instead of leaving a stale
// page. The event carries no credential, only the failure fact.
export const SESSION_EXPIRED_EVENT = 'maestro:session-expired';

async function responseBody(response) {
  const contentType = response.headers.get('content-type') || '';
  if (!contentType.includes('application/json')) return null;
  try {
    return await response.json();
  } catch {
    return null;
  }
}

// The v1 tree answers { data } envelopes while the frozen /api/v3 tree
// returns the bare contract payload; both shapes resolve to one value.
function payloadOf(body) {
  if (body && typeof body === 'object' && 'data' in body) return body.data ?? null;
  return body ?? null;
}

function errorHeaders(options) {
  const headers = {};
  if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey;
  if (options.ifMatch) headers['If-Match'] = options.ifMatch;
  if (options.ifNoneMatch) headers['If-None-Match'] = options.ifNoneMatch;
  return headers;
}

export async function apiRequest(path, { method = 'GET', body, signal, ...headers } = {}) {
  const init = {
    method,
    headers: { Accept: 'application/json', ...errorHeaders(headers) },
    credentials: 'same-origin',
    signal,
  };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  const response = await fetch(path, init);
  const parsed = await responseBody(response);
  if (!response.ok) {
    if (response.status === 401) {
      window.dispatchEvent(new CustomEvent(SESSION_EXPIRED_EVENT));
    }
    throw new APIError(
      parsed?.error || `Request failed with HTTP ${response.status}`,
      {
        status: response.status,
        code: parsed?.error_code || 'REQUEST_FAILED',
        correlationId: parsed?.correlation_id || '',
      },
    );
  }
  return payloadOf(parsed);
}

export async function apiGet(path, options = {}) {
  return apiRequest(path, { method: 'GET', signal: options.signal });
}

export async function apiPost(path, body, options = {}) {
  return apiRequest(path, { method: 'POST', body, ...options });
}

export async function apiPut(path, body, options = {}) {
  return apiRequest(path, { method: 'PUT', body, ...options });
}

// Every write carries a fresh idempotency key; retries within one user
// intent reuse the same key, so this helper is deliberately called once
// per confirmed submit, not per fetch attempt.
export function newIdempotencyKey() {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID();
  }
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

// Stable copy for the known HTTP/status-code families of the frozen
// contract. Codes that stay English (SEPARATION_OF_DUTIES and friends)
// are wire identifiers, not user text; the sentence around them is the
// user-facing explanation.
const CODE_COPY = {
  SEPARATION_OF_DUTIES: '审批被拒：审批人必须不同于豁免请求人（职责分离）。',
  WAIVER_TUPLE_MISMATCH: '豁免必须绑定该闸门当前的精确 SHA 与检查项。',
  WAIVER_EXISTS: '该闸门与 SHA 已存在豁免，不能重复申请。',
  WAIVER_STATE_CONFLICT: '豁免状态已被他人变更，请刷新后重试。',
  WAIVER_NOT_FOUND: '豁免记录不存在或不在当前项目范围内。',
  GATE_NOT_FOUND: '闸门快照不存在或不在当前项目范围内。',
  WORK_ITEM_NOT_FOUND: '工作项不存在或不在当前项目范围内。',
  MERGE_REQUEST_NOT_FOUND: '合并请求不存在或不在当前项目范围内。',
  POLICY_WEAKENED: '项目策略只能加强、不能削弱公司基线。',
  ROUTE_NOT_FOUND: '该接口在当前部署中未开放（可能未启用 PostgreSQL/OIDC 控制面）。',
  REMOTE_WRITE_DISABLED: '远程写操作被服务端关闭（REMOTE_WRITE=false）。',
  DEAD_LETTER_NOT_FOUND: '没有处于隔离（dead letter）状态的投递匹配该 ID——可能已被重放或 ID 有误。',
  REPLAY_APPROVAL_INVALID: '重放审批未通过：原因需为不少于 16 个字符的实质说明。',
  APPROVER_IDENTITY_MISMATCH: '重放审批人只能是你当前登录的身份（服务端从凭据推导，不接受代填）。',
  SLO_AVAILABILITY_UNMEASURED: '窗口内没有可用性遥测数据，SLO 快照拒绝编造数字（fail-closed）。',
  GRAPH_VERSION_MISMATCH: '工作图版本不匹配：图已被其他操作修改，请读取最新图后重试（CAS）。',
  NODE_VERSION_MISMATCH: '节点版本不匹配：节点已被其他操作修改，请读取最新状态后重试（CAS）。',
  REVISION_SEALED: '该计划修订已封板（不可变）；结构变更需要新修订（重规划）。',
  SEAL_REJECTED: '封板被拒绝：请检查必填输入端口绑定与图版本后重试。',
  WORK_PLAN_NOT_FOUND: '工作计划不存在或不在当前项目范围内。',
  ASSET_NOT_FOUND: '资产版本不存在或不在当前项目范围内。',
  ASSET_ALREADY_REGISTERED: '该资产版本已登记（幂等冲突）。',
  ASSET_TRANSITION_INVALID: '资产生命周期状态不允许该流转（draft→reviewed→approved）。',
  OPERATION_DISABLED: '该操作在当前部署未启用（需要 PostgreSQL 控制面）。',
};

const STATUS_COPY = {
  400: '请求参数不符合契约，请检查输入后重试。',
  401: '登录状态已过期或缺失，请重新登录。',
  403: '当前身份没有执行此操作的权限。',
  404: '资源不存在，或不在你的项目可见范围内。',
  409: '状态冲突：资源已被其他操作修改，请刷新后重试。',
  412: '版本冲突：服务端已是新版本，请刷新后重试。',
  422: '语义校验未通过，内容被拒绝且未落库。',
  428: '缺少必需的前置条件（版本或幂等键），请刷新页面后重试。',
  503: '依赖暂不可用或系统处于安全降级模式，请稍后重试。',
};

export function describeAPIError(error) {
  if (!(error instanceof APIError)) {
    return '无法连接服务，请确认后端运行状态后重试。';
  }
  if (CODE_COPY[error.code]) return CODE_COPY[error.code];
  if (STATUS_COPY[error.status]) return STATUS_COPY[error.status];
  const details = [error.code];
  if (error.correlationId) details.push(`correlation ${error.correlationId}`);
  return `${error.message}（${details.join(', ')}）`;
}
