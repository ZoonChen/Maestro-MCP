import { useState, useEffect, useCallback, useRef } from 'preact/hooks';
import { ErrorNotice } from './ErrorNotice';
import { apiGet, describeAPIError } from '../api/client';

const EVENT_ICONS = {
  'task.created': '+',
  'task.claimed': '→',
  'task.submitted': '✓',
  'task.unblocked': '→',
  'task.verifying': '◉',
  'task.approved': '✓✓',
  'task.rejected': '✗',
  'task.blocked': '⚠',
  'task.cancelled': '⊘',
  'task.done': '✓✓✓',
  'task.merge_requested': '↔',
  'task.followup_created': '↗',
  'task.reopened': '↺',
  'validation.passed': '✓',
  'validation.failed': '✗',
  'session.online': '●',
  'session.offline': '○',
  'feature.created': '★',
  'feature.updated': '★',
};

const EVENT_COLORS = {
  'task.approved': 'text-green',
  'task.done': 'text-green',
  'validation.passed': 'text-green',
  'task.created': 'text-blue',
  'task.claimed': 'text-blue',
  'feature.created': 'text-blue',
  'task.submitted': 'text-blue',
  'task.unblocked': 'text-blue',
  'task.verifying': 'text-yellow',
  'task.followup_created': 'text-yellow',
  'task.reopened': 'text-yellow',
  'task.rejected': 'text-red',
  'task.blocked': 'text-red',
  'task.cancelled': 'text-red',
  'validation.failed': 'text-red',
  'task.needs_human': 'text-red',
};

function normalizedEntry(entry) {
  if (entry.type) {
    return {
      ...entry,
      eventType: entry.type,
      createdAt: entry.timestamp || entry.occurred_at,
      taskId: entry.payload?.task_id,
      detail: entry.payload,
    };
  }
  const prefix = entry.task_id ? 'task.' : '';
  return {
    ...entry,
    eventType: entry.event_type || `${prefix}${entry.action || 'activity'}`,
    createdAt: entry.created_at,
    taskId: entry.task_id,
  };
}

function formatDetail(action, detail) {
  if (!detail) return action;
  try {
    const d = typeof detail === 'string' ? JSON.parse(detail) : detail;
    const parts = [];
    if (d.worker_id) parts.push(`by ${d.worker_id}`);
    if (d.reason) parts.push(d.reason);
    if (d.reassign !== undefined) parts.push(d.reassign ? '(reassigned)' : '(to pending)');
    if (d.merge_commit) parts.push(d.merge_commit.substring(0, 8));
    if (d.previous_status) parts.push(`from ${d.previous_status}`);
    if (d.new_task_id) parts.push(`→ ${d.new_task_id}`);
    if (d.passed !== undefined) parts.push(d.passed ? 'PASSED' : 'FAILED');
    return parts.length > 0 ? parts.join(' ') : action;
  } catch {
    return action;
  }
}

export function ActivityLog({ projectId, wsEvents, refreshVersion, wsStatus }) {
  const [activity, setActivity] = useState([]);
  const [showWs, setShowWs] = useState(false);
  const [error, setError] = useState('');
  const requestVersion = useRef(0);

  const fetchActivity = useCallback(async () => {
    const currentRequest = ++requestVersion.current;
    setError('');
    try {
      const nextActivity = await apiGet(`/api/v1/projects/${projectId}/board/activity?limit=50`);
      if (currentRequest !== requestVersion.current) return;
      setActivity(nextActivity || []);
    } catch (requestError) {
      if (currentRequest !== requestVersion.current) return;
      setError(describeAPIError(requestError));
    }
  }, [projectId]);

  useEffect(() => {
    fetchActivity();
    return () => { requestVersion.current += 1; };
  }, [fetchActivity]);
  useEffect(() => {
    if (refreshVersion > 0) fetchActivity();
  }, [refreshVersion, fetchActivity]);

  const formatTime = (ts) => {
    if (!ts) return '';
    return ts.replace('T', ' ').substring(0, 16);
  };

  const entries = showWs
    ? [...wsEvents.slice(0, 50), ...activity]
    : activity;

  return (
    <div class="activity-log">
      <div class="activity-header">
        <h3 class="section-title">Activity Log</h3>
        <label class="toggle-label">
          <input
            type="checkbox"
            checked={showWs}
            onChange={(e) => setShowWs(e.target.checked)}
          />
          Live events
        </label>
      </div>
      <span class={`connection-status ${wsStatus}`}>WebSocket: {wsStatus}</span>
      <ErrorNotice message={error} onRetry={fetchActivity} />
      <div class="activity-list">
        {entries.slice(0, 50).map((entry, i) => {
          const normalized = normalizedEntry(entry);
          const eventType = normalized.eventType;
          const icon = EVENT_ICONS[eventType] || '·';
          const colorClass = EVENT_COLORS[eventType] || '';
          return (
            <div key={`${entry.id || ''}-${i}`} class="activity-entry">
              <span class="activity-icon">{icon}</span>
              <span class="activity-time">{formatTime(normalized.createdAt)}</span>
              <span class={`activity-text ${colorClass}`}>
                {formatDetail(eventType, normalized.detail)}
              </span>
              {normalized.taskId && (
                <span class="activity-task">{normalized.taskId}</span>
              )}
            </div>
          );
        })}
        {entries.length === 0 && (
          <p class="empty-hint">No activity yet</p>
        )}
      </div>
    </div>
  );
}
