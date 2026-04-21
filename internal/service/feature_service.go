package service

import (
	"context"
	"fmt"

	"github.com/ZoonChen/Maestro-MCP/internal/model"
	"github.com/ZoonChen/Maestro-MCP/internal/store"
)

// FeatureService implements feature CRUD and automatic status transitions
// based on the states of a feature's child tasks.
// All methods enforce L4 isolation by passing projectID through to the Store layer.
type FeatureService struct {
	featureStore store.FeatureStore
	taskStore    store.TaskStore
	eventEmitter EventEmitter
}

// NewFeatureService creates a new FeatureService with the given store dependencies.
func NewFeatureService(featureStore store.FeatureStore, taskStore store.TaskStore, eventEmitter EventEmitter) *FeatureService {
	return &FeatureService{
		featureStore: featureStore,
		taskStore:    taskStore,
		eventEmitter: eventEmitter,
	}
}

// CreateFeature creates a new feature within the given project.
func (s *FeatureService) CreateFeature(ctx context.Context, projectID string, f *model.Feature) error {
	if err := s.featureStore.Create(ctx, projectID, f); err != nil {
		return fmt.Errorf("create feature %s in project %s: %w", f.ID, projectID, err)
	}
	safeEmit(s.eventEmitter, "feature.created", projectID, map[string]string{"feature_id": f.ID})
	return nil
}

// GetFeature retrieves a feature by ID within the given project.
// Returns ErrFeatureNotFound if the feature does not exist or belongs to a different project.
func (s *FeatureService) GetFeature(ctx context.Context, projectID, id string) (*model.Feature, error) {
	f, err := s.featureStore.GetByID(ctx, projectID, id)
	if err != nil {
		return nil, fmt.Errorf("get feature %s in project %s: %w", id, projectID, err)
	}
	return f, nil
}

// ListFeatures returns all features within the given project, ordered by creation time.
func (s *FeatureService) ListFeatures(ctx context.Context, projectID string) ([]*model.Feature, error) {
	features, err := s.featureStore.List(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("list features in project %s: %w", projectID, err)
	}
	return features, nil
}

// UpdateFeature updates mutable feature fields (title, description, reference_urls, status)
// within the given project.
func (s *FeatureService) UpdateFeature(ctx context.Context, projectID string, f *model.Feature) error {
	if err := s.featureStore.Update(ctx, projectID, f); err != nil {
		return fmt.Errorf("update feature %s in project %s: %w", f.ID, projectID, err)
	}
	safeEmit(s.eventEmitter, "feature.updated", projectID, map[string]string{"feature_id": f.ID})
	return nil
}

// AutoTransitionStatus examines the task statuses for a feature and automatically
// transitions the feature status according to business rules:
//
//   - If the feature is in "planning" status and any non-cancelled task exists,
//     transition to "active".
//   - If all non-cancelled tasks are in "done" status, transition to "completed".
//   - "closed" is manual-only and is never auto-set.
//
// This method should be called after task state changes (create, status update, cancel)
// to keep the feature status consistent with its tasks.
func (s *FeatureService) AutoTransitionStatus(ctx context.Context, projectID, featureID string) error {
	f, err := s.featureStore.GetByID(ctx, projectID, featureID)
	if err != nil {
		return fmt.Errorf("auto transition feature %s in project %s: %w", featureID, projectID, err)
	}

	// Only auto-transition from "planning" or "active" states.
	// "completed" can be revisited (e.g. new tasks added), "closed" is manual-only.
	if f.Status != model.FeatureStatusPlanning && f.Status != model.FeatureStatusActive && f.Status != model.FeatureStatusCompleted {
		return nil
	}

	// List all tasks for this feature.
	tasks, err := s.taskStore.List(ctx, projectID, store.TaskFilter{FeatureID: featureID})
	if err != nil {
		return fmt.Errorf("auto transition feature %s: list tasks: %w", featureID, err)
	}

	// Partition tasks into non-cancelled and cancelled groups.
	var nonCancelled []*model.Task
	for _, t := range tasks {
		if t.Status != model.TaskStatusCancelled {
			nonCancelled = append(nonCancelled, t)
		}
	}

	// Rule 1: If no non-cancelled tasks exist, do not auto-transition.
	if len(nonCancelled) == 0 {
		return nil
	}

	// Rule 2: If feature is "planning" and any non-cancelled task exists, set to "active".
	if f.Status == model.FeatureStatusPlanning {
		f.Status = model.FeatureStatusActive
		if err := s.featureStore.Update(ctx, projectID, f); err != nil {
			return fmt.Errorf("auto transition feature %s to active: %w", featureID, err)
		}
		return nil
	}

	// Rule 3: If all non-cancelled tasks are "done", set to "completed".
	allDone := true
	for _, t := range nonCancelled {
		if t.Status != model.TaskStatusDone {
			allDone = false
			break
		}
	}

	if allDone {
		f.Status = model.FeatureStatusCompleted
		if err := s.featureStore.Update(ctx, projectID, f); err != nil {
			return fmt.Errorf("auto transition feature %s to completed: %w", featureID, err)
		}
		return nil
	}

	// If previously "completed" but a new non-done task appeared, revert to "active".
	if f.Status == model.FeatureStatusCompleted {
		f.Status = model.FeatureStatusActive
		if err := s.featureStore.Update(ctx, projectID, f); err != nil {
			return fmt.Errorf("auto transition feature %s back to active: %w", featureID, err)
		}
	}

	return nil
}
