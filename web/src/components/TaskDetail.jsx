import { useState, useEffect, useRef } from 'preact/hooks';
import { ErrorNotice } from './ErrorNotice';
import { APIError, apiGet, describeAPIError } from '../api/client';

export function TaskDetail({ task, projectId, onClose }) {
  const [validations, setValidations] = useState([]);
  const [diff, setDiff] = useState('');
  const [tab, setTab] = useState('info');
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [diffUnavailable, setDiffUnavailable] = useState(false);
  const modalRef = useRef(null);
  const closeButtonRef = useRef(null);
  const returnFocusRef = useRef(null);

  useEffect(() => {
    returnFocusRef.current = document.activeElement;
    closeButtonRef.current?.focus();
    const handleKeyDown = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        onClose();
        return;
      }
      if (event.key !== 'Tab' || !modalRef.current) return;
      const focusable = [...modalRef.current.querySelectorAll(
        'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), [tabindex]:not([tabindex="-1"])',
      )];
      if (focusable.length === 0) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener('keydown', handleKeyDown);
    return () => {
      document.removeEventListener('keydown', handleKeyDown);
      returnFocusRef.current?.focus();
    };
  }, [task.id]);

  useEffect(() => {
    async function load() {
      setLoading(true);
      setError('');
      setDiffUnavailable(false);
      const [validationResult, diffResult] = await Promise.allSettled([
          apiGet(`/api/v1/projects/${projectId}/tasks/${task.id}/validation`),
          apiGet(`/api/v1/projects/${projectId}/tasks/${task.id}/diff`),
      ]);
      const errors = [];
      if (validationResult.status === 'fulfilled') {
        setValidations(validationResult.value || []);
      } else {
        errors.push(describeAPIError(validationResult.reason));
      }
      if (diffResult.status === 'fulfilled') {
        setDiff(diffResult.value?.diff || '');
      } else if (
        diffResult.reason instanceof APIError
        && diffResult.reason.code === 'WORKTREE_NOT_FOUND'
      ) {
        setDiff('');
        setDiffUnavailable(true);
      } else {
        errors.push(describeAPIError(diffResult.reason));
      }
      setError(errors.join(' '));
      setLoading(false);
    }
    if (task) load();
  }, [task, projectId]);

  if (!task) return null;

  return (
    <div class="modal-overlay" onClick={onClose}>
      <div
        ref={modalRef}
        class="modal"
        role="dialog"
        aria-modal="true"
        aria-labelledby={`task-detail-title-${task.id}`}
        onClick={(e) => e.stopPropagation()}
      >
        <div class="modal-header">
          <h3 id={`task-detail-title-${task.id}`}>{task.title}</h3>
          <button ref={closeButtonRef} type="button" class="modal-close" onClick={onClose} aria-label="Close task details">x</button>
        </div>
        <div class="modal-tabs">
          <button type="button" aria-pressed={tab === 'info'} class={`modal-tab ${tab === 'info' ? 'active' : ''}`} onClick={() => setTab('info')}>Info</button>
          <button type="button" aria-pressed={tab === 'validation'} class={`modal-tab ${tab === 'validation' ? 'active' : ''}`} onClick={() => setTab('validation')}>Validation</button>
          <button type="button" aria-pressed={tab === 'diff'} class={`modal-tab ${tab === 'diff' ? 'active' : ''}`} onClick={() => setTab('diff')}>Diff</button>
        </div>
        <div class="modal-body">
          <ErrorNotice message={error} />
          {loading ? (
            <div class="loading">Loading...</div>
          ) : tab === 'info' ? (
            <div class="task-info">
              <div class="info-row"><span class="info-label">ID</span><span class="info-value task-id">{task.id}</span></div>
              <div class="info-row"><span class="info-label">Status</span><span class={`status-text status-${task.status || 'unknown'}`}>{task.status || 'unknown'}</span></div>
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
                        <td>{v.coverage ?? '-'}</td>
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
                <p class="empty-hint">
                  {diffUnavailable ? 'Diff is not available until a worktree exists' : 'No changes detected'}
                </p>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
