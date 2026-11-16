//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

type triggeredPayload struct {
	Trigger string `json:"trigger"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	Due     string `json:"due,omitempty"`
	Seq     uint64 `json:"seq,omitempty"`
}

func testSupervisorWithClock(t *testing.T, rt testRuntimeConfig, clk clock.Clock, jobs ...config.JobConfig) *Supervisor {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("supervisor tests require Linux flock, process groups and parent-death signals")
	}
	cfg := config.Default()
	cfg.EventLog.Dir = rt.EventLogDir
	cfg.EventLog.SigningKeyPath = rt.KeyPath
	cfg.EventLog.PublicKey = rt.PublicKey
	cfg.Jobs = jobs
	sup, err := New(cfg, Options{
		Clock:        clk,
		ShimCommand:  []string{os.Args[0], "-test.run=TestHelperProcessShim", "--"},
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Setenv("EPITROP_TEST_SHIM", "1")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testWaitBound)
		defer cancel()
		if stopErr := sup.Stop(ctx, ""); stopErr != nil {
			t.Logf("cleanup Stop(all) = %v", stopErr)
		}
		endLeftoverSandboxedJobs(ctx, t, sup)
		waitForShimWaiters(t, sup, 0)
	})
	return sup
}

// recordCollector keeps the records one replay reduces. ReplayStable runs the reducer again from
// the first record when the log moved under a replay (a shim appending job.exited), so a seq at or
// below the last one kept starts a new pass and the records of the earlier pass are dropped.
type recordCollector struct {
	records []eventlog.Record
}

func (c *recordCollector) reduce(st eventlog.State, rec eventlog.Record) (eventlog.State, error) {
	if len(c.records) > 0 && rec.Seq <= c.records[len(c.records)-1].Seq {
		c.records = c.records[:0]
	}
	c.records = append(c.records, rec)
	return JobReducer(st, rec)
}

func collectJobRecords(t *testing.T, sup *Supervisor) []eventlog.Record {
	t.Helper()
	var c recordCollector
	if _, err := sup.replayStateWithReducer(testContext(t), c.reduce); err != nil {
		t.Fatalf("replayStateWithReducer() = %v, want nil", err)
	}
	return c.records
}

func collectTriggeredEvents(t *testing.T, sup *Supervisor, jobName string) []triggeredPayload {
	t.Helper()
	records := collectJobRecords(t, sup)
	var out []triggeredPayload
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec.Type == "job.triggered" && rec.TaskID == jobName {
			var p triggeredPayload
			if err := json.Unmarshal(rec.Payload, &p); err != nil {
				t.Fatalf("Unmarshal(job.triggered payload) = %v, want nil", err)
			}
			out = append(out, p)
		}
	}
	return out
}

func TestIntervalTrigger_T1(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 1, 59, 0, 0, time.UTC))
	job := testJob("cron-t1", []string{"/bin/true"})
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	sup := testSupervisorWithClock(t, rt, clk, job)
	ctx := testContext(t)

	checkIntervalTriggerArmed(t, sup, rt, ctx)
	checkIntervalTriggerFired(t, sup, rt, clk, ctx)
	checkIntervalTriggerNoDuplicate(t, sup, clk, ctx)
}

func checkIntervalTriggerArmed(t *testing.T, sup *Supervisor, rt testRuntimeConfig, ctx context.Context) {
	t.Helper()
	// Step 1: clock at 01:59:00 -> arms, starts nothing.
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(01:59) = %v, want nil", err)
	}
	if armed := countEvents(t, rt.EventLogDir, "job.armed"); armed != 1 {
		t.Fatalf("job.armed events = %d, want 1", armed)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 0 {
		t.Fatalf("job.started events at 01:59 = %d, want 0", starts)
	}
}

func checkIntervalTriggerFired(t *testing.T, sup *Supervisor, rt testRuntimeConfig, clk *clockwork.FakeClock, ctx context.Context) {
	t.Helper()
	// Step 2: clock at 02:00:59 -> one job.triggered started (due 02:00) and one start.
	clk.Advance(119 * time.Second)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(02:00:59) = %v, want nil", err)
	}
	triggers := collectTriggeredEvents(t, sup, "cron-t1")
	if len(triggers) != 1 || triggers[0].Outcome != "started" || triggers[0].Due != "2026-10-14T02:00:00Z" {
		t.Fatalf("triggers after 02:00:59 = %+v, want one started due 02:00:00", triggers)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 1 {
		t.Fatalf("job.started events at 02:00:59 = %d, want 1", starts)
	}
}

func checkIntervalTriggerNoDuplicate(t *testing.T, sup *Supervisor, clk *clockwork.FakeClock, ctx context.Context) {
	t.Helper()
	// Step 3: Reconcile again at 02:01:00 -> no second start.
	clk.Advance(time.Second)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(02:01:00) = %v, want nil", err)
	}
	triggers := collectTriggeredEvents(t, sup, "cron-t1")
	if len(triggers) != 1 {
		t.Fatalf("triggers after 02:01:00 = %d, want 1 (no duplicate)", len(triggers))
	}
}

func TestOverlapSkippedRunning_T2(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 1, 59, 0, 0, time.UTC))
	job := testJob("cron-overlap", longJobCommand(t))
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	sup := testSupervisorWithClock(t, rt, clk, job)
	ctx := testContext(t)

	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(01:59) = %v, want nil", err)
	}
	clk.Advance(90 * time.Second) // 02:00:30
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(02:00:30) = %v, want nil", err)
	}
	waitForState(t, sup, "cron-overlap", StateRunning)

	// Advance 24 hours to next due time (2026-10-15T02:00:00Z).
	clk.Advance(24 * time.Hour)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(next day) = %v, want nil", err)
	}

	triggers := collectTriggeredEvents(t, sup, "cron-overlap")
	if len(triggers) != 2 {
		t.Fatalf("triggered events count = %d, want 2", len(triggers))
	}
	second := triggers[1]
	if second.Outcome != "skipped" || second.Reason != "running" || second.Due != "2026-10-15T02:00:00Z" {
		t.Fatalf("second trigger = %+v, want skipped outcome with reason running", second)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 1 {
		t.Fatalf("job.started events = %d, want exactly 1 (no second start)", starts)
	}
	if err := sup.Stop(ctx, "cron-overlap"); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
}

func TestMissedDueTimesOneStart_T3(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 1, 59, 0, 0, time.UTC))
	job := testJob("cron-jump", []string{"/bin/true"})
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	sup := testSupervisorWithClock(t, rt, clk, job)
	ctx := testContext(t)

	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(01:59) = %v, want nil", err)
	}
	// Clock jumps 3 days ahead.
	clk.Advance(3*24*time.Hour + 6*time.Minute) // 2026-10-17T02:05:00Z
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(jump) = %v, want nil", err)
	}

	triggers := collectTriggeredEvents(t, sup, "cron-jump")
	if len(triggers) != 1 {
		t.Fatalf("triggered count = %d, want exactly 1 for newest due time", len(triggers))
	}
	if triggers[0].Due != "2026-10-17T02:00:00Z" || triggers[0].Outcome != "started" {
		t.Fatalf("trigger = %+v, want newest due 2026-10-17T02:00:00Z and started", triggers[0])
	}
}

func TestNewJobNeverFiresForHistory_T4(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	job := testJob("cron-nohistory", []string{"/bin/true"})
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	sup := testSupervisorWithClock(t, rt, clk, job)
	ctx := testContext(t)

	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(10:00) = %v, want nil", err)
	}
	if triggers := collectTriggeredEvents(t, sup, "cron-nohistory"); len(triggers) != 0 {
		t.Fatalf("triggers at 10:00 = %d, want 0", len(triggers))
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 0 {
		t.Fatalf("job.started at 10:00 = %d, want 0", starts)
	}

	// Advance clock to next 02:00:30 (next day).
	clk.Advance(16*time.Hour + 30*time.Second) // 2026-10-15T02:00:30Z
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(next 02:00) = %v, want nil", err)
	}
	triggers := collectTriggeredEvents(t, sup, "cron-nohistory")
	if len(triggers) != 1 || triggers[0].Due != "2026-10-15T02:00:00Z" {
		t.Fatalf("triggers = %+v, want 1 started at 2026-10-15T02:00:00Z", triggers)
	}
}

func TestEventTriggerReplayDeduplication_T5(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	jobA := testJob("job-a", []string{"/bin/true"})
	jobA.Schedule = "once"
	jobB := testJob("job-b", []string{"/bin/true"})
	jobB.Schedule = "event"
	jobB.Restart = config.JobRestartConfig{Policy: "never"}
	jobB.OnEvent = &config.JobEventTrigger{Type: "job.exited", TaskID: "job-a"}

	sup := testSupervisorWithClock(t, rt, clk, jobA, jobB)
	ctx := testContext(t)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}

	appendAndReconcileEventTriggers(t, sup, ctx)

	// Restart supervisor on same log: starts nothing more.
	supRestart := testSupervisorWithClock(t, rt, clk, jobA, jobB)
	checkReplayDeduplicationTriggers(t, sup, supRestart, ctx)
}

func appendAndReconcileEventTriggers(t *testing.T, sup *Supervisor, ctx context.Context) {
	t.Helper()
	// Append two job.exited records for job-a.
	if err := sup.appendJobEvent(ctx, "job.exited", "job-a", map[string]any{"exit_code": 0}); err != nil {
		t.Fatalf("appendJobEvent(1) = %v, want nil", err)
	}
	if err := sup.appendJobEvent(ctx, "job.exited", "job-a", map[string]any{"exit_code": 0}); err != nil {
		t.Fatalf("appendJobEvent(2) = %v, want nil", err)
	}

	// First reconcile starts job-b for record 1.
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(first event) = %v, want nil", err)
	}
	waitForState(t, sup, "job-b", StateDead)

	// Second reconcile starts job-b for record 2.
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(second event) = %v, want nil", err)
	}
	waitForState(t, sup, "job-b", StateDead)
}

func checkReplayDeduplicationTriggers(t *testing.T, sup *Supervisor, supRestart *Supervisor, ctx context.Context) {
	t.Helper()
	triggers := collectTriggeredEvents(t, sup, "job-b")
	if len(triggers) != 2 {
		t.Fatalf("triggers count = %d, want 2", len(triggers))
	}
	if triggers[0].Seq >= triggers[1].Seq || triggers[0].Outcome != "started" || triggers[1].Outcome != "started" {
		t.Fatalf("triggers order/outcome invalid: %+v", triggers)
	}

	if err := supRestart.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(restart) = %v, want nil", err)
	}
	if after := collectTriggeredEvents(t, supRestart, "job-b"); len(after) != 2 {
		t.Fatalf("triggers after restart = %d, want 2 (no duplicate starts): %+v", len(after), after)
	}
}

func TestEventQueuedWhileRunning_T6(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	jobA := testJob("job-a", []string{"/bin/true"})
	jobB := testJob("job-b", longJobCommand(t))
	jobB.Schedule = "event"
	jobB.Restart = config.JobRestartConfig{Policy: "never"}
	jobB.OnEvent = &config.JobEventTrigger{Type: "job.exited", TaskID: "job-a"}

	sup := testSupervisorWithClock(t, rt, clk, jobA, jobB)
	ctx := testContext(t)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}

	// Start job-b first so it is running.
	if err := sup.Start(ctx, "job-b"); err != nil {
		t.Fatalf("Start(job-b) = %v, want nil", err)
	}
	waitForState(t, sup, "job-b", StateRunning)

	// Append job.exited for job-a while job-b is alive.
	if err := sup.appendJobEvent(ctx, "job.exited", "job-a", map[string]any{"exit_code": 0}); err != nil {
		t.Fatalf("appendJobEvent() = %v, want nil", err)
	}

	// Reconcile while job-b is running: nothing recorded, match waits in queue.
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(running) = %v, want nil", err)
	}
	if count := countEvents(t, rt.EventLogDir, "job.triggered"); count != 0 {
		t.Fatalf("job.triggered count while running = %d, want 0", count)
	}

	// Stop job-b.
	if err := sup.Stop(ctx, "job-b"); err != nil {
		t.Fatalf("Stop(job-b) = %v, want nil", err)
	}
	waitForState(t, sup, "job-b", StateStopped)

	// Reconcile after job-b ends: queued match starts job-b.
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(after stop) = %v, want nil", err)
	}
	if count := countEvents(t, rt.EventLogDir, "job.triggered"); count != 1 {
		t.Fatalf("job.triggered count after stop = %d, want 1", count)
	}
	if err := sup.Stop(ctx, "job-b"); err != nil {
		t.Fatalf("cleanup Stop(job-b) = %v, want nil", err)
	}
}

func TestEventHistoryIgnored_T7(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	jobA := testJob("job-a", []string{"/bin/true"})
	supA := testSupervisorWithClock(t, rt, clk, jobA)
	ctx := testContext(t)

	// Append job.exited before job-b is ever configured or armed.
	if err := supA.appendJobEvent(ctx, "job.exited", "job-a", map[string]any{"exit_code": 0}); err != nil {
		t.Fatalf("appendJobEvent() = %v, want nil", err)
	}

	// Now configure job-b with event trigger on job-a.
	jobB := testJob("job-b", []string{"/bin/true"})
	jobB.Schedule = "event"
	jobB.Restart = config.JobRestartConfig{Policy: "never"}
	jobB.OnEvent = &config.JobEventTrigger{Type: "job.exited", TaskID: "job-a"}

	sup := testSupervisorWithClock(t, rt, clk, jobA, jobB)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}

	if count := countEvents(t, rt.EventLogDir, "job.triggered"); count != 0 {
		t.Fatalf("job.triggered count = %d, want 0 for historical events", count)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 0 {
		t.Fatalf("job.started count = %d, want 0", starts)
	}
}

func TestRearmOnConfigChange_T9(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 1, 59, 0, 0, time.UTC))
	job := testJob("cron-rearm", []string{"/bin/true"})
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}

	sup := testSupervisorWithClock(t, rt, clk, job)
	ctx := testContext(t)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(01:59) = %v, want nil", err)
	}

	clk.Advance(90 * time.Second) // 02:00:30
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(02:00:30) = %v, want nil", err)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 1 {
		t.Fatalf("job.started count = %d, want 1", starts)
	}

	// Change cron schedule in configuration to 0 3 * * *.
	clk.Advance(30 * time.Minute) // 02:30:30
	job.Schedule = "0 3 * * *"
	sup2 := testSupervisorWithClock(t, rt, clk, job)

	if err := sup2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(re-arm) = %v, want nil", err)
	}
	if armed := countEvents(t, rt.EventLogDir, "job.armed"); armed != 2 {
		t.Fatalf("job.armed count = %d, want 2 after config change", armed)
	}
	// At 02:30:30, new schedule 0 3 * * * due was 03:00 yesterday, which is before ArmedAt, so no new start.
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 1 {
		t.Fatalf("job.started count at 02:30:30 = %d, want 1", starts)
	}

	// Advance to 03:00:30 -> fires for new schedule.
	clk.Advance(30 * time.Minute) // 03:00:30
	if err := sup2.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(03:00:30) = %v, want nil", err)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 2 {
		t.Fatalf("job.started count at 03:00:30 = %d, want 2", starts)
	}
}

func TestTriggerConsumedWhenStartFails_M8(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 1, 59, 0, 0, time.UTC))
	job := testJob("cron-fail-start", []string{"/nonexistent/failing/binary"})
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}

	sup := testSupervisorWithClock(t, rt, clk, job)
	ctx := testContext(t)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(01:59) = %v, want nil", err)
	}

	// Advance clock to due time.
	clk.Advance(90 * time.Second) // 02:00:30
	err := sup.Reconcile(ctx)
	if err == nil {
		t.Fatalf("Reconcile(failing binary) = nil, want Start error")
	}

	// job.triggered must have been appended BEFORE Start.
	if count := countEvents(t, rt.EventLogDir, "job.triggered"); count != 1 {
		t.Fatalf("job.triggered count after failing start = %d, want 1", count)
	}

	// Reconcile again: trigger is consumed (LastDue advanced), so no repeat attempt.
	err2 := sup.Reconcile(ctx)
	if err2 != nil {
		t.Fatalf("Reconcile(second try) = %v, want nil (trigger consumed)", err2)
	}
	if count := countEvents(t, rt.EventLogDir, "job.triggered"); count != 1 {
		t.Fatalf("job.triggered count after second reconcile = %d, want still 1", count)
	}
}

// An event job's job.triggered record also comes before its start: a start that fails still
// consumes the match, so the next pass does not try the same record again.
func TestEventTriggerConsumedWhenStartFails(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	jobA := testJob("job-a", []string{"/bin/true"})
	jobA.Schedule = "once"
	jobB := testJob("job-b", []string{"/nonexistent/failing/binary"})
	jobB.Schedule = "event"
	jobB.Restart = config.JobRestartConfig{Policy: "never"}
	jobB.OnEvent = &config.JobEventTrigger{Type: "job.exited", TaskID: "job-a"}

	sup := testSupervisorWithClock(t, rt, clk, jobA, jobB)
	ctx := testContext(t)
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(arm) = %v, want nil", err)
	}
	if err := sup.appendJobEvent(ctx, "job.exited", "job-a", map[string]any{"exit_code": 0}); err != nil {
		t.Fatalf("appendJobEvent(job.exited) = %v, want nil", err)
	}
	if err := sup.Reconcile(ctx); err == nil {
		t.Fatalf("Reconcile(failing binary) = nil, want Start error")
	}
	if got := collectTriggeredEvents(t, sup, "job-b"); len(got) != 1 || got[0].Outcome != "started" {
		t.Fatalf("job-b triggers after failing start = %+v, want one started", got)
	}
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(second pass) = %v, want nil (match consumed)", err)
	}
	if got := collectTriggeredEvents(t, sup, "job-b"); len(got) != 1 {
		t.Fatalf("job-b triggers after second pass = %d, want still 1", len(got))
	}
}

func TestJobReducerArmedAndTriggered_HISS15(t *testing.T) {
	t.Run("job.armed positive and boundary", func(t *testing.T) {
		st := eventlog.State{Jobs: map[string]eventlog.Job{}}
		rec := eventlog.Record{
			Seq:     7,
			Type:    "job.armed",
			TaskID:  "my-job",
			Payload: []byte(`{"trigger":"schedule 0 2 * * *","at":"2026-10-14T02:00:00Z"}`),
		}
		res, err := JobReducer(st, rec)
		if err != nil {
			t.Fatalf("JobReducer(job.armed) = %v, want nil", err)
		}
		j := res.Jobs["my-job"]
		if j.ArmedTrigger != "schedule 0 2 * * *" || j.ArmedAt != "2026-10-14T02:00:00Z" || j.ArmedSeq != 7 {
			t.Fatalf("reduced job = %+v, want armed fields set and ArmedSeq = the record's seq 7", j)
		}
	})

	t.Run("job.armed invalid payload", func(t *testing.T) {
		st := eventlog.State{Jobs: map[string]eventlog.Job{}}
		rec := eventlog.Record{
			Type:    "job.armed",
			TaskID:  "my-job",
			Payload: []byte(`{invalid`),
		}
		if _, err := JobReducer(st, rec); err == nil {
			t.Fatal("JobReducer(bad payload) = nil, want error")
		}
	})

	t.Run("job.triggered positive", func(t *testing.T) {
		st := eventlog.State{Jobs: map[string]eventlog.Job{}}
		rec := eventlog.Record{
			Type:    "job.triggered",
			TaskID:  "my-job",
			Payload: []byte(`{"trigger":"schedule 0 2 * * *","outcome":"started","due":"2026-10-14T02:00:00Z","seq":12}`),
		}
		res, err := JobReducer(st, rec)
		if err != nil {
			t.Fatalf("JobReducer(job.triggered) = %v, want nil", err)
		}
		j := res.Jobs["my-job"]
		if j.LastDue != "2026-10-14T02:00:00Z" || j.LastTriggerSeq != 12 {
			t.Fatalf("reduced job = %+v, want LastDue and LastTriggerSeq set", j)
		}
	})

	t.Run("job.triggered invalid payload", func(t *testing.T) {
		st := eventlog.State{Jobs: map[string]eventlog.Job{}}
		rec := eventlog.Record{
			Type:    "job.triggered",
			TaskID:  "my-job",
			Payload: []byte(`{bad`),
		}
		if _, err := JobReducer(st, rec); err == nil {
			t.Fatal("JobReducer(bad payload) = nil, want error")
		}
	})
}

// TestEventJobArmedAfterSamePassRecords: job-a (always) starts in the same reconcile pass that
// arms job-b on job-a's job.started. That start was appended before job-b was armed, so it is
// history for job-b and must never start it.
func TestEventJobArmedAfterSamePassRecords(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC))
	jobA := testJob("job-a", longJobCommand(t))
	jobB := testJob("job-b", []string{"/bin/true"})
	jobB.Schedule = "event"
	jobB.Restart = config.JobRestartConfig{Policy: "never"}
	jobB.OnEvent = &config.JobEventTrigger{Type: "job.started", TaskID: "job-a"}
	sup := testSupervisorWithClock(t, rt, clk, jobA, jobB)
	ctx := testContext(t)

	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(first) = %v, want nil", err)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 1 {
		t.Fatalf("job.started after the first pass = %d, want 1 (job-a only)", starts)
	}
	if err := sup.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile(second) = %v, want nil", err)
	}
	if triggers := collectTriggeredEvents(t, sup, "job-b"); len(triggers) != 0 {
		t.Fatalf("job-b triggers = %+v, want none: job-a's start came before job-b was armed", triggers)
	}
}

func TestEventMatchCollectorKeepsEachSeqOnce(t *testing.T) {
	job := testJob("job-b", []string{"/bin/true"})
	job.Schedule = "event"
	job.OnEvent = &config.JobEventTrigger{Type: "job.exited", TaskID: "job-a"}
	other := testJob("job-c", []string{"/bin/true"})
	c := &eventMatchCollector{jobs: []config.JobConfig{job, other}, matches: map[string][]uint64{}}
	recs := []eventlog.Record{
		{Seq: 1, Type: "job.exited", TaskID: "job-a", Payload: []byte(`{}`)},
		{Seq: 2, Type: "job.exited", TaskID: "job-x", Payload: []byte(`{}`)},
		{Seq: 3, Type: "job.triggered", TaskID: "job-a", Payload: []byte(`{"trigger":"t","outcome":"started"}`)},
		{Seq: 4, Type: "job.exited", TaskID: "job-a", Payload: []byte(`{}`)},
	}
	// Two passes over the same records, as ReplayStable makes when the log moved under a replay.
	for pass := 0; pass < 2; pass++ {
		st := eventlog.State{Jobs: map[string]eventlog.Job{}}
		for i := 0; i < len(recs); i++ {
			var err error
			if st, err = c.reduce(st, recs[i]); err != nil {
				t.Fatalf("reduce(pass %d, seq %d) = %v, want nil", pass, recs[i].Seq, err)
			}
		}
	}
	if got := c.matches["job-b"]; len(got) != 2 || got[0] != 1 || got[1] != 4 {
		t.Fatalf("matches[job-b] = %v, want [1 4]", got)
	}
	if got := c.matches["job-c"]; len(got) != 0 {
		t.Fatalf("matches[job-c] = %v, want none: job-c has no event trigger", got)
	}
	if c.head != 4 {
		t.Fatalf("head = %d, want 4", c.head)
	}
}

func TestRecordCollectorKeepsEachSeqOnce(t *testing.T) {
	var c recordCollector
	recs := []eventlog.Record{
		{Seq: 1, Type: "job.armed", TaskID: "job-a", Payload: []byte(`{"trigger":"t","at":"2026-10-14T01:59:00Z"}`)},
		{Seq: 2, Type: "job.triggered", TaskID: "job-a", Payload: []byte(`{"trigger":"t","outcome":"started","due":"2026-10-17T02:00:00Z"}`)},
	}
	// Two passes over the same records, as ReplayStable makes when the log moved under a replay.
	for pass := 0; pass < 2; pass++ {
		st := eventlog.State{Jobs: map[string]eventlog.Job{}}
		for i := 0; i < len(recs); i++ {
			var err error
			if st, err = c.reduce(st, recs[i]); err != nil {
				t.Fatalf("reduce(pass %d, seq %d) = %v, want nil", pass, recs[i].Seq, err)
			}
		}
	}
	if len(c.records) != 2 || c.records[0].Seq != 1 || c.records[1].Seq != 2 {
		t.Fatalf("records = %+v, want [seq 1, seq 2]", c.records)
	}
}

func TestCronJobUnreadableLastDueIsAnError(t *testing.T) {
	rt := testRuntime(t)
	clk := clockwork.NewFakeClockAt(time.Date(2026, 10, 14, 2, 0, 30, 0, time.UTC))
	job := testJob("cron-bad", []string{"/bin/true"})
	job.Schedule = "0 2 * * *"
	job.Restart = config.JobRestartConfig{Policy: "never"}
	sup := testSupervisorWithClock(t, rt, clk, job)
	st := eventlog.Job{Name: "cron-bad", ArmedAt: "2026-10-13T10:00:00Z", LastDue: "yesterday"}
	err := sup.reconcileCronJob(testContext(t), job, st)
	if err == nil || !strings.Contains(err.Error(), "last_due") {
		t.Fatalf("reconcileCronJob(LastDue %q) = %v, want an error naming last_due", st.LastDue, err)
	}
	if starts := countEvents(t, rt.EventLogDir, "job.started"); starts != 0 {
		t.Fatalf("job.started = %d, want 0: an unreadable cursor never starts the job", starts)
	}
}
