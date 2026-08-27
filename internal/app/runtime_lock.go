package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// AcquireRuntimeLock obtains the same database ownership lock held by the M0
// HTTP runtime. Explicit maintenance commands must hold it before performing
// DDL so migrations cannot race a live server.
func AcquireRuntimeLock(databasePath string) (io.Closer, error) {
	lock, err := acquireRuntimeLock(databasePath)
	if lock == nil {
		return nil, err
	}
	return lock, err
}

func canonicalRuntimeLockPath(databasePath string) (string, error) {
	if databasePath == "" || databasePath == ":memory:" {
		return "", nil
	}
	absolutePath, err := filepath.Abs(databasePath)
	if err != nil {
		return "", fmt.Errorf("resolve absolute database path: %w", err)
	}
	if info, err := os.Lstat(absolutePath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("database path must not be a symbolic link")
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("database path must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect database path: %w", err)
	}

	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(absolutePath))
	if err != nil {
		return "", fmt.Errorf("resolve database parent: %w", err)
	}
	canonicalDatabase := filepath.Join(resolvedParent, filepath.Base(absolutePath))
	lockPath := canonicalDatabase + ".runtime.lock"
	if info, err := os.Lstat(lockPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("runtime lock path must not be a symbolic link")
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect runtime lock path: %w", err)
	}
	return lockPath, nil
}
