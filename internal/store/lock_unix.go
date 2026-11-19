//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrLocked is returned when another process holds the database's writer lock.
var ErrLocked = errors.New("store: database is open for writing by another process")

// lockFile takes an exclusive, non-blocking advisory lock on path. The lock
// is released when the returned file is closed or the process exits.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("store: lock: %w", err)
	}
	return f, nil
}
