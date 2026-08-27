package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// GCService handles data lifecycle garbage collection.
// It cleans up old records from activity_log and external test log files.
// Security audit events and validation evidence are append-only and are never
// deleted by this runtime service.
type GCService struct {
	db *sql.DB
}

// NewGCService creates a new GCService.
func NewGCService(db *sql.DB) *GCService {
	return &GCService{db: db}
}

// RunActivityLogGC deletes activity_log entries older than the given retention period.
func (s *GCService) RunActivityLogGC(ctx context.Context, retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 90
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM activity_log WHERE created_at < datetime('now', ?||' days')`,
		fmt.Sprintf("-%d", retentionDays),
	)
	if err != nil {
		return fmt.Errorf("gc activity_log: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		slog.Info("GC: cleaned activity_log entries", "count", affected, "retention_days", retentionDays)
	}
	return nil
}

// RunTestLogGC removes test log files older than the given retention period.
// Test logs are stored under data/logs/validation/.
func (s *GCService) RunTestLogGC(ctx context.Context, retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	const logDir = "data/logs/validation"

	workspaceRoot, err := os.OpenRoot(".")
	if err != nil {
		return fmt.Errorf("gc test logs open workspace root: %w", err)
	}
	defer func() { _ = workspaceRoot.Close() }()

	logRoot, err := workspaceRoot.OpenRoot(logDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("gc test logs open root: %w", err)
	}
	defer func() { _ = logRoot.Close() }()

	var cleaned int
	err = fs.WalkDir(logRoot.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if walkErr != nil {
			return nil //nolint:nilerr // skip individual file errors in walk
		}
		if entry.IsDir() {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			if rmErr := logRoot.Remove(path); rmErr == nil {
				cleaned++
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("gc test logs walk: %w", err)
	}
	if cleaned > 0 {
		slog.Info("GC: cleaned test log files", "count", cleaned, "retention_days", retentionDays)
	}
	return nil
}

// RunAll executes all GC operations with standard retention periods.
func (s *GCService) RunAll(ctx context.Context) error {
	if err := s.RunActivityLogGC(ctx, 90); err != nil {
		slog.Error("GC activity_log error", "error", err)
	}
	if err := s.RunTestLogGC(ctx, 30); err != nil {
		slog.Error("GC test logs error", "error", err)
	}
	return nil
}
