import { useState, useEffect, useCallback } from 'preact/hooks';
import { useWebSocket } from './hooks/useWebSocket';
import { Sidebar } from './components/Sidebar';
import { Overview } from './components/Overview';
import { ProjectBoard } from './components/ProjectBoard';

export function App() {
  const [projects, setProjects] = useState([]);
  const [selectedProjectId, setSelectedProjectId] = useState(null);
  const [wsEvents, setWsEvents] = useState([]);
  const [theme, setTheme] = useState(() => localStorage.getItem('maestro-theme') || 'dark');

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    localStorage.setItem('maestro-theme', theme);
  }, [theme]);

  const toggleTheme = useCallback(() => {
    setTheme((prev) => (prev === 'dark' ? 'light' : 'dark'));
  }, []);

  // Fetch project list
  const fetchProjects = useCallback(async () => {
    try {
      const res = await fetch('/api/v1/projects');
      const json = await res.json();
      if (json.data) setProjects(json.data);
    } catch (e) {
      console.error('Failed to fetch projects:', e);
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
      setWsEvents((prev) => [event, ...prev].slice(0, 200));
    } catch {}
  }, []);

  useWebSocket(wsUrl, onWsMessage);

  // Refresh project list on WS events
  useEffect(() => {
    if (wsEvents.length > 0) fetchProjects();
  }, [wsEvents.length, fetchProjects]);

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
        {selectedProjectId ? (
          <ProjectBoard
            projectId={selectedProjectId}
            projects={projects}
            wsEvents={wsEvents}
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
