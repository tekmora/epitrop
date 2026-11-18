//go:build linux

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tekmora/epitrop/internal/eventlog"
)

func TestStopRunningRecordsUnderJobLock(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatal(err)
	}
	held := false
	interceptAppendEvent(t, func(_ context.Context, ev eventlog.Event) error {
		if ev.Type == "job.stopped" {
			var err error
			held, err = lockHeld(sup.lockPath("daemon"))
			if err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("job.stopped appended without job lock")
	}
}

func TestStopStaleRecordDuringStart(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	if err := os.MkdirAll(filepath.Dir(sup.recordPath("daemon")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sup.recordPath("daemon"), []byte(`{"pid":99999999,"shim_pid":99999998}`), 0o600); err != nil {
		t.Fatal(err)
	}
	assertStopAfterRecordlessRelease(t, sup)
}

func TestStopStartingShimReleasesWithoutRecord(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	assertStopAfterRecordlessRelease(t, sup)
}

func assertStopAfterRecordlessRelease(t *testing.T, sup *Supervisor) {
	t.Helper()
	lock, err := holdLock(sup.lockPath("daemon"))
	if err != nil {
		t.Fatal(err)
	}
	releaseOnSecondRecordRead(t, lock)
	held := false
	interceptAppendEvent(t, func(_ context.Context, ev eventlog.Event) error {
		if ev.Type == "job.stopped" {
			var lockErr error
			held, lockErr = lockHeld(sup.lockPath("daemon"))
			if lockErr != nil {
				t.Error(lockErr)
			}
		}
		return nil
	})
	if err = sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop after shim release: %v", err)
	}
	if !held {
		t.Fatal("job.stopped appended without job lock")
	}
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.stopped" {
		t.Fatalf("last event = %s, want job.stopped", got.LastEvent)
	}
}

func releaseOnSecondRecordRead(t *testing.T, lock *heldLock) {
	t.Helper()
	released := false
	t.Cleanup(func() {
		if !released {
			if closeErr := lock.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
	})
	oldRead := jobRecordReadFile
	reads := 0
	jobRecordReadFile = func(path string) ([]byte, error) {
		reads++
		if reads == 2 {
			if closeErr := lock.Close(); closeErr != nil {
				t.Error(closeErr)
			}
			released = true
		}
		return oldRead(path)
	}
	t.Cleanup(func() { jobRecordReadFile = oldRead })
}
