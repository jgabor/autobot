// Package lock prevents overlapping autobot runs.
package lock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrBusy means another autobot process holds the lock.
var ErrBusy = errors.New("another autobot run is in progress")

// Lock is a held file lock.
type Lock struct{ f *os.File }

// Acquire takes an exclusive lock on path without waiting.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return &Lock{f}, nil
}

// Release frees the lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
