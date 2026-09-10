import { useCallback, useState } from 'preact/hooks';
import { apiGet, apiPost, describeAPIError } from '../api/client';
import { ACTION_PERMISSION_HINTS } from '../governance';
import { ScopePicker, effectiveScopeId } from './ScopePicker';

const PROPOSAL_CHIP = {
  applied: 'gov-chip-passed',
  rejected: 'gov-chip-failed',
  submitted: 'gov-chip-stale',
};

// Decomposition proposal review (task brief J2c-2, the HITL surface of
// ADR-009 §2/§4): the Coordinator proposes through the MCP tool, the
// server validates and applies; the human approval step is the SEAL —
// freezing the draft revision behind the graph CAS so work becomes
// claimable. Sealing is deliberately console-only: the MCP catalog has
// no seal tool and delegated principals have no path to it.
export function ProposalReviewView({ projects, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = effectiveScopeId(projectId, manualProjectId);

  const [status, setStatus] = useState('idle'); // idle|loading|ready|error
  const [error, setError] = useState('');
  const [plans, setPlans] = useState(null);
  const [planId, setPlanId] = useState('');
  const [detail, setDetail] = useState(null);

  const [expectedVersion, setExpectedVersion] = useState('');
  const [idempotencyKey, setIdempotencyKey] = useState('');
  const [sealing, setSealing] = useState(false);
  const [sealResult, setSealResult] = useState(null);
  const [sealError, setSealError] = useState('');

  const loadPlans = useCallback(async (event) => {
    event.preventDefault();
    if (!effectiveProjectId) {
      setError('请先选择或输入项目。');
      setStatus('error');
      return;
    }
    setError('');
    setStatus('loading');
    try {
      const row = await apiGet(`/api/v3/projects/${effectiveProjectId}/work-graph`);
      setPlans(Array.isArray(row?.plans) ? row.plans : []);
      setPlanId('');
      setDetail(null);
      setStatus('ready');
    } catch (e) {
      setPlans(null);
      setDetail(null);
      setError(describeAPIError(e));
      setStatus('error');
    }
  }, [effectiveProjectId]);

  const loadPlan = useCallback(async (id) => {
    if (!id || !effectiveProjectId) return;
    setPlanId(id);
    setError('');
    setSealResult(null);
    setSealError('');
    try {
      const row = await apiGet(`/api/v3/projects/${effectiveProjectId}/work-graph/plans/${id}`);
      setDetail(row);
      setExpectedVersion(String(row?.plan?.graph_version || ''));
    } catch (e) {
      setDetail(null);
      setError(describeAPIError(e));
    }
  }, [effectiveProjectId]);

  const seal = useCallback(async (event) => {
    event.preventDefault();
    if (!effectiveProjectId || !planId) return;
    const version = Number(expectedVersion);
    if (!Number.isInteger(version) || version < 1) {
      setSealError('expected_graph_version 必须是 ≥1 的整数（图 CAS 令牌）。');
      return;
    }
    if (idempotencyKey.trim().length < 16) {
      setSealError('Idempotency-Key 至少 16 个字符。');
      return;
    }
    setSealing(true);
    setSealError('');
    try {
      const result = await apiPost(
        `/api/v3/projects/${effectiveProjectId}/work-graph/plans/${planId}/seal`,
        { expected_graph_version: version },
        { 'Idempotency-Key': idempotencyKey.trim() },
      );
      setSealResult(result);
      const refreshed = await apiGet(`/api/v3/projects/${effectiveProjectId}/work-graph/plans/${planId}`);
      setDetail(refreshed);
    } catch (e) {
      setSealError(describeAPIError(e));
    } finally {
      setSealing(false);
    }
  }, [effectiveProjectId, planId, expectedVersion, idempotencyKey]);

  const revision = detail?.current_revision || {};
  const proposals = Array.isArray(detail?.proposals) ? detail.proposals : [];
  const sealed = revision.status === 'sealed';

  return (
    <section class="gov-page" aria-labelledby="proposal-review-title">
      <header class="gov-header">
        <h1 id="proposal-review-title">拆解提案审批</h1>
        <p class="gov-lead">
          Coordinator 只能提交 DecompositionProposal（MCP 工具），服务端独占校验/改图；人工审批点是
          封板（seal）：冻结当前草稿修订，之后结构不可变、工作项才可领取。已决定提案是不可变协议事实。
        </p>
        <p class="gov-note" role="note">
          封板需 {`project_policy.strengthen`}（{ACTION_PERMISSION_HINTS['project_policy.strengthen']}）。
          封板是控制台专属操作：MCP 目录不含封板工具，Agent 无路径触达。
        </p>
      </header>

      <form onSubmit={loadPlans}>
        <div class="gov-fieldset">
          <ScopePicker
            projects={projects}
            projectScope={projectScope}
            projectId={projectId}
            onProjectId={setProjectId}
            manualProjectId={manualProjectId}
            onManualProjectId={setManualProjectId}
          />
          <button type="submit" class="gov-button">读取工作计划</button>
        </div>
      </form>

      {status === 'loading' ? <p role="status" class="gov-status">正在读取拆解提案…</p> : null}
      {status === 'error' ? <p role="alert" class="gov-status gov-status-error">{error}</p> : null}

      {status === 'ready' && plans?.length > 0 ? (
        <div class="gov-fieldset" role="group" aria-label="选择工作计划">
          {plans.map((plan) => (
            <button
              key={plan.id}
              type="button"
              class={`gov-button ${planId === plan.id ? '' : 'gov-button-ghost'}`}
              data-plan-option={plan.human_code}
              onClick={() => loadPlan(plan.id)}
            >
              {plan.human_code} · {plan.status}
            </button>
          ))}
        </div>
      ) : null}
      {status === 'ready' && plans?.length === 0 ? (
        <p class="gov-empty">该项目还没有工作计划（首个 Coordinator 提案会创建计划）。</p>
      ) : null}

      {detail ? (
        <div data-proposal-plan={detail.plan?.human_code}>
          <div class="gov-table-wrap">
            <h2 class="gov-subhead">当前修订与封板（HITL）</h2>
            <p class="gov-dim" data-revision-state={revision.status || 'unavailable'}>
              修订 #{revision.revision_no ?? '—'} · 状态 {revision.status || '不可用'}
              {revision.sealed_at ? ` · 封板于 ${revision.sealed_at}` : ' · 未封板（草稿可继续接受提案）'}
              {revision.spec_digest ? ` · digest ${revision.spec_digest.slice(7, 19)}…` : ''}
            </p>
            {sealed ? (
              <p class="gov-empty" data-seal-state="sealed">
                该修订已封板（不可变）。重规划需要新修订（J2b-2 replan 协议）。
              </p>
            ) : (
              <form onSubmit={seal} class="gov-fieldset" data-seal-form="draft">
                <label>
                  expected_graph_version（图 CAS 令牌，来自上方图版本）
                  <input
                    data-seal-input="expected_graph_version"
                    value={expectedVersion}
                    onInput={(e) => setExpectedVersion(e.target.value)}
                  />
                </label>
                <label>
                  Idempotency-Key（≥16 字符）
                  <input
                    data-seal-input="idempotency_key"
                    value={idempotencyKey}
                    onInput={(e) => setIdempotencyKey(e.target.value)}
                    placeholder="seal-20260910-…"
                  />
                </label>
                <button type="submit" class="gov-button" disabled={sealing}>
                  {sealing ? '正在封板…' : '批准并封板该修订'}
                </button>
                {sealError ? (
                  <p role="alert" class="gov-status gov-status-error">{sealError}</p>
                ) : null}
              </form>
            )}
            {sealResult ? (
              <p role="status" class="gov-status" data-seal-result={sealResult.revision?.status}>
                已封板：修订 #{sealResult.revision?.revision_no}
                {sealResult.replay ? '（幂等重放：此前已封板）' : ''} · digest {sealResult.revision?.spec_digest?.slice(7, 19)}…
              </p>
            ) : null}
          </div>

          <div class="gov-table-wrap">
            <h2 class="gov-subhead">已决定提案（{proposals.length}）</h2>
            {proposals.length === 0 ? (
              <p class="gov-empty">尚无已决定提案（诚实空态）。</p>
            ) : (
              <table class="gov-table" aria-label="拆解提案">
                <thead>
                  <tr>
                    <th scope="col">提案</th>
                    <th scope="col">决定</th>
                    <th scope="col">CAS 令牌</th>
                    <th scope="col">提交方</th>
                    <th scope="col">决定时间</th>
                    <th scope="col">违规 / 落图节点</th>
                  </tr>
                </thead>
                <tbody>
                  {proposals.map((proposal) => {
                    const violations = Array.isArray(proposal.violations) ? proposal.violations : [];
                    const applied = Array.isArray(proposal.applied_node_ids) ? proposal.applied_node_ids : [];
                    return (
                      <tr
                        key={proposal.id}
                        data-proposal-row={proposal.id}
                        data-proposal-status={proposal.status}
                      >
                        <td class="gov-mono">{proposal.id.slice(0, 8)}…</td>
                        <td>
                          <span class={`gov-chip ${PROPOSAL_CHIP[proposal.status] || ''}`}>
                            {proposal.status}
                          </span>
                        </td>
                        <td class="gov-mono">v{proposal.expected_graph_version}</td>
                        <td class="gov-mono">{proposal.submitted_by}</td>
                        <td>{proposal.decided_at || '—'}</td>
                        <td>
                          {violations.length > 0 ? (
                            <details>
                              <summary>{violations.length} 条违规（稳定错误码）</summary>
                              <ul class="gov-pending-list">
                                {violations.map((violation, index) => (
                                  <li key={index} class="gov-pending-item" data-violation-code={violation.code}>
                                    <span class="gov-mono">{violation.code}</span>
                                    {' '}{violation.message}
                                    {violation.node ? `（节点 ${violation.node}）` : ''}
                                  </li>
                                ))}
                              </ul>
                            </details>
                          ) : applied.length > 0 ? (
                            <span class="gov-dim">{applied.length} 个节点落图</span>
                          ) : (
                            <span class="gov-dim">—</span>
                          )}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </div>
        </div>
      ) : null}
    </section>
  );
}
