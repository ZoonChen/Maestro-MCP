import { useCallback, useState } from 'preact/hooks';
import { apiGet, describeAPIError } from '../api/client';
import { ScopePicker, effectiveScopeId } from './ScopePicker';

const STATE_CHIP = { healthy: 'gov-chip-passed', at_risk: 'gov-chip-stale', breached: 'gov-chip-failed', no_data: '' };
const STATE_COPY = { healthy: '健康', at_risk: '逼近预算', breached: '已击穿', no_data: '无数据' };

const OBJECTIVE_COPY = {
  api_p95_latency_ms: 'API p95 延迟',
  webhook_ingest_p95_latency_ms: 'Webhook 摄取 p95 延迟',
  inbox_lag_p95_seconds: 'Inbox 积压 p95',
  gate_eval_p95_latency_ms: 'Gate 评估 p95 延迟',
  rpo_minutes: 'RPO（恢复点目标）',
  rto_minutes: 'RTO（恢复时间目标）',
  backup_success_rate_percent: '备份成功率',
};

const WINDOW_COPY = { rolling_30d: '滚动 30 天', rolling_7d: '滚动 7 天', calendar_month: '自然月' };

function formatNumber(value) {
  return typeof value === 'number' ? Number(value.toFixed(2)).toString() : '—';
}

// SLO snapshot view (M4-UI-001 B2-3). Renders the frozen slo-status
// wire: availability with its error budget, the declared and deployment
// objectives with state coloring, and firing alerts with their runbook
// reference. Missing states stay honest: an unconfigured deployment
// answers 404 and a window without availability telemetry answers 503 —
// both render as explicit boundary copy, never a fabricated snapshot.
export function SLOSnapshotView({ projects, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = effectiveScopeId(projectId, manualProjectId);

  const [status, setStatus] = useState('idle'); // idle|loading|ready|error
  const [error, setError] = useState('');
  const [snapshot, setSnapshot] = useState(null);

  const loadSnapshot = useCallback(async (event) => {
    event.preventDefault();
    if (!effectiveProjectId) {
      setError('请先选择或输入项目。');
      setStatus('error');
      return;
    }
    setError('');
    setStatus('loading');
    try {
      const row = await apiGet(`/api/v3/projects/${effectiveProjectId}/slo-snapshot`);
      setSnapshot(row);
      setStatus('ready');
    } catch (e) {
      setSnapshot(null);
      setError(describeAPIError(e));
      setStatus('error');
    }
  }, [effectiveProjectId]);

  const availability = snapshot?.availability;

  return (
    <section class="gov-panel" aria-label="SLO 快照">
      <h2>SLO 快照</h2>
      <p class="gov-note">
        请求时评估的 slo-status：可用性目标与错误预算、声明目标（延迟类）与部署目标（RPO/RTO/备份成功率）。
        窗口内缺少可用性遥测时服务端 fail-closed（503），不编造数字；部署未配置 SLO 策略时端点不开放（404）。
      </p>
      <form onSubmit={loadSnapshot}>
        <div class="gov-fieldset">
          <ScopePicker
            projects={projects}
            projectScope={projectScope}
            projectId={projectId}
            onProjectId={setProjectId}
            manualProjectId={manualProjectId}
            onManualProjectId={setManualProjectId}
          />
          <button type="submit" class="gov-button">读取快照</button>
        </div>
      </form>

      {status === 'loading' ? <p role="status" class="gov-status">正在评估 SLO 快照…</p> : null}
      {status === 'error' ? <p role="alert" class="gov-status gov-status-error">{error}</p> : null}

      {status === 'ready' && snapshot ? (
        <>
          <dl class="gov-kv" data-slo-availability>
            <dt>窗口</dt>
            <dd>{WINDOW_COPY[snapshot.window?.kind] || snapshot.window?.kind || '—'}</dd>
            <dt>可用性目标</dt><dd class="gov-mono">{availability?.target_percent ?? '—'}%</dd>
            <dt>实测可用性</dt><dd class="gov-mono">{formatNumber(availability?.measured_percent)}%</dd>
            <dt>状态</dt>
            <dd>
              <span class={`gov-chip ${STATE_CHIP[availability?.state] || ''}`}>
                {STATE_COPY[availability?.state] || availability?.state || '—'}
              </span>
            </dd>
            <dt>错误预算余量</dt>
            <dd class="gov-mono">
              {typeof availability?.error_budget_remaining_percent === 'number'
                ? `${formatNumber(availability.error_budget_remaining_percent)}%`
                : '（未评估）'}
            </dd>
          </dl>

          {snapshot.degradation?.active ? (
            <p role="alert" class="gov-status gov-status-error">
              平台处于降级模式：{snapshot.degradation.mode}（自 {snapshot.degradation.since}）。
            </p>
          ) : null}

          {Array.isArray(snapshot.objectives) && snapshot.objectives.length > 0 ? (
            <div class="gov-table-wrap">
              <table class="gov-table" aria-label="SLO 目标">
                <thead>
                  <tr>
                    <th scope="col">目标</th>
                    <th scope="col">目标值</th>
                    <th scope="col">实测</th>
                    <th scope="col">状态</th>
                    <th scope="col">告警</th>
                  </tr>
                </thead>
                <tbody>
                  {snapshot.objectives.map((objective) => (
                    <tr key={objective.kind} data-slo-objective={objective.kind}>
                      <td>{OBJECTIVE_COPY[objective.kind] || objective.kind}</td>
                      <td class="gov-mono">{formatNumber(objective.target)}{objective.unit ? ` ${objective.unit}` : ''}</td>
                      <td class="gov-mono">{objective.state === 'no_data' ? '—' : formatNumber(objective.measured)}</td>
                      <td>
                        <span class={`gov-chip ${STATE_CHIP[objective.state] || ''}`}>
                          {STATE_COPY[objective.state] || objective.state}
                        </span>
                      </td>
                      <td>
                        {objective.alert?.firing ? (
                          <span class={`gov-chip ${objective.alert.severity === 'critical' ? 'gov-chip-failed' : 'gov-chip-stale'}`}>
                            告警中（{objective.alert.severity === 'critical' ? '严重' : '警告'}）→ {objective.alert.runbook_ref}
                          </span>
                        ) : '—'}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ) : (
            <p class="gov-empty">该部署未声明任何 SLO 目标（诚实空态）。</p>
          )}
          <p class="gov-note">快照生成时间：{snapshot.generated_at}</p>
        </>
      ) : null}
    </section>
  );
}
