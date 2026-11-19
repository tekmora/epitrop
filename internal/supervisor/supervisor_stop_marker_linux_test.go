//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// Without the marker (a supervisor that crashed) a running record with a free lock is a lost
// job, counted against the cap as before.
func TestLostJobWithoutMarkerIsStillCounted(t *testing.T) {
	tests := []struct {
		name      string
		cap       int
		seeded    int
		restarted bool
	}{{"budget free", 2, 0, true}, {"budget spent", 1, 1, false}}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			sup, _ := stopSupervisor(t, stopJob("on-failure", tests[i].cap, 0, longJobCommand(t)))
			appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "2026-11-18T08:00:00Z"})
			for n := 0; n < tests[i].seeded; n++ {
				appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead"})
			}
			appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "2026-11-18T08:01:00Z"})
			reconcileOnce(t, sup, "record the exit")
			if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.exited" || got.LastReason != "lost" || got.State != string(StateDead) {
				t.Fatalf("job without a marker = %+v, want job.exited with reason lost", got)
			}
			reconcileOnce(t, sup, "restart")
			assertLostJobOutcome(t, sup, tests[i].restarted)
		})
	}
}

func assertLostJobOutcome(t *testing.T, sup *Supervisor, restarted bool) {
	t.Helper()
	got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon")
	if got.Restarts != 1 || hist.Interrupted != 0 || hist.Exited != 1 || hist.Unclean != 1 {
		t.Fatalf("job after a lost job = %+v, history %+v, want 1 exited, 1 unclean, 1 restart and 0 interrupted", got, hist)
	}
	if !restarted {
		if got.LastEvent != "job.exited" {
			t.Fatalf("job after a lost job with spent budget = %+v, want LastEvent job.exited", got)
		}
		return
	}
	if got.LastEvent != "job.started" || got.State != string(StateRunning) || statusOne(t, sup, "daemon").State != StateRunning {
		t.Fatalf("job after a lost job with free budget = %+v, status %+v, want job.started recorded and the job running", got, statusOne(t, sup, "daemon"))
	}
}

