export function Sidebar({ projects, selectedId, onSelect, theme, onToggleTheme }) {
  return (
    <aside class="sidebar">
      <div class="sidebar-header">
        <h1>Maestro MCP</h1>
        <button class="theme-toggle" onClick={onToggleTheme} title="Toggle theme">
          {theme === 'dark' ? '☀' : '☾'}
        </button>
      </div>
      <div class="sidebar-selector">
        <select
          class="project-select"
          value={selectedId || ''}
          onChange={(e) => onSelect(e.target.value || null)}
        >
          <option value="">-- Overview --</option>
          {projects.map((p) => (
            <option key={p.id} value={p.id}>{p.name || p.id}</option>
          ))}
        </select>
      </div>
      <nav class="sidebar-nav">
        <button
          class={`sidebar-item ${!selectedId ? 'active' : ''}`}
          onClick={() => onSelect(null)}
        >
          <span class="sidebar-icon">◉</span>
          Overview
        </button>
        <div class="sidebar-section">Projects</div>
        {projects.map((p) => (
          <button
            key={p.id}
            class={`sidebar-item ${selectedId === p.id ? 'active' : ''}`}
            onClick={() => onSelect(p.id)}
            title={p.status === 'archived' ? 'Archived' : 'Active'}
          >
            <span class={`sidebar-dot ${p.status === 'archived' ? 'archived' : 'active'}`}>
              ●
            </span>
            {p.name || p.id}
          </button>
        ))}
      </nav>
    </aside>
  );
}
