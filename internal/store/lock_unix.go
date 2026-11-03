//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// acquireLock takes an exclusive, non-blocking flock on path so only one
// Relay process uses a database. The kernel drops it if the process dies.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- derived from operator DSN
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w (%s)", ErrLocked, path)
		}
		return nil, err
	}
	return f, nil
}
