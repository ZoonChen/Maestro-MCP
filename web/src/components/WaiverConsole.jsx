import { useCallback, useEffect, useMemo, useState } from 'preact/hooks';
import { ErrorNotice } from './ErrorNotice';
import {
  APIError,
  apiGet,
  apiPost,
  describeAPIError,
  newIdempotencyKey,
} from '../api/client';
import { ACTION_PERMISSION_HINTS } from '../governance';
import { ScopePicker } from './ScopePicker';

const MAX_WAIVER_DAYS = 7;

// Remaining-validity copy for the queue rows: the server reports whole
// seconds; the console renders a coarse human window (the receipt keeps
// the exact timestamp).
function remainingCopy(row) {
  if (!['requested', 'approved', 'active'].includes(row.status)) return '—';
  const seconds = Number(row.remaining_seconds);
  if (!Number.isFinite(seconds) || seconds <= 0) return '已到期';
  if (seconds < 3600) return `剩 ${Math.floor(seconds / 60)} 分钟`;
  if (seconds < 86400) return `剩 ${Math.floor(seconds / 3600)} 小时`;
  return `剩 ${Math.floor(seconds / 86400)} 天`;
}

function shortSha(sha) {
  return typeof sha === 'string' && sha.length > 12 ? `${sha.slice(0, 12)}…` : sha || '—';
}

function localDateTimeMax() {
  const max = new Date(Date.now() + MAX_WAIVER_DAYS * 24 * 60 * 60 * 1000);
  max.setSeconds(0, 0);
  const pad = (n) => String(n).padStart(2, '0');
  return `${max.getFullYear()}-${pad(max.getMonth() + 1)}-${pad(max.getDate())}T${pad(max.getHours())}:${pad(max.getMinutes())}`;
}

function toRFC3339(localValue) {
  const date = new Date(localValue);
  return Number.isNaN(date.getTime()) ? '' : date.toISOString();
}

const GATE_STATE_COPY = {
  pending: '等待中',
  running: '运行中',
  passed: '通过',
  failed: '失败',
  error: '错误',
  stale: '过期',
  waived: '已豁免',
};

const WAIVER_STATE_COPY = {
  requested: '待审批',
  approved: '已批准',
  rejected: '已拒绝',
  active: '生效中',
  expired: '已过期',
  revoked: '已撤销',
};

