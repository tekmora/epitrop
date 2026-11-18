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
	spent := eventlog.Job{State: string(StateDead), LastEvent: eventInterrupted, LastReason: reasonSupervisorStop, Restarts: 1, RestartTimes: []string{recent}}
	stopped := spent
	stopped.State = string(StateStopped)
	stoppedBudgetFree := eventlog.Job{State: string(StateStopped), LastEvent: "job.stopped", LastReason: reasonSupervisorStop}
	exited := spent
	exited.LastEvent = "job.exited"
	other := exited
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
		{"on-failure with free budget stopped after an interruption is not restarted", withRestartPolicy(boundedJob("daemon", 3, 0, 0), "on-failure"), stoppedBudgetFree, false},
		{"job.exited does not resume with the budget spent", withRestartPolicy(boundedJob("daemon", 1, 0, 0), "on-failure"), exited, false},
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
	st = reduceMarkerStep(t, st, "job.started", `{"state":"running","pid":4,"since":"t1"}`)
	st = reduceMarkerStep(t, st, eventSupervisorStopped, `{"supervisor_pid":9,"since":"t1"}`)
	if got := st.Jobs["daemon"]; !got.SupervisorStopped || got.State != "running" || got.LastEvent != "job.started" || got.Restarts != 0 {
		t.Fatalf("job after the marker = %+v, want the marker set and state, last event and restarts unchanged", got)
	}
	st = reduceMarkerStep(t, st, "job.adopted", `{"state":"running","pid":4,"since":"t1"}`)
	if got := st.Jobs["daemon"]; got.SupervisorStopped {
		t.Fatalf("job after job.adopted = %+v, want the marker cleared", got)
	}
	st = reduceMarkerStep(t, st, "job.started", `{"state":"running","pid":5,"since":"t2"}`)
	st = reduceMarkerStep(t, st, eventSupervisorStopped, `{"supervisor_pid":9,"since":"t1"}`)
	if got := st.Jobs["daemon"]; got.SupervisorStopped {
		t.Fatalf("job after a stale marker from run t1 on run t2 = %+v, want SupervisorStopped false", got)
	}
	st = reduceMarkerStep(t, st, "job.exited", `{"state":"dead","code":0}`)
	st = reduceMarkerStep(t, st, eventSupervisorStopped, `{"supervisor_pid":9,"since":"t2"}`)
	if got := st.Jobs["daemon"]; got.SupervisorStopped {
		t.Fatalf("job after a marker on a dead job = %+v, want SupervisorStopped false", got)
	}
	if _, err := JobReducer(st, eventlog.Record{Seq: 1, Type: eventSupervisorStopped, TaskID: "daemon", Payload: json.RawMessage(`{`)}); err == nil {
		t.Fatal("JobReducer(bad marker payload) = nil, want error")
	}
}

func assertInterruptedState(t *testing.T, got eventlog.Job) {
	t.Helper()
	if got.State != string(StateDead) || got.LastEvent != eventInterrupted || got.LastReason != reasonSupervisorStop {
		t.Fatalf("job after job.interrupted = %+v, want dead, last event job.interrupted, reason supervisor-stop", got)
	}
	if got.LastExitCode != nil || got.SupervisorStopped {
		t.Fatalf("job after job.interrupted = %+v, want no exit code and marker cleared", got)
	}
}

func TestJobReducerInterruptedClearsTheMarkerAndKeepsRestarts(t *testing.T) {
	st := eventlog.State{Jobs: map[string]eventlog.Job{}}
	st = reduceMarkerStep(t, st, "job.started", `{"state":"running","pid":4,"since":"t1"}`)
	st = reduceMarkerStep(t, st, "job.exited", `{"state":"dead","code":1}`)
	st = reduceMarkerStep(t, st, "job.restarted", `{"state":"dead"}`)
	st = reduceMarkerStep(t, st, "job.started", `{"state":"running","pid":5,"since":"t2"}`)
	st = reduceMarkerStep(t, st, eventSupervisorStopped, `{"supervisor_pid":9,"since":"t2"}`)
	st = reduceMarkerStep(t, st, eventInterrupted, `{"state":"dead","reason":"supervisor-stop"}`)
	got := st.Jobs["daemon"]
	assertInterruptedState(t, got)
	if got.Restarts != 1 || len(got.RestartTimes) != 1 {
		t.Fatalf("restarts after job.interrupted = %d (%v), want 1 and 1 time unchanged", got.Restarts, got.RestartTimes)
	}
}

