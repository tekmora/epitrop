//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const noJobAliveHold = 300 * time.Millisecond

// budgetJob is an always job that restarts on failure at most max times inside an hour.
func budgetJob(name string, limit int, command []string) config.JobConfig {
	job := testJob(name, command)
	job.Restart = config.JobRestartConfig{Policy: "on-failure", MaxRestarts: limit, WindowSeconds: 3600}
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

// captureSupervisorStderr gives the supervisor a buffer for its stderr. Only Supervise's goroutine
// writes it, and tests read it after Supervise returned.
func captureSupervisorStderr(sup *Supervisor) *bytes.Buffer {
	var buf bytes.Buffer
	sup.stderr = &buf
	return &buf
}

// superviseHeld runs Supervise, expects it to keep running for noJobAliveHold, cancels it
// and returns how it ended.
func superviseHeld(t *testing.T, sup *Supervisor, ready func() bool) error {
	t.Helper()
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx) }()
	if ready != nil && !waitUntil(ready) {
		cancel()
		t.Fatalf("Supervise did not reach the state the test waits for; it ended with %v", <-done)
	}
	select {
	case err := <-done:
		t.Fatalf("Supervise ended on its own with %v, want it to keep running", err)
	case <-time.After(noJobAliveHold):
	}
	cancel()
	return <-done
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
				"alpha dead (" + tc.exit + ", restarts 2/2 in 3600s), " +
				"beta dead (" + tc.exit + ", restarts 2/2 in 3600s); supervise exits 3\n"
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
	want := "epitrop supervisor: finding: no job alive: broken dead (exit 1, restarts 2/2 in 3600s); supervise exits 3\n"
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
