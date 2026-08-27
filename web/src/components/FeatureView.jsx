import { useState, useEffect, useCallback, useRef } from 'preact/hooks';
import { ErrorNotice } from './ErrorNotice';
import { apiGet, describeAPIError } from '../api/client';

export function FeatureView({ projectId, onSelectFeature, selectedFeatureId, refreshVersion }) {
  const [features, setFeatures] = useState([]);
  const [taskCounts, setTaskCounts] = useState({});
  const [error, setError] = useState('');
  const requestVersion = useRef(0);

  const fetchFeatures = useCallback(async () => {
    const currentRequest = ++requestVersion.current;
    setError('');
    try {
      const [nextFeatures, tasks] = await Promise.all([
        apiGet(`/api/v1/projects/${projectId}/features`),
        apiGet(`/api/v1/projects/${projectId}/tasks`),
      ]);
      if (currentRequest !== requestVersion.current) return;
      const counts = {};
      (tasks || []).forEach((task) => {
        if (!counts[task.feature_id]) counts[task.feature_id] = { total: 0, done: 0 };
        counts[task.feature_id].total += 1;
        if (task.status === 'done') counts[task.feature_id].done += 1;
      });
      setFeatures(nextFeatures || []);
      setTaskCounts(counts);
    } catch (requestError) {
      if (currentRequest !== requestVersion.current) return;
      setError(describeAPIError(requestError));
    }
  }, [projectId]);

  useEffect(() => {
    fetchFeatures();
    return () => { requestVersion.current += 1; };
  }, [fetchFeatures, refreshVersion]);

  return (
    <div class="feature-view">
      <h3 class="section-title">Features</h3>
      <ErrorNotice message={error} />
      <div class="feature-grid">
        {features.map((f) => {
          const counts = taskCounts[f.id] || { total: 0, done: 0 };
          const pct = counts.total > 0 ? Math.round((counts.done / counts.total) * 100) : 0;
          return (
            <button
              type="button"
              key={f.id}
              class={`feature-card ${selectedFeatureId === f.id ? 'selected' : ''}`}
              onClick={() => onSelectFeature(selectedFeatureId === f.id ? null : f.id)}
              aria-pressed={selectedFeatureId === f.id}
            >
              <div class="feature-card-header">
                <span class={`status-badge ${f.status}`}>{f.status}</span>
                <span class="feature-title">{f.title}</span>
              </div>
              <p class="feature-desc">{f.description}</p>
              <div class="feature-progress">
                <span class="feature-count">{counts.done}/{counts.total} tasks</span>
                <div class="progress-bar">
                  <div class="progress-fill" style={`width: ${pct}%`}></div>
                </div>
                <span class="feature-pct">{pct}%</span>
              </div>
            </button>
          );
        })}
        {features.length === 0 && (
          <p class="empty-hint">No features defined</p>
        )}
      </div>
    </div>
  );
}
