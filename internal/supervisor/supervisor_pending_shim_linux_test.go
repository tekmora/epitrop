//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// TestAdoptClearsStopMarkerOnNonAlwaysSchedules: once and cron jobs marked
// running while their lock is held record job.adopted, clear SupervisorStopped without a new
// job.started and keep the cron next-fire unchanged; releasing the lock without an exit
// record then writes no job.interrupted.
func TestAdoptClearsStopMarkerOnNonAlwaysSchedules(t *testing.T) {
	onceJob := testJob("once-job", []string{"/bin/true"})
	onceJob.Schedule = "once"
	cronJob := testJob("cron-job", []string{"/bin/true"})
	cronJob.Schedule = "0 2 * * *"
	cronJob.Restart = config.JobRestartConfig{Policy: "never"}
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 1, 59, 0, 0, time.UTC))
	sup := testSupervisorWithClock(t, testRuntime(t), clk, onceJob, cronJob)
	reconcileOnce(t, sup, "arm cron-job")
	clk.Advance(90 * time.Second)
	onceLock := seedMarkedRunningNonAlwaysJob(t, sup, onceJob, false)
	cronLock := seedMarkedRunningNonAlwaysJob(t, sup, cronJob, true)
	reconcileOnce(t, sup, "adopt marked once and cron jobs")
	assertAdoptedMarkedNonAlwaysJobs(t, sup)
	if err := onceLock.Close(); err != nil {
		t.Fatalf("Close(onceLock) = %v, want nil", err)
	}
	if err := cronLock.Close(); err != nil {
		t.Fatalf("Close(cronLock) = %v, want nil", err)
	}
	clk.Advance(time.Minute)
	reconcileOnce(t, sup, "after lock release with no exit record")
	if got := countEvents(t, sup.cfg.EventLog.Dir, eventInterrupted); got != 0 {
		t.Fatalf("job.interrupted count after unlocked post-adopt = %d, want 0", got)
	}
	clk.Advance(24 * time.Hour)
	reconcileOnce(t, sup, "cron next due")
	if triggers := collectTriggeredEvents(t, sup, "cron-job"); len(triggers) != 2 || triggers[1].Due != "2026-11-19T02:00:00Z" {
		t.Fatalf("cron triggers = %+v, want second fire at unchanged next due 2026-11-19T02:00:00Z", triggers)
	}
}

func seedMarkedRunningNonAlwaysJob(t *testing.T, sup *Supervisor, job config.JobConfig, triggered bool) *heldLock {
	t.Helper()
	const since = "2026-11-18T02:00:00Z"
	lock, err := holdLock(sup.lockPath(job.Name))
	if err != nil {
		t.Fatalf("holdLock(%s) = %v, want nil", job.Name, err)
	}
	cfg := shimConfig{Name: job.Name, RecordPath: sup.recordPath(job.Name), LogPath: job.LogPath}
	if err = writeRecord(cfg, 400, since); err != nil {
		t.Fatalf("writeRecord(%s) = %v, want nil", job.Name, err)
	}
	if triggered {
		appendHistory(t, sup, "job.triggered", job.Name, map[string]any{"trigger": canonicalTrigger(job), "outcome": "started", "due": since})
	}
	appendHistory(t, sup, "job.started", job.Name, map[string]any{"state": "running", "pid": 400, "shim_pid": os.Getpid(), "since": since})
	appendHistory(t, sup, eventSupervisorStopped, job.Name, map[string]any{"supervisor_pid": 1, "since": since})
	return lock
}

