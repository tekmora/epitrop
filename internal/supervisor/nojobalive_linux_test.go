//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

const noJobAliveHold = 300 * time.Millisecond

// budgetJob is an always job that restarts on failure at most max times without a window.
func budgetJob(name string, limit int, command []string) config.JobConfig {
	job := testJob(name, command)
	job.Restart = config.JobRestartConfig{Policy: "on-failure", MaxRestarts: limit, WindowSeconds: 0}
	return job
}

// plantExit records an exit of the job: its code, or its reason when code is nil.
func plantExit(t *testing.T, sup *Supervisor, name string, code *int, reason string) {
	t.Helper()
	payload := map[string]any{"state": string(StateDead)}
	if code != nil {
		payload["code"] = *code
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if err := sup.appendJobEvent(testContext(t), "job.exited", name, payload); err != nil {
		t.Fatalf("appendJobEvent(job.exited, %s) = %v, want nil", name, err)
	}
}

// plantSpentBudget records restarts restarts of the job, each after an exit, then a last
// exit: the log of a job that crashed until its budget was gone.
func plantSpentBudget(t *testing.T, sup *Supervisor, name string, restarts int, code *int, reason string) {
	t.Helper()
	for i := 0; i < restarts; i++ {
		plantExit(t, sup, name, code, reason)
		if err := sup.appendJobEvent(testContext(t), "job.restarted", name, map[string]any{"state": string(StateDead)}); err != nil {
			t.Fatalf("appendJobEvent(job.restarted, %s) = %v, want nil", name, err)
		}
	}
	plantExit(t, sup, name, code, reason)
}

func plantStopped(t *testing.T, sup *Supervisor, name string) {
	t.Helper()
	if err := sup.appendJobEvent(testContext(t), "job.stopped", name, map[string]any{"state": string(StateStopped)}); err != nil {
		t.Fatalf("appendJobEvent(job.stopped, %s) = %v, want nil", name, err)
	}
}

func intPtr(v int) *int { return &v }

// captureSupervisorStderr gives the supervisor a buffer for stderr. Tests read it after
// Supervise returns, once its successful finding write has finished.
func captureSupervisorStderr(sup *Supervisor) *bytes.Buffer {
	var buf bytes.Buffer
	sup.stderr = &buf
	return &buf
}

// superviseHeld runs Supervise, expects it to keep running for noJobAliveHold, cancels it
// and returns how it ended.
func superviseHeld(t *testing.T, sup *Supervisor, ready func() bool, beforeCancel ...func()) error {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := make(chan error, 1)
	firstTick := firstLivenessTick(sup)
	go func() { done <- sup.Supervise(ctx) }()
	awaitLivenessTick(t, firstTick, done)
	if ready != nil && !waitUntil(ready) {
		cancel()
		t.Fatalf("Supervise did not reach the state the test waits for; it ended with %v", <-done)
	}
	select {
	case err := <-done:
		t.Fatalf("Supervise ended on its own with %v, want it to keep running", err)
	case <-time.After(noJobAliveHold):
	}
	for i := 0; i < len(beforeCancel); i++ {
		beforeCancel[i]()
	}
	cancel()
	return <-done
}

func firstLivenessTick(sup *Supervisor) <-chan struct{} {
	firstTick := make(chan struct{}, 1)
	afterTick := sup.afterTick
	sup.afterTick = func() {
		if afterTick != nil {
			afterTick()
		}
		select {
		case firstTick <- struct{}{}:
		default:
		}
	}
	return firstTick
}

func awaitLivenessTick(t *testing.T, firstTick <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-firstTick:
	case err := <-done:
		t.Fatalf("Supervise ended before its first tick: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Supervise did not complete its first liveness check within 10s")
	}
}

