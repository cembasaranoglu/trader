//go:build !unix

package store

import (
	"errors"
	"os"
)

// ErrLocked is returned when another process holds the database's writer lock.
var ErrLocked = errors.New("store: database is open for writing by another process")

// lockFile only creates the lock file on platforms without flock; a second
// writer is then detected by the event triggers (seq must be max+1) instead.
// Production runs on Linux (ADR-001, D-12).
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}