func assertAdoptedMarkedNonAlwaysJobs(t *testing.T, sup *Supervisor) {
	t.Helper()
	names := []string{"once-job", "cron-job"}
	for i := 0; i < len(names); i++ {
		got := eventlogJob(t, sup, names[i])
		if got.LastEvent != "job.adopted" || got.SupervisorStopped {
			t.Fatalf("%s after reconcile = %+v, want job.adopted and SupervisorStopped false", names[i], got)
		}
	}
	if adopted := countEvents(t, sup.cfg.EventLog.Dir, "job.adopted"); adopted != 2 {
		t.Fatalf("job.adopted count = %d, want 2", adopted)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 2 {
		t.Fatalf("job.started count = %d, want 2 (no new start)", starts)
	}
	if triggers := collectTriggeredEvents(t, sup, "cron-job"); len(triggers) != 1 || triggers[0].Due != "2026-11-18T02:00:00Z" {
		t.Fatalf("cron triggers after adopt = %+v, want unchanged single trigger at 2026-11-18T02:00:00Z", triggers)
	}
}

// TestPendingShimObserveStartedOnCancel: a cancelled start wait joins any final
// replay error from observeStartedOnCancel with context.Canceled, and returns only the cancel
// leaf when the final replay succeeds.
func TestPendingShimObserveStartedOnCancel(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	okCtx, okPID := startStubShimUntilCancel(t, sup, "daemon")
	err := sup.waitStarted(okCtx, "daemon", eventlog.Job{}, okPID)
	if !errors.Is(err, context.Canceled) || !errors.Is(errors.Unwrap(err), context.Canceled) || strings.Contains(err.Error(), "last replay:") {
		t.Fatalf("waitStarted(cancel, replay ok) = %v, want only context.Canceled leaf", err)
	}
	failCtx, failPID := startStubShimUntilCancel(t, sup, "daemon")
	eventsPath := filepath.Join(sup.cfg.EventLog.Dir, "events")
	if writeErr := os.WriteFile(eventsPath, []byte("bad"), 0o600); writeErr != nil {
		t.Fatalf("WriteFile(events) = %v, want nil", writeErr)
	}
	t.Cleanup(func() {
		if rmErr := os.Remove(eventsPath); rmErr != nil && !os.IsNotExist(rmErr) {
			t.Errorf("Remove(events) = %v, want nil", rmErr)
		}
	})
	err = sup.waitStarted(failCtx, "daemon", eventlog.Job{}, failPID)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), "last replay:") {
		t.Fatalf("waitStarted(cancel, replay failed) = %v, want context.Canceled and ENOTDIR replay error", err)
	}
}

// TestSpawnedShimIgnoresStaleCollector: when shim A is followed by shim B, A's
// late collector neither overwrites collectedShim for B nor closes B's done channel.
func TestSpawnedShimIgnoresStaleCollector(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	cmdA := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 1")
	if err := cmdA.Start(); err != nil {
		t.Fatalf("Start(A) = %v, want nil", err)
	}
	pidA := cmdA.Process.Pid
	pidB := pidA + 100
	sup.recordSpawnedShim("daemon", pidA)
	sup.shimWaiters.Add(1)
	sup.recordSpawnedShim("daemon", pidB)
	sup.mu.Lock()
	doneB := sup.spawnedShim["daemon"].done
	sup.mu.Unlock()
	sup.collectShim(cmdA, "daemon")
	sup.mu.Lock()
	gotCollected, hasCollected := sup.collectedShim["daemon"]
	curSpawned := sup.spawnedShim["daemon"]
	sup.mu.Unlock()
	if hasCollected {
		t.Fatalf("collectedShim after stale collector A = %+v, want none for B", gotCollected)
	}
	if curSpawned.pid != pidB || curSpawned.done == nil {
		t.Fatalf("spawnedShim after stale collector A = %+v, want pid %d with open done", curSpawned, pidB)
	}
	select {
	case <-doneB:
		t.Fatal("B's done channel was closed by A's collector")
	default:
	}
	sup.shimWaiters.Add(1)
	sup.finishShimCollector("daemon", pidB)
	select {
	case <-doneB:
	default:
		t.Fatal("B's done channel was not closed by B's finishShimCollector")
	}
}

