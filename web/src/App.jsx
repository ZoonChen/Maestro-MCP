import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { useWebSocket } from './hooks/useWebSocket';
import { Sidebar } from './components/Sidebar';
import { Overview } from './components/Overview';
import { ProjectBoard } from './components/ProjectBoard';
import { ErrorNotice } from './components/ErrorNotice';
import { IdentityBar } from './components/IdentityBar';
import { WaiverConsole } from './components/WaiverConsole';
import { MergePipelineView } from './components/MergePipelineView';
import { ScenarioMap } from './components/ScenarioMap';
import { apiGet, describeAPIError } from './api/client';
import { projectFromHash, rolesOfAuth, viewFromHash } from './governance';

// First-generation placeholder for the admin / operations areas: the
// navigation shape is frozen now, the live panels arrive with their
// backend surfaces (runner registration codes, policy versions, audit
// export, runbook drills) — the page says so instead of faking content.
function AreaPlaceholder({ title, items }) {
  return (
    <section class="gov-page" aria-labelledby="area-placeholder-title">
      <header class="gov-header">
        <h1 id="area-placeholder-title">{title}</h1>
        <p class="gov-lead">该区域为第一代骨架：入口已按角色点亮，面板待对应后端能力接入。</p>
      </header>
      <ul class="gov-pending-list">
        {items.map((item) => (
          <li key={item} class="gov-pending-item">{item}<span class="gov-chip gov-chip-diagnostic">未接线</span></li>
        ))}
      </ul>
    </section>
  );
}

export function App({ auth }) {
  const [projects, setProjects] = useState([]);
  const [wsEvents, setWsEvents] = useState([]);
  const [wsVersion, setWsVersion] = useState(0);
  const [projectsLoading, setProjectsLoading] = useState(true);
  const [projectsError, setProjectsError] = useState('');
  const [theme, setTheme] = useState(() => localStorage.getItem('maestro-theme') || 'dark');
  const [hash, setHash] = useState(() => window.location.hash || '#/');
  const refreshTimer = useRef(null);

  useEffect(() => {
    const onHashChange = () => setHash(window.location.hash || '#/');
    window.addEventListener('hashchange', onHashChange);
    return () => window.removeEventListener('hashchange', onHashChange);
  }, []);

  const view = viewFromHash(hash);
  // Project selection rides the hash (#/project/:id) so governance views
  // and the workbench share one addressable routing mechanism.
  const selectedProjectId = projectFromHash(hash);

  const roles = rolesOfAuth(auth);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem('maestro-theme', theme);
  }, [theme]);

  const toggleTheme = useCallback(() => {
    setTheme((prev) => (prev === 'dark' ? 'light' : 'dark'));
  }, []);

  // Fetch project list
  const fetchProjects = useCallback(async () => {
    setProjectsLoading(true);
    setProjectsError('');
    try {
      const overview = await apiGet('/api/v1/overview');
      setProjects(overview?.projects || []);
    } catch (e) {
      setProjectsError(describeAPIError(e));
    } finally {
      setProjectsLoading(false);
    }
  }, []);

  useEffect(() => { fetchProjects(); }, [fetchProjects]);

  // WebSocket
  const wsUrl = selectedProjectId
    ? `${location.protocol === 'https:' ? 'wss:' : 'ws:'}//${location.host}/api/v1/projects/${selectedProjectId}/ws`
    : null;

  const onWsMessage = useCallback((msg) => {
    try {
      const event = JSON.parse(msg);
      if (event.project_id !== selectedProjectId) return;
      setWsEvents((prev) => [event, ...prev].slice(0, 200));
      if (refreshTimer.current === null) {
        refreshTimer.current = setTimeout(() => {
          refreshTimer.current = null;
          setWsVersion((previous) => previous + 1);
        }, 150);
      }
    } catch {}
  }, [selectedProjectId]);

  const wsStatus = useWebSocket(wsUrl, onWsMessage);

  useEffect(() => {
    clearTimeout(refreshTimer.current);
    refreshTimer.current = null;
    setWsEvents([]);
    setWsVersion(0);
  }, [selectedProjectId]);

  useEffect(() => () => clearTimeout(refreshTimer.current), []);

  // Refresh project list on WS events
  useEffect(() => {
    if (wsVersion > 0) fetchProjects();
  }, [wsVersion, fetchProjects]);

  const navigate = useCallback((target) => {
    window.location.hash = target;
  }, []);
  const selectProject = useCallback((id) => {
    navigate(id ? `#/project/${id}` : '#/');
  }, [navigate]);

  return (
    <div class="app">
      <Sidebar
        projects={projects}
        selectedId={selectedProjectId}
        onSelect={selectProject}
        theme={theme}
        onToggleTheme={toggleTheme}
        view={view}
        onNavigate={navigate}
        roles={roles}
      />
      <main class="main">
        <IdentityBar auth={auth} />
        {view === 'waivers' ? (
          <WaiverConsole projects={projects} roles={roles} />
        ) : view === 'mrs' ? (
          <MergePipelineView projects={projects} />
        ) : view === 'scenarios' ? (
          <ScenarioMap roles={roles} />
        ) : view === 'admin' ? (
          <AreaPlaceholder
            title="管理"
            items={['项目/成员管理', 'Runner 注册码与批准', '策略版本管理', '审计导出（依赖契约 PR 端点）']}
          />
        ) : view === 'operations' ? (
          <AreaPlaceholder
            title="运维"
            items={['Runbook 演练锚点', '紧急停止 / 凭据撤销', 'SLO 快照（依赖契约 PR 端点）', '备份恢复记录']}
          />
        ) : (
          <>
            <ErrorNotice message={projectsError} onRetry={fetchProjects} />
            {projectsLoading && projects.length === 0 ? (
              <div class="loading">Loading projects...</div>
            ) : selectedProjectId ? (
              <ProjectBoard
                key={selectedProjectId}
                projectId={selectedProjectId}
                projects={projects}
                wsEvents={wsEvents}
                wsVersion={wsVersion}
                wsStatus={wsStatus}
              />
            ) : (
              <Overview
                projects={projects}
                onSelect={selectProject}
              />
            )}
          </>
        )}
      </main>
    </div>
  );
}
