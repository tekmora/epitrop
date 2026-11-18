//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// stopJob is the daemon of the supervisor-stop tests: policy, cap and backoff as given, no
// window of its own, so it takes the default one.
func stopJob(policy string, restarts int, backoff int, command []string) config.JobConfig {
	job := testJob("daemon", command)
	job.Restart = config.JobRestartConfig{Policy: policy, MaxRestarts: restarts, BackoffSeconds: backoff}
	return job
}

func stopSupervisor(t *testing.T, job config.JobConfig) (*Supervisor, *clockwork.FakeClock) {
	t.Helper()
	clk := clockwork.NewFakeClock()
	return testSupervisorWithClock(t, testRuntime(t), clk, job), clk
}

func reconcileOnce(t *testing.T, sup *Supervisor, what string) {
	t.Helper()
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(%s) = %v, want nil", what, err)
	}
}

func historyOne(t *testing.T, sup *Supervisor, name string) JobHistory {
	t.Helper()
	return historyOf(t, sup, name, time.Time{})[0]
}

// A job whose budget is spent at t is not restarted before its restart is older than the
// default window (one hour), and is restarted once it is.
func TestSpentBudgetIsFreedWhenTheOldestRestartAgesOut(t *testing.T) {
	sup, clk := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 1})
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead"})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 1})
	reconcileOnce(t, sup, "budget spent at t")
	clk.Advance(3599 * time.Second)
	reconcileOnce(t, sup, "t+3599s")
	if got := eventlogJob(t, sup, "daemon"); got.Restarts != 1 || got.LastEvent != "job.exited" {
		t.Fatalf("job at t+3599s = %+v, want 1 restart and no new one", got)
	}
	clk.Advance(2 * time.Second)
	reconcileOnce(t, sup, "t+3601s")
	if got := eventlogJob(t, sup, "daemon"); got.Restarts != 2 {
		t.Fatalf("job at t+3601s = %+v, want a restart (2 in the log, 1 in the window)", got)
	}
}

func TestShouldRestartResumesAfterASupervisorStop(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Minute).Format(time.RFC3339Nano)
	spent := eventlog.Job{State: string(StateDead), LastReason: reasonSupervisorStop, Restarts: 1, RestartTimes: []string{recent}}
	stopped := spent
	stopped.State = string(StateStopped)
	other := spent
	other.LastReason = "lost"
	tests := []struct {
		name string
		job  config.JobConfig
		st   eventlog.Job
		want bool
	}{
		{"on-failure resumes with the budget spent", withRestartPolicy(boundedJob("daemon", 1, 0, 0), "on-failure"), spent, true},
		{"always resumes with the budget spent", withRestartPolicy(boundedJob("daemon", 1, 0, 0), "always"), spent, true},
		{"never does not resume", withRestartPolicy(boundedJob("daemon", 1, 0, 0), "never"), spent, false},
		{"a job stopped on purpose is not resumed", withRestartPolicy(boundedJob("daemon", 1, 0, 0), "always"), stopped, false},
		{"a lost job with the budget spent is not restarted", withRestartPolicy(boundedJob("daemon", 1, 0, 0), "on-failure"), other, false},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			if got := shouldRestart(tests[i].job, tests[i].st, now); got != tests[i].want {
				t.Fatalf("shouldRestart(%s) = %v, want %v", tests[i].name, got, tests[i].want)
			}
		})
	}
}

func reduceMarkerStep(t *testing.T, st eventlog.State, typ string, payload string) eventlog.State {
	t.Helper()
	st, err := JobReducer(st, eventlog.Record{Seq: 1, Type: typ, TaskID: "daemon", Time: "2026-11-18T08:00:00Z", Payload: json.RawMessage(payload)})
	if err != nil {
		t.Fatalf("JobReducer(%s) = %v, want nil", typ, err)
	}
	return st
}

func TestJobReducerSupervisorStoppedMarker(t *testing.T) {
	st := eventlog.State{Jobs: map[string]eventlog.Job{}}
	st = reduceMarkerStep(t, st, "job.started", `{"state":"running","pid":4}`)
	st = reduceMarkerStep(t, st, eventSupervisorStopped, `{"supervisor_pid":9}`)
	if got := st.Jobs["daemon"]; !got.SupervisorStopped || got.State != "running" || got.LastEvent != "job.started" || got.Restarts != 0 {
		t.Fatalf("job after the marker = %+v, want the marker set and state, last event and restarts unchanged", got)
	}
	st = reduceMarkerStep(t, st, "job.adopted", `{"state":"running","pid":4}`)
	if got := st.Jobs["daemon"]; got.SupervisorStopped {
		t.Fatalf("job after job.adopted = %+v, want the marker cleared", got)
	}
}

