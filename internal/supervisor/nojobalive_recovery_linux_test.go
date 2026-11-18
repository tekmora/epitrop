//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

func TestRestartDueAt(t *testing.T) {
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
	tests := []struct {
		name, policy string
		window, max  int
		code         int
		times        []string
		want         time.Duration
		ok           bool
	}{
		{"exactly max", "on-failure", 2, 1, 1, []string{stamp(-time.Second)}, time.Second, true},
		{"sorted need two", "always", 2, 2, 0, []string{stamp(-time.Second / 2), stamp(-time.Second * 3 / 2), stamp(-time.Second)}, time.Second, true},
		{"one unreadable", "always", 2, 2, 1, []string{"bad", stamp(-time.Second)}, time.Second, true},
		{"window zero", "always", 0, 1, 1, []string{stamp(0)}, 0, false},
		{"never", "never", 2, 1, 1, []string{stamp(0)}, 0, false},
		{"success", "on-failure", 2, 1, 0, []string{stamp(0)}, 0, false},
		{"unreadable at max", "always", 2, 1, 1, []string{"bad", stamp(0)}, 0, false},
		{"zero max", "always", 2, 0, 1, []string{stamp(0)}, 0, false},
		{"cutoff excluded", "always", 2, 1, 1, []string{stamp(-2 * time.Second)}, 0, false},
		{"below max", "always", 2, 2, 1, []string{stamp(-time.Second)}, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("restartDueAt panicked: %v", r)
				}
			}()
			job := testJob("window", []string{"/bin/true"})
			job.Restart = config.JobRestartConfig{Policy: tc.policy, WindowSeconds: tc.window, MaxRestarts: tc.max}
			st := eventlog.Job{State: "dead", LastExitCode: intPtr(tc.code), RestartTimes: tc.times, Restarts: len(tc.times)}
			due, ok := restartDueAt(job, st, now)
			if ok != tc.ok || (ok && !due.Equal(now.Add(tc.want))) {
				t.Fatalf("restartDueAt = %s, %t; want offset %s, %t", due, ok, tc.want, tc.ok)
			}
			if ok && (shouldRestart(job, st, due.Add(-time.Nanosecond)) || !shouldRestart(job, st, due)) {
				t.Fatal("restart budget did not change at the predicted cutoff")
			}
		})
	}
}