// TestPendingShimCollectorBlocksStopMarker: when a shim exited with status 1 and
// its collector is delayed at beforeRecordCollectedShim, markSupervisorStopped waits for the
// collector and writes no marker; if the collector stays pending past stopMarkTimeout, no
// marker is written and Supervise's error names the job.
func TestPendingShimCollectorBlocksStopMarker(t *testing.T) {
	assertDelayedCollectorSkipsMarker(t)
	assertPendingCollectorPastTimeoutFailsSupervise(t)
}

func assertDelayedCollectorSkipsMarker(t *testing.T) {
	t.Helper()
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	atSeam, release := make(chan struct{}), make(chan struct{})
	oldSeam := beforeRecordCollectedShim
	beforeRecordCollectedShim = func(string, int) {
		close(atSeam)
		<-release
	}
	defer func() { beforeRecordCollectedShim = oldSeam }()
	startExitedStubShimWithDelayedCollector(t, sup, atSeam, "job.started")
	realAppend := appendEvent
	appendEvent = func(w *eventlog.Writer, c context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		return realAppend(w, c, func(cctx context.Context) (eventlog.Event, error) {
			appendEvent = realAppend
			close(release)
			return check(cctx)
		})
	}
	defer func() { appendEvent = realAppend }()
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped(delayed collector) = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 0 {
		t.Fatalf("supervisor stop markers with delayed exit-1 collector = %d, want 0", got)
	}
}

func assertPendingCollectorPastTimeoutFailsSupervise(t *testing.T) {
	t.Helper()
	oldTimeout := stopMarkTimeout
	stopMarkTimeout = 30 * time.Millisecond
	t.Cleanup(func() { stopMarkTimeout = oldTimeout })
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	atSeam, release := make(chan struct{}), make(chan struct{})
	oldSeam := beforeRecordCollectedShim
	beforeRecordCollectedShim = func(string, int) {
		close(atSeam)
		<-release
	}
	t.Cleanup(func() { beforeRecordCollectedShim = oldSeam })
	defer close(release)
	shimPID := startExitedStubShimWithDelayedCollector(t, sup, atSeam, "job.adopted")
	lock := holdRunningRecordForShim(t, sup, "daemon", shimPID, "2026-11-18T08:00:00Z")
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := sup.Supervise(ctx)
	if !errors.Is(err, ErrStopMarkersNotWritten) || !strings.Contains(err.Error(), "job daemon") {
		t.Fatalf("Supervise(collector past timeout) = %v, want ErrStopMarkersNotWritten naming daemon", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 0 {
		t.Fatalf("supervisor stop markers past timeout = %d, want 0", got)
	}
}

func startExitedStubShimWithDelayedCollector(t *testing.T, sup *Supervisor, atSeam <-chan struct{}, eventType string) int {
	t.Helper()
	const since = "2026-11-18T08:00:00Z"
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "exit 1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(exit 1) = %v, want nil", err)
	}
	shimPID := cmd.Process.Pid
	sup.recordSpawnedShim("daemon", shimPID)
	sup.shimWaiters.Add(1)
	appendHistory(t, sup, eventType, "daemon", map[string]any{"state": "running", "pid": shimPID + 1, "shim_pid": shimPID, "since": since})
	sup.observeRunning("daemon", since)
	go sup.collectShim(cmd, "daemon")
	<-atSeam
	return shimPID
}

func holdRunningRecordForShim(t *testing.T, sup *Supervisor, name string, shimPID int, since string) *heldLock {
	t.Helper()
	lock, err := holdLock(sup.lockPath(name))
	if err != nil {
		t.Fatalf("holdLock(%s) = %v, want nil", name, err)
	}
	cfg := shimConfig{Name: name, RecordPath: sup.recordPath(name), LogPath: sup.cfg.Jobs[0].LogPath}
	if err = writeRecord(cfg, shimPID+1, since); err != nil {
		t.Fatalf("writeRecord(%s) = %v, want nil", name, err)
	}
	return lock
}
