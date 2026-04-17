export function Overview({ projects, onSelect }) {
  return (
    <div class="overview">
      <h2 class="page-title">Dashboard Overview</h2>
      <p class="page-subtitle">{projects.length} project{projects.length !== 1 ? 's' : ''} registered</p>
      <div class="overview-grid">
        {projects.map((p) => {
          const tc = p.task_counts || {};
          const total = Object.values(tc).reduce((a, b) => a + b, 0);
          const done = tc.done || 0;
          const pct = total > 0 ? Math.round((done / total) * 100) : 0;
          return (
            <button
              key={p.id}
              class="overview-card"
              onClick={() => onSelect(p.id)}
            >
              <div class="overview-card-header">
                <span class={`status-badge ${p.status}`}>{p.status}</span>
                <h3>{p.name || p.id}</h3>
              </div>
              <p class="overview-card-id">{p.id}</p>
              {total > 0 && (
                <div class="overview-tasks">
                  <div class="overview-task-counts">
                    <span class="ov-count" title="Pending">{tc.pending || 0} pending</span>
                    <span class="ov-count" title="In Progress">{tc.in_progress || 0} active</span>
                    <span class="ov-count" title="Done">{done} done</span>
                  </div>
                  <div class="progress-bar" style="margin-top:6px">
                    <div class="progress-fill" style={`width: ${pct}%`}></div>
                  </div>
                </div>
              )}
            </button>
          );
        })}
      </div>
      {projects.length === 0 && (
        <div class="empty-state">
          <p>No projects yet. Create one via the MCP protocol or REST API.</p>
        </div>
      )}
    </div>
  );
}
