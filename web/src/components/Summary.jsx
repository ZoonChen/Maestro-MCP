export function Summary({ data }) {
  const counts = data.task_counts || {};
  const total = data.total_tasks || 0;
  const done = counts.done || 0;
  const pct = total > 0 ? Math.round((done / total) * 100) : 0;

  const featureWarning = (data.total_features || 0) > 50;
  const taskWarning = total > 500;

  return (
    <div class="summary">
      <div class="summary-stats">
        <div class="stat">
          <span class="stat-value">
            {data.total_features || 0}
            {featureWarning && <span class="stat-warning" title="Soft limit: 50 features">!</span>}
          </span>
          <span class="stat-label">Features</span>
        </div>
        <div class="stat">
          <span class="stat-value">
            {total}
            {taskWarning && <span class="stat-warning" title="Soft limit: 500 tasks">!</span>}
          </span>
          <span class="stat-label">Tasks</span>
        </div>
        <div class="stat">
          <span class="stat-value">{done}</span>
          <span class="stat-label">Done</span>
        </div>
        <div class="stat">
          <span class="stat-value">{pct}%</span>
          <span class="stat-label">Progress</span>
        </div>
      </div>
      <div class="progress-bar">
        <div class="progress-fill" style={`width: ${pct}%`}></div>
      </div>
    </div>
  );
}
