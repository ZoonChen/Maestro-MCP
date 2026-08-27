import { useState, useEffect, useCallback, useRef } from 'preact/hooks';
import { useWebSocket } from './hooks/useWebSocket';
import { Sidebar } from './components/Sidebar';
import { Overview } from './components/Overview';
import { ProjectBoard } from './components/ProjectBoard';
import { ErrorNotice } from './components/ErrorNotice';
import { apiGet, describeAPIError } from './api/client';

export function App() {
  const [projects, setProjects] = useState([]);
  const [selectedProjectId, setSelectedProjectId] = useState(null);
  const [wsEvents, setWsEvents] = useState([]);
  const [wsVersion, setWsVersion] = useState(0);
  const [projectsLoading, setProjectsLoading] = useState(true);
  const [projectsError, setProjectsError] = useState('');
  const [theme, setTheme] = useState(() => localStorage.getItem('maestro-theme') || 'dark');
  const refreshTimer = useRef(null);

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

  return (
    <div class="app">
      <Sidebar
        projects={projects}
        selectedId={selectedProjectId}
        onSelect={setSelectedProjectId}
        theme={theme}
        onToggleTheme={toggleTheme}
      />
      <main class="main">
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
            onSelect={setSelectedProjectId}
          />
        )}
      </main>
    </div>
  );
}
