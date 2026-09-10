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
import { PilotFlagsView } from './components/PilotFlagsView';
import { JiraConnectorView } from './components/JiraConnectorView';
import { AuditExportView } from './components/AuditExportView';
import { SLOSnapshotView } from './components/SLOSnapshotView';
import { DeadLetterView } from './components/DeadLetterView';
import { apiGet, describeAPIError } from './api/client';
import { projectFromHash, rolesOfAuth, viewFromHash } from './governance';

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
  // The server-reported governance scope: the union of the v1 workbench
  // list and this session's memberships drives the governance pickers
  // (the v1/v3 transition bridge, UI-6).
  const projectScope = auth?.projectScope || [];

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
          <WaiverConsole projects={projects} roles={roles} projectScope={projectScope} />
        ) : view === 'mrs' ? (
          <MergePipelineView projects={projects} />
        ) : view === 'scenarios' ? (
          <ScenarioMap roles={roles} />
        ) : view === 'pilot' ? (
          <PilotFlagsView projects={projects} projectScope={projectScope} />
        ) : view === 'jira' ? (
          <JiraConnectorView projects={projects} projectScope={projectScope} />
        ) : view === 'admin' ? (
          <section class="gov-page" aria-labelledby="admin-area-title">
            <header class="gov-header">
              <h1 id="admin-area-title">管理</h1>
              <p class="gov-lead">平台管理面。审计链导出已接线真实端点；其余面板待对应后端能力接入。</p>
            </header>
            <AuditExportView projects={projects} projectScope={projectScope} />
            <ul class="gov-pending-list">
              <li class="gov-pending-item">项目/成员管理<span class="gov-chip gov-chip-diagnostic">未接线</span></li>
              <li class="gov-pending-item">Runner 注册码与批准<span class="gov-chip gov-chip-diagnostic">未接线</span></li>
              <li class="gov-pending-item">策略版本管理<span class="gov-chip gov-chip-diagnostic">未接线</span></li>
            </ul>
          </section>
        ) : view === 'operations' ? (
          <section class="gov-page" aria-labelledby="operations-area-title">
            <header class="gov-header">
              <h1 id="operations-area-title">运维</h1>
              <p class="gov-lead">
                平台运维面。SLO 快照与 Webhook DLQ 重放已接线真实端点；其余面板待对应后端能力接入。
              </p>
            </header>
            <SLOSnapshotView projects={projects} projectScope={projectScope} />
            <DeadLetterView principal={auth?.principal || ''} />
            <ul class="gov-pending-list">
              <li class="gov-pending-item">Runbook 演练锚点<span class="gov-chip gov-chip-diagnostic">未接线</span></li>
              <li class="gov-pending-item">紧急停止 / 凭据撤销<span class="gov-chip gov-chip-diagnostic">未接线</span></li>
              <li class="gov-pending-item">备份恢复记录<span class="gov-chip gov-chip-diagnostic">未接线</span></li>
            </ul>
          </section>
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
