//go:build unix

package state

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked means another goroutine or process is syncing the repository.
var ErrLocked = errors.New("repository is locked by another sync")

// Lock is an advisory flock held on a per-repository lock file. flock locks
// belong to the open file description, so they exclude other goroutines in the
// same process as well as other processes (serve vs. sync --once).
type Lock struct{ f *os.File }

// TryLock acquires the lock without blocking.
func TryLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. The lock file itself is intentionally left in place.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
