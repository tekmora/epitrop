//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

func startAndCollectStubShim(t *testing.T, sup *Supervisor, name string) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(exit 1) = %v, want nil", err)
	}
	pid := cmd.Process.Pid
	sup.recordSpawnedShim(name, pid)
	sup.shimWaiters.Add(1)
	sup.collectShim(cmd, name)
	return pid
}

// A shim this supervisor started that exited with code 1 without recording job.exited was not
// ended by SIGTERM: it gets no stop marker, and the next supervisor records lost and spends
// restart budget.
func TestSupervisorStopSkipsShimThatExitedWithoutSIGTERM(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 2, 0, longJobCommand(t)))
	const since = "2026-11-18T08:00:00Z"
	shimPID := startAndCollectStubShim(t, sup, "daemon")
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": shimPID + 1, "shim_pid": shimPID, "since": since})
	sup.observeRunning("daemon", since)
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 0 {
		t.Fatalf("supervisor stop markers for shim that exited 1 = %d, want 0", got)
	}
	reconcileOnce(t, sup, "record the loss")
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.exited" || got.LastReason != "lost" {
		t.Fatalf("job after non-SIGTERM shim exit = %+v, want job.exited with reason lost", got)
	}
	reconcileOnce(t, sup, "restart after loss")
	if got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon"); got.Restarts != 1 || hist.Restarted != 1 {
		t.Fatalf("job after restart = %+v, history %+v, want 1 counted restart", got, hist)
	}
}

// A shim this supervisor started (TestHelperProcessShim, a Go process without a TERM handler)
// that is killed by SIGTERM reports Signaled SIGTERM through cmd.Wait, gets its stop marker,
// and resumes via job.interrupted without spending restart budget.
func TestSupervisorStopMarksShimEndedBySIGTERM(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	running := statusOne(t, sup, "daemon")
	assertSafeSignalPID(t, running.ShimPID)
	if err := syscall.Kill(running.ShimPID, syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM shim %d = %v, want nil", running.ShimPID, err)
	}
	waitForProcessGone(t, running.PID)
	waitForUnlock(t, sup, sup.cfg.Jobs[0])
	waitForShimWaiters(t, sup, 0)
	sup.mu.Lock()
	gotCollected := sup.collectedShim["daemon"]
	sup.mu.Unlock()
	if gotCollected != (collectedShim{pid: running.ShimPID, sigterm: true}) {
		t.Fatalf("collected shim after SIGTERM = %+v, want pid %d and sigterm true", gotCollected, running.ShimPID)
	}
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers for SIGTERM-killed shim = %d, want 1", got)
	}
	reconcileOnce(t, sup, "record the interruption")
	assertInterruptedState(t, eventlogJob(t, sup, "daemon"))
	reconcileOnce(t, sup, "resume the interrupted job")
	if got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon"); got.Restarts != 0 || hist.Restarted != 0 || got.State != string(StateRunning) {
		t.Fatalf("job after SIGTERM shim resume = %+v, history %+v, want 0 restarts and running", got, hist)
	}
}

// A collected shim exit whose pid does not match the running record's shim_pid is ignored and
// does not block the stop marker; a later spawn for the same job clears the collected entry so
// a run that reuses the collected pid still gets its marker.
func TestSupervisorStopIgnoresCollectedShimWithDifferentPID(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	const since = "2026-11-18T08:00:00Z"
	oldShimPID := startAndCollectStubShim(t, sup, "daemon")
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": oldShimPID + 20, "shim_pid": oldShimPID + 10, "since": since})
	sup.observeRunning("daemon", since)
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers with older collected shim pid = %d, want 1", got)
	}
	assertMarkerCarriesSince(t, sup, "daemon", since)
	assertSpawnClearsCollectedShim(t)
}

func assertSpawnClearsCollectedShim(t *testing.T) {
	t.Helper()
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	reusedPID := startAndCollectStubShim(t, sup, "daemon")
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	const since = "2026-11-18T08:10:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": reusedPID + 1, "shim_pid": reusedPID, "since": since})
	sup.observeRunning("daemon", since)
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers after spawn cleared collected pid = %d, want 1", got)
	}
}

func TestMarkedSurvivorWithUnreadableRecordDefersInterruption(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("on-failure", 2, 0, longJobCommand(t)))
	const since = "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": since})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": since})
	lock := holdRunningRecordForShim(t, sup, "daemon", os.Getpid(), since)
	released := false
	defer func() {
		if !released {
			if err := lock.Close(); err != nil {
				t.Errorf("Close(lock) = %v", err)
			}
		}
	}()
	old := jobRecordReadFile
	jobRecordReadFile = func(string) ([]byte, error) { return nil, errors.New("transient read failure") }
	defer func() { jobRecordReadFile = old }()
	reconcileOnce(t, sup, "unreadable survivor")
	if got := eventlogJob(t, sup, "daemon"); !got.SupervisorStopped || got.State != string(StateRunning) {
		t.Fatalf("job after unreadable record = %+v, want marked running", got)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	released = true
	reconcileOnce(t, sup, "unreadable record with free lock")
	assertInterruptedState(t, eventlogJob(t, sup, "daemon"))
}

func TestStopAcquiresLockAfterLastTermination(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	oldWait, oldTerminate := waitStopTargetForStop, terminateStopTarget
	defer func() { waitStopTargetForStop, terminateStopTarget = oldWait, oldTerminate }()
	attempts := 0
	heldDuringAppend := false
	interceptAppendEvent(t, func(_ context.Context, ev eventlog.Event) error {
		if ev.Type == "job.stopped" {
			held, err := lockHeld(sup.lockPath("daemon"))
			heldDuringAppend = err == nil && held
		}
		return nil
	})
	waitStopTargetForStop = func(s *Supervisor, _ context.Context, job config.JobConfig) (*heldLock, jobSignalTarget, error) {
		if attempts < maxStopAttempts {
			return nil, jobSignalTarget{}, nil
		}
		lock, err := holdLock(s.lockPath(job.Name))
		return lock, jobSignalTarget{}, err
	}
	terminateStopTarget = func(_ *Supervisor, _ context.Context, _ config.JobConfig, _ jobSignalTarget) error {
		attempts++
		return nil
	}
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop after final termination = %v", err)
	}
	if attempts != maxStopAttempts || !heldDuringAppend {
		t.Fatalf("terminations = %d, lock held during append = %t", attempts, heldDuringAppend)
	}
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.stopped" {
		t.Fatalf("last event = %s, want job.stopped", got.LastEvent)
	}
}

func TestStopReportsBusyAfterLastTermination(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	held, err := holdLock(sup.lockPath("daemon"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := held.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	oldWait, oldTerminate := waitStopTargetForStop, terminateStopTarget
	defer func() { waitStopTargetForStop, terminateStopTarget = oldWait, oldTerminate }()
	calls := 0
	waitStopTargetForStop = func(_ *Supervisor, _ context.Context, _ config.JobConfig) (*heldLock, jobSignalTarget, error) {
		return nil, jobSignalTarget{}, nil
	}
	terminateStopTarget = func(_ *Supervisor, _ context.Context, _ config.JobConfig, _ jobSignalTarget) error {
		calls++
		return nil
	}
	err = sup.Stop(testContext(t), "daemon")
	if err == nil || !strings.Contains(err.Error(), "lock remained busy") || calls != maxStopAttempts {
		t.Fatalf("Stop with busy lock = %v after %d terminations", err, calls)
	}
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent == "job.stopped" {
		t.Fatal("job.stopped recorded while lock held")
	}
}
