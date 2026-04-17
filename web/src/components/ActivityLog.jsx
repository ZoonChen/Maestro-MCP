import { useState, useEffect, useCallback } from 'preact/hooks';

const EVENT_ICONS = {
  task_created: '+',
  task_claimed: '→',
  task_submitted: '✓',
  task_unblocked: '→',
  task_verifying: '◉',
  task_approved: '✓✓',
  task_rejected: '✗',
  task_blocked: '⚠',
  merge_conflicted: '⚡',
  conflict_reopened: '↺',
  task_cancelled: '⊘',
  task_merged: '✓✓✓',
  task_done: '✓✓✓',
  task_merge_requested: '↔',
  followup_created: '↗',
  reopened: '↺',
  validation_passed: '✓',
  validation_failed: '✗',
  session_online: '●',
  session_offline: '○',
  feature_created: '★',
  feature_updated: '★',
};

const EVENT_COLORS = {
  task_approved: 'text-green',
  task_merged: 'text-green',
  task_done: 'text-green',
  validation_passed: 'text-green',
  task_created: 'text-blue',
  task_claimed: 'text-blue',
  feature_created: 'text-blue',
  task_submitted: 'text-blue',
  task_unblocked: 'text-blue',
  task_verifying: 'text-yellow',
  followup_created: 'text-yellow',
  reopened: 'text-yellow',
  task_rejected: 'text-red',
  task_blocked: 'text-red',
  task_cancelled: 'text-red',
  validation_failed: 'text-red',
  merge_conflicted: 'text-red',
};

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

export function ActivityLog({ projectId, wsEvents }) {
  const [activity, setActivity] = useState([]);
  const [showWs, setShowWs] = useState(false);

  const fetchActivity = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/projects/${projectId}/board/activity?limit=50`);
      const json = await res.json();
      if (json.data) setActivity(json.data);
    } catch {}
  }, [projectId]);

  useEffect(() => { fetchActivity(); }, [fetchActivity]);

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
      <div class="activity-list">
        {entries.slice(0, 50).map((entry, i) => {
          const eventType = entry.event_type || entry.action || '';
          const icon = EVENT_ICONS[eventType] || '·';
          const colorClass = EVENT_COLORS[eventType] || '';
          return (
            <div key={`${entry.id || ''}-${i}`} class="activity-entry">
              <span class="activity-icon">{icon}</span>
              <span class="activity-time">{formatTime(entry.created_at)}</span>
              <span class={`activity-text ${colorClass}`}>
                {formatDetail(eventType, entry.detail)}
              </span>
              {entry.task_id && (
                <span class="activity-task">{entry.task_id}</span>
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
