import { useCallback, useState } from 'preact/hooks';
import { apiGet, describeAPIError } from '../api/client';
import { ScopePicker, effectiveScopeId } from './ScopePicker';
import { ACTION_PERMISSION_HINTS } from '../governance';

const STAGE_CHIP = { off: '', shadow: 'gov-chip-stale', gray: 'gov-chip-stale', full: 'gov-chip-passed', rolled_back: 'gov-chip-failed' };
const STAGE_COPY = { off: '关闭', shadow: '影子', gray: '灰度', full: '全量', rolled_back: '已回滚' };

// Pilot rollout flags view (M4-UI-001 B2-3, consumes task brief G's
// read surface). Read-mostly by design: the console renders every flag
// with its frozen stage, gray percentage and last recorded decision;
// rollout decisions (pilot.write) stay on the API surface this
// generation — the view states that boundary instead of hiding it.
export function PilotFlagsView({ projects, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = effectiveScopeId(projectId, manualProjectId);

  const [status, setStatus] = useState('idle'); // idle|loading|ready|error
  const [error, setError] = useState('');
  const [flags, setFlags] = useState(null);

  const loadFlags = useCallback(async (event) => {
    event.preventDefault();
    if (!effectiveProjectId) {
      setError('请先选择或输入项目。');
      setStatus('error');
      return;
    }
    setError('');
    setStatus('loading');
    try {
      const row = await apiGet(`/api/v3/projects/${effectiveProjectId}/pilot-flags`);
      setFlags(Array.isArray(row?.flags) ? row.flags : []);
      setStatus('ready');
    } catch (e) {
      setFlags(null);
      setError(describeAPIError(e));
      setStatus('error');
    }
  }, [effectiveProjectId]);

  return (
    <section class="gov-page" aria-labelledby="pilot-flags-title">
      <header class="gov-header">
        <h1 id="pilot-flags-title">试点发布</h1>
        <p class="gov-lead">
          M4-PILOT-001 试点投放旗标（读为主）：每个旗标的冻结阶段、灰度百分比与最近一次决策。
          生命周期固定为 off → shadow → gray → full，任意活跃阶段可直接回滚（rolled_back 为终态，即杀伤开关）。
        </p>
        <p class="gov-note" role="note">
          投放决策记录（PUT，{ACTION_PERMISSION_HINTS['pilot.write']}）本代不进控制台，走 API/MCP；
          此处只读展示。读取需 pilot.read（项目成员角色普遍持有，服务端判定为准）。
        </p>
      </header>

      <form onSubmit={loadFlags}>
        <div class="gov-fieldset">
          <ScopePicker
            projects={projects}
            projectScope={projectScope}
            projectId={projectId}
            onProjectId={setProjectId}
            manualProjectId={manualProjectId}
            onManualProjectId={setManualProjectId}
          />
          <button type="submit" class="gov-button">读取旗标</button>
        </div>
      </form>

      {status === 'loading' ? <p role="status" class="gov-status">正在读取试点旗标…</p> : null}
      {status === 'error' ? <p role="alert" class="gov-status gov-status-error">{error}</p> : null}

      {status === 'ready' && flags?.length === 0 ? (
        <p class="gov-empty">该项目尚未登记任何试点旗标（诚实空态：未投放不虚构）。</p>
      ) : null}

      {status === 'ready' && flags?.length > 0 ? (
        <div class="gov-table-wrap">
          <table class="gov-table" aria-label="试点投放旗标">
            <thead>
              <tr>
                <th scope="col">旗标</th>
                <th scope="col">阶段</th>
                <th scope="col">灰度</th>
                <th scope="col">最近决策</th>
                <th scope="col">决策人</th>
                <th scope="col">更新时间</th>
              </tr>
            </thead>
            <tbody>
              {flags.map((flag) => (
                <tr key={flag.flag} data-pilot-flag={flag.flag}>
                  <td class="gov-mono">{flag.flag}</td>
                  <td>
                    <span class={`gov-chip ${STAGE_CHIP[flag.stage] || ''}`}>
                      {STAGE_COPY[flag.stage] || flag.stage}
                    </span>
                  </td>
                  <td class="gov-mono">{flag.stage === 'gray' ? `${flag.gray_percent}%` : '—'}</td>
                  <td>{flag.reason}</td>
                  <td class="gov-mono">{flag.changed_by}</td>
                  <td>{flag.updated_at}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );
}
