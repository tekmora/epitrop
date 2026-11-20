//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/eventlog"
)

func plantRunningOnce(t *testing.T, sup *Supervisor, name, since string) eventlog.State {
	t.Helper()
	cfg := shimConfig{Name: name, RecordPath: sup.recordPath(name), LogPath: sup.cfg.Jobs[0].LogPath}
	if err := writeRecord(cfg, 4242, since); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"state": string(StateRunning), "pid": 4242, "shim_pid": 4241, "since": since, "sandbox": "off"}
	if err := sup.appendJobEvent(testContext(t), "job.started", name, payload); err != nil {
		t.Fatal(err)
	}
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestNoJobAliveLostOnce(t *testing.T) {
	t.Run("lock free once and spent always is terminal finding", testLostOnceFinding)
	t.Run("held lock once stays alive", testHeldLockOnceAlive)
	t.Run("new start before confirm replay cancels finding", testLostOnceNewStartBeforeConfirm)
	t.Run("exit recorded before confirm", testLostOnceExitBeforeConfirm)
	t.Run("other job starts before confirm", testOtherJobStartsBeforeConfirm)
	t.Run("changed exit preserves reported episode", testChangedExitPreservesEpisode)
	t.Run("confirm replay names its own down jobs", testConfirmReplayNamesItsDownJobs)
}

func testLostOnceFinding(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, terminalBudgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	sup.afterTick = cancel
	if err := sup.Supervise(ctx); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise = %v, want ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: manual dead (exit lost, restarts 0/0 in 3600s), broken dead (exit 1, restarts 1/1 in 3600s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func testHeldLockOnceAlive(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, terminalBudgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	lock, err := holdLock(sup.lockPath("manual"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockForShim(lock)
	episode := findingEpisode{reported: true}
	if err = sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" || episode.reported {
		t.Fatalf("checkNoJobAlive = %v, stderr = %q, reported = %t; want alive once job", err, stderr.String(), episode.reported)
	}
}

func testLostOnceNewStartBeforeConfirm(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, terminalBudgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	sup.beforeConfirmReplay = func() {
		plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:01Z")
	}
	episode := findingEpisode{reported: true, pending: make(chan error)}
	if err := sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" || !episode.reported || episode.pending == nil {
		t.Fatalf("checkNoJobAlive = %v, stderr = %q, episode = %+v; want no finding and preserved episode", err, stderr.String(), episode)
	}
}

func testLostOnceExitBeforeConfirm(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, terminalBudgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	sup.beforeConfirmReplay = func() {
		plantExit(t, sup, "manual", intPtr(1), "")
	}
	episode := findingEpisode{reported: true, pending: make(chan error)}
	if err := sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" || !episode.reported || episode.pending == nil {
		t.Fatalf("tick 1 (exit before confirm): err = %v, stderr = %q, episode = %+v; want nil, empty, preserved", err, stderr.String(), episode)
	}
	sup.beforeConfirmReplay = nil
	fresh, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.checkNoJobAlive(testContext(t), fresh, &episode); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("tick 2 (after exit): checkNoJobAlive = %v, want ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: manual dead (exit 1, restarts 0/0 in 3600s), broken dead (exit 1, restarts 1/1 in 3600s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("tick 2 stderr = %q, want %q", got, want)
	}
}

func testOtherJobStartsBeforeConfirm(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, terminalBudgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	sup.beforeConfirmReplay = func() {
		if err := sup.appendJobEvent(testContext(t), "job.started", "broken", map[string]any{"state": "running"}); err != nil {
			t.Fatal(err)
		}
	}
	episode := findingEpisode{reported: true}
	err := sup.checkNoJobAlive(testContext(t), state, &episode)
	if err != nil || stderr.String() != "" || episode.reported {
		t.Fatalf("finding = %v, stderr = %q, reported = %t; want no finding and reset", err, stderr.String(), episode.reported)
	}
}

func testChangedExitPreservesEpisode(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	window := budgetJob("window", 1, []string{"/bin/false"})
	window.Restart.WindowSeconds = 3600
	sup := testSupervisor(t, once, window)
	sup.clk = clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "window", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	var episode findingEpisode
	checkFindingEpisode(t, sup, state, &episode)
	sup.beforeConfirmReplay = func() { plantExit(t, sup, "manual", intPtr(1), "") }
	checkFindingEpisode(t, sup, state, &episode)
	sup.beforeConfirmReplay = nil
	fresh, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	checkFindingEpisode(t, sup, fresh, &episode)
	if got := strings.Count(stderr.String(), "\n"); got != 1 {
		t.Fatalf("recoverable lines = %d, want 1: %q", got, stderr.String())
	}
}

// testConfirmReplayNamesItsDownJobs: a job stopped between the two replays leaves the
// finding, so the line names only the jobs the confirm replay still counts down.
func testConfirmReplayNamesItsDownJobs(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, terminalBudgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	sup.beforeConfirmReplay = func() { plantStopped(t, sup, "broken") }
	var episode findingEpisode
	if err := sup.checkNoJobAlive(testContext(t), state, &episode); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("checkNoJobAlive = %v, want ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: manual dead (exit lost, restarts 0/0 in 3600s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func TestOnceWithoutRecordHeldLock(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once)
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	if err := os.Remove(sup.recordPath("manual")); err != nil {
		t.Fatal(err)
	}
	lock, err := holdLock(sup.lockPath("manual"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockForShim(lock)
	stderr := captureSupervisorStderr(sup)
	var episode findingEpisode
	if err := sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" {
		t.Fatalf("held lock: err = %v, stderr = %q; want no finding", err, stderr.String())
	}
}

func TestOnceLockProbeError(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once)
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	old := probeJobLock
	probeJobLock = func(string) (bool, error) { return false, errors.New("probe failed") }
	t.Cleanup(func() { probeJobLock = old })
	stderr := captureSupervisorStderr(sup)
	var episode findingEpisode
	if err := sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" {
		t.Fatalf("probe error: err = %v, stderr = %q; want no finding", err, stderr.String())
	}
}

func TestLostOnceFirstSeenOnConfirm(t *testing.T) {
	a := testJob("A", []string{"/bin/true"})
	a.Schedule = "once"
	b := testJob("B", []string{"/bin/true"})
	b.Schedule = "once"
	sup := testSupervisor(t, a, b)
	stderr := captureSupervisorStderr(sup)
	state := plantRunningOnce(t, sup, "A", "2026-11-18T08:00:00Z")
	sup.beforeConfirmReplay = func() { plantRunningOnce(t, sup, "B", "2026-11-18T08:00:01Z") }
	episode := findingEpisode{reported: true, pending: make(chan error)}
	if err := sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" || !episode.reported || episode.pending == nil {
		t.Fatalf("confirm introduced B: err = %v, stderr = %q; want no finding", err, stderr.String())
	}
	sup.beforeConfirmReplay = nil
	plantExit(t, sup, "B", intPtr(0), "")
	fresh, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err = sup.checkNoJobAlive(testContext(t), fresh, &episode); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("next tick = %v, want ErrNoJobAlive", err)
	}
	if got := stderr.String(); !strings.Contains(got, "A dead (exit lost") || strings.Contains(got, "B dead") {
		t.Fatalf("next tick stderr = %q, want A only", got)
	}
}
