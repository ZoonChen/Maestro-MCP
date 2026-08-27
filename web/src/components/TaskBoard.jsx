const COLUMNS = [
  { key: 'draft', label: 'Draft', statuses: ['draft'] },
  { key: 'queued', label: 'Queued', statuses: ['queued'] },
  { key: 'active', label: 'In Progress', statuses: ['leased', 'executing'] },
  { key: 'review', label: 'Review', statuses: ['validating', 'ready_for_human_merge'] },
  { key: 'done', label: 'Done', statuses: ['done'] },
  { key: 'attention', label: 'Needs Attention', statuses: ['blocked', 'failed', 'needs_human', 'cancelling'] },
];
const KNOWN_STATUSES = new Set([...COLUMNS.flatMap((column) => column.statuses), 'cancelled']);

function needsAttention(status) {
  return ['blocked', 'failed', 'needs_human', 'cancelling'].includes(status) || !KNOWN_STATUSES.has(status);
}

function TaskCard({ task, onClick }) {
  const displayStatus = String(task.status || 'unknown');
  const statusClass = task.status === 'blocked' ? 'blocked'
    : needsAttention(task.status) ? 'conflicted'
    : '';

  return (
    <button type="button" class={`task-card ${statusClass}`} title={task.description} onClick={() => onClick(task)}>
      <div class="task-card-header">
        <span class="task-id">{task.id}</span>
        <span class="task-role">{task.role}</span>
      </div>
      <p class="task-title">{task.title}</p>
      {task.status === 'blocked' && task.blocker_reason && (
        <p class="task-blocker">⚠ {task.blocker_reason}</p>
      )}
      {needsAttention(task.status) && (
        <p class="task-conflict">⚡ {displayStatus.replaceAll('_', ' ')}</p>
      )}
      {task.assigned_worker_id && (
        <span class="task-worker">{task.assigned_worker_id}</span>
      )}
      {task.priority && task.priority !== 'normal' && (
        <span class={`task-priority ${task.priority}`}>{task.priority}</span>
      )}
    </button>
  );
}

export function TaskBoard({ tasks, showCancelled, onTaskClick, featureFilter }) {
  const filteredTasks = tasks.filter((t) => {
    if (!showCancelled && t.status === 'cancelled') return false;
    if (featureFilter && t.feature_id !== featureFilter) return false;
    return true;
  });

  return (
    <div class="task-board">
      {COLUMNS.map((col) => {
        const colTasks = filteredTasks.filter((t) => (
          col.statuses.includes(t.status) || (col.key === 'attention' && !KNOWN_STATUSES.has(t.status))
        ));
        return (
          <div key={col.key} class="board-column">
            <div class="column-header">
              <span class="column-title">{col.label}</span>
              <span class="column-count">{colTasks.length}</span>
            </div>
            <div class="column-body">
              {colTasks.map((t) => (
                <TaskCard key={t.id} task={t} onClick={onTaskClick} />
              ))}
              {colTasks.length === 0 && (
                <div class="column-empty">-</div>
              )}
            </div>
          </div>
        );
      })}
      {showCancelled && (
        <div class="board-column">
          <div class="column-header">
            <span class="column-title">Cancelled</span>
            <span class="column-count">
              {filteredTasks.filter((t) => t.status === 'cancelled').length}
            </span>
          </div>
          <div class="column-body">
            {filteredTasks
              .filter((t) => t.status === 'cancelled')
              .map((t) => (
                <TaskCard key={t.id} task={t} onClick={onTaskClick} />
              ))}
          </div>
        </div>
      )}
    </div>
  );
}
