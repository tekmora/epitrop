//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// lockRetryDelay is how long acquireLock waits between two tries of a held lock.
const lockRetryDelay = 10 * time.Millisecond

// fileLock is the exclusive lock on the events directory: flock(2) on Unix, LockFileEx on
// Windows, both through gofrs/flock.
type fileLock struct {
	lock *flock.Flock
}

func acquireLock(ctx context.Context, eventsDir string, timeout time.Duration) (*fileLock, error) {
	path := filepath.Join(eventsDir, ".lock")
	lock := flock.New(path, flock.SetFlag(os.O_CREATE|os.O_RDWR))
	locked, err := tryLock(ctx, lock, timeout)
	if err != nil {
		return nil, fmt.Errorf("eventlog: lock %s: %w", path, err)
	}
	if !locked {
		return nil, fmt.Errorf("%w: %s", ErrLockTimeout, path)
	}
	return &fileLock{lock: lock}, nil
}

// tryLock tries once, and when the lock is held and timeout is positive, again until timeout
// or ctx ends, which both report as not locked. The first try comes before the wait: the
// timeout bounds the wait for a held lock, and TryLockContext gives up on an ended context
// before it tries, so a short timeout that ran out on a busy machine would refuse a free lock.
func tryLock(ctx context.Context, lock *flock.Flock, timeout time.Duration) (bool, error) {
	locked, err := lock.TryLock()
	if locked || err != nil || timeout <= 0 {
		return locked, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	locked, err = lock.TryLockContext(waitCtx, lockRetryDelay)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false, nil
	}
	return locked, err
}

func (l *fileLock) Close() error {
	if err := l.lock.Unlock(); err != nil {
		return fmt.Errorf("eventlog: unlock %s: %w", l.lock.Path(), err)
	}
	return nil
}