// A job that survived the stop is adopted, which clears the marker; its later loss records
// lost and is counted.
func TestAdoptionClearsTheSupervisorStopMarker(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("on-failure", 2, 0, longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	reconcileOnce(t, sup, "first adoption")
	if got := eventlogJob(t, sup, "daemon").LastEvent; got != "job.adopted" {
		t.Fatalf("last event after the first reconcile = %q, want job.adopted", got)
	}
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": eventlogJob(t, sup, "daemon").Since})
	reconcileOnce(t, sup, "adoption of a marked survivor")
	got := eventlogJob(t, sup, "daemon")
	if got.SupervisorStopped || got.State != string(StateRunning) || countEvents(t, sup.cfg.EventLog.Dir, "job.adopted") != 2 {
		t.Fatalf("job after adopting a marked survivor = %+v, want job.adopted again, the marker cleared, still running", got)
	}
	running := statusOne(t, sup, "daemon")
	assertSafeSignalPID(t, running.ShimPID)
	if err := syscall.Kill(running.ShimPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL shim %d = %v, want nil", running.ShimPID, err)
	}
	waitForProcessGone(t, running.PID)
	waitForUnlock(t, sup, sup.cfg.Jobs[0])
	reconcileOnce(t, sup, "record the loss")
	if got = eventlogJob(t, sup, "daemon"); got.LastReason != "lost" {
		t.Fatalf("job lost after the adoption = %+v, want reason lost, not supervisor-stop", got)
	}
	reconcileOnce(t, sup, "restart")
	if got = eventlogJob(t, sup, "daemon"); got.Restarts != 1 {
		t.Fatalf("job restarted after the loss = %+v, want 1 counted restart", got)
	}
}

func interceptAppendEvent(t *testing.T, fn func(context.Context, eventlog.Event) error) {
	t.Helper()
	realAppend := appendEvent
	appendEvent = func(w *eventlog.Writer, c context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		return realAppend(w, c, func(cctx context.Context) (eventlog.Event, error) {
			ev, err := check(cctx)
			if err != nil {
				return eventlog.Event{}, err
			}
			if err = fn(cctx, ev); err != nil {
				return eventlog.Event{}, err
			}
			return ev, nil
		})
	}
	t.Cleanup(func() { appendEvent = realAppend })
}

// A marker that cannot be written is part of Supervise's error, wrapped in
// ErrStopMarkersNotWritten beside the reason Supervise ended.
func TestSuperviseReturnsAMarkerWriteFailureWithItsOwnError(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	errDisk := errors.New("marker disk failure")
	interceptAppendEvent(t, func(_ context.Context, ev eventlog.Event) error {
		if ev.Type == eventSupervisorStopped {
			return errDisk
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), testWaitBound)
	defer cancel()
	sup.afterTick = cancel
	err := sup.Supervise(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errDisk) || !errors.Is(err, ErrStopMarkersNotWritten) {
		t.Fatalf("Supervise() = %v, want an error wrapping cancellation, ErrStopMarkersNotWritten and the marker failure", err)
	}
	if !strings.Contains(err.Error(), eventSupervisorStopped) {
		t.Fatalf("Supervise() error = %q, want it to name %s", err.Error(), eventSupervisorStopped)
	}
}

// Each job's stop marker gets its own deadline: when the first job's marker append blocks
// until its context expires, the second job still gets its marker and Supervise returns an
// error naming the first job.
func TestSupervisorStopGivesEachMarkerAppendItsOwnDeadline(t *testing.T) {
	oldTimeout := stopMarkTimeout
	stopMarkTimeout = 50 * time.Millisecond
	t.Cleanup(func() { stopMarkTimeout = oldTimeout })
	jobA := stopJob("always", 1, 0, longJobCommand(t))
	jobA.Name = "job-a"
	jobB := stopJob("always", 1, 0, longJobCommand(t))
	jobB.Name = "job-b"
	sup := testSupervisor(t, jobA, jobB)
	if err := sup.Start(testContext(t), ""); err != nil {
		t.Fatalf("Start(all) = %v, want nil", err)
	}
	interceptAppendEvent(t, func(cctx context.Context, ev eventlog.Event) error {
		if ev.Type == eventSupervisorStopped && ev.TaskID == "job-a" {
			<-cctx.Done()
			return cctx.Err()
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), testWaitBound)
	defer cancel()
	sup.afterTick = cancel
	err := sup.Supervise(ctx)
	if !errors.Is(err, ErrStopMarkersNotWritten) || !strings.Contains(err.Error(), "job job-a") {
		t.Fatalf("Supervise() = %v, want ErrStopMarkersNotWritten naming job-a", err)
	}
	if got := historyOne(t, sup, "job-a").SupervisorStops; got != 0 {
		t.Fatalf("supervisor stop markers of job-a = %d, want 0", got)
	}
	if got := historyOne(t, sup, "job-b").SupervisorStops; got != 1 || !eventlogJob(t, sup, "job-b").SupervisorStopped {
		t.Fatalf("job-b after stop = %+v (markers %d), want 1 marker and SupervisorStopped true", eventlogJob(t, sup, "job-b"), got)
	}
}

// Once, cron and event jobs that were marked running at a supervisor stop record
// job.interrupted on the first reconcile once their lock is free, and do not resume on the
// next reconcile without a due time or matching event; an unmarked once job records nothing.
func TestSupervisorStopInterruptsNonAlwaysSchedulesWithoutResuming(t *testing.T) {
	cases := []struct {
		name     string
		schedule string
		onEvent  *config.JobEventTrigger
	}{
		{"once", "once", nil},
		{"cron", "0 2 * * *", nil},
		{"event", "event", &config.JobEventTrigger{Type: "job.exited", TaskID: "other"}},
	}
	for i := 0; i < len(cases); i++ {
		t.Run(cases[i].name, func(t *testing.T) {
			assertNonAlwaysMarkedStopInterrupted(t, cases[i].schedule, cases[i].onEvent)
		})
	}
	assertUnmarkedOnceJobUnchanged(t)
}

func assertNonAlwaysMarkedStopInterrupted(t *testing.T, schedule string, onEvent *config.JobEventTrigger) {
	t.Helper()
	job := testJob("scheduled", []string{"/bin/true"})
	job.Schedule = schedule
	job.OnEvent = onEvent
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 1, 59, 0, 0, time.UTC))
	sup := testSupervisorWithClock(t, testRuntime(t), clk, job)
	appendHistory(t, sup, "job.started", job.Name, map[string]any{"state": "running", "since": "2026-11-18T01:58:00Z"})
	appendHistory(t, sup, eventSupervisorStopped, job.Name, map[string]any{"supervisor_pid": 1, "since": "2026-11-18T01:58:00Z"})
	reconcileOnce(t, sup, "record interruption")
	assertInterruptedState(t, eventlogJob(t, sup, job.Name))
	if exits := countEvents(t, sup.cfg.EventLog.Dir, "job.exited"); exits != 0 {
		t.Fatalf("job.exited count = %d, want 0", exits)
	}
	reconcileOnce(t, sup, "second pass without due time or match")
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 1 {
		t.Fatalf("job.started count after second reconcile = %d, want 1 (no resume)", starts)
	}
}

func assertUnmarkedOnceJobUnchanged(t *testing.T) {
	t.Helper()
	job := testJob("once-unmarked", []string{"/bin/true"})
	job.Schedule = "once"
	sup, _ := stopSupervisor(t, job)
	appendHistory(t, sup, "job.started", job.Name, map[string]any{"state": "running", "since": "2026-11-18T01:58:00Z"})
	reconcileOnce(t, sup, "unmarked once")
	if got := eventlogJob(t, sup, job.Name); got.LastEvent != "job.started" || countEvents(t, sup.cfg.EventLog.Dir, eventInterrupted) != 0 || countEvents(t, sup.cfg.EventLog.Dir, "job.exited") != 0 {
		t.Fatalf("unmarked once job after reconcile = %+v, want nothing recorded", got)
	}
}

// Checked appends re-verify the job state under the event-log lock: a racing stop or exit
// appended after the reconcile snapshot skips the stale interruption, adoption or loss.
func TestCheckedJobEventsSkipStaleSnapshots(t *testing.T) {
	assertCheckedInterruptedSkipsAfterStop(t)
	assertCheckedAdoptedSkipsAfterExit(t)
	assertCheckedLostSkipsAfterStop(t)
}

func assertCheckedInterruptedSkipsAfterStop(t *testing.T) {
	t.Helper()
	job := stopJob("always", 1, 0, []string{"/bin/true"})
	sup, _ := stopSupervisor(t, job)
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "t1"})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": "t1"})
	snap := eventlogJob(t, sup, "daemon")
	if err := sup.recordInterrupted(testContext(t), job, snap); err != nil {
		t.Fatalf("recordInterrupted(matching snapshot) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, eventInterrupted); got != 1 {
		t.Fatalf("job.interrupted count on matching snapshot = %d, want 1", got)
	}
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "t2"})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": "t2"})
	stale := eventlogJob(t, sup, "daemon")
	appendHistory(t, sup, "job.stopped", "daemon", map[string]any{"state": "stopped"})
	if err := sup.recordInterrupted(testContext(t), job, stale); err != nil {
		t.Fatalf("recordInterrupted(after job.stopped) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, eventInterrupted); got != 1 {
		t.Fatalf("job.interrupted count after racing job.stopped = %d, want still 1", got)
	}
}

