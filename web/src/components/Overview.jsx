export function Overview({ projects, onSelect }) {
  return (
    <div class="overview">
      <h2 class="page-title">Dashboard Overview</h2>
      <p class="page-subtitle">{projects.length} project{projects.length !== 1 ? 's' : ''} registered</p>
      <div class="overview-grid">
        {projects.map((p) => {
          const tc = p.task_counts || {};
          const total = Object.values(tc).reduce((a, b) => a + b, 0) - (tc.cancelled || 0);
          const done = tc.done || 0;
          const draft = tc.draft || 0;
          const queued = tc.queued || 0;
          const active = (tc.leased || 0) + (tc.executing || 0);
          const review = (tc.validating || 0) + (tc.ready_for_human_merge || 0);
          const attention = (tc.blocked || 0) + (tc.failed || 0)
            + (tc.needs_human || 0) + (tc.cancelling || 0);
          const pct = total > 0 ? Math.round((done / total) * 100) : 0;
          return (
            <button
              type="button"
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
                    <span class="ov-count" title="Not dispatched">{draft} draft</span>
                    <span class="ov-count" title="Available for lease">{queued} queued</span>
                    <span class="ov-count" title="Leased or executing">{active} active</span>
                    <span class="ov-count" title="Validating or ready for human merge">{review} review</span>
                    <span class="ov-count" title="Blocked or needs intervention">{attention} attention</span>
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
