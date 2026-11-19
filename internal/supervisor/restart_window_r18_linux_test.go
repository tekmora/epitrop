//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"testing"
	"time"
)

func r18LiveProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `sleep "$1"`, "sh", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		var exitErr *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exitErr) {
			t.Error(err)
		}
	})
	return cmd.Process.Pid
}

func r18StaleRecord(t *testing.T, shim int) (*Supervisor, *heldLock) {
	t.Helper()
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	lock := holdRunningRecordForShim(t, sup, "daemon", shim, "2026-11-18T08:00:00Z")
	pid := exitedShimPID(t)
	record := fmt.Sprintf(`{"pid":%d,"shim_pid":%d,"started_at":"2026-11-18T08:00:00Z"}`, pid, shim)
	if err := os.WriteFile(sup.recordPath("daemon"), []byte(record), 0o600); err != nil {
		closeLockForShim(lock)
		t.Fatal(err)
	}
	return sup, lock
}

func r18WaitForRelease(t *testing.T, shim int) {
	t.Helper()
	sup, lock := r18StaleRecord(t, shim)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Stop(ctx, "daemon") }()
	select {
	case err := <-done:
		closeLockForShim(lock)
		t.Fatalf("Stop before release: %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.stopped" {
		t.Fatalf("job = %+v", got)
	}
}

func TestR18StopWaitsForExitedShim(t *testing.T) { r18WaitForRelease(t, exitedShimPID(t)) }
func TestR18StopWaitsForReusedShim(t *testing.T) { r18WaitForRelease(t, r18LiveProcess(t)) }

func TestR18StopUnreadableCmdline(t *testing.T) {
	sup, lock := r18StaleRecord(t, r18LiveProcess(t))
	defer closeLockForShim(lock)
	old := procCmdlineReadFile
	procCmdlineReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	t.Cleanup(func() { procCmdlineReadFile = old })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := sup.Stop(ctx, "daemon")
	if err == nil || ctx.Err() != nil {
		t.Fatalf("Stop = %v, context = %v", err, ctx.Err())
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.stopped"); got != 0 {
		t.Fatalf("stopped events = %d", got)
	}
}

func TestR18StopStaleCancellation(t *testing.T) {
	sup, lock := r18StaleRecord(t, exitedShimPID(t))
	defer closeLockForShim(lock)
	deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Stop(ctx, "daemon") }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Stop = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not return after cancellation")
	}
}

func TestR18MarkedReusedShimNotAdopted(t *testing.T) {
	shim := r18LiveProcess(t)
	sup, lock := r18StaleRecord(t, shim)
	defer closeLockForShim(lock)
	since := "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": exitedShimPID(t), "shim_pid": shim, "since": since})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": since})
	reconcileOnce(t, sup, "reused shim")
	r18AssertMarked(t, sup)
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, sup, "released lock")
	assertInterruptedState(t, eventlogJob(t, sup, "daemon"))
}

func r18AssertMarked(t *testing.T, sup *Supervisor) {
	t.Helper()
	if got := eventlogJob(t, sup, "daemon"); !got.SupervisorStopped || countEvents(t, sup.cfg.EventLog.Dir, "job.adopted") != 0 {
		t.Fatalf("marked job = %+v", got)
	}
}

func TestR18MarkedUnreadableStateNotAdopted(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatal(err)
	}
	since := eventlogJob(t, sup, "daemon").Since
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": since})
	old := procStatusReadFile
	procStatusReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	t.Cleanup(func() { procStatusReadFile = old })
	reconcileOnce(t, sup, "unreadable process state")
	r18AssertMarked(t, sup)
}
