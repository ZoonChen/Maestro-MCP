import { useState, useEffect } from 'preact/hooks';

export function TaskDetail({ task, projectId, onClose }) {
  const [validations, setValidations] = useState([]);
  const [diff, setDiff] = useState('');
  const [tab, setTab] = useState('info');
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    async function load() {
      setLoading(true);
      try {
        const [vRes, dRes] = await Promise.all([
          fetch(`/api/v1/projects/${projectId}/tasks/${task.id}/validation`),
          fetch(`/api/v1/projects/${projectId}/tasks/${task.id}/diff`),
        ]);
        const vJson = await vRes.json();
        const dJson = await dRes.json();
        if (vJson.data) setValidations(vJson.data);
        if (dJson.data) setDiff(dJson.data.diff || '');
      } catch (e) {
        console.error('Failed to load task detail:', e);
      }
      setLoading(false);
    }
    if (task) load();
  }, [task, projectId]);

  if (!task) return null;

  return (
    <div class="modal-overlay" onClick={onClose}>
      <div class="modal" onClick={(e) => e.stopPropagation()}>
        <div class="modal-header">
          <h3>{task.title}</h3>
          <button class="modal-close" onClick={onClose}>x</button>
        </div>
        <div class="modal-tabs">
          <button class={`modal-tab ${tab === 'info' ? 'active' : ''}`} onClick={() => setTab('info')}>Info</button>
          <button class={`modal-tab ${tab === 'validation' ? 'active' : ''}`} onClick={() => setTab('validation')}>Validation</button>
          <button class={`modal-tab ${tab === 'diff' ? 'active' : ''}`} onClick={() => setTab('diff')}>Diff</button>
        </div>
        <div class="modal-body">
          {loading ? (
            <div class="loading">Loading...</div>
          ) : tab === 'info' ? (
            <div class="task-info">
              <div class="info-row"><span class="info-label">ID</span><span class="info-value task-id">{task.id}</span></div>
              <div class="info-row"><span class="info-label">Status</span><span class={`status-text status-${task.status}`}>{task.status}</span></div>
              <div class="info-row"><span class="info-label">Role</span><span class="info-value">{task.role}</span></div>
              <div class="info-row"><span class="info-label">Priority</span><span class="info-value">{task.priority}</span></div>
              <div class="info-row"><span class="info-label">Feature</span><span class="info-value">{task.feature_id}</span></div>
              <div class="info-row"><span class="info-label">Description</span></div>
              <p class="info-desc">{task.description}</p>
              {task.allowed_directories && (
                <div class="info-row"><span class="info-label">Allowed dirs</span><span class="info-value">{task.allowed_directories}</span></div>
              )}
              {task.summary && (
                <div class="info-row"><span class="info-label">Summary</span><span class="info-value">{task.summary}</span></div>
              )}
            </div>
          ) : tab === 'validation' ? (
            <div class="validation-list">
              {validations.length === 0 ? (
                <p class="empty-hint">No validation runs yet</p>
              ) : (
                <table class="validation-table">
                  <thead>
                    <tr>
                      <th>#</th>
                      <th>Result</th>
                      <th>Test</th>
                      <th>Boundary</th>
                      <th>Coverage</th>
                      <th>Duration</th>
                    </tr>
                  </thead>
                  <tbody>
                    {validations.map((v) => (
                      <tr key={v.id}>
                        <td>{v.attempt}</td>
                        <td class={`status-text status-${v.result}`}>{v.result}</td>
                        <td class={v.test_ok ? 'text-green' : 'text-red'}>{v.test_ok ? 'PASS' : 'FAIL'}</td>
                        <td class={v.boundary_ok ? 'text-green' : 'text-red'}>{v.boundary_ok ? 'OK' : 'FAIL'}</td>
                        <td>{v.coverage || '-'}</td>
                        <td>{v.duration_ms ? `${v.duration_ms}ms` : '-'}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
              {validations.length > 0 && validations[0].test_output && (
                <details class="test-output-details">
                  <summary>Test Output (latest)</summary>
                  <pre class="test-output">{validations[0].test_output}</pre>
                </details>
              )}
            </div>
          ) : (
            <div class="diff-view">
              {diff ? (
                <pre class="diff-output">{diff}</pre>
              ) : (
                <p class="empty-hint">No changes detected or no worktree available</p>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
