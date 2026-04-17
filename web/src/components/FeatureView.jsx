import { useState, useEffect, useCallback } from 'preact/hooks';

export function FeatureView({ projectId, onSelectFeature, selectedFeatureId }) {
  const [features, setFeatures] = useState([]);
  const [taskCounts, setTaskCounts] = useState({});

  const fetchFeatures = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/projects/${projectId}/features`);
      const json = await res.json();
      if (json.data) setFeatures(json.data);
    } catch {}
  }, [projectId]);

  const fetchTaskCounts = useCallback(async () => {
    try {
      const res = await fetch(`/api/v1/projects/${projectId}/tasks`);
      const json = await res.json();
      if (json.data) {
        const counts = {};
        json.data.forEach((t) => {
          if (!counts[t.feature_id]) {
            counts[t.feature_id] = { total: 0, done: 0 };
          }
          counts[t.feature_id].total++;
          if (t.status === 'done') counts[t.feature_id].done++;
        });
        setTaskCounts(counts);
      }
    } catch {}
  }, [projectId]);

  useEffect(() => {
    fetchFeatures();
    fetchTaskCounts();
  }, [fetchFeatures, fetchTaskCounts]);

  return (
    <div class="feature-view">
      <h3 class="section-title">Features</h3>
      <div class="feature-grid">
        {features.map((f) => {
          const counts = taskCounts[f.id] || { total: 0, done: 0 };
          const pct = counts.total > 0 ? Math.round((counts.done / counts.total) * 100) : 0;
          return (
            <div
              key={f.id}
              class={`feature-card ${selectedFeatureId === f.id ? 'selected' : ''}`}
              onClick={() => onSelectFeature(selectedFeatureId === f.id ? null : f.id)}
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
            </div>
          );
        })}
        {features.length === 0 && (
          <p class="empty-hint">No features defined</p>
        )}
      </div>
    </div>
  );
}