func TestNoJobAliveCancellationWins(t *testing.T) {
	sup := testSupervisor(t, testJob("broken", []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, "broken", intPtr(1), "")
	state, err := sup.reconcile(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	down := sup.downJobsIfNoneAlive(state, sup.clk.Now())
	if len(down) != 1 {
		t.Fatalf("down = %+v, want one candidate before cancellation", down)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	reported := false
	if err = sup.reportNoJobAlive(ctx, down, &reported); !errors.Is(err, context.Canceled) {
		t.Fatalf("report = %v, want original cancellation", err)
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want no finding on cancellation", got)
	}
}

func TestNoJobAliveReplaysCandidate(t *testing.T) {
	sup := testSupervisor(t, testJob("changed", []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, "changed", intPtr(1), "")
	old, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if down := sup.downJobsIfNoneAlive(old, sup.clk.Now()); len(down) != 1 {
		t.Fatalf("initial judgment = %+v, want one down job", down)
	}
	if err = sup.appendJobEvent(testContext(t), "job.started", "changed", map[string]any{"state": "running"}); err != nil {
		t.Fatal(err)
	}
	reported := true
	if err = sup.checkNoJobAlive(testContext(t), old, &reported); err != nil {
		t.Fatalf("fresh judgment = %v, want nil after concurrent start", err)
	}
	if got := stderr.String(); got != "" || reported {
		t.Fatalf("stderr = %q, reported = %t, want no finding", got, reported)
	}
}

type stalledFindingWriter struct{ release <-chan struct{} }

func (w stalledFindingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

func TestFindingWriteDeadline(t *testing.T) {
	sup := testSupervisor(t, testJob("broken", []string{"/bin/true"}))
	if sup.writeTimeout != 5*time.Second {
		t.Fatalf("writeTimeout = %s, want 5s", sup.writeTimeout)
	}
	release := make(chan struct{})
	defer close(release)
	sup.stderr = stalledFindingWriter{release: release}
	sup.writeTimeout = 20 * time.Millisecond
	plantExit(t, sup, "broken", intPtr(1), "")
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(testContext(t)) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNoJobAlive) || !errors.Is(err, ErrFindingLineNotWritten) || !errors.Is(err, ErrWriteTimedOut) {
			t.Fatalf("Supervise = %v, want both sentinels and timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("finding write exceeded 1s, want configured 20ms timeout")
	}
}

type failedFindingWriter struct{ err error }

func (w failedFindingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestFindingWriteError(t *testing.T) {
	sup := testSupervisor(t, testJob("broken", []string{"/bin/true"}))
	failure := errors.New("disk full")
	sup.stderr = failedFindingWriter{err: failure}
	plantExit(t, sup, "broken", intPtr(1), "")
	err := sup.Supervise(testContext(t))
	if !errors.Is(err, failure) || !errors.Is(err, ErrNoJobAlive) || !errors.Is(err, ErrFindingLineNotWritten) {
		t.Fatalf("Supervise = %v, want output failure and both sentinels", err)
	}
}

func TestNoJobAliveFoldsNameToOneLine(t *testing.T) {
	job := testJob("split\n\tname", []string{"/bin/true"})
	sup := testSupervisor(t, job)
	stderr := captureSupervisorStderr(sup)
	plantExit(t, sup, job.Name, intPtr(1), "")
	if err := sup.Supervise(testContext(t)); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise = %v, want ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: split name dead (exit 1, restarts 0/0 in 0s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestNoJobAliveEpisodeResets(t *testing.T) {
	job := budgetJob("window", 1, []string{"/bin/false"})
	job.Restart.WindowSeconds = 2
	sup := testSupervisor(t, job)
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, job.Name, 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	checkFindingEpisode(t, sup, state, &reported)
	checkFindingEpisode(t, sup, state, &reported)
	if got := strings.Count(stderr.String(), "\n"); got != 1 {
		t.Fatalf("lines in first episode = %d, want 1", got)
	}
	alive := eventlog.State{Jobs: map[string]eventlog.Job{job.Name: {State: "running"}}}
	checkFindingEpisode(t, sup, alive, &reported)
	if reported {
		t.Fatal("episode did not end when a job became alive")
	}
	checkFindingEpisode(t, sup, state, &reported)
	if got := strings.Count(stderr.String(), "\n"); got != 2 {
		t.Fatalf("lines across two episodes = %d, want 2", got)
	}
}

func checkFindingEpisode(t *testing.T, sup *Supervisor, state eventlog.State, reported *bool) {
	t.Helper()
	if err := sup.checkNoJobAlive(testContext(t), state, reported); err != nil {
		t.Fatal(err)
	}
}

type synchronizedFindingBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedFindingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedFindingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestSuperviseWaitsForWindowAndRestarts(t *testing.T) {
	job := budgetJob("window", 1, []string{"/bin/false"})
	job.Restart.WindowSeconds = 2
	sup := testSupervisor(t, job)
	clk := clockwork.NewFakeClockAt(time.Now())
	sup.clk = clk
	var stderr synchronizedFindingBuffer
	sup.stderr = &stderr
	plantSpentBudget(t, sup, job.Name, 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	due, ok := restartDueAt(job, state.Jobs[job.Name], clk.Now())
	if !ok {
		t.Fatal("spent window has no recovery time")
	}
	err = superviseHeld(t, sup, nil, func() {
		want := fmt.Sprintf("epitrop supervisor: finding: no job alive: window dead (exit 1, restarts 1/1 in 2s, restart due %s); supervise keeps running\n", due.UTC().Format(time.RFC3339Nano))
		if got := stderr.String(); got != want {
			t.Fatalf("stderr = %q, want exactly one episode line %q", got, want)
		}
		if got := countEvents(t, sup.cfg.EventLog.Dir, "job.restarted"); got != 1 {
			t.Fatalf("restarts before window expires = %d, want 1", got)
		}
		started := countEvents(t, sup.cfg.EventLog.Dir, "job.started")
		clk.Advance(2 * time.Second)
		if !waitUntil(func() bool { return countEvents(t, sup.cfg.EventLog.Dir, "job.started") > started }) {
			t.Fatal("no further job.started after window expired")
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v, want canceled after recovery", err)
	}
}

func TestStoppedEventRemainsAlive(t *testing.T) {
	sup := testSupervisor(t)
	job := testJob("armed", []string{"/bin/true"})
	job.Schedule = "event"
	st := eventlog.Job{State: "stopped", LastEvent: "job.stopped"}
	if !sup.jobAlive(job, st, sup.clk.Now()) {
		t.Fatal("stopped event job is not alive, want armed for its next match")
	}
}

func TestShimErrorWriteDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	done := make(chan struct{})
	go func() {
		reportShimError(newLineSink(stalledFindingWriter{release: release}), 20*time.Millisecond, "broken", errors.New("wait failed"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shim error write exceeded 1s, want configured 20ms timeout")
	}
}

func TestNoJobAliveTerminalAfterRecoverableFinding(t *testing.T) {
	window := budgetJob("A", 1, []string{"/bin/false"})
	window.Restart.WindowSeconds = 3600
	spent := budgetJob("B", 1, []string{"/bin/false"})
	sup := testSupervisor(t, window, spent)
	now := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	sup.clk = clockwork.NewFakeClockAt(now)
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "A", 1, intPtr(1), "")
	plantSpentBudget(t, sup, "B", 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	checkFindingEpisode(t, sup, state, &reported)
	want := "epitrop supervisor: finding: no job alive: A dead (exit 1, restarts 1/1 in 3600s, restart due 2026-11-18T09:00:00Z), B dead (exit 1, restarts 1/1 in 0s); supervise keeps running\n"
	if got := stderr.String(); got != want || !reported {
		t.Fatalf("first finding = %q, reported = %t, want %q, true", got, reported, want)
	}
	checkFindingEpisode(t, sup, state, &reported)
	if got := stderr.String(); got != want {
		t.Fatalf("unchanged episode = %q, want no second line after %q", got, want)
	}
	plantStopped(t, sup, "A")
	if err = sup.checkNoJobAlive(testContext(t), state, &reported); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("terminal finding = %v, want ErrNoJobAlive", err)
	}
	want += "epitrop supervisor: finding: no job alive: B dead (exit 1, restarts 1/1 in 0s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("terminal episode = %q, want %q", got, want)
	}
}

type delayedFindingWriter struct{}

func (delayedFindingWriter) Write(p []byte) (int, error) {
	<-time.After(20 * time.Millisecond)
	return len(p), nil
}

func TestFindingWriteDefaultTimeout(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			if err := writeLine(newLineSink(delayedFindingWriter{}), "finding", timeout); err != nil {
				t.Fatalf("writeLine(timeout %s) = %v, want default deadline to allow a 20ms write", timeout, err)
			}
		})
	}
}

func TestUnrecordedAlwaysJobPreventsFinding(t *testing.T) {
	down := budgetJob("down", 0, []string{"/bin/false"})
	unrecorded := testJob("unrecorded", []string{"/bin/true"})
	sup := testSupervisor(t, down, unrecorded)
	state := eventlog.State{Jobs: map[string]eventlog.Job{
		down.Name: {State: "dead", LastEvent: "job.exited", LastExitCode: intPtr(1)},
	}}
	if got := sup.downJobsIfNoneAlive(state, sup.clk.Now()); got != nil {
		t.Fatalf("down jobs = %+v, want nil while an always job has no record", got)
	}
}

func TestForbiddenRestartHasNoDueFinding(t *testing.T) {
	for _, tc := range []struct {
		policy string
		code   int
	}{{"never", 1}, {"on-failure", 0}} {
		t.Run(tc.policy, func(t *testing.T) {
			job := budgetJob("forbidden", 1, []string{"/bin/false"})
			job.Restart.Policy, job.Restart.WindowSeconds = tc.policy, 3600
			sup := testSupervisor(t, job)
			sup.clk = clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC))
			stderr := captureSupervisorStderr(sup)
			plantSpentBudget(t, sup, job.Name, 1, intPtr(tc.code), "")
			state, err := sup.replayState(testContext(t))
			if err != nil {
				t.Fatal(err)
			}
			reported := false
			if err = sup.checkNoJobAlive(testContext(t), state, &reported); !errors.Is(err, ErrNoJobAlive) {
				t.Fatalf("finding = %v, want ErrNoJobAlive for a forbidden restart", err)
			}
			want := fmt.Sprintf("epitrop supervisor: finding: no job alive: forbidden dead (exit %d, restarts 1/1 in 3600s); supervise exits 3\n", tc.code)
			if got := stderr.String(); got != want {
				t.Fatalf("stderr = %q, want terminal line without restart due: %q", got, want)
			}
		})
	}
}

func TestNoJobAliveDuePreservesFraction(t *testing.T) {
	job := budgetJob("window", 1, []string{"/bin/false"})
	job.Restart.WindowSeconds = 2
	sup := testSupervisor(t, job)
	sup.clk = clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 8, 0, 0, 900000000, time.UTC))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, job.Name, 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	checkFindingEpisode(t, sup, state, &reported)
	if got := stderr.String(); !strings.Contains(got, "restart due 2026-11-18T08:00:02.9Z") {
		t.Fatalf("stderr = %q, want fractional due time 2026-11-18T08:00:02.9Z", got)
	}
}

type retryFindingWriter struct {
	calls int
	buf   bytes.Buffer
}

func (w *retryFindingWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		return 0, errors.New("disk full")
	}
	return w.buf.Write(p)
}

func TestRecoverableFindingRetriesFailedWrite(t *testing.T) {
	job := budgetJob("window", 1, []string{"/bin/false"})
	job.Restart.WindowSeconds = 2
	sup := testSupervisor(t, job)
	sup.clk = clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC))
	w := &retryFindingWriter{}
	sup.stderr = w
	plantSpentBudget(t, sup, job.Name, 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	if err = sup.checkNoJobAlive(testContext(t), state, &reported); err != nil || reported {
		t.Fatalf("failed recoverable write = %v, reported = %t, want nil, false", err, reported)
	}
	checkFindingEpisode(t, sup, state, &reported)
	if got := w.buf.String(); !reported || strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, "; supervise keeps running\n") {
		t.Fatalf("retry = %q, reported = %t, want one recoverable line, true", got, reported)
	}
}
