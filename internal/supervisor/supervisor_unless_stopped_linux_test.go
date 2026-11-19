//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/eventlog"
)

// TestUnlessStoppedReconcileResume: when reconcile decides to resume
// an interrupted always job and job.stopped is appended before checkShimNotStopped, the shim
// exits without starting the job, the record stays stopped, no job.restarted is written and
// Reconcile returns nil; with no stop in between, resume starts with no job.restarted.
func TestUnlessStoppedReconcileResume(t *testing.T) {
	assertUnlessStoppedResumeHonorsStop(t)
	assertUnlessStoppedResumeStartsWhenNotStopped(t)
}

func assertUnlessStoppedResumeHonorsStop(t *testing.T) {
	t.Helper()
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	seedInterruptedAlwaysJob(t, sup, "2026-11-18T08:00:00Z")
	heldDuringStop := false
	interceptAppendEvent(t, func(_ context.Context, ev eventlog.Event) error {
		if ev.Type == "job.stopped" {
			held, err := lockHeld(sup.lockPath("daemon"))
			heldDuringStop = err == nil && held
		}
		return nil
	})
	stopOnSecondRecordRead(t, sup, "daemon")
	reconcileOnce(t, sup, "resume raced by stop")
	if !heldDuringStop {
		t.Fatal("stopNotRunning appended job.stopped without holding the job lock")
	}
	got := eventlogJob(t, sup, "daemon")
	if got.State != string(StateStopped) || got.LastEvent != "job.stopped" || got.Restarts != 0 {
		t.Fatalf("job after stopped resume = %+v, want stopped with 0 restarts", got)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 1 {
		t.Fatalf("job.started count = %d, want 1 (no start after stop)", starts)
	}
	if restarts := countEvents(t, sup.cfg.EventLog.Dir, "job.restarted"); restarts != 0 {
		t.Fatalf("job.restarted count = %d, want 0", restarts)
	}
}

func assertUnlessStoppedResumeStartsWhenNotStopped(t *testing.T) {
	t.Helper()
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	seedInterruptedAlwaysJob(t, sup, "2026-11-18T08:00:00Z")
	reconcileOnce(t, sup, "resume without stop")
	got := eventlogJob(t, sup, "daemon")
	if got.State != string(StateRunning) || got.LastEvent != "job.started" || got.Restarts != 0 {
		t.Fatalf("job after uninterrupted resume = %+v, want running with 0 restarts", got)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 2 {
		t.Fatalf("job.started count = %d, want 2", starts)
	}
	if restarts := countEvents(t, sup.cfg.EventLog.Dir, "job.restarted"); restarts != 0 {
		t.Fatalf("job.restarted count = %d, want 0", restarts)
	}
}

func seedInterruptedAlwaysJob(t *testing.T, sup *Supervisor, since string) {
	t.Helper()
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": since})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": since})
	reconcileOnce(t, sup, "record interruption")
}

func stopOnSecondRecordRead(t *testing.T, sup *Supervisor, name string) {
	t.Helper()
	oldRead := jobRecordReadFile
	reads := 0
	jobRecordReadFile = func(path string) ([]byte, error) {
		reads++
		if reads == 2 {
			jobRecordReadFile = oldRead
			if err := sup.Stop(testContext(t), name); err != nil {
				t.Errorf("Stop(%s) during reconcile = %v, want nil", name, err)
			}
		}
		return oldRead(path)
	}
	t.Cleanup(func() { jobRecordReadFile = oldRead })
}

// TestUnlessStoppedRestartBackoff: a job.stopped appended during restartJob's
// backoff prevents the start and restartJob returns nil.
func TestUnlessStoppedRestartBackoff(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 2, 1, longJobCommand(t)))
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "2026-11-18T08:00:00Z"})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 1})
	restarted := make(chan struct{}, 1)
	realAppend := appendEvent
	appendEvent = func(w *eventlog.Writer, c context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		rec, err := realAppend(w, c, check)
		if err == nil && rec.Type == "job.restarted" {
			restarted <- struct{}{}
		}
		return rec, err
	}
	t.Cleanup(func() { appendEvent = realAppend })
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(testContext(t))
	finished := false
	defer stopReconcileWorker(t, cancel, done, &finished)
	go func() { done <- sup.Reconcile(ctx) }()
	finished, err := waitForRestart(restarted, done, time.After(testWaitBound))
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop(during backoff) = %v, want nil", err)
	}
	err = <-done
	finished = true
	if err != nil {
		t.Fatalf("Reconcile(stopped during backoff) = %v, want nil", err)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 1 {
		t.Fatalf("job.started count = %d, want 1 (no start after backoff stop)", starts)
	}
	if got := eventlogJob(t, sup, "daemon"); got.State != string(StateStopped) || got.LastEvent != "job.stopped" {
		t.Fatalf("job after backoff stop = %+v, want stopped", got)
	}
}