func TestSuperviseEndsWhenNoJobIsAlive(t *testing.T) {
	tests := []struct {
		name   string
		code   *int
		reason string
		exit   string
	}{
		{name: "exit code 1", code: intPtr(1), exit: "exit 1"},
		{name: "lost", reason: "lost", exit: "exit lost"},
	}
	for i := 0; i < len(tests); i++ {
		tc := tests[i]
		t.Run(tc.name, func(t *testing.T) {
			sup := testSupervisor(t, budgetJob("alpha", 2, []string{"/bin/true"}), budgetJob("beta", 2, []string{"/bin/true"}))
			stderr := captureSupervisorStderr(sup)
			plantSpentBudget(t, sup, "alpha", 2, tc.code, tc.reason)
			plantSpentBudget(t, sup, "beta", 2, tc.code, tc.reason)
			err := sup.Supervise(testContext(t))
			if !errors.Is(err, ErrNoJobAlive) {
				t.Fatalf("Supervise() = %v, want an error wrapping ErrNoJobAlive", err)
			}
			want := "epitrop supervisor: finding: no job alive: " +
				"alpha dead (" + tc.exit + ", restarts 2/2 in 0s), " +
				"beta dead (" + tc.exit + ", restarts 2/2 in 0s); supervise exits 3\n"
			if got := stderr.String(); got != want {
				t.Fatalf("stderr = %q, want exactly the one finding line %q", got, want)
			}
			if !strings.Contains(err.Error(), "alpha dead") || !strings.Contains(err.Error(), "beta dead") {
				t.Fatalf("Supervise() = %q, want the down jobs in the error", err.Error())
			}
		})
	}
}

func TestSuperviseKeepsRunningWhileOneJobHasBudget(t *testing.T) {
	sup := testSupervisor(t, budgetJob("spent", 2, []string{"/bin/true"}), budgetJob("spare", 2, longJobCommand(t)))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "spent", 2, intPtr(1), "")
	plantSpentBudget(t, sup, "spare", 1, intPtr(1), "")
	err := superviseHeld(t, sup, func() bool { return statusOne(t, sup, "spare").State == StateRunning })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise() = %v, want context.Canceled: one job still has budget", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want no finding while a job is alive", got)
	}
}

func TestSuperviseNoJobAliveBoundaries(t *testing.T) {
	t.Run("every job stopped is no finding", boundaryEveryJobStopped)
	t.Run("one stopped and one down is a finding", boundaryOneStoppedOneDown)
	t.Run("a cron job beside a down always job is no finding", boundaryCronBesideDownJob)
	t.Run("a never job that failed alone is a finding", boundaryNeverJobFailedAlone)
}

func boundaryEveryJobStopped(t *testing.T) {
	first, second := budgetJob("one", 2, []string{"/bin/true"}), budgetJob("two", 2, []string{"/bin/true"})
	first.Restart.Policy, second.Restart.Policy = "always", "always"
	sup := testSupervisor(t, first, second)
	stderr := captureSupervisorStderr(sup)
	plantStopped(t, sup, "one")
	plantStopped(t, sup, "two")
	if err := superviseHeld(t, sup, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise(all stopped) = %v, want context.Canceled: a stop is intentional", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want no finding for stopped jobs", got)
	}
}

func boundaryOneStoppedOneDown(t *testing.T) {
	stopped := budgetJob("quiet", 2, []string{"/bin/true"})
	stopped.Restart.Policy = "always"
	sup := testSupervisor(t, stopped, budgetJob("broken", 2, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantStopped(t, sup, "quiet")
	plantSpentBudget(t, sup, "broken", 2, intPtr(1), "")
	err := sup.Supervise(testContext(t))
	if !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise() = %v, want an error wrapping ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: broken dead (exit 1, restarts 2/2 in 0s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q: the stopped job is not named", got, want)
	}
}

func boundaryCronBesideDownJob(t *testing.T) {
	cron := testJob("nightly", []string{"/bin/true"})
	cron.Schedule = "0 2 * * *"
	sup := testSupervisor(t, budgetJob("broken", 2, []string{"/bin/true"}), cron)
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 2, intPtr(1), "")
	if err := superviseHeld(t, sup, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise(cron beside a down job) = %v, want context.Canceled: the cron job is armed", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want no finding while a cron job is armed", got)
	}
}

