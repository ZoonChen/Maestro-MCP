import { useCallback, useState } from 'preact/hooks';
import { apiGet, describeAPIError } from '../api/client';
import { ScopePicker, effectiveScopeId } from './ScopePicker';

const NODE_STATUS_CHIP = {
  done: 'gov-chip-passed', satisfied: 'gov-chip-passed', ready_for_human_merge: 'gov-chip-passed',
  failed: 'gov-chip-failed', needs_human: 'gov-chip-failed', blocked: 'gov-chip-failed',
  executing: 'gov-chip-stale', validating: 'gov-chip-stale', leased: 'gov-chip-stale',
  aggregating: 'gov-chip-stale', queued: 'gov-chip-stale',
};
const NODE_TYPE_COPY = { work_package: '工作包', work_item: '工作项', gate: '门' };
const JOIN_COPY = { all: '全部通过', any: '任一通过', quorum: '法定数' };

function thresholdCopy(nodeSpecs, nodeId) {
  const spec = (nodeSpecs || []).find((entry) => entry.node_id === nodeId);
  const threshold = spec?.success_threshold;
  if (!threshold) return '—';
  if (threshold.kind === 'quorum') return `${JOIN_COPY.quorum} k=${threshold.k}`;
  return JOIN_COPY[threshold.kind] || threshold.kind;
}

// Work Graph view (task brief J2c-2, ADR-009): the containment tree with
// node statuses and each aggregating node's JoinPolicy waiting rule.
// Read-only — the graph only changes through Coordinator proposals
// (MCP decomposition_propose) and the server-exclusive seal (the HITL
// surface on the proposal view).
export function WorkGraphView({ projects, projectScope }) {
  const [projectId, setProjectId] = useState(projects[0]?.id || '');
  const [manualProjectId, setManualProjectId] = useState('');
  const effectiveProjectId = effectiveScopeId(projectId, manualProjectId);

  const [status, setStatus] = useState('idle'); // idle|loading|ready|error
  const [error, setError] = useState('');
  const [plans, setPlans] = useState(null);
  const [planId, setPlanId] = useState('');
  const [detail, setDetail] = useState(null);

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
    try {
      const row = await apiGet(`/api/v3/projects/${effectiveProjectId}/work-graph/plans/${id}`);
      setDetail(row);
    } catch (e) {
      setDetail(null);
      setError(describeAPIError(e));
    }
  }, [effectiveProjectId]);

  const nodes = Array.isArray(detail?.nodes) ? detail.nodes : [];
  const specs = Array.isArray(detail?.node_specs) ? detail.node_specs : [];
  const childrenOf = (parentId) => nodes.filter((node) => node.parent_node_id === parentId);
  const roots = nodes.filter((node) => !node.parent_node_id);
  const revision = detail?.current_revision || {};

  return (
    <section class="gov-page" aria-labelledby="work-graph-title">
      <header class="gov-header">
        <h1 id="work-graph-title">Work Graph</h1>
        <p class="gov-lead">
          分层类型化工作图（ADR-009）：contains 树回答"属于谁"，requires 边回答"先做什么"。
          本视图只读（project.read）；改图只能由 Coordinator 经 MCP 提交拆解提案，封板走拆解提案审批视图。
        </p>
        <p class="gov-note" role="note">
          Work Graph 与资产台账存于 PostgreSQL 控制面；SQLite 部署（本地 m0 基线）不暴露本面，读取会得到明确边界提示。
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

      {status === 'loading' ? <p role="status" class="gov-status">正在读取 Work Graph…</p> : null}
      {status === 'error' ? <p role="alert" class="gov-status gov-status-error">{error}</p> : null}

      {status === 'ready' && plans?.length === 0 ? (
        <p class="gov-empty">该项目还没有工作计划（诚实空态：计划由 Coordinator 首个拆解提案创建）。</p>
      ) : null}

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

      {detail ? (
        <div class="gov-table-wrap" data-work-graph-plan={detail.plan?.human_code}>
          <h2 class="gov-subhead">
            {detail.plan?.human_code} — {detail.plan?.title}
          </h2>
          <p class="gov-dim" role="note">
            计划状态 <span class="gov-mono">{detail.plan?.status}</span> ·
            图版本 <span class="gov-mono">v{detail.plan?.graph_version}</span> ·
            当前修订 <span class="gov-mono">#{revision.revision_no ?? '—'} {revision.status || '不可用'}</span>
            {revision.sealed_at ? `（封板 ${revision.sealed_at}）` : '（草稿，未封板）'}
          </p>
          <table class="gov-table" aria-label="Work Graph 父子树">
            <thead>
              <tr>
                <th scope="col">层级 / 编号</th>
                <th scope="col">类型</th>
                <th scope="col">slot</th>
                <th scope="col">状态</th>
                <th scope="col">JoinPolicy</th>
                <th scope="col">子节点</th>
              </tr>
            </thead>
            <tbody>
              {roots.map((root) => renderNode(root, 0))}
            </tbody>
          </table>
        </div>
      ) : null}
    </section>
  );

  function renderNode(node, depth) {
    const children = childrenOf(node.id);
    return (
      <>
        <tr key={node.id} data-graph-node={node.human_code} data-node-status={node.status}>
          <td>
            <span style={{ paddingLeft: `${depth * 1.2}rem` }} class="gov-mono">
              {depth > 0 ? '└ ' : ''}{node.human_code}
            </span>
          </td>
          <td>{NODE_TYPE_COPY[node.node_type] || node.node_type}</td>
          <td class="gov-mono">{node.slot_key}</td>
          <td>
            <span class={`gov-chip ${NODE_STATUS_CHIP[node.status] || 'gov-chip-diagnostic'}`}>
              {node.status}
            </span>
          </td>
          <td>{thresholdCopy(specs, node.id)}</td>
          <td class="gov-mono">{children.length}</td>
        </tr>
        {children.map((child) => renderNode(child, depth + 1))}
      </>
    );
  }
}
