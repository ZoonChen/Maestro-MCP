import { useCallback, useMemo, useState } from 'preact/hooks';
import { apiGet, describeAPIError } from '../api/client';
import { ScopePicker, effectiveScopeId } from './ScopePicker';

const STATUS_CHIP = {
  approved: 'gov-chip-passed',
  reviewed: 'gov-chip-stale',
  draft: 'gov-chip-diagnostic',
  superseded: 'gov-chip-failed',
};
const SENSITIVITY_COPY = { public: '公开', internal: '内部', confidential: '机密' };
const SENSITIVITY_CHIP = { public: 'gov-chip-passed', internal: 'gov-chip-stale', confidential: 'gov-chip-failed' };

// Asset ledger view (task brief J2c-2): every asset version with its
// lifecycle, supersedes version chain and sensitivity. Read-only; the
// lifecycle moves through the MCP register/review/approve tools under
// the frozen functional permissions — the console never invents a
// transition.
export function AssetLedgerView({ projects, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = effectiveScopeId(projectId, manualProjectId);

  const [status, setStatus] = useState('idle'); // idle|loading|ready|error
  const [error, setError] = useState('');
  const [assets, setAssets] = useState(null);
  const [waitingGates, setWaitingGates] = useState([]);
  const [sensitivity, setSensitivity] = useState('');
  const [lifecycle, setLifecycle] = useState('');
  const [assetType, setAssetType] = useState('');

  const load = useCallback(async (event) => {
    event.preventDefault();
    if (!effectiveProjectId) {
      setError('请先选择或输入项目。');
      setStatus('error');
      return;
    }
    setError('');
    setStatus('loading');
    try {
      const row = await apiGet(`/api/v3/projects/${effectiveProjectId}/assets`);
      setAssets(Array.isArray(row?.assets) ? row.assets : []);
      setWaitingGates(Array.isArray(row?.waiting_gates) ? row.waiting_gates : []);
      setStatus('ready');
    } catch (e) {
      setAssets(null);
      setWaitingGates([]);
      setError(describeAPIError(e));
      setStatus('error');
    }
  }, [effectiveProjectId]);

  const types = useMemo(() => {
    const set = new Set((assets || []).map((asset) => asset.asset_type));
    return [...set].sort();
  }, [assets]);

  // Group by asset identity: every version renders as one chain row set
  // (v1 → v2 via supersedes_ref), so the single-successor invariant is
  // visible instead of implied.
  const chains = useMemo(() => {
    const byID = new Map();
    for (const asset of assets || []) {
      if (sensitivity && asset.sensitivity !== sensitivity) continue;
      if (lifecycle && asset.status !== lifecycle) continue;
      if (assetType && asset.asset_type !== assetType) continue;
      const list = byID.get(asset.asset_id) || [];
      list.push(asset);
      byID.set(asset.asset_id, list);
    }
    return [...byID.values()]
      .map((versions) => versions.sort((a, b) => a.version - b.version))
      .sort((a, b) => (a[0]?.asset_id || '').localeCompare(b[0]?.asset_id || ''));
  }, [assets, sensitivity, lifecycle, assetType]);

  return (
    <section class="gov-page" aria-labelledby="asset-ledger-title">
      <header class="gov-header">
        <h1 id="asset-ledger-title">资产台账</h1>
        <p class="gov-lead">
          制品资产的事实源（登记/评审/放行/查询）：每行是一个版本，生命周期 draft→reviewed→approved→superseded，
          后继版本经 supersedes 链钉住前驱。机密（confidential）资产只入摘要+指针，正文永不进控制面。
        </p>
        <p class="gov-note" role="note">
          本视图只读（asset.read）；登记与生命周期流转走 MCP 工具（asset_register / asset_review /
          asset_approve，职能角色审批），控制台不提供旁路。
        </p>
      </header>

      <form onSubmit={load}>
        <div class="gov-fieldset">
          <ScopePicker
            projects={projects}
            projectScope={projectScope}
            projectId={projectId}
            onProjectId={setProjectId}
            manualProjectId={manualProjectId}
            onManualProjectId={setManualProjectId}
          />
          <button type="submit" class="gov-button">读取台账</button>
        </div>
      </form>

      {status === 'loading' ? <p role="status" class="gov-status">正在读取资产台账…</p> : null}
      {status === 'error' ? <p role="alert" class="gov-status gov-status-error">{error}</p> : null}

      {status === 'ready' ? (
        <>
          <div class="gov-fieldset" role="group" aria-label="台账筛选">
            <label>
              sensitivity
              <select data-asset-filter="sensitivity" value={sensitivity}
                onChange={(e) => setSensitivity(e.target.value)}>
                <option value="">全部</option>
                {Object.entries(SENSITIVITY_COPY).map(([value, label]) => (
                  <option key={value} value={value}>{label}</option>
                ))}
              </select>
            </label>
            <label>
              生命周期
              <select data-asset-filter="status" value={lifecycle}
                onChange={(e) => setLifecycle(e.target.value)}>
                <option value="">全部</option>
                {['draft', 'reviewed', 'approved', 'superseded'].map((value) => (
                  <option key={value} value={value}>{value}</option>
                ))}
              </select>
            </label>
            <label>
              类型
              <select data-asset-filter="asset_type" value={assetType}
                onChange={(e) => setAssetType(e.target.value)}>
                <option value="">全部</option>
                {types.map((value) => (
                  <option key={value} value={value}>{value}</option>
                ))}
              </select>
            </label>
          </div>

          {waitingGates.length > 0 ? (
            <div class="gov-fieldset" data-waiting-gates="panel" role="group" aria-label="locked_gate 下游等待面">
              <h2>locked_gate 下游等待面</h2>
              <p class="gov-note" role="note">
                下列工作项的 Gate 绑定已随制品 supersede 转 stale，派发被 fail-closed 阻断；
                「等待版本」即 Gate 等待重绑的制品版本，重绑已 approved 的后继版本即愈合。
              </p>
              <div class="gov-table-wrap">
                <table class="gov-table" aria-label="等待面">
                  <thead>
                    <tr>
                      <th scope="col">工作项</th>
                      <th scope="col">Gate</th>
                      <th scope="col">制品 / 绑定版本</th>
                      <th scope="col">等待版本</th>
                      <th scope="col">绑定态</th>
                    </tr>
                  </thead>
                  <tbody>
                    {waitingGates.map((gate) => (
                      <tr key={`${gate.work_item_id}:${gate.gate_id}`} data-waiting-gate-row={`${gate.asset_id}@${gate.bound_version}`}>
                        <td class="gov-mono">{gate.work_item_id}</td>
                        <td class="gov-mono">{gate.gate_id}</td>
                        <td class="gov-mono">{gate.asset_id}@{gate.bound_version}</td>
                        <td class="gov-mono">
                          {gate.latest_version > 0
                            ? `v${gate.latest_version}（${gate.latest_status || '未知'}）`
                            : '—'}
                        </td>
                        <td>
                          <span class="gov-chip gov-chip-failed">{gate.binding_status}</span>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
          ) : null}

          {chains.length === 0 ? (
            <p class="gov-empty">没有匹配的资产行（诚实空态：未登记的制品不产生台账行）。</p>
          ) : (
            <div class="gov-table-wrap" data-asset-ledger="table">
              <table class="gov-table" aria-label="资产台账">
                <thead>
                  <tr>
                    <th scope="col">资产 / 版本链</th>
                    <th scope="col">类型</th>
                    <th scope="col">标题</th>
                    <th scope="col">生命周期</th>
                    <th scope="col">sensitivity</th>
                    <th scope="col">owner</th>
                    <th scope="col">supersedes</th>
                    <th scope="col">digest</th>
                  </tr>
                </thead>
                <tbody>
                  {chains.map((versions) => versions.map((asset, index) => (
                    <tr
                      key={`${asset.asset_id}@${asset.version}`}
                      data-asset-row={`${asset.asset_id}@${asset.version}`}
                      data-asset-sensitivity={asset.sensitivity}
                      data-asset-status={asset.status}
                    >
                      <td class="gov-mono">
                        {index === 0 ? (
                          <strong>{asset.asset_id}</strong>
                        ) : (
                          <span class="gov-dim">↳ 续</span>
                        )}{' '}
                        v{asset.version}
                      </td>
                      <td class="gov-mono">{asset.asset_type}</td>
                      <td>{asset.title}</td>
                      <td>
                        <span class={`gov-chip ${STATUS_CHIP[asset.status] || ''}`}>{asset.status}</span>
                      </td>
                      <td>
                        <span class={`gov-chip ${SENSITIVITY_CHIP[asset.sensitivity] || ''}`}>
                          {SENSITIVITY_COPY[asset.sensitivity] || asset.sensitivity}
                        </span>
                      </td>
                      <td class="gov-mono">{asset.owner_principal}</td>
                      <td class="gov-mono">{asset.supersedes_ref || '—'}</td>
                      <td class="gov-mono" title={asset.source_digest}>
                        {asset.source_digest ? asset.source_digest.slice(7, 19) + '…' : '—'}
                      </td>
                    </tr>
                  )))}
                </tbody>
              </table>
            </div>
          )}
        </>
      ) : null}
    </section>
  );
}