func TestJobReducerCountsEveryRestart(t *testing.T) {
	st := eventlog.State{Jobs: map[string]eventlog.Job{}}
	payloads := []string{``, `{"state":"dead"}`, `{"state":"dead","reason":"supervisor-stop"}`}
	for i := 0; i < len(payloads); i++ {
		st = reduceMarkerStep(t, st, "job.restarted", payloads[i])
		got := st.Jobs["daemon"]
		want := i + 1
		if got.Restarts != want || len(got.RestartTimes) != want || got.LastEvent != "job.restarted" {
			t.Fatalf("after job.restarted %q: restarts %d, times %d, last event %q; want %d, %d, job.restarted", payloads[i], got.Restarts, len(got.RestartTimes), got.LastEvent, want, want)
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
	got := eventlogJob(t, sup, "daemon")
	if !got.SupervisorStopped || got.State != string(StateRunning) || got.Since == "" {
		t.Fatalf("daemon after the stop = %+v, want the marker set, since set, and the state still running", got)
	}
	assertMarkerCarriesSince(t, sup, "daemon", got.Since)
}

func assertMarkerCarriesSince(t *testing.T, sup *Supervisor, name string, wantSince string) {
	t.Helper()
	recs := collectJobRecords(t, sup)
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Type != eventSupervisorStopped || recs[i].TaskID != name {
			continue
		}
		var payload struct {
			SupervisorPID int    `json:"supervisor_pid"`
			Since         string `json:"since"`
		}
		if err := json.Unmarshal(recs[i].Payload, &payload); err != nil {
			t.Fatalf("unmarshal marker payload = %v, want nil", err)
		}
		if payload.Since != wantSince || payload.SupervisorPID == 0 {
			t.Fatalf("marker payload = %+v, want since %q and non-zero supervisor_pid", payload, wantSince)
		}
		return
	}
	t.Fatalf("no %s record found for %s", eventSupervisorStopped, name)
}

// A job recorded running whose lock is already free when the supervisor stops (as after the
// unit's SIGTERM reaches the shims at the same time) still gets a stop marker carrying its
// since; the next reconcile records job.interrupted with reason supervisor-stop and no
// job.exited, and the following reconcile resumes it with Restarts unchanged.
func TestSupervisorStopMarksARecordedRunningJobWhoseLockIsFree(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	const since = "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": since})
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 1 {
		t.Fatalf("supervisor stop markers of an unlocked running job = %d, want 1", got)
	}
	assertMarkerCarriesSince(t, sup, "daemon", since)
	reconcileOnce(t, sup, "record the interruption")
	assertInterruptedState(t, eventlogJob(t, sup, "daemon"))
	if exits := countEvents(t, sup.cfg.EventLog.Dir, "job.exited"); exits != 0 {
		t.Fatalf("job.exited count = %d, want 0", exits)
	}
	reconcileOnce(t, sup, "resume the interrupted job")
	if got := eventlogJob(t, sup, "daemon"); got.Restarts != 0 || got.State != string(StateRunning) {
		t.Fatalf("job after resuming the interrupted job = %+v, want 0 counted restarts and running", got)
	}
}

// A job whose exit the shim recorded before the supervisor stopped is dead, not running:
// it gets no stop marker, and the next reconcile restarts it with Restarts +1 and a
// job.restarted record.
func TestSupervisorStopLeavesARecordedCrashCharged(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("on-failure", 2, 0, longJobCommand(t)))
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "2026-11-18T08:00:00Z"})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 1})
	if err := sup.markSupervisorStopped(); err != nil {
		t.Fatalf("markSupervisorStopped() = %v, want nil", err)
	}
	if got := historyOne(t, sup, "daemon").SupervisorStops; got != 0 {
		t.Fatalf("supervisor stop markers of a crashed job = %d, want 0", got)
	}
	reconcileOnce(t, sup, "restart the crashed job")
	got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon")
	if got.Restarts != 1 || got.State != string(StateRunning) || hist.Restarted != 1 {
		t.Fatalf("job after restarting the crashed job = %+v, history %+v, want 1 counted restart, 1 job.restarted and running", got, hist)
	}
}

// The next reconcile with the lock free records job.interrupted, then resumes the job with a
// plain start outside the budget: no job.exited, no job.restarted, no backoff, even with the
// cap already reached.
func TestSupervisorStopExitResumesOutsideTheBudget(t *testing.T) {
	tests := []struct {
		policy  string
		resumes bool
	}{{"on-failure", true}, {"always", true}, {"never", false}}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].policy, func(t *testing.T) {
			sup, _ := stopSupervisor(t, stopJob(tests[i].policy, 1, config.MaxRestartBackoff, longJobCommand(t)))
			running := map[string]any{"state": "running", "since": "2026-11-18T08:00:00Z"}
			appendHistory(t, sup, "job.started", "daemon", running)
			appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead"})
			appendHistory(t, sup, "job.started", "daemon", running)
			appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": "2026-11-18T08:00:00Z"})
			reconcileOnce(t, sup, "record the interruption")
			assertInterruptedState(t, eventlogJob(t, sup, "daemon"))
			reconcileOnce(t, sup, "resume")
			assertResume(t, sup, tests[i].resumes)
		})
	}
}

func recordsAfterSeq(t *testing.T, sup *Supervisor, after uint64) []eventlog.Record {
	t.Helper()
	recs, err := eventlog.ReplayAfter(testContext(t), sup.cfg.EventLog.Dir, sup.PublicKey(), sup.eventLogLimits(), after)
	if err != nil {
		t.Fatalf("ReplayAfter(%d) = %v, want nil", after, err)
	}
	return recs
}