// HITL waiver console (M4-UI-001 B3, second generation). The page walks
// the frozen waiver contract: work-item gate snapshots -> time-limited
// waiver request -> independent approval / revocation. The queue view
// consumes the real per-work-item waivers list (task brief E / UI-2)
// with live remaining-validity windows; listing needs quality.read, so
// roles without it (project_admin) still bind manually from a
// colleague's view — the boundary is stated on the page instead of
// being papered over.
export function WaiverConsole({ projects, roles, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  // M1 interim: the v1 project registry (task substrate) and the /api/v3
  // governance scope live in different stores, so a governance project
  // may be addressable before it appears in the workbench list. The
  // manual id overrides the select; the server still hides unknown
  // scopes (404) — this input grants nothing.
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = manualProjectId.trim() || projectId;
  const [workItems, setWorkItems] = useState([]);
  const [workItemsError, setWorkItemsError] = useState('');
  const [workItemId, setWorkItemId] = useState('');
  const [manualWorkItemId, setManualWorkItemId] = useState('');
  const [gates, setGates] = useState([]);
  const [gatesStatus, setGatesStatus] = useState('idle'); // idle|loading|ready|error
  const [gatesError, setGatesError] = useState('');
  const [selectedGateId, setSelectedGateId] = useState(null);

  // The real HITL queue (UI-2): every requested/approved/terminal waiver
  // of the work item with its live remaining-validity window.
  const [queue, setQueue] = useState([]);
  const [queueStatus, setQueueStatus] = useState('idle'); // idle|loading|ready|error
  const [queueError, setQueueError] = useState('');

  // Manual gate binding: waiver.request holders without quality.read
  // (project_admin in the frozen matrix) cannot list snapshots, so the
  // console accepts the gate tuple from a colleague's view instead of
  // blocking the flow. The server still rejects any drifted tuple.
  const [manualGate, setManualGate] = useState({ id: '', version: '', sourceSha: '', check: '' });

  const [reason, setReason] = useState('');
  const [expiresAt, setExpiresAt] = useState('');
  const [mrIid, setMrIid] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [receipt, setReceipt] = useState(null);
  const [requestNotice, setRequestNotice] = useState('');

  const [actionWaiverId, setActionWaiverId] = useState('');
  const [actionVersion, setActionVersion] = useState('');
  const [actionReason, setActionReason] = useState('');
  const [actionResult, setActionResult] = useState(null); // {kind:'ok'|'error', text}

  const effectiveWorkItemId = workItemId || manualWorkItemId.trim();

  const canRequest = useMemo(
    () => roles.includes('project_admin') || roles.includes('platform_admin'),
    [roles],
  );

  useEffect(() => {
    if (projects.length > 0 && !projectId) {
      setProjectId(projects[0].id);
    }
  }, [projects, projectId]);

  const loadWorkItems = useCallback(async (pid) => {
    setWorkItemsError('');
    setWorkItems([]);
    if (!pid) return;
    try {
      const tasks = await apiGet(`/api/v1/projects/${pid}/tasks`);
      setWorkItems(Array.isArray(tasks) ? tasks : []);
    } catch (error) {
      setWorkItemsError(describeAPIError(error));
    }
  }, []);

  useEffect(() => { loadWorkItems(effectiveProjectId); }, [effectiveProjectId, loadWorkItems]);

  const loadGates = useCallback(async () => {
    if (!effectiveProjectId || !effectiveWorkItemId) {
      setGatesStatus('idle');
      setGates([]);
      setQueueStatus('idle');
      setQueue([]);
      return;
    }
    setGatesStatus('loading');
    setGatesError('');
    setQueueStatus('loading');
    setQueueError('');
    // Both reads need quality.read; the queue fetch rides the same
    // trigger so one click refreshes the whole HITL picture.
    try {
      const rows = await apiGet(
        `/api/v3/projects/${effectiveProjectId}/work-items/${encodeURIComponent(effectiveWorkItemId)}/gates`,
      );
      setGates(Array.isArray(rows) ? rows : []);
      setGatesStatus('ready');
    } catch (error) {
      setGates([]);
      setGatesError(describeAPIError(error));
      setGatesStatus('error');
    }
    try {
      const queueRows = await apiGet(
        `/api/v3/projects/${effectiveProjectId}/work-items/${encodeURIComponent(effectiveWorkItemId)}/waivers`,
      );
      setQueue(Array.isArray(queueRows) ? queueRows : []);
      setQueueStatus('ready');
    } catch (error) {
      setQueue([]);
      setQueueError(describeAPIError(error));
      setQueueStatus('error');
    }
  }, [effectiveProjectId, effectiveWorkItemId]);

  // A queue row binds straight into the action form: the row carries the
  // authoritative id + version, so the operator never retypes them.
  const bindQueueRow = useCallback((row) => {
    setActionWaiverId(row.id || '');
    setActionVersion(String(row.version ?? ''));
    setActionResult(null);
  }, []);

  const rowSelectedGate = gates.find((gate) => gate.id === selectedGateId) || null;
  const manualGateComplete = ['id', 'version', 'sourceSha', 'check'].every((key) => manualGate[key].trim() !== '');
  const selectedGate = rowSelectedGate || (manualGateComplete
    ? {
      id: manualGate.id.trim(),
      version: Number.parseInt(manualGate.version, 10),
      source_sha: manualGate.sourceSha.trim(),
      check: manualGate.check.trim(),
    }
    : null);

  const submitWaiverRequest = useCallback(async (event) => {
    event.preventDefault();
    if (!selectedGate) return;
    const trimmedReason = reason.trim();
    const isoExpiry = toRFC3339(expiresAt);
    const iid = Number.parseInt(mrIid, 10);
    let validation = '';
    if (trimmedReason.length < 16 || trimmedReason.length > 4000) {
      validation = '原因必须为 16–4000 个字符。';
    } else if (!isoExpiry) {
      validation = '请选择豁免到期时间。';
    } else {
      const expiryMs = new Date(isoExpiry).getTime();
      if (expiryMs <= Date.now()) validation = '到期时间必须晚于当前时间。';
      if (expiryMs > Date.now() + MAX_WAIVER_DAYS * 24 * 60 * 60 * 1000) {
        validation = `豁免期限最长 ${MAX_WAIVER_DAYS} 天。`;
      }
    }
    if (!Number.isInteger(iid) || iid < 1) {
      validation = validation || '请填写有效的合并请求 IID（正整数）。';
    }
    if (validation) {
      setRequestNotice(validation);
      return;
    }
    setRequestNotice('');
    setSubmitting(true);
    try {
      const created = await apiPost(
        `/api/v3/projects/${effectiveProjectId}/gates/${encodeURIComponent(selectedGate.id)}/waivers`,
        { source_sha: selectedGate.source_sha, merge_request_iid: iid, check: selectedGate.check, reason: trimmedReason, expires_at: isoExpiry },
        { idempotencyKey: newIdempotencyKey(), ifMatch: `"${selectedGate.version}"` },
      );
      setReceipt(created);
      setActionWaiverId(created?.id || '');
      setActionVersion(String(created?.version || ''));
      setActionResult(null);
      setRequestNotice(`豁免请求已提交（状态：${WAIVER_STATE_COPY[created?.status] || created?.status || '待审批'}）。`);
      loadGates();
    } catch (error) {
      setRequestNotice(describeAPIError(error));
    } finally {
      setSubmitting(false);
    }
  }, [effectiveProjectId, selectedGate, reason, expiresAt, mrIid, loadGates]);

  const runWaiverAction = useCallback(async (action) => {
    const trimmedReason = actionReason.trim();
    if (trimmedReason.length < 8 || trimmedReason.length > 2000) {
      setActionResult({ kind: 'error', text: '原因必须为 8–2000 个字符。' });
      return;
    }
    if (!actionWaiverId.trim() || !actionVersion.trim()) {
      setActionResult({ kind: 'error', text: '请填写豁免 ID 与版本（可由上方回执自动带入）。' });
      return;
    }
    setActionResult(null);
    try {
      const updated = await apiPost(
        `/api/v3/projects/${effectiveProjectId}/waivers/${encodeURIComponent(actionWaiverId.trim())}/${action}`,
        { reason: trimmedReason },
        { idempotencyKey: newIdempotencyKey(), ifMatch: `"${actionVersion.trim()}"` },
      );
      setReceipt(updated);
      setActionVersion(String(updated?.version || ''));
      setActionResult({ kind: 'ok', text: `操作完成：豁免状态现为「${WAIVER_STATE_COPY[updated?.status] || updated?.status}」。` });
      // The queue shows live state: refresh it so the row reflects the
      // transition the server just recorded.
      loadGates();
    } catch (error) {
      const text = describeAPIError(error);
      if (error instanceof APIError && error.status === 403 && error.code === 'SEPARATION_OF_DUTIES') {
        setActionResult({ kind: 'error', text: `${text} 申请人与审批人不能是同一人——请由其他负责人操作。` });
      } else {
        setActionResult({ kind: 'error', text });
      }
    }
  }, [effectiveProjectId, actionWaiverId, actionVersion, actionReason, loadGates]);

  return (
    <section class="gov-page" aria-labelledby="waiver-console-title">
      <header class="gov-header">
        <h1 id="waiver-console-title">HITL 豁免审批</h1>
        <p class="gov-lead">
          按工作项闸门快照发起限时豁免，并由独立审批人批准或撤销。
          每一步都要求当前版本（If-Match）与幂等键，冲突将被服务端拒绝。
        </p>
        <p class="gov-note" role="note">
          待审队列按工作项读取真实豁免列表（含剩余有效期）；读取需 quality.read。
          跨项目聚合队列端点尚未冻结（已登记为集成交接缺口）。
        </p>
        <p class="gov-note" role="note">
          权限提示：申请/撤销需 {ACTION_PERMISSION_HINTS['waiver.request']}；审批需 {ACTION_PERMISSION_HINTS['waiver.approve']}。
          审批权当前仅授给职能角色（security_owner / qa_owner），而身份层尚未建模职能角色——
          现阶段所有可登录身份执行审批都会被服务端以 403 拒绝（已登记 UI-4 遗留），
          队列中的待审项需由拥有职能角色的负责人经 API/MCP 审批。
          {canRequest ? ' 当前身份可申请豁免。' : ' 当前身份仅供查看（授权以服务端判定为准）。'}
        </p>
      </header>

      <div class="gov-fieldset">
        <ScopePicker
          projects={projects}
          projectScope={projectScope}
          projectId={projectId}
          onProjectId={setProjectId}
          manualProjectId={manualProjectId}
          onManualProjectId={setManualProjectId}
        />
        <label class="gov-field">
          <span>工作项</span>
          <select value={workItemId} onChange={(e) => { setWorkItemId(e.target.value); setManualWorkItemId(''); }}>
            <option value="">— 手动输入 —</option>
            {workItems.map((item) => (
              <option key={item.id} value={item.id}>{item.title || item.id}</option>
            ))}
          </select>
        </label>
        {!workItemId ? (
          <label class="gov-field">
            <span>工作项 ID</span>
            <input
              value={manualWorkItemId}
              onInput={(e) => setManualWorkItemId(e.target.value)}
              placeholder="如 018f7500-0000-7000-8000-000000000003"
            />
          </label>
        ) : null}
        <button type="button" class="gov-button" onClick={loadGates} disabled={!effectiveProjectId || !effectiveWorkItemId}>
          读取闸门快照与队列
        </button>
      </div>

      {workItemsError ? <ErrorNotice message={workItemsError} onRetry={() => loadWorkItems(effectiveProjectId)} /> : null}

      {gatesStatus === 'loading' ? <p role="status" class="gov-status">正在读取闸门快照…</p> : null}
      {gatesStatus === 'error' ? (
        <div class="gov-error" role="alert">
          <p>{gatesError}</p>
          <button type="button" class="gov-button" onClick={loadGates}>重试</button>
        </div>
      ) : null}
      {gatesStatus === 'ready' && gates.length === 0 ? (
        <p class="gov-empty">该工作项暂无闸门快照（可能尚未产生 CI 证据）。</p>
      ) : null}

      <div class="gov-panel" aria-label="豁免待审队列">
        <h2>豁免队列（按工作项）</h2>
        {queueStatus === 'loading' ? <p role="status" class="gov-status">正在读取豁免队列…</p> : null}
        {queueStatus === 'error' ? (
          <p role="alert" class="gov-status gov-status-error">{queueError}</p>
        ) : null}
        {queueStatus === 'ready' && queue.length === 0 ? (
          <p class="gov-empty">该工作项暂无豁免记录（诚实空态）。</p>
        ) : null}
        {queueStatus === 'ready' && queue.length > 0 ? (
          <div class="gov-table-wrap">
            <table class="gov-table" aria-label="豁免队列记录">
              <thead>
                <tr>
                  <th scope="col">豁免 ID</th>
                  <th scope="col">检查项</th>
                  <th scope="col">状态</th>
                  <th scope="col">请求人</th>
                  <th scope="col">审批人</th>
                  <th scope="col">到期</th>
                  <th scope="col">剩余有效期</th>
                  <th scope="col">操作</th>
                </tr>
              </thead>
              <tbody>
                {queue.map((row) => (
                  <tr key={row.id} data-waiver-id={row.id}>
                    <td class="gov-mono" title={row.id}>{shortSha(row.id)}</td>
                    <td>{row.check || '—'}</td>
                    <td><span class="gov-chip">{WAIVER_STATE_COPY[row.status] || row.status}</span></td>
                    <td class="gov-mono">{row.requester_id}</td>
                    <td class="gov-mono">
                      {row.approver_id || (row.status === 'requested' ? '（待独立审批）' : '—')}
                    </td>
                    <td>{row.expires_at || '—'}</td>
                    <td>{remainingCopy(row)}</td>
                    <td>
                      <button
                        type="button"
                        class="gov-button gov-button-small"
                        onClick={() => bindQueueRow(row)}
                      >
                        带入审批表单
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </div>

      <details class="gov-panel" aria-label="手动绑定闸门">
        <summary>手动绑定闸门（当无法读取闸门快照时）</summary>
        <p class="gov-note">
          仅持有 waiver.request 而无 quality.read 的角色（如 project_admin）无法列出快照；
          可凭同事提供的闸门元组绑定。SHA/版本漂移仍会被服务端以 422/412 拒绝。
        </p>
        <div class="gov-fieldset" data-testid="manual-gate">
          <label class="gov-field"><span>闸门行 ID</span>
            <input value={manualGate.id} onInput={(e) => setManualGate({ ...manualGate, id: e.target.value })} />
          </label>
          <label class="gov-field"><span>闸门版本</span>
            <input type="number" min="1" step="1" value={manualGate.version} onInput={(e) => setManualGate({ ...manualGate, version: e.target.value })} />
          </label>
          <label class="gov-field"><span>source SHA</span>
            <input class="gov-mono" value={manualGate.sourceSha} onInput={(e) => setManualGate({ ...manualGate, sourceSha: e.target.value })} />
          </label>
          <label class="gov-field"><span>检查项</span>
            <input value={manualGate.check} onInput={(e) => setManualGate({ ...manualGate, check: e.target.value })} />
          </label>
        </div>
      </details>

      {gates.length > 0 ? (
        <div class="gov-table-wrap">
          <table class="gov-table" aria-label="闸门快照">
            <thead>
              <tr>
                <th scope="col">检查项</th>
                <th scope="col">状态</th>
                <th scope="col">source SHA</th>
                <th scope="col">target SHA</th>
                <th scope="col">策略版本</th>
                <th scope="col">版本</th>
                <th scope="col">操作</th>
              </tr>
            </thead>
            <tbody>
              {gates.map((gate) => (
                <tr key={gate.id} data-gate-id={gate.id}>
                  <td>{gate.check}</td>
                  <td><span class={`gov-chip gov-chip-${gate.state}`}>{GATE_STATE_COPY[gate.state] || gate.state}</span></td>
                  <td class="gov-mono">{shortSha(gate.source_sha)}</td>
                  <td class="gov-mono">{shortSha(gate.target_sha)}</td>
                  <td class="gov-mono">{gate.policy_version}</td>
                  <td class="gov-mono">{gate.version}</td>
                  <td>
                    <button
                      type="button"
                      class="gov-button gov-button-small"
                      aria-pressed={selectedGateId === gate.id}
                      onClick={() => setSelectedGateId(gate.id === selectedGateId ? null : gate.id)}
                    >
                      {selectedGateId === gate.id ? '取消选择' : '申请豁免'}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {selectedGate ? (
        <form class="gov-panel" onSubmit={submitWaiverRequest} aria-label="豁免申请">
          <h2>为「{selectedGate.check}」申请豁免</h2>
          <p class="gov-note">
            豁免将精确绑定 source SHA <span class="gov-mono">{shortSha(selectedGate.source_sha)}</span> 与当前闸门版本
            <span class="gov-mono"> v{selectedGate.version}</span>；SHA 漂移会被服务端拒绝（422）。
          </p>
          <label class="gov-field">
            <span>合并请求 IID</span>
            <input
              type="number"
              min="1"
              step="1"
              required
              value={mrIid}
              onInput={(e) => setMrIid(e.target.value)}
              placeholder="如 12"
            />
          </label>
          <label class="gov-field">
            <span>到期时间（最长 {MAX_WAIVER_DAYS} 天）</span>
            <input
              type="datetime-local"
              required
              max={localDateTimeMax()}
              value={expiresAt}
              onInput={(e) => setExpiresAt(e.target.value)}
            />
          </label>
          <label class="gov-field">
            <span>原因（16–4000 字符）</span>
            <textarea
              required
              maxlength="4000"
              rows="3"
              value={reason}
              onInput={(e) => setReason(e.target.value)}
              placeholder="说明影响范围、补偿措施与恢复计划"
            />
          </label>
          <button type="submit" class="gov-button gov-button-primary" disabled={submitting}>
            {submitting ? '提交中…' : '提交豁免申请'}
          </button>
          {requestNotice ? <p role="status" class="gov-status">{requestNotice}</p> : null}
        </form>
      ) : null}

      {receipt ? (
        <div class="gov-panel" aria-label="豁免回执">
          <h2>豁免回执</h2>
          <dl class="gov-kv">
            <dt>豁免 ID</dt><dd class="gov-mono">{receipt.id}</dd>
            <dt>状态</dt><dd><span class="gov-chip">{WAIVER_STATE_COPY[receipt.status] || receipt.status}</span></dd>
            <dt>检查项</dt><dd>{receipt.check || '—'}</dd>
            <dt>请求人</dt><dd class="gov-mono">{receipt.requester_id}</dd>
            <dt>审批人</dt><dd class="gov-mono">{receipt.approver_id || '（待独立审批）'}</dd>
            <dt>MR IID</dt><dd>{receipt.merge_request_iid ?? '—'}</dd>
            <dt>到期</dt><dd>{receipt.expires_at || '—'}</dd>
            <dt>版本</dt><dd class="gov-mono">v{receipt.version}</dd>
          </dl>
        </div>
      ) : null}

      <form
        class="gov-panel"
        aria-label="批准或撤销豁免"
        onSubmit={(e) => { e.preventDefault(); runWaiverAction('approve'); }}
      >
        <h2>审批 / 撤销豁免</h2>
        <p class="gov-note">
          审批人与请求人必须不同（职责分离，违反将被 403 SEPARATION_OF_DUTIES 拒绝）。
          当前身份未获审批授权时，服务端会以 403 拒绝——以下提示即为真实策略结果。
        </p>
        <div class="gov-fieldset">
          <label class="gov-field">
            <span>豁免 ID</span>
            <input value={actionWaiverId} onInput={(e) => setActionWaiverId(e.target.value)} required />
          </label>
          <label class="gov-field">
            <span>版本（If-Match）</span>
            <input value={actionVersion} onInput={(e) => setActionVersion(e.target.value)} required />
          </label>
        </div>
        <label class="gov-field">
          <span>原因（8–2000 字符）</span>
          <textarea
            required
            maxlength="2000"
            rows="2"
            value={actionReason}
            onInput={(e) => setActionReason(e.target.value)}
            placeholder="审批/撤销理由（写入审计）"
          />
        </label>
        <div class="gov-actions">
          <button type="submit" class="gov-button gov-button-primary">批准</button>
          <button type="button" class="gov-button gov-button-danger" onClick={() => runWaiverAction('revoke')}>
            撤销
          </button>
        </div>
        {actionResult ? (
          <p role={actionResult.kind === 'error' ? 'alert' : 'status'} class={`gov-status ${actionResult.kind === 'error' ? 'gov-status-error' : ''}`}>
            {actionResult.text}
          </p>
        ) : null}
      </form>
    </section>
  );
}