func stopReconcileWorker(t *testing.T, cancel context.CancelFunc, done <-chan error, finished *bool) {
	t.Helper()
	cancel()
	if !*finished {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Reconcile cleanup = %v", err)
		}
	}
}

func waitForRestart(restarted <-chan struct{}, done <-chan error, deadline <-chan time.Time) (bool, error) {
	select {
	case <-restarted:
		return false, nil
	case err := <-done:
		return true, fmt.Errorf("Reconcile ended before job.restarted: %w", err)
	case <-deadline:
		return false, errors.New("deadline waiting for job.restarted")
	}
}

func TestWaitForRestartReportsEarlyFailure(t *testing.T) {
	done := make(chan error, 1)
	done <- errors.New("append failed")
	finished, err := waitForRestart(make(chan struct{}), done, make(chan time.Time))
	if !finished || err == nil || !strings.Contains(err.Error(), "append failed") {
		t.Fatalf("waitForRestart = %t, %v; want early append failure", finished, err)
	}
	deadline := make(chan time.Time)
	close(deadline)
	finished, err = waitForRestart(make(chan struct{}), make(chan error), deadline)
	if finished || err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("waitForRestart deadline = %t, %v", finished, err)
	}
}

// TestStopNotRunningWaitsForStartingShimRecord: when stopOne finds the lock
// held by a starting shim without a record yet, it waits for the record, terminates the job
// and appends job.stopped; if the record is never written, it returns an error naming the job
// and appends no job.stopped.
func TestStopNotRunningWaitsForStartingShimRecord(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	oldRead := jobRecordReadFile
	reads := 0
	jobRecordReadFile = func(path string) ([]byte, error) {
		reads++
		if reads <= 2 {
			return nil, os.ErrNotExist
		}
		return oldRead(path)
	}
	t.Cleanup(func() { jobRecordReadFile = oldRead })
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop(record written within bound) = %v, want nil", err)
	}
	jobRecordReadFile = oldRead
	if reads < 3 || statusOne(t, sup, "daemon").State != StateStopped || eventlogJob(t, sup, "daemon").LastEvent != "job.stopped" {
		t.Fatalf("after Stop: reads=%d status=%+v job=%+v, want polled record, StateStopped and job.stopped", reads, statusOne(t, sup, "daemon"), eventlogJob(t, sup, "daemon"))
	}
	assertStopNotRunningWithoutRecordErrors(t)
}

func assertStopNotRunningWithoutRecordErrors(t *testing.T) {
	t.Helper()
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	lock, err := holdLock(sup.lockPath("daemon"))
	if err != nil {
		t.Fatalf("holdLock(daemon) = %v, want nil", err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Millisecond)
	defer cancel()
	err = sup.Stop(ctx, "daemon")
	if err == nil || !strings.Contains(err.Error(), "stop daemon:") || !strings.Contains(err.Error(), "missing or invalid") {
		t.Fatalf("Stop(no record) = %v, want error naming daemon and missing record", err)
	}
	if stopped := countEvents(t, sup.cfg.EventLog.Dir, "job.stopped"); stopped != 0 {
		t.Fatalf("job.stopped count = %d, want 0", stopped)
	}
}

// TestUnlessStoppedOperatorStartAfterStop: operator Start and StartOnce pass
// no --unless-stopped flag to the shim and start a previously stopped job.
func TestUnlessStoppedOperatorStartAfterStop(t *testing.T) {
	job := stopJob("always", 1, 0, longJobCommand(t))
	sup, _ := stopSupervisor(t, job)
	if lastArgIndex(sup.shimCmd(testContext(t), job).Args, "--unless-stopped") != -1 {
		t.Fatalf("shimCmd args = %v, want no --unless-stopped", sup.shimCmd(testContext(t), job).Args)
	}
	if lastArgIndex(sup.shimCmdFor(testContext(t), job, false).Args, "--unless-stopped") != -1 {
		t.Fatalf("shimCmdFor(false) args = %v, want no --unless-stopped", sup.shimCmdFor(testContext(t), job, false).Args)
	}
	if lastArgIndex(sup.shimCmdFor(testContext(t), job, true).Args, "--unless-stopped") == -1 {
		t.Fatalf("shimCmdFor(true) args = %v, want --unless-stopped", sup.shimCmdFor(testContext(t), job, true).Args)
	}
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop(initial) = %v, want nil", err)
	}
	if err := sup.Start(testContext(t), "daemon"); err != nil || statusOne(t, sup, "daemon").State != StateRunning {
		t.Fatalf("Start(after stop) = %v, state %s, want nil and running", err, statusOne(t, sup, "daemon").State)
	}
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop(second) = %v, want nil", err)
	}
	if err := sup.StartOnce(testContext(t), "daemon"); err != nil || statusOne(t, sup, "daemon").State != StateRunning {
		t.Fatalf("StartOnce(after stop) = %v, state %s, want nil and running", err, statusOne(t, sup, "daemon").State)
	}
}