func assertInterruptedRecord(t *testing.T, rec eventlog.Record) {
	t.Helper()
	if rec.Type != eventInterrupted || rec.TaskID != "daemon" {
		t.Fatalf("first record after the marker = %+v, want job.interrupted for daemon", rec)
	}
	var payload struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatalf("unmarshal job.interrupted payload = %v, want nil", err)
	}
	if payload.State != string(StateDead) || payload.Reason != reasonSupervisorStop {
		t.Fatalf("job.interrupted payload = %+v, want state dead and reason supervisor-stop", payload)
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
	after := recordsAfterSeq(t, sup, 4)
	if len(after) != 2 || after[1].Type != "job.started" {
		t.Fatalf("records after the marker = %+v, want [job.interrupted, job.started] with no job.exited and no job.restarted", after)
	}
	assertInterruptedRecord(t, after[0])
	got := eventlogJob(t, sup, "daemon")
	if got.Restarts != 1 || len(got.RestartTimes) != 1 || got.SupervisorStopped {
		t.Fatalf("job after the resume = %+v, want 1 counted restart as before, no new one, the marker cleared", got)
	}
	hist := historyOne(t, sup, "daemon")
	want := JobHistory{Name: "daemon", Started: 3, Restarted: 1, Interrupted: 1, SupervisorStops: 1, First: hist.First, Last: hist.Last}
	if hist != want {
		t.Fatalf("history after the resume = %+v, want %+v", hist, want)
	}
}

func assertNotResumed(t *testing.T, sup *Supervisor) {
	t.Helper()
	after := recordsAfterSeq(t, sup, 4)
	if len(after) != 1 {
		t.Fatalf("records after the marker under policy never = %+v, want only [job.interrupted]", after)
	}
	assertInterruptedRecord(t, after[0])
	got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon")
	want := JobHistory{Name: "daemon", Started: 2, Restarted: 1, Interrupted: 1, SupervisorStops: 1, First: hist.First, Last: hist.Last}
	if got.LastEvent != eventInterrupted || hist != want {
		t.Fatalf("job under policy never = %+v, history %+v, want it left dead after job.interrupted (%+v)", got, hist, want)
	}
}

// A job the operator stopped between the interruption and the resume stays stopped, under
// both always and on-failure with budget free.
func TestSupervisorStopExitIsNotResumedAfterAStopOnPurpose(t *testing.T) {
	for _, policy := range []string{"always", "on-failure"} {
		t.Run(policy, func(t *testing.T) {
			sup, _ := stopSupervisor(t, stopJob(policy, 3, 0, longJobCommand(t)))
			appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "since": "2026-11-18T08:00:00Z"})
			appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": "2026-11-18T08:00:00Z"})
			reconcileOnce(t, sup, "record the interruption")
			appendHistory(t, sup, "job.stopped", "daemon", map[string]any{"state": "stopped"})
			reconcileOnce(t, sup, "after the stop")
			got, hist := eventlogJob(t, sup, "daemon"), historyOne(t, sup, "daemon")
			want := JobHistory{Name: "daemon", Started: 1, Interrupted: 1, Stopped: 1, SupervisorStops: 1, First: hist.First, Last: hist.Last}
			if got.LastEvent != "job.stopped" || hist != want {
				t.Fatalf("job (%s) after a stop on purpose = %+v, history %+v, want it left stopped (%+v)", policy, got, hist, want)
			}
		})
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
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	err := sup.Supervise(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errDisk) || !errors.Is(err, ErrStopMarkersNotWritten) {
		t.Fatalf("Supervise() = %v, want an error wrapping the deadline, ErrStopMarkersNotWritten and the marker failure", err)
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
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
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
}

func TestHistoryCountsInterruptionsApartFromExitsAndRestarts(t *testing.T) {
	sup, _ := historySupervisor(t)
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 0})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 1})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "reason": "lost"})
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"state": "dead"})
	appendHistory(t, sup, eventInterrupted, "daemon", map[string]any{"state": "dead", "reason": reasonSupervisorStop})
	appendHistory(t, sup, eventInterrupted, "daemon", map[string]any{"state": "dead", "reason": reasonSupervisorStop})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1})
	appendHistory(t, sup, eventSupervisorStopped, "other", map[string]any{"supervisor_pid": 1})
	got := historyOne(t, sup, "daemon")
	wantDaemon := JobHistory{Name: "daemon", Exited: 3, Unclean: 2, Restarted: 1, Interrupted: 2, SupervisorStops: 1, First: got.First, Last: got.Last}
	if got != wantDaemon {
		t.Fatalf("History(daemon) = %+v, want %+v", got, wantDaemon)
	}
	other := historyOne(t, sup, "other")
	wantOther := JobHistory{Name: "other", SupervisorStops: 1, First: other.First, Last: other.Last}
	if other != wantOther {
		t.Fatalf("History(other) = %+v, want %+v", other, wantOther)
	}
}