func boundaryNeverJobFailedAlone(t *testing.T) {
	sup := testSupervisor(t, testJob("solo", []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, "solo", intPtr(1), "")
	err := sup.Supervise(testContext(t))
	if !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise() = %v, want an error wrapping ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: solo dead (exit 1, restarts 0/0 in 0s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestNoJobAliveFindingFoldsAReasonToOneLine(t *testing.T) {
	sup := testSupervisor(t, testJob("refused", []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, "refused", nil, "sandbox\nrefused:\n\tno bwrap")
	if err := sup.Supervise(testContext(t)); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise() = %v, want an error wrapping ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: refused dead (exit sandbox refused: no bwrap, restarts 0/0 in 0s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// A job whose budget allows a restart is alive on the state a tick replayed, whether or not
// the restart has started a process by the time the tick is judged. Nothing runs here, so
// only the restart decision can make the job alive.
func TestDownJobsIfNoneAliveCountsARestartableJobAsAlive(t *testing.T) {
	sup := testSupervisor(t, budgetJob("spent", 2, []string{"/bin/true"}), budgetJob("spare", 2, []string{"/bin/true"}))
	plantSpentBudget(t, sup, "spent", 2, intPtr(1), "")
	plantSpentBudget(t, sup, "spare", 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatalf("replayState() = %v, want nil", err)
	}
	if down := sup.downJobsIfNoneAlive(state, sup.clk.Now()); len(down) != 0 {
		t.Fatalf("downJobsIfNoneAlive(one job with budget) = %d down jobs, want none: spare restarts", len(down))
	}
	plantSpentBudget(t, sup, "spare", 1, intPtr(1), "")
	state, err = sup.replayState(testContext(t))
	if err != nil {
		t.Fatalf("replayState(spent) = %v, want nil", err)
	}
	if down := sup.downJobsIfNoneAlive(state, sup.clk.Now()); len(down) != 2 {
		t.Fatalf("downJobsIfNoneAlive(both spent) = %d down jobs, want 2", len(down))
	}
}

func TestNoJobAliveExcludesOnceAndStoppedAlways(t *testing.T) {
	for _, schedule := range []string{"once", "always"} {
		t.Run(schedule, func(t *testing.T) {
			quiet := budgetJob("quiet", 3, []string{"/bin/true"})
			quiet.Schedule = schedule
			if schedule == "once" {
				quiet.Restart.Policy = "never"
			}
			sup := testSupervisor(t, quiet, testJob("broken", []string{"/bin/true"}))
			stderr := captureSupervisorStderr(sup)
			if schedule == "always" {
				plantExit(t, sup, "quiet", intPtr(-1), "")
				plantStopped(t, sup, "quiet")
			}
			plantExit(t, sup, "broken", intPtr(1), "")
			state, err := sup.replayState(testContext(t))
			if err != nil {
				t.Fatal(err)
			}
			assertOnlyBrokenDown(t, sup, state, stderr)
		})
	}
}

func assertOnlyBrokenDown(t *testing.T, sup *Supervisor, state eventlog.State, stderr *bytes.Buffer) {
	t.Helper()
	down := sup.downJobsIfNoneAlive(state, sup.clk.Now())
	if len(down) != 1 || down[0].job.Name != "broken" {
		t.Fatalf("down = %+v, want only broken; quiet excluded", down)
	}
	if err := sup.Supervise(testContext(t)); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise = %v, want ErrNoJobAlive", err)
	}
	if got := stderr.String(); strings.Contains(got, "quiet") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr = %q, want one finding naming only broken", got)
	}
}

func TestStoppedOnFailureNeverRestarts(t *testing.T) {
	job := budgetJob("quiet", 3, longJobCommand(t))
	sup := testSupervisor(t, job)
	stderr := captureSupervisorStderr(sup)
	if shouldRestart(job, eventlog.Job{State: "stopped", LastExitCode: intPtr(-1)}, sup.clk.Now()) {
		t.Fatal("stopped on-failure job is restartable, want stop to stick")
	}
	if err := sup.Start(testContext(t), "quiet"); err != nil {
		t.Fatal(err)
	}
	if err := sup.Stop(testContext(t), "quiet"); err != nil {
		t.Fatal(err)
	}
	if err := superviseHeld(t, sup, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v, want canceled", err)
	}
	assertNoStartAfterStop(t, sup)
	if st := statusOne(t, sup, "quiet"); st.State != StateStopped {
		t.Fatalf("status = %+v, want stopped", st)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty", got)
	}
}

func assertNoStartAfterStop(t *testing.T, sup *Supervisor) {
	t.Helper()
	records := collectJobRecords(t, sup)
	stopped := false
	for i := 0; i < len(records); i++ {
		typ := records[i].Type
		if stopped && (typ == "job.started" || typ == "job.restarted") {
			t.Fatalf("%s after job.stopped, want no restart", typ)
		}
		stopped = stopped || typ == "job.stopped"
	}
	if !stopped {
		t.Fatal("no job.stopped record, want stop evidence")
	}
}

func TestRunningOnceBesideDownJobStaysAlive(t *testing.T) {
	once := testJob("manual", longJobCommand(t))
	once.Schedule = "once"
	sup := testSupervisor(t, testJob("broken", []string{"/bin/true"}), once)
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, "broken", intPtr(1), "")
	if err := sup.Start(testContext(t), "manual"); err != nil {
		t.Fatal(err)
	}
	if err := superviseHeld(t, sup, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v, want canceled", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty while once job runs", got)
	}
}

func TestStoppedCronRemainsArmed(t *testing.T) {
	cron := testJob("nightly", []string{"/bin/true"})
	cron.Schedule = "0 2 * * *"
	sup := testSupervisor(t, testJob("broken", []string{"/bin/true"}), cron)
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, "broken", intPtr(1), "")
	plantStopped(t, sup, "nightly")
	if err := superviseHeld(t, sup, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v, want canceled", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want empty while cron remains armed", got)
	}
}

