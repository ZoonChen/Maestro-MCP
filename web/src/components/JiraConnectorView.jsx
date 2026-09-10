import { useCallback, useState } from 'preact/hooks';
import { apiGet, describeAPIError } from '../api/client';
import { ScopePicker, effectiveScopeId } from './ScopePicker';

const FIELD_COPY = {
  title: '标题',
  assignee: '指派人',
  iteration_label: '迭代标签',
  status_label: '状态标签',
};
const STATE_CHIP = { open: 'gov-chip-stale', escalated: 'gov-chip-failed', resolved: 'gov-chip-passed' };
const STATE_COPY = { open: '待裁决', escalated: '已升级', resolved: '已裁决' };

// Jira connector view (task brief J3-5): the WorkItem↔issue anchors
// with both sides of the mirror, and the reconcile divergence list.
// Read-only by design — anchoring and adjudication stay on their
// server-side surfaces this generation; the view states that boundary
// instead of hiding it. Sync semantics (SOLUTION-BLUEPRINT §1.2):
// Maestro is the SoR; a live divergence freezes its field's mirror
// push until a human adjudicates; two cycles unresolved escalate.
export function JiraConnectorView({ projects, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = effectiveScopeId(projectId, manualProjectId);

  const [status, setStatus] = useState('idle'); // idle|loading|ready|error
  const [error, setError] = useState('');
  const [anchors, setAnchors] = useState(null);
  const [items, setItems] = useState(null);
  const [history, setHistory] = useState(null);
  const [showHistory, setShowHistory] = useState(false);

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
      const [anchorRow, itemRow] = await Promise.all([
        apiGet(`/api/v3/projects/${effectiveProjectId}/jira-anchors`),
        apiGet(`/api/v3/projects/${effectiveProjectId}/jira-reconcile-items`),
      ]);
      setAnchors(Array.isArray(anchorRow?.anchors) ? anchorRow.anchors : []);
      setItems(Array.isArray(itemRow?.items) ? itemRow.items : []);
      setHistory(null);
      setShowHistory(false);
      setStatus('ready');
    } catch (e) {
      setAnchors(null);
      setItems(null);
      setError(describeAPIError(e));
      setStatus('error');
    }
  }, [effectiveProjectId]);

  const loadHistory = useCallback(async () => {
    if (!effectiveProjectId) return;
    try {
      const row = await apiGet(`/api/v3/projects/${effectiveProjectId}/jira-reconcile-items?state=resolved`);
      setHistory(Array.isArray(row?.items) ? row.items : []);
      setShowHistory(true);
    } catch (e) {
      setError(describeAPIError(e));
    }
  }, [effectiveProjectId]);

  return (
    <section class="gov-page" aria-labelledby="jira-connector-title">
      <header class="gov-header">
        <h1 id="jira-connector-title">Jira 连接器</h1>
        <p class="gov-lead">
          WorkItem↔issue 锚定与镜像对账（只读）。Maestro 是任务事实源：标题/指派/迭代标签/状态标签单向镜像到
          Jira（状态只进 maestro: 标签，不改 Jira issue 状态字段）；Jira 侧字段分歧进对账清单，人工裁决后才更新镜像，连续两个周期未裁决升级 owner。
        </p>
        <p class="gov-note" role="note">
          本视图只读（读取需 project.read）。锚点创建与分歧裁决（accept_sor / accept_mirror）本代走服务端接口，不进控制台。
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
          <button type="submit" class="gov-button">读取锚点与对账清单</button>
        </div>
      </form>

      {status === 'loading' ? <p role="status" class="gov-status">正在读取 Jira 连接器数据…</p> : null}
      {status === 'error' ? <p role="alert" class="gov-status gov-status-error">{error}</p> : null}

      {status === 'ready' && anchors?.length === 0 ? (
        <p class="gov-empty">该项目尚未锚定任何 Jira issue（诚实空态：未锚定的 issue 不产生 Maestro 副作用）。</p>
      ) : null}

      {status === 'ready' && anchors?.length > 0 ? (
        <div class="gov-table-wrap">
          <h2 class="gov-subhead">锚点（{anchors.length}）</h2>
          <table class="gov-table" aria-label="Jira 锚点">
            <thead>
              <tr>
                <th scope="col">Issue</th>
                <th scope="col">Maestro 侧（SoR）</th>
                <th scope="col">状态标签</th>
                <th scope="col">指派 / 迭代</th>
                <th scope="col">Jira 侧快照（只读）</th>
                <th scope="col">最近镜像</th>
              </tr>
            </thead>
            <tbody>
              {anchors.map((anchor) => (
                <tr key={anchor.issue_key} data-jira-anchor={anchor.issue_key}>
                  <td class="gov-mono">{anchor.issue_key}</td>
                  <td>
                    <span class="gov-mono">{anchor.sor_title}</span>
                    <br />
                    <span class="gov-dim">{anchor.sor_status}</span>
                  </td>
                  <td class="gov-mono">{anchor.sor_status_label}</td>
                  <td class="gov-mono">{anchor.sor_assignee || '—'} / {anchor.sor_iteration_label || '—'}</td>
                  <td>
                    <span class="gov-mono">{anchor.issue_title || '（无快照）'}</span>
                    <br />
                    <span class="gov-dim">
                      {anchor.issue_status || '—'} · {(anchor.issue_labels || []).join(', ') || '无标签'}
                    </span>
                  </td>
                  <td>
                    {anchor.last_mirror_ok ? (
                      <span class="gov-chip gov-chip-passed">正常</span>
                    ) : anchor.last_mirror_at ? (
                      <span class="gov-chip gov-chip-failed" title={anchor.last_error || ''}>降级</span>
                    ) : (
                      <span class="gov-chip gov-chip-diagnostic">未运行</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}

      {status === 'ready' ? (
        <div class="gov-table-wrap">
          <h2 class="gov-subhead">对账清单{items?.length ? `（待裁决 ${items.length}）` : ''}</h2>
          {items?.length === 0 ? (
            <p class="gov-empty">当前无待裁决分歧（镜像与 SoR 一致，或已全部裁决）。</p>
          ) : (
            <table class="gov-table" aria-label="Jira 对账清单">
              <thead>
                <tr>
                  <th scope="col">Issue</th>
                  <th scope="col">字段</th>
                  <th scope="col">Maestro（SoR）</th>
                  <th scope="col">Jira 侧</th>
                  <th scope="col">状态</th>
                  <th scope="col">周期</th>
                  <th scope="col">检出时间</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item) => (
                  <tr key={item.id} data-reconcile-item={item.id} data-reconcile-state={item.state}>
                    <td class="gov-mono">{item.issue_key}</td>
                    <td>{FIELD_COPY[item.field] || item.field}</td>
                    <td class="gov-mono">{item.sor_value}</td>
                    <td class="gov-mono">{item.mirror_value}</td>
                    <td>
                      <span class={`gov-chip ${STATE_CHIP[item.state] || ''}`}>
                        {STATE_COPY[item.state] || item.state}
                      </span>
                    </td>
                    <td class="gov-mono">{item.open_cycles}</td>
                    <td>{item.detected_at}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {showHistory && history?.length > 0 ? (
            <details>
              <summary>已裁决历史（{history.length}）</summary>
              <table class="gov-table" aria-label="已裁决历史">
                <thead>
                  <tr>
                    <th scope="col">Issue</th>
                    <th scope="col">字段</th>
                    <th scope="col">裁决</th>
                    <th scope="col">裁决人</th>
                    <th scope="col">备注</th>
                  </tr>
                </thead>
                <tbody>
                  {history.map((item) => (
                    <tr key={item.id} data-reconcile-history={item.id}>
                      <td class="gov-mono">{item.issue_key}</td>
                      <td>{FIELD_COPY[item.field] || item.field}</td>
                      <td class="gov-mono">{item.resolution}</td>
                      <td class="gov-mono">{item.resolved_by}</td>
                      <td>{item.resolution_note || '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </details>
          ) : (
            <p>
              <button type="button" class="gov-button gov-button-ghost" onClick={loadHistory}>
                查看已裁决历史
              </button>
            </p>
          )}
        </div>
      ) : null}
    </section>
  );
}
