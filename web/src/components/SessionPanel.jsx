export function SessionPanel({ sessions }) {
  if (!sessions || sessions.length === 0) {
    return (
      <div class="session-panel">
        <h3 class="section-title">Sessions</h3>
        <p class="empty-hint">No sessions</p>
      </div>
    );
  }

  return (
    <div class="session-panel">
      <h3 class="section-title">Sessions ({sessions.length})</h3>
      <div class="session-list">
        {sessions.map((s) => (
          <div key={s.id} class="session-card">
            <div class="session-header">
              <span class={`session-status ${s.status}`}>●</span>
              <span class="session-name">{s.id}</span>
              <span class="session-role">{s.role}</span>
              <span class="session-capacity">
                {s.client_type || 'unknown'}
              </span>
            </div>
            {s.workers && s.workers.length > 0 && (
              <div class="worker-list">
                {s.workers.map((w) => (
                  <div key={w.id} class="worker-item">
                    <span class="worker-icon">├──</span>
                    <span class="worker-id">{w.id}</span>
                    <span class={`worker-status ${w.status || 'unknown'}`}>
                      {w.status || 'unknown'}
                      {w.current_task_id ? ` · T: ${w.current_task_id}` : ''}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  );
}