func assertCheckedAdoptedSkipsAfterExit(t *testing.T) {
	t.Helper()
	job := stopJob("on-failure", 1, 0, []string{"/bin/true"})
	sup, _ := stopSupervisor(t, job)
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": 4, "shim_pid": 3, "since": "t1"})
	snap := eventlogJob(t, sup, "daemon")
	status := JobStatus{Name: "daemon", State: StateRunning, PID: 4, ShimPID: 3, Since: "t1"}
	if err := sup.adoptIfFreshStart(testContext(t), job, snap, status); err != nil {
		t.Fatalf("adoptIfFreshStart(matching snapshot) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.adopted"); got != 1 {
		t.Fatalf("job.adopted count on matching snapshot = %d, want 1", got)
	}
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": 5, "shim_pid": 4, "since": "t2"})
	stale := eventlogJob(t, sup, "daemon")
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 0})
	if err := sup.adoptIfFreshStart(testContext(t), job, stale, JobStatus{Name: "daemon", State: StateRunning, PID: 5, ShimPID: 4, Since: "t2"}); err != nil {
		t.Fatalf("adoptIfFreshStart(after job.exited) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.adopted"); got != 1 {
		t.Fatalf("job.adopted count after racing job.exited = %d, want still 1", got)
	}
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": 6, "shim_pid": 5, "since": "t3"})
	staleUnmarked := eventlogJob(t, sup, "daemon")
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": "t3"})
	if err := sup.adoptIfFreshStart(testContext(t), job, staleUnmarked, JobStatus{Name: "daemon", State: StateRunning, PID: 6, ShimPID: 5, Since: "t3"}); err != nil {
		t.Fatalf("adoptIfFreshStart(stale SupervisorStopped) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.adopted"); got != 1 {
		t.Fatalf("job.adopted count after stale SupervisorStopped = %d, want still 1", got)
	}
}

func assertCheckedLostSkipsAfterStop(t *testing.T) {
	t.Helper()
	job := stopJob("on-failure", 1, 0, []string{"/bin/true"})
	sup, _ := stopSupervisor(t, job)
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "t1"})
	snap := eventlogJob(t, sup, "daemon")
	if err := sup.recordExit(testContext(t), job, snap, "lost"); err != nil {
		t.Fatalf("recordExit(matching snapshot) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.exited"); got != 1 {
		t.Fatalf("job.exited count on matching snapshot = %d, want 1", got)
	}
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "t2"})
	stale := eventlogJob(t, sup, "daemon")
	appendHistory(t, sup, "job.stopped", "daemon", map[string]any{"state": "stopped"})
	if err := sup.recordExit(testContext(t), job, stale, "lost"); err != nil {
		t.Fatalf("recordExit(after job.stopped) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.exited"); got != 1 {
		t.Fatalf("job.exited count after racing job.stopped = %d, want still 1", got)
	}
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "t3"})
	staleUnmarked := eventlogJob(t, sup, "daemon")
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": "t3"})
	if err := sup.recordExit(testContext(t), job, staleUnmarked, "lost"); err != nil {
		t.Fatalf("recordExit(after marker) = %v, want nil", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.exited"); got != 1 {
		t.Fatalf("job.exited count after racing marker = %d, want still 1", got)
	}
	reconcileOnce(t, sup, "record interruption after skipped stale loss")
	assertInterruptedState(t, eventlogJob(t, sup, "daemon"))
}