func TestJobReducerSupervisorStopExitKeepsTheMarkerUntilTheNextStart(t *testing.T) {
	st := eventlog.State{Jobs: map[string]eventlog.Job{}}
	st = reduceMarkerStep(t, st, "job.started", `{"state":"running","pid":4}`)
	st = reduceMarkerStep(t, st, eventSupervisorStopped, `{"supervisor_pid":9}`)
	st = reduceMarkerStep(t, st, "job.exited", `{"state":"dead","reason":"supervisor-stop"}`)
	if got := st.Jobs["daemon"]; got.State != "dead" || got.LastReason != reasonSupervisorStop || got.LastExitCode != nil {
		t.Fatalf("job after the supervisor-stop exit = %+v, want dead, that reason, no code", got)
	}
	st = reduceMarkerStep(t, st, "job.started", `{"state":"running","pid":5}`)
	if got := st.Jobs["daemon"]; got.SupervisorStopped {
		t.Fatalf("job after job.started = %+v, want the marker cleared", got)
	}
}

func TestJobReducerResumeIsNotACountedRestart(t *testing.T) {
	st := eventlog.State{Jobs: map[string]eventlog.Job{}}
	apply := func(payload string) {
		t.Helper()
		var err error
		st, err = JobReducer(st, eventlog.Record{Seq: 1, Type: "job.restarted", TaskID: "daemon", Time: "2026-11-18T08:00:00Z", Payload: json.RawMessage(payload)})
		if err != nil {
			t.Fatalf("JobReducer(job.restarted %q) = %v, want nil", payload, err)
		}
	}
	steps := []struct {
		payload       string
		restarts, len int
	}{
		{``, 1, 1},         // no payload: counted, as before the reason existed
		{`not json`, 2, 2}, // unreadable payload: counted, never a fold error
		{`{"state":"dead","reason":"supervisor-stop"}`, 2, 2}, // a resume: not counted
		{`{"state":"dead","reason":"something else"}`, 3, 3},  // any other reason: counted
		{`{"state":"dead"}`, 4, 4},                            // no reason: counted
		{`{"state":"dead","reason":"supervisor-stop"}`, 4, 4}, // a second resume: still not counted
	}
	for i := 0; i < len(steps); i++ {
		apply(steps[i].payload)
		got := st.Jobs["daemon"]
		if got.Restarts != steps[i].restarts || len(got.RestartTimes) != steps[i].len || got.LastEvent != "job.restarted" {
			t.Fatalf("after job.restarted %q: restarts %d, times %d, last event %q; want %d, %d, job.restarted", steps[i].payload, got.Restarts, len(got.RestartTimes), got.LastEvent, steps[i].restarts, steps[i].len)
		}
	}
}

// Supervise with a running job ends on its context: one marker for that job, none for a job
// that is dead or stopped.
func TestSupervisorStopMarksOnlyRunningJobs(t *testing.T) {
	daemon := stopJob("always", 1, 0, longJobCommand(t))
	gone := testJob("gone", []string{"true"})
	parked := withRestartPolicy(boundedJob("parked", 1, 0, 0), "always")
	sup, _ := stopSupervisor(t, daemon)
	sup.cfg.Jobs = append(sup.cfg.Jobs, gone, parked)
	appendHistory(t, sup, "job.exited", "gone", map[string]any{"state": "dead", "code": 1})
	appendHistory(t, sup, "job.stopped", "parked", map[string]any{"state": "stopped"})
	ctx, cancel := context.WithTimeout(t.Context(), testWaitBound)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx) }()
	if !waitUntil(func() bool { return eventlogJob(t, sup, "daemon").State == string(StateRunning) }) {
		t.Fatalf("daemon was not recorded running within %s", testWaitBound)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Supervise() = %v, want an error wrapping context.Canceled", err)
		}
	case <-time.After(testWaitBound):
		t.Fatalf("Supervise did not return within %s of its context ending", testWaitBound)
	}
	for name, want := range map[string]int{"daemon": 1, "gone": 0, "parked": 0} {
		if got := historyOne(t, sup, name).SupervisorStops; got != want {
			t.Fatalf("supervisor stop markers of %s = %d, want %d", name, got, want)
		}
	}
	if got := eventlogJob(t, sup, "daemon"); !got.SupervisorStopped || got.State != string(StateRunning) {
		t.Fatalf("daemon after the stop = %+v, want the marker set and the state still running", got)
	}
}

// The next reconcile with the lock free records a supervisor-stop exit, then resumes the job
// outside the budget: no counted restart, no backoff, even with the cap already reached.
func TestSupervisorStopExitResumesOutsideTheBudget(t *testing.T) {
	tests := []struct {
		policy  string
		resumes bool
	}{{"on-failure", true}, {"always", true}, {"never", false}}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].policy, func(t *testing.T) {
			sup, _ := stopSupervisor(t, stopJob(tests[i].policy, 1, config.MaxRestartBackoff, longJobCommand(t)))
			running := map[string]any{"state": "running"}
			appendHistory(t, sup, "job.started", "daemon", running)
			appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead"})
			appendHistory(t, sup, "job.started", "daemon", running)
			appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1})
			reconcileOnce(t, sup, "record the exit")
			got := eventlogJob(t, sup, "daemon")
			if got.State != string(StateDead) || got.LastReason != reasonSupervisorStop || got.LastExitCode != nil {
				t.Fatalf("job after the first reconcile = %+v, want dead, reason supervisor-stop, no code", got)
			}
			reconcileOnce(t, sup, "resume")
			assertResume(t, sup, tests[i].resumes)
		})
	}
}

