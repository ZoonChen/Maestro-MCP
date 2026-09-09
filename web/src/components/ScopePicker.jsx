// Shared governance-scope picker (M4-UI-001 second generation).
//
// The v1 workbench list and the /api/v3 governance scope live in
// different deployments during the v1/v3 transition (UI-6): the
// workbench select can be empty while the session reports a governance
// project scope, and vice versa. The picker unions both sources and
// keeps the manual UUID input as the honest bridge — the server still
// hides every unknown scope (404), so this input grants nothing.
export function scopeOptions(projects, projectScope) {
  const seen = new Set();
  const options = [];
  for (const project of Array.isArray(projects) ? projects : []) {
    if (project?.id && !seen.has(project.id)) {
      seen.add(project.id);
      options.push({ id: project.id, label: project.name || project.id, source: 'workbench' });
    }
  }
  for (const id of Array.isArray(projectScope) ? projectScope : []) {
    if (id && !seen.has(id)) {
      seen.add(id);
      options.push({ id, label: id, source: 'scope' });
    }
  }
  return options;
}

export function ScopePicker({
  projects,
  projectScope = [],
  projectId,
  onProjectId,
  manualProjectId,
  onManualProjectId,
}) {
  const options = scopeOptions(projects, projectScope);
  return (
    <>
      <label class="gov-field">
        <span>项目</span>
        <select value={projectId} onChange={(e) => onProjectId(e.target.value)}>
          {options.length === 0 ? <option value="">（无可见项目）</option> : null}
          {options.map((option) => (
            <option key={option.id} value={option.id}>
              {option.source === 'scope' ? `${option.label}（会话范围）` : option.label}
            </option>
          ))}
        </select>
      </label>
      <label class="gov-field">
        <span>项目 ID（当项目不在上方列表时手动输入）</span>
        <input
          value={manualProjectId}
          onInput={(e) => onManualProjectId(e.target.value)}
          placeholder="治理范围的项目 UUID"
        />
      </label>
    </>
  );
}

// The state half of the picker: one hook-owned id that the select writes
// and the manual input overrides. Views keep their own copy of this
// state via useState + these helpers.
export function effectiveScopeId(projectId, manualProjectId) {
  return String(manualProjectId || '').trim() || projectId || '';
}
