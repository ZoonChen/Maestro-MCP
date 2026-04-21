import { useState, useEffect, useCallback } from 'preact/hooks';
import { Summary } from './Summary';
import { TaskBoard } from './TaskBoard';
import { ActivityLog } from './ActivityLog';
import { SessionPanel } from './SessionPanel';
import { FeatureView } from './FeatureView';
import { TaskDetail } from './TaskDetail';

export function ProjectBoard({ projectId, projects, wsEvents }) {
  const [boardData, setBoardData] = useState(null);
  const [sessions, setSessions] = useState([]);
  const [tasks, setTasks] = useState([]);
  const [showCancelled, setShowCancelled] = useState(false);
  const [featureFilter, setFeatureFilter] = useState(null);
  const [selectedTask, setSelectedTask] = useState(null);

  const project = projects.find((p) => p.id === projectId);

  const fetchBoard = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/projects/${projectId}/board`);
      const json = await res.json();
      if (json.data) setBoardData(json.data);
    } catch (e) {
      console.error('Failed to fetch board:', e);
    }
  }, [projectId]);

  const fetchSessions = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/projects/${projectId}/sessions`);
      const json = await res.json();
      if (json.data) setSessions(json.data);
    } catch {}
  }, [projectId]);

  const fetchTasks = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/projects/${projectId}/tasks`);
      const json = await res.json();
      if (json.data) setTasks(json.data);
    } catch {}
  }, [projectId]);

  useEffect(() => {
    fetchBoard();
    fetchSessions();
    fetchTasks();
  }, [fetchBoard, fetchSessions, fetchTasks]);

  // Refresh on WS events
  useEffect(() => {
    if (wsEvents.length > 0) {
      fetchBoard();
      fetchSessions();
      fetchTasks();
    }
  }, [wsEvents.length, fetchBoard, fetchSessions, fetchTasks]);

  if (!boardData) {
    return <div class="loading">Loading...</div>;
  }

  return (
    <div class="project-board">
      <div class="board-header">
        <h2>{project?.name || projectId}</h2>
        <span class={`status-badge ${project?.status || 'active'}`}>
          {project?.status || 'active'}
        </span>
      </div>

      <Summary data={boardData} />

      <FeatureView
        projectId={projectId}
        onSelectFeature={setFeatureFilter}
        selectedFeatureId={featureFilter}
      />

      <SessionPanel sessions={sessions} />

      <div class="board-controls">
        <h3 class="section-title">
          Task Board
          {featureFilter && (
            <span class="feature-filter-hint" onClick={() => setFeatureFilter(null)}>
              (filtered - clear)
            </span>
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

      <ActivityLog projectId={projectId} wsEvents={wsEvents} />

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
