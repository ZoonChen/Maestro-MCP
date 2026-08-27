import { useState, useEffect, useCallback, useRef } from 'preact/hooks';
import { Summary } from './Summary';
import { TaskBoard } from './TaskBoard';
import { ActivityLog } from './ActivityLog';
import { SessionPanel } from './SessionPanel';
import { FeatureView } from './FeatureView';
import { TaskDetail } from './TaskDetail';
import { ErrorNotice } from './ErrorNotice';
import { apiGet, describeAPIError } from '../api/client';

export function ProjectBoard({ projectId, projects, wsEvents, wsVersion, wsStatus }) {
  const [boardData, setBoardData] = useState(null);
  const [sessions, setSessions] = useState([]);
  const [tasks, setTasks] = useState([]);
  const [showCancelled, setShowCancelled] = useState(false);
  const [featureFilter, setFeatureFilter] = useState(null);
  const [selectedTask, setSelectedTask] = useState(null);
  const [loadError, setLoadError] = useState('');
  const requestVersion = useRef(0);

  const project = projects.find((p) => p.id === projectId);

  const fetchProjectData = useCallback(async () => {
    const currentRequest = ++requestVersion.current;
    setLoadError('');
    try {
      const [nextBoard, nextSessions, nextTasks] = await Promise.all([
        apiGet(`/api/v1/projects/${projectId}/board`),
        apiGet(`/api/v1/projects/${projectId}/sessions`),
        apiGet(`/api/v1/projects/${projectId}/tasks`),
      ]);
      const sessionsWithWorkers = await Promise.all((nextSessions || []).map(async (session) => ({
        ...session,
        workers: await apiGet(`/api/v1/projects/${projectId}/sessions/${session.id}/workers`) || [],
      })));
      if (currentRequest !== requestVersion.current) return;
      setBoardData(nextBoard);
      setSessions(sessionsWithWorkers);
      setTasks(nextTasks || []);
      setSelectedTask((current) => (
        current ? (nextTasks || []).find((task) => task.id === current.id) || null : null
      ));
    } catch (error) {
      if (currentRequest !== requestVersion.current) return;
      setLoadError(describeAPIError(error));
    }
  }, [projectId]);

  useEffect(() => {
    requestVersion.current += 1;
    setBoardData(null);
    setSessions([]);
    setTasks([]);
    setFeatureFilter(null);
    setSelectedTask(null);
    fetchProjectData();
    return () => { requestVersion.current += 1; };
  }, [fetchProjectData]);

  // Refresh on WS events
  useEffect(() => {
    if (wsVersion > 0) {
      fetchProjectData();
    }
  }, [wsVersion, fetchProjectData]);

  if (!boardData) {
    return (
      <div>
        <ErrorNotice message={loadError} onRetry={fetchProjectData} />
        {!loadError && <div class="loading">Loading project...</div>}
      </div>
    );
  }

  return (
    <div class="project-board">
      <div class="board-header">
        <h2>{project?.name || projectId}</h2>
        <span class={`status-badge ${project?.status || 'active'}`}>
          {project?.status || 'active'}
        </span>
        <span class={`connection-status ${wsStatus}`}>Live: {wsStatus}</span>
      </div>

      <ErrorNotice message={loadError} onRetry={fetchProjectData} />

      <Summary data={boardData} />

      <FeatureView
        projectId={projectId}
        onSelectFeature={setFeatureFilter}
        selectedFeatureId={featureFilter}
        refreshVersion={wsVersion}
      />

      <SessionPanel sessions={sessions} />

      <div class="board-controls">
        <h3 class="section-title">
          Task Board
          {featureFilter && (
            <button type="button" class="feature-filter-hint" onClick={() => setFeatureFilter(null)}>
              (filtered - clear)
            </button>
          )}
        </h3>
        <label class="toggle-label">
          <input
            type="checkbox"
            checked={showCancelled}
            onChange={(e) => setShowCancelled(e.target.checked)}
          />
          Show cancelled
        </label>
      </div>

      <TaskBoard
        tasks={tasks}
        showCancelled={showCancelled}
        onTaskClick={setSelectedTask}
        featureFilter={featureFilter}
      />

      <ActivityLog
        projectId={projectId}
        wsEvents={wsEvents}
        refreshVersion={wsVersion}
        wsStatus={wsStatus}
      />

      {selectedTask && (
        <TaskDetail
          task={selectedTask}
          projectId={projectId}
          onClose={() => setSelectedTask(null)}
        />
      )}
    </div>
  );
}
