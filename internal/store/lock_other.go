//go:build !unix

package store

import "os"

// acquireLock is a no-op where flock is unavailable; single-instance
// operation is then the operator's responsibility.
func acquireLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- derived from operator DSN
}
