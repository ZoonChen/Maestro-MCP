import { useCallback, useEffect, useState } from 'preact/hooks';
import { apiGet, apiPost, describeAPIError, newIdempotencyKey } from '../api/client';

function shortSha(sha) {
  return typeof sha === 'string' && sha.length > 12 ? `${sha.slice(0, 12)}…` : sha || '—';
}

const MR_STATE_COPY = { opened: '开放', closed: '关闭', merged: '已合并' };

// Read-only MR / Pipeline view (M4-UI-001 B4). It renders the reconciled
// GitLab projection plus the immutable evidence chain, and marks every
// evidence row by its authority: merge_gate is the only放行依据,
// diagnostic rows never satisfy a merge gate (ADR-006).
export function MergePipelineView({ projects }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  // Same interim rationale as the waiver console: the governance scope
  // may live outside the v1 project list; the server still hides every
  // unknown scope, this input grants nothing.
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = manualProjectId.trim() || projectId;

  const [mrIid, setMrIid] = useState('');
  const [projection, setProjection] = useState(null);
  const [mrState, setMrState] = useState('idle');
  const [mrError, setMrError] = useState('');

  const [mapping, setMapping] = useState(null);
  const [mappingError, setMappingError] = useState('');

  const [evidenceWorkItem, setEvidenceWorkItem] = useState('');
  const [evidenceRows, setEvidenceRows] = useState([]);
  const [evidenceState, setEvidenceState] = useState('idle');
  const [evidenceError, setEvidenceError] = useState('');

  const [reconcileReason, setReconcileReason] = useState('');
  const [reconcileResult, setReconcileResult] = useState('');

  useEffect(() => {
    if (projects.length > 0 && !projectId) setProjectId(projects[0].id);
  }, [projects, projectId]);

  const loadMapping = useCallback(async (pid) => {
    setMapping(null);
    setMappingError('');
    if (!pid) return;
    try {
      const row = await apiGet(`/api/v3/projects/${pid}/gitlab-mapping`);
      setMapping(row);
    } catch (error) {
      setMappingError(describeAPIError(error));
    }
  }, []);

  useEffect(() => { loadMapping(effectiveProjectId); }, [effectiveProjectId, loadMapping]);

  const loadProjection = useCallback(async (event) => {
    event.preventDefault();
    const iid = Number.parseInt(mrIid, 10);
    if (!effectiveProjectId || !Number.isInteger(iid) || iid < 1) {
      setMrError('请填写有效的合并请求 IID（正整数）。');
      return;
    }
    setMrError('');
    setMrState('loading');
    try {
      const row = await apiGet(
        `/api/v3/projects/${effectiveProjectId}/gitlab/merge-requests/${iid}`,
      );
      setProjection(row);
      setMrState('ready');
    } catch (error) {
      setProjection(null);
      setMrError(describeAPIError(error));
      setMrState('error');
    }
  }, [effectiveProjectId, mrIid]);

  const loadEvidence = useCallback(async (event) => {
    event.preventDefault();
    const wid = evidenceWorkItem.trim();
    if (!effectiveProjectId || !wid) {
      setEvidenceError('请填写工作项 ID。');
      return;
    }
    setEvidenceError('');
    setEvidenceState('loading');
    try {
      const rows = await apiGet(
        `/api/v3/projects/${effectiveProjectId}/work-items/${encodeURIComponent(wid)}/evidence`,
      );
      setEvidenceRows(Array.isArray(rows) ? rows : []);
      setEvidenceState('ready');
    } catch (error) {
      setEvidenceRows([]);
      setEvidenceError(describeAPIError(error));
      setEvidenceState('error');
    }
  }, [effectiveProjectId, evidenceWorkItem]);

  const requestReconcile = useCallback(async () => {
    const trimmed = reconcileReason.trim();
    if (trimmed.length < 8 || trimmed.length > 2000) {
      setReconcileResult('对账原因必须为 8–2000 个字符。');
      return;
    }
    if (!effectiveProjectId || !projection?.iid || !mapping?.version) {
      setReconcileResult('缺少 MR 投影或映射版本，无法发起对账。');
      return;
    }
    setReconcileResult('');
    try {
      const accepted = await apiPost(
        `/api/v3/projects/${effectiveProjectId}/gitlab/merge-requests/${projection.iid}/reconcile`,
        { reason: trimmed },
        { idempotencyKey: newIdempotencyKey(), ifMatch: `"${mapping.version}"` },
      );
      setReconcileResult(`对账请求已受理（operation ${accepted?.operation_id || '?'}）。`);
    } catch (error) {
      setReconcileResult(describeAPIError(error));
    }
  }, [effectiveProjectId, projection, mapping, reconcileReason]);

  return (
    <section class="gov-page" aria-labelledby="mr-view-title">
      <header class="gov-header">
        <h1 id="mr-view-title">MR · Pipeline 视图</h1>
        <p class="gov-lead">
          只读展示已对账的 GitLab 合并请求投影与不可变证据链。
          权威性规则：只有 authority=merge_gate 的 GitLab CI 证据可作为放行依据；
          diagnostic（本地 Runner）证据永不满足合并门禁。
        </p>
      </header>

      <div class="gov-fieldset">
        <label class="gov-field">
          <span>项目</span>
          <select value={projectId} onChange={(e) => setProjectId(e.target.value)}>
            {projects.length === 0 ? <option value="">（无可见项目）</option> : null}
            {projects.map((p) => <option key={p.id} value={p.id}>{p.name || p.id}</option>)}
          </select>
        </label>
        <label class="gov-field">
          <span>项目 ID（当项目不在上方列表时手动输入）</span>
          <input
            value={manualProjectId}
            onInput={(e) => setManualProjectId(e.target.value)}
            placeholder="治理范围的项目 UUID"
          />
        </label>
      </div>

      <div class="gov-panel" aria-label="GitLab 映射">
        <h2>仓库映射</h2>
        {mapping ? (
          <dl class="gov-kv">
            <dt>GitLab 实例</dt><dd class="gov-mono">{mapping.gitlab_instance_id}</dd>
            <dt>项目数字 ID</dt><dd>{mapping.gitlab_project_numeric_id}</dd>
            <dt>目标分支</dt><dd class="gov-mono">{mapping.target_branch}</dd>
            <dt>状态</dt><dd>{mapping.state}</dd>
            <dt>映射版本</dt><dd class="gov-mono">{mapping.version ? `v${mapping.version}` : '—'}</dd>
          </dl>
        ) : (
          <p class="gov-empty">{mappingError || '该项目暂无 GitLab 映射。'}</p>
        )}
        {mappingError ? <p role="alert" class="gov-status gov-status-error">{mappingError}</p> : null}
      </div>

      <form class="gov-panel" onSubmit={loadProjection} aria-label="合并请求查询">
        <h2>合并请求投影</h2>
        <div class="gov-fieldset">
          <label class="gov-field">
            <span>MR IID</span>
            <input
              type="number"
              min="1"
              step="1"
              value={mrIid}
              onInput={(e) => setMrIid(e.target.value)}
              placeholder="如 7"
            />
          </label>
          <button type="submit" class="gov-button">查询</button>
        </div>
        {mrState === 'loading' ? <p role="status" class="gov-status">正在读取投影…</p> : null}
        {mrState === 'error' ? <p role="alert" class="gov-status gov-status-error">{mrError}</p> : null}
        {mrState === 'ready' && projection ? (
          <>
            <dl class="gov-kv" data-mr-projection={projection.iid}>
              <dt>IID</dt><dd>!{projection.iid}</dd>
              <dt>状态</dt>
              <dd>
                <span class="gov-chip">{MR_STATE_COPY[projection.state] || projection.state}</span>
                {projection.stale ? <span class="gov-chip gov-chip-stale">投影过期</span> : null}
              </dd>
              <dt>source SHA</dt><dd class="gov-mono">{shortSha(projection.source_sha)}</dd>
              <dt>target SHA</dt><dd class="gov-mono">{shortSha(projection.target_sha)}</dd>
              <dt>Pipeline</dt>
              <dd>{projection.pipeline_id ? `#${projection.pipeline_id}` : '（暂无关联 Pipeline）'}</dd>
            </dl>
            <div class="gov-field">
              <label>
                <span>对账原因（8–2000 字符，将触发只读对账）</span>
                <textarea
                  rows="2"
                  minlength="8"
                  maxlength="2000"
                  value={reconcileReason}
                  onInput={(e) => setReconcileReason(e.target.value)}
                  placeholder="如：Webhook 延迟后核对 MR 状态"
                />
              </label>
              <button type="button" class="gov-button" onClick={requestReconcile}>请求与 GitLab 对账</button>
              {reconcileResult ? <p role="status" class="gov-status">{reconcileResult}</p> : null}
            </div>
          </>
        ) : null}
        {mrState === 'ready' && !projection ? <p class="gov-empty">未找到该合并请求的投影。</p> : null}
      </form>

      <form class="gov-panel" onSubmit={loadEvidence} aria-label="证据链查询">
        <h2>证据链（按工作项）</h2>
        <div class="gov-fieldset">
          <label class="gov-field">
            <span>工作项 ID</span>
            <input
              value={evidenceWorkItem}
              onInput={(e) => setEvidenceWorkItem(e.target.value)}
              placeholder="如 018f7500-0000-7000-8000-000000000003"
            />
          </label>
          <button type="submit" class="gov-button">查询</button>
        </div>
        {evidenceState === 'loading' ? <p role="status" class="gov-status">正在读取证据…</p> : null}
        {evidenceState === 'error' ? <p role="alert" class="gov-status gov-status-error">{evidenceError}</p> : null}
        {evidenceState === 'ready' && evidenceRows.length === 0 ? (
          <p class="gov-empty">该工作项暂无证据记录。</p>
        ) : null}
        {evidenceRows.length > 0 ? (
          <div class="gov-table-wrap">
            <table class="gov-table" aria-label="证据记录">
              <thead>
                <tr>
                  <th scope="col">证据 ID</th>
                  <th scope="col">权威性</th>
                  <th scope="col">类型</th>
                  <th scope="col">状态</th>
                  <th scope="col">source SHA</th>
                  <th scope="col">Pipeline / Job</th>
                  <th scope="col">策略版本</th>
                </tr>
              </thead>
              <tbody>
                {evidenceRows.map((row) => (
                  <tr key={row.evidence_id} data-evidence-id={row.evidence_id}>
                    <td class="gov-mono">{shortSha(row.evidence_id)}</td>
                    <td>
                      <span class={`gov-chip ${row.authority === 'merge_gate' ? 'gov-chip-authoritative' : 'gov-chip-diagnostic'}`}>
                        {row.authority === 'merge_gate' ? '权威（merge gate）' : '诊断（非放行依据）'}
                      </span>
                    </td>
                    <td>{row.kind}</td>
                    <td>{row.status}</td>
                    <td class="gov-mono">{shortSha(row.source_sha)}</td>
                    <td class="gov-mono">
                      {row.pipeline_id ? `#${row.pipeline_id}` : '—'}
                      {row.job_id ? ` / #${row.job_id}` : ''}
                    </td>
                    <td class="gov-mono">{row.policy_version}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : null}
      </form>
    </section>
  );
}
