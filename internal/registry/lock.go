package registry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

var ErrLockBusy = errors.New("another grove process holds the lock")

const LockTimeout = 30 * time.Second

const lockRetry = 75 * time.Millisecond

type Lock struct {
	fl   *flock.Flock
	path string
	what string
}

func NewLock(path, what string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return &Lock{fl: flock.New(path), path: path, what: what}, nil
}

func (l *Lock) Acquire(ctx context.Context, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ok, err := l.fl.TryLockContext(ctx, lockRetry)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w: waited %s for %s (%s)", ErrLockBusy, timeout, l.what, l.path)
		}
		return fmt.Errorf("lock %s: %w", l.path, err)
	}
	if !ok {
		return fmt.Errorf("%w: %s (%s)", ErrLockBusy, l.what, l.path)
	}
	return nil
}

func (l *Lock) TryAcquire() (bool, error) { return l.fl.TryLock() }

func (l *Lock) Busy() bool {
	ok, err := l.fl.TryLock()
	if err != nil {
		return false
	}
	if ok {
		_ = l.fl.Unlock()
		return false
	}
	return true
}

func (l *Lock) Release() error { return l.fl.Unlock() }

func (l *Lock) Path() string { return l.path }

func ReplaceFile(tmp, dst string) error { return replaceFileRetry(tmp, dst) }
