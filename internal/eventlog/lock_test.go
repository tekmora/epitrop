//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || windows

package eventlog

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireLock(t *testing.T) {
	t.Run("missing directory fails as not found", testAcquireLockMissingDir)
	t.Run("timeout while held and reacquire after close", testAcquireLockTimeoutAndReacquire)
	t.Run("a held lock and no time to wait", testAcquireLockZeroTimeout)
	t.Run("a held lock and an ended context", testAcquireLockEndedContext)
	t.Run("a free lock and a deadline already past", testAcquireLockFreeAfterDeadline)
}

// holdLock takes the lock on dir for the rest of the test.
func holdLock(t *testing.T, dir string) {
	t.Helper()
	held, err := acquireLock(context.Background(), dir, time.Second)
	if err != nil {
		t.Fatalf("acquireLock(held) = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := held.Close(); err != nil {
			t.Errorf("held.Close() = %v, want nil", err)
		}
	})
}

func testAcquireLockZeroTimeout(t *testing.T) {
	dir := t.TempDir()
	free, err := acquireLock(context.Background(), dir, 0)
	if err != nil {
		t.Fatalf("acquireLock(free, 0) = %v, want nil: one try takes a free lock", err)
	}
	if err = free.Close(); err != nil {
		t.Fatalf("free.Close() = %v, want nil", err)
	}
	holdLock(t, dir)
	if _, err = acquireLock(context.Background(), dir, 0); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("acquireLock(held, 0) = %v, want ErrLockTimeout", err)
	}
}

func testAcquireLockEndedContext(t *testing.T) {
	dir := t.TempDir()
	holdLock(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := acquireLock(ctx, dir, time.Minute); !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("acquireLock(held, ended context) = %v, want ErrLockTimeout", err)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("acquireLock(held, ended context) waited %s, want it to return at once", waited)
	}
}

// A writer whose deadline ran out before its first try, as a short lock timeout can on a
// busy machine, still takes a free lock: the timeout bounds the wait, and a free lock needs none.
func testAcquireLockFreeAfterDeadline(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	free, err := acquireLock(ctx, dir, time.Millisecond)
	if err != nil {
		t.Fatalf("acquireLock(free, deadline past) = %v, want nil: a free lock needs no wait", err)
	}
	if err = free.Close(); err != nil {
		t.Fatalf("free.Close() = %v, want nil", err)
	}
}

func testAcquireLockMissingDir(t *testing.T) {
	ctx := context.Background()
	missingDir := filepath.Join(t.TempDir(), "missing-dir")
	missingLock, err := acquireLock(ctx, missingDir, 50*time.Millisecond)
	if err == nil {
		if closeErr := missingLock.Close(); closeErr != nil {
			t.Errorf("missingLock.Close() = %v", closeErr)
		}
		t.Fatal("acquireLock(missing dir) succeeded, want error")
	}
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrLockTimeout) {
		t.Fatalf("acquireLock(missing dir) = %v, want a not-exist error, no timeout", err)
	}
}

func testAcquireLockTimeoutAndReacquire(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	lock1, err := acquireLock(ctx, dir, time.Second)
	if err != nil {
		t.Fatalf("acquireLock(1) = %v", err)
	}
	lock2, err := acquireLock(ctx, dir, 20*time.Millisecond)
	if !errors.Is(err, ErrLockTimeout) {
		if err == nil {
			if closeErr := lock2.Close(); closeErr != nil {
				t.Errorf("lock2.Close() = %v", closeErr)
			}
		}
		if closeErr := lock1.Close(); closeErr != nil {
			t.Errorf("lock1.Close() = %v", closeErr)
		}
		t.Fatalf("acquireLock(2) while held = %v, want ErrLockTimeout", err)
	}
	if err := lock1.Close(); err != nil {
		t.Fatalf("lock1.Close() = %v", err)
	}

	lock3, err := acquireLock(ctx, dir, time.Second)
	if err != nil {
		t.Fatalf("acquireLock(3) after close = %v", err)
	}
	if err := lock3.Close(); err != nil {
		t.Fatalf("lock3.Close() = %v", err)
	}
}