func TestNoJobAliveOnceEnds(t *testing.T) {
	tests := []struct {
		name   string
		code   *int
		reason string
		stop   bool
		down   bool
	}{
		{"failed", intPtr(1), "", false, true},
		{"completed", intPtr(0), "", false, false},
		{"lost", nil, "lost", false, true},
		{"refused", nil, "refused", false, true},
		{"deadline", nil, "deadline", false, true},
		{"deadline exit 0", intPtr(0), "deadline", false, true},
		{"unknown", nil, "", false, true},
		{"stopped", intPtr(1), "", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			job := testJob("manual", []string{"/bin/true"})
			job.Schedule = "once"
			sup := testSupervisor(t, job)
			sup.clk = clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC))
			stderr := captureSupervisorStderr(sup)
			plantExit(t, sup, job.Name, tc.code, tc.reason)
			if tc.stop {
				plantStopped(t, sup, job.Name)
			}
			state, err := sup.replayState(testContext(t))
			if err != nil {
				t.Fatal(err)
			}
			var episode findingEpisode
			err = sup.checkNoJobAlive(testContext(t), state, &episode)
			assertOnceFinding(t, err, stderr.String(), episode.reported, tc.down)
			if tc.name == "deadline exit 0" && !strings.Contains(stderr.String(), "manual dead (exit 0,") {
				t.Fatalf("deadline exit 0 line = %q, want exit 0 detail", stderr.String())
			}
		})
	}
}

func assertOnceFinding(t *testing.T, err error, line string, reported, down bool) {
	t.Helper()
	if down {
		if !errors.Is(err, ErrNoJobAlive) || strings.Count(line, "\n") != 1 || !strings.HasSuffix(line, "; supervise exits 3\n") {
			t.Fatalf("once end = %v, stderr = %q, want terminal finding", err, line)
		}
		return
	}
	if err != nil || line != "" || reported {
		t.Fatalf("excluded once = %v, stderr = %q, reported = %t, want nil, empty, false", err, line, reported)
	}
}

func TestNoJobAliveUnrecordedOnceBesideSpentAlways(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, budgetJob("broken", 1, []string{"/bin/true"}))
	sup.clk = clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	var episode findingEpisode
	if err = sup.checkNoJobAlive(testContext(t), state, &episode); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("checkNoJobAlive = %v, want ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: broken dead (exit 1, restarts 1/1 in 0s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want only the spent always job: %q", got, want)
	}
}

func TestNoJobAliveOnceDeadlineZeroBesideLiveJob(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, testJob("live", []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, "manual", intPtr(0), "deadline")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	var episode findingEpisode
	if err = sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" || episode.reported {
		t.Fatalf("checkNoJobAlive = %v, stderr = %q, reported = %t; want no finding beside live job", err, stderr.String(), episode.reported)
	}
}
