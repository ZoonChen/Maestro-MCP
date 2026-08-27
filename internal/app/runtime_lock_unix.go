//go:build darwin || linux

package app

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// runtimeLock is an OS-owned advisory lock. The file intentionally remains on
// disk after unlock; ownership is attached to the open descriptor and is
// released by the kernel on clean shutdown or process death.
type runtimeLock struct {
	file *os.File
}

func acquireRuntimeLock(databasePath string) (*runtimeLock, error) {
	lockPath, err := canonicalRuntimeLockPath(databasePath)
	if err != nil {
		return nil, err
	}
	if lockPath == "" {
		return nil, nil
	}
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runtime lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open runtime lock: invalid file descriptor")
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf("another Maestro server owns this database")
		}
		return nil, fmt.Errorf("lock runtime database: %w", err)
	}
	return &runtimeLock{file: file}, nil
}

func (l *runtimeLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("unlock runtime database: %w", unlockErr)
	}
	return closeErr
}