func assertResume(t *testing.T, sup *Supervisor, resumes bool) {
	t.Helper()
	if !resumes {
		assertNotResumed(t, sup)
		return
	}
	if !waitUntil(func() bool { return eventlogJob(t, sup, "daemon").State == string(StateRunning) }) {
		t.Fatalf("job was not running again within %s", testWaitBound)
	}
	got := eventlogJob(t, sup, "daemon")
	if got.Restarts != 1 || len(got.RestartTimes) != 1 || got.SupervisorStopped {
		t.Fatalf("job after the resume = %+v, want 1 counted restart as before, no new one, the marker cleared", got)
	}
	if hist := historyOne(t, sup, "daemon"); hist.Resumed != 1 || hist.Restarted != 1 || hist.SupervisorStops != 1 {
		t.Fatalf("history after the resume = %+v, want 1 resume apart from 1 restart, and 1 supervisor stop", hist)
	}
}

func assertNotResumed(t *testing.T, sup *Supervisor) {
	t.Helper()
	got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon")
	if got.LastEvent != "job.exited" || hist.Resumed != 0 {
		t.Fatalf("job under policy never = %+v, history %+v, want it left dead and not resumed", got, hist)
	}
}

// A job the operator stopped between the supervisor-stop exit and the resume stays stopped.
func TestSupervisorStopExitIsNotResumedAfterAStopOnPurpose(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running"})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1})
	reconcileOnce(t, sup, "record the exit")
	appendHistory(t, sup, "job.stopped", "daemon", map[string]any{"state": "stopped"})
	reconcileOnce(t, sup, "after the stop")
	if got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon"); got.LastEvent != "job.stopped" || hist.Resumed != 0 {
		t.Fatalf("job after a stop on purpose = %+v, history %+v, want it left stopped", got, hist)
	}
}

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
			appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running"})
			for n := 0; n < tests[i].seeded; n++ {
				appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead"})
			}
			appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running"})
			reconcileOnce(t, sup, "record the exit")
			if got := eventlogJob(t, sup, "daemon"); got.LastReason != "lost" || got.State != string(StateDead) {
				t.Fatalf("job without a marker = %+v, want dead with reason lost", got)
			}
			reconcileOnce(t, sup, "restart")
			got := eventlogJob(t, sup, "daemon")
			if got.Restarts != 1 || historyOne(t, sup, "daemon").Resumed != 0 {
				t.Fatalf("job after a lost job = %+v, want exactly 1 counted restart and no resume", got)
			}
			if tests[i].restarted != (got.LastEvent != "job.exited") {
				t.Fatalf("job after a lost job = %+v, want restarted = %v", got, tests[i].restarted)
			}
		})
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
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1})
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

// A marker that cannot be written is part of Supervise's error, beside the reason it ended.
func TestSuperviseReturnsAMarkerWriteFailureWithItsOwnError(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	errDisk := errors.New("marker disk failure")
	realAppend := appendEvent
	appendEvent = func(w *eventlog.Writer, c context.Context, ev eventlog.Event) (eventlog.Record, error) {
		if ev.Type == eventSupervisorStopped {
			return eventlog.Record{}, errDisk
		}
		return realAppend(w, c, ev)
	}
	t.Cleanup(func() { appendEvent = realAppend })
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	err := sup.Supervise(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errDisk) {
		t.Fatalf("Supervise() = %v, want an error wrapping both the deadline and the marker failure", err)
	}
	if !strings.Contains(err.Error(), eventSupervisorStopped) {
		t.Fatalf("Supervise() error = %q, want it to name %s", err.Error(), eventSupervisorStopped)
	}
}

func TestHistoryCountsResumesApartFromRestarts(t *testing.T) {
	sup, _ := historySupervisor(t)
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead"})
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"restarts": 1})
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead", "reason": reasonSupervisorStop})
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead", "reason": reasonSupervisorStop})
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead", "reason": "other"})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1})
	appendHistory(t, sup, eventSupervisorStopped, "other", map[string]any{"supervisor_pid": 1})
	got := historyOne(t, sup, "daemon")
	if got.Restarted != 3 || got.Resumed != 2 || got.SupervisorStops != 1 {
		t.Fatalf("History(daemon) = %+v, want 3 restarted, 2 resumed, 1 supervisor stop", got)
	}
	if other := historyOne(t, sup, "other"); other.SupervisorStops != 1 || other.Restarted != 0 || other.Resumed != 0 {
		t.Fatalf("History(other) = %+v, want only its own supervisor stop", other)
	}
}