// TestUnlessStoppedShimCheckAndHoldLock: unit tests for checkShimNotStopped
// and holdShimLock.
func TestUnlessStoppedShimCheckAndHoldLock(t *testing.T) {
	assertCheckShimNotStoppedCases(t)
	assertHoldShimLockCases(t)
}

func assertCheckShimNotStoppedCases(t *testing.T) {
	t.Helper()
	cfg := validShimConfig(t, "daemon")
	unreadable := cfg
	unreadable.SigningKey = filepath.Join(t.TempDir(), "missing-key")
	eventsPath := filepath.Join(cfg.EventLogDir, "events")
	if err := os.WriteFile(eventsPath, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(events) = %v, want nil", err)
	}
	if err := checkShimNotStopped(unreadable); err != nil {
		t.Fatalf("checkShimNotStopped(flag absent) = %v, want nil without reading log", err)
	}
	if err := os.Remove(eventsPath); err != nil {
		t.Fatalf("Remove(events) = %v, want nil", err)
	}
	cfg.UnlessStopped = true
	if err := checkShimNotStopped(cfg); err != nil {
		t.Fatalf("checkShimNotStopped(no event log) = %v, want nil", err)
	}
	appendShimTestEvent(t, cfg, "job.started", `{"state":"running","pid":10,"shim_pid":9,"since":"t1"}`)
	if err := checkShimNotStopped(cfg); err != nil {
		t.Fatalf("checkShimNotStopped(running) = %v, want nil", err)
	}
	appendShimTestEvent(t, cfg, "job.exited", `{"state":"dead","code":0}`)
	if err := checkShimNotStopped(cfg); err != nil {
		t.Fatalf("checkShimNotStopped(dead) = %v, want nil", err)
	}
	appendShimTestEvent(t, cfg, "job.stopped", `{"state":"stopped"}`)
	if err := checkShimNotStopped(cfg); !errors.Is(err, errShimStopped) {
		t.Fatalf("checkShimNotStopped(stopped) = %v, want errShimStopped", err)
	}
	args := append([]string{"__job-shim", "--unless-stopped"}, shimArgs(t, cfg)[1:]...)
	if code := RunShim(args); code != shimExitStopped {
		t.Fatalf("RunShim(stopped) = %d, want %d", code, shimExitStopped)
	}
}

func appendShimTestEvent(t *testing.T, cfg shimConfig, typ string, payload string) {
	t.Helper()
	writer, err := shimWriter(cfg)
	if err != nil {
		t.Fatalf("shimWriter() = %v, want nil", err)
	}
	if _, err = writer.Append(testContext(t), eventlog.Event{Type: typ, TaskID: cfg.Name, Payload: []byte(payload)}); err != nil {
		t.Fatalf("Append(%s) = %v, want nil", typ, err)
	}
}

func assertHoldShimLockCases(t *testing.T) {
	t.Helper()
	cfg := validShimConfig(t, "lock-job")
	held, err := holdLock(cfg.LockPath)
	if err != nil {
		t.Fatalf("holdLock(initial) = %v, want nil", err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			closeTestHeldLock(t, held)
		}
	})
	start := time.Now()
	if got, lockErr := holdShimLock(cfg); got != nil || !errors.Is(lockErr, errLockBusy) || time.Since(start) >= 200*time.Millisecond {
		t.Fatalf("holdShimLock(flag absent, busy) = %v, %v in %s, want immediate errLockBusy", got, lockErr, time.Since(start))
	}
	cfg.UnlessStopped = true
	oldBusy := afterShimLockBusy
	t.Cleanup(func() { afterShimLockBusy = oldBusy })
	called := false
	afterShimLockBusy = func() {
		called = true
		released = true
		closeTestHeldLock(t, held)
	}
	got, err := holdShimLock(cfg)
	afterShimLockBusy = oldBusy
	if !called {
		t.Fatal("holdShimLock did not observe the initial busy lock")
	}
	if err != nil || got == nil {
		t.Fatalf("holdShimLock(busy then free) = %v, %v, want lock", got, err)
	}
	if err = got.Close(); err != nil {
		t.Fatalf("Close(got) = %v, want nil", err)
	}
	assertHoldShimLockBusyPastBound(t, cfg)
}

func closeTestHeldLock(t *testing.T, held *heldLock) {
	t.Helper()
	if err := held.Close(); err != nil {
		t.Errorf("Close(held) = %v", err)
	}
}

func assertHoldShimLockBusyPastBound(t *testing.T, cfg shimConfig) {
	t.Helper()
	held, err := holdLock(cfg.LockPath)
	if err != nil {
		t.Fatalf("holdLock(past bound) = %v, want nil", err)
	}
	defer func() {
		if closeErr := held.Close(); closeErr != nil {
			t.Fatalf("Close(held past bound) = %v, want nil", closeErr)
		}
	}()
	if got, lockErr := holdShimLock(cfg); got != nil || !errors.Is(lockErr, errLockBusy) {
		t.Fatalf("holdShimLock(busy past bound) = %v, %v, want nil and errLockBusy", got, lockErr)
	}
}
