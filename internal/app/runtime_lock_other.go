//go:build !darwin && !linux

package app

import (
	"fmt"
	"os"
)

// M0 production targets are Linux and development supports macOS. Other
// targets use an exclusive marker as a fail-closed compatibility fallback.
// Unlike the kernel lock used on supported platforms, an unclean process may
// require an operator to inspect and remove this marker.
type runtimeLock struct {
	file *os.File
	path string
}

func acquireRuntimeLock(databasePath string) (*runtimeLock, error) {
	lockPath, err := canonicalRuntimeLockPath(databasePath)
	if err != nil {
		return nil, err
	}
	if lockPath == "" {
		return nil, nil
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create exclusive runtime lock: %w", err)
	}
	return &runtimeLock{file: file, path: lockPath}, nil
}

func (l *runtimeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	closeErr := l.file.Close()
	removeErr := os.Remove(l.path)
	l.file = nil
	if closeErr != nil {
		return closeErr
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}