// A run this supervisor started in its last tick, with no Reconcile after Start, is in the
// observed set and gets its stop marker.
func TestSupervisorStopMarksRunStartedInLastTick(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	reconcileOnce(t, sup, "first tick starts daemon")
	wantSince := eventlogJob(t, sup, "daemon").Since
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers of run started in last tick = %d, want 1", got)
	}
	assertMarkerCarriesSince(t, sup, "daemon", wantSince)
}

// A stop that ends the start wait when the shim exits on the cancel returns context.Canceled,
// and when the shim recorded job.started before the cancel its Since is observed and marked.
func TestSupervisorStopMarksRunStartedBeforeStartWaitEnded(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	emptyCtx, emptyPID := startStubShimUntilCancel(t, sup, "daemon")
	if err := sup.waitStarted(emptyCtx, "daemon", eventlog.Job{}, emptyPID, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitStarted(canceled, exited without start) = %v, want context.Canceled", err)
	}
	ctx, shimPID := startStubShimUntilCancel(t, sup, "daemon")
	const since = "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": shimPID + 10, "shim_pid": shimPID, "since": since})
	if err := sup.waitStarted(ctx, "daemon", eventlog.Job{}, shimPID, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitStarted(canceled) = %v, want context.Canceled", err)
	}
	sup.mu.Lock()
	gotSince := sup.observedSince["daemon"]
	sup.mu.Unlock()
	if gotSince != since {
		t.Fatalf("observedSince[daemon] = %q, want %q", gotSince, since)
	}
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers of run started before the wait ended = %d, want 1", got)
	}
	assertMarkerCarriesSince(t, sup, "daemon", since)
}

func startStubShimUntilCancel(t *testing.T, sup *Supervisor, name string) (context.Context, int) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(sleep) = %v, want nil", err)
	}
	pid := cmd.Process.Pid
	sup.recordSpawnedShim(name, pid)
	sup.shimWaiters.Add(1)
	cancel()
	sup.collectShim(cmd, name)
	return ctx, pid
}
