//go:build linux

package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestLostOnceTerminalDoesNotMarkStop(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	broken := budgetJob("broken", 1, []string{"/bin/true"})
	broken.Restart.Policy = "never"
	sup := testSupervisor(t, once, broken)
	captureSupervisorStderr(sup)
	plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	if err := sup.Supervise(testContext(t)); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise = %v", err)
	}
	if st := eventlogJob(t, sup, "manual"); st.SupervisorStopped {
		t.Fatal("lost once received stop marker")
	}
	next, err := New(sup.cfg, Options{Clock: sup.clk, ShimCommand: sup.shimCommand, PollInterval: sup.pollInterval})
	if err != nil {
		t.Fatal(err)
	}
	stderr := captureSupervisorStderr(next)
	if err := next.Supervise(testContext(t)); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("next Supervise = %v", err)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.interrupted"); got != 0 {
		t.Fatalf("interrupted = %d, want 0", got)
	}
	if !strings.Contains(stderr.String(), "manual dead (exit lost") {
		t.Fatalf("finding = %q, want exit lost", stderr.String())
	}
}

func TestLostOnceBeforeNormalStop(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, testJob("alive", longJobCommand(t)))
	plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	ticks := 0
	sup.afterTick = func() {
		ticks++
		if ticks == 2 {
			cancel()
		}
	}
	if err := sup.Supervise(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v", err)
	}
	if st := eventlogJob(t, sup, "manual"); st.SupervisorStopped {
		t.Fatal("lost once received stop marker")
	}
	if st := eventlogJob(t, sup, "alive"); !st.SupervisorStopped {
		t.Fatal("alive job missing stop marker")
	}
}

func TestLostOnceHeldUntilStopGetsMarker(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once)
	plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	lock, err := holdLock(sup.lockPath("manual"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockForShim(lock)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	sup.afterTick = cancel
	if err = sup.Supervise(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v", err)
	}
	if st := eventlogJob(t, sup, "manual"); !st.SupervisorStopped {
		t.Fatal("held once missing marker")
	}
}

func TestLostOnceReleasedAtCancelGetsMarker(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once)
	plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	lock, err := holdLock(sup.lockPath("manual"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	sup.afterTick = func() { closeLockForShim(lock); cancel() }
	if err = sup.Supervise(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v", err)
	}
	if st := eventlogJob(t, sup, "manual"); !st.SupervisorStopped {
		t.Fatal("once released at stop missing marker")
	}
}

func TestLostOnceNewRunGetsMarker(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, testJob("alive", longJobCommand(t)))
	plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	ticks := 0
	sup.afterTick = func() {
		ticks++
		if ticks == 2 {
			plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:01Z")
		}
		if ticks == 3 {
			cancel()
		}
	}
	if err := sup.Supervise(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Supervise = %v", err)
	}
	if st := eventlogJob(t, sup, "manual"); !st.SupervisorStopped {
		t.Fatal("new once run missing marker")
	}
}
