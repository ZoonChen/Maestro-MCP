package service

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// GCService handles data lifecycle garbage collection.
// It cleans up old records from activity_log, audit_log, and test log files
// based on configurable retention periods.
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

// RunAuditLogGC deletes audit_log entries older than the given retention period.
func (s *GCService) RunAuditLogGC(ctx context.Context, retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 180
	}
	result, err := s.db.ExecContext(ctx,
		`DELETE FROM audit_log WHERE created_at < datetime('now', ?||' days')`,
		fmt.Sprintf("-%d", retentionDays),
	)
	if err != nil {
		return fmt.Errorf("gc audit_log: %w", err)
	}
	affected, _ := result.RowsAffected()
	if affected > 0 {
		slog.Info("GC: cleaned audit_log entries", "count", affected, "retention_days", retentionDays)
	}
	return nil
}

// RunTestLogGC removes test log files older than the given retention period.
// Test logs are stored under data/logs/validation/.
func (s *GCService) RunTestLogGC(_ context.Context, retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	logDir := "data/logs/validation"

	info, err := os.Stat(logDir)
	if err != nil || !info.IsDir() {
		// Directory doesn't exist or is not a directory; nothing to clean.
		return nil //nolint:nilerr // expected: missing dir is not an error
	}

	var cleaned int
	err = filepath.Walk(logDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip individual file errors in walk
		}
		if fi.IsDir() {
			return nil
		}
		if fi.ModTime().Before(cutoff) {
			if rmErr := os.Remove(path); rmErr == nil {
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
	if err := s.RunAuditLogGC(ctx, 180); err != nil {
		slog.Error("GC audit_log error", "error", err)
	}
	if err := s.RunTestLogGC(ctx, 30); err != nil {
		slog.Error("GC test logs error", "error", err)
	}
	return nil
}
