// Workbench navigation plus the governance areas (M4-UI-001). Areas are
// navigation only — data visibility and every action stay decided by the
// server (UI-RULE-001); unknown-role sessions see the read-only workbench
// and the boundary notice rendered by the identity bar.
export function Sidebar({
  projects, selectedId, onSelect, theme, onToggleTheme, view, onNavigate, roles = [],
}) {
  const navItem = (label, target, active, icon = '◈') => (
    <button
      type="button"
      class={`sidebar-item ${active ? 'active' : ''}`}
      onClick={() => onNavigate(target)}
    >
      <span class="sidebar-icon">{icon}</span>
      {label}
    </button>
  );

  const adminVisible = roles.includes('platform_admin');
  const operationsVisible = ['platform_admin', 'operations_owner', 'security_owner']
    .some((role) => roles.includes(role));

  return (
    <aside class="sidebar">
      <div class="sidebar-header">
        <h1>Maestro MCP</h1>
        <button
          type="button"
          class="theme-toggle"
          onClick={onToggleTheme}
          title="Toggle theme"
          aria-label={`Switch to ${theme === 'dark' ? 'light' : 'dark'} theme`}
        >
          {theme === 'dark' ? '☀' : '☾'}
        </button>
      </div>
      <div class="sidebar-selector">
        <label class="sr-only" for="project-select">Select project</label>
        <select
          id="project-select"
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
      <nav class="sidebar-nav" aria-label="控制台导航">
        <div class="sidebar-section">态势</div>
        {navItem('概览', '#/', view === 'overview', '◉')}
        <div class="sidebar-section">执行</div>
        {projects.map((p) => (
          <button
            type="button"
            key={p.id}
            class={`sidebar-item ${view === 'project' && selectedId === p.id ? 'active' : ''}`}
            onClick={() => onSelect(p.id)}
            title={p.status === 'archived' ? 'Archived' : 'Active'}
          >
            <span class={`sidebar-dot ${p.status === 'archived' ? 'archived' : 'active'}`}>
              ●
            </span>
            {p.name || p.id}
          </button>
        ))}
        <div class="sidebar-section">治理</div>
        {navItem('HITL 豁免审批', '#/waivers', view === 'waivers')}
        {navItem('试点发布', '#/pilot', view === 'pilot')}
        {navItem('Jira 连接器', '#/jira', view === 'jira')}
        {navItem('Work Graph', '#/workgraph', view === 'workgraph')}
        {navItem('资产台账', '#/assets', view === 'assets')}
        {navItem('拆解提案审批', '#/proposals', view === 'proposals')}
        {navItem('八场景地图', '#/scenarios', view === 'scenarios')}
        <div class="sidebar-section">质量</div>
        {navItem('MR · Pipeline', '#/mrs', view === 'mrs')}
        {adminVisible ? (
          <>
            <div class="sidebar-section">管理</div>
            {navItem('管理面板', '#/admin', view === 'admin', '⚙')}
          </>
        ) : null}
        {operationsVisible ? (
          <>
            <div class="sidebar-section">运维</div>
            {navItem('运维面板', '#/operations', view === 'operations', '⚑')}
          </>
        ) : null}
      </nav>
    </aside>
  );
}
