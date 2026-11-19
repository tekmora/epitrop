//go:build linux

package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

func TestR21StartWaitBounds(t *testing.T) {
	want := maxStatusPolls*startPollInterval + shimReplayTimeout + maxStartPolls*startPollInterval
	if got := time.Duration(startWaitPolls(true)) * startPollInterval; got < want {
		t.Fatalf("automatic wait = %v, want at least %v", got, want)
	}
	if got := startWaitPolls(false); got != maxStartPolls {
		t.Fatalf("manual polls = %d, want %d", got, maxStartPolls)
	}
}

func TestR21CanceledStopDoesNotSignal(t *testing.T) {
	job := testJob("daemon", longJobCommand(t))
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	old := jobRecordReadFile
	jobRecordReadFile = func(path string) ([]byte, error) { cancel(); return old(path) }
	t.Cleanup(func() { jobRecordReadFile = old })
	signals := withRecordingSignaller(t)
	err := sup.Stop(ctx, job.Name)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop = %v, want canceled", err)
	}
	if len(*signals) != 0 {
		t.Fatalf("signals = %v, want none", *signals)
	}
	if n := countEvents(t, sup.cfg.EventLog.Dir, "job.stopped"); n != 0 {
		t.Fatalf("stopped events = %d", n)
	}
	held, err := lockHeld(sup.lockPath(job.Name))
	if err != nil || !held {
		t.Fatalf("lock held = %v, %v", held, err)
	}
}

func TestR21MarkedCronConsumesDueTick(t *testing.T) {
	job := testJob("cron", longJobCommand(t))
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 1, 59, 0, 0, time.UTC))
	sup := testSupervisorWithClock(t, testRuntime(t), clk, job)
	reconcileOnce(t, sup, "arm")
	clk.Advance(90 * time.Second)
	seedMarkedRunningNonAlwaysJob(t, sup, job, false)
	reconcileOnce(t, sup, "adopt and consume tick")
	triggers := collectTriggeredEvents(t, sup, job.Name)
	if len(triggers) != 1 || triggers[0].Outcome != "skipped" || triggers[0].Reason != "running" || triggers[0].Due != "2026-11-18T02:00:00Z" {
		t.Fatalf("triggers = %+v, want skipped running 02:00", triggers)
	}
	assertR21Adopted(t, sup, job.Name)
}

func TestR21MarkedEventKeepsMatchQueued(t *testing.T) {
	job := testJob("event-job", longJobCommand(t))
	job.Schedule = "event"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	job.OnEvent = &config.JobEventTrigger{Type: "job.exited", TaskID: "other"}
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 1, 59, 0, 0, time.UTC))
	sup := testSupervisorWithClock(t, testRuntime(t), clk, job)
	reconcileOnce(t, sup, "arm")
	seedMarkedRunningNonAlwaysJob(t, sup, job, false)
	before := eventlogJob(t, sup, job.Name)
	appendHistory(t, sup, "job.exited", "other", map[string]any{"state": "dead"})
	reconcileOnce(t, sup, "adopt with queued match")
	after := eventlogJob(t, sup, job.Name)
	if after.ArmedSeq != before.ArmedSeq || after.LastTriggerSeq != before.LastTriggerSeq {
		t.Fatalf("cursor before=%+v after=%+v", before, after)
	}
	if triggers := collectTriggeredEvents(t, sup, job.Name); len(triggers) != 0 {
		t.Fatalf("triggers = %+v, want none", triggers)
	}
	assertR21Adopted(t, sup, job.Name)
}

func assertR21Adopted(t *testing.T, sup *Supervisor, name string) {
	t.Helper()
	if n := countEvents(t, sup.cfg.EventLog.Dir, "job.adopted"); n != 1 {
		t.Fatalf("adopted count = %d", n)
	}
	if n := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); n != 1 {
		t.Fatalf("started count = %d", n)
	}
	if got := eventlogJob(t, sup, name); got.SupervisorStopped {
		t.Fatalf("marker remains: %+v", got)
	}
}

func TestR21AdoptionAppendErrorReturned(t *testing.T) {
	job := testJob("cron", longJobCommand(t))
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 1, 59, 0, 0, time.UTC))
	sup := testSupervisorWithClock(t, testRuntime(t), clk, job)
	reconcileOnce(t, sup, "arm")
	seedMarkedRunningNonAlwaysJob(t, sup, job, false)
	failure := errors.New("adoption append failed")
	interceptAppendEvent(t, func(_ context.Context, ev eventlog.Event) error {
		if ev.Type == "job.adopted" {
			return failure
		}
		return nil
	})
	if err := sup.Reconcile(testContext(t)); !errors.Is(err, failure) {
		t.Fatalf("Reconcile = %v, want adoption failure", err)
	}
}
