const COLUMNS = [
  { key: 'pending', label: 'Pending', statuses: ['pending', 'blocked'] },
  { key: 'in_progress', label: 'In Progress', statuses: ['in_progress'] },
  { key: 'review', label: 'Review', statuses: ['submitted', 'verifying', 'ready_to_merge'] },
  { key: 'done', label: 'Done', statuses: ['done'] },
  { key: 'conflicts', label: 'Conflicts', statuses: ['merge_conflicted'] },
];

function TaskCard({ task, onClick }) {
  const statusClass = task.status === 'blocked' ? 'blocked'
    : task.status === 'merge_conflicted' ? 'conflicted'
    : '';

  return (
    <div class={`task-card ${statusClass}`} title={task.description} onClick={() => onClick(task)}>
      <div class="task-card-header">
        <span class="task-id">{task.id}</span>
        <span class="task-role">{task.role}</span>
      </div>
      <p class="task-title">{task.title}</p>
      {task.status === 'blocked' && task.blocker_reason && (
        <p class="task-blocker">⚠ {task.blocker_reason}</p>
      )}
      {task.status === 'merge_conflicted' && (
        <p class="task-conflict">⚡ Merge conflict</p>
      )}
      {task.assigned_worker_id && (
        <span class="task-worker">{task.assigned_worker_id}</span>
      )}
      {task.priority && task.priority !== 'normal' && (
        <span class={`task-priority ${task.priority}`}>{task.priority}</span>
      )}
    </div>
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
        const colTasks = filteredTasks.filter((t) =>
          col.statuses.includes(t.status)
        );
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
