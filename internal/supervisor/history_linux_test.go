//go:build linux

package supervisor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// historySupervisor configures two jobs, starts neither, and writes events itself at the
// fake clock's times.
func historySupervisor(t *testing.T) (*Supervisor, *clockwork.FakeClock) {
	t.Helper()
	clk := clockwork.NewFakeClock()
	sup := testSupervisorWithClock(t, testRuntime(t), clk, testJob("daemon", []string{"true"}), testJob("other", []string{"true"}))
	return sup, clk
}

func appendHistory(t *testing.T, sup *Supervisor, typ string, name string, payload map[string]any) {
	t.Helper()
	if err := sup.appendJobEvent(testContext(t), typ, name, payload); err != nil {
		t.Fatalf("appendJobEvent(%s) = %v, want nil", typ, err)
	}
}

func historyOf(t *testing.T, sup *Supervisor, name string, since time.Time) []JobHistory {
	t.Helper()
	got, err := sup.History(testContext(t), name, since)
	if err != nil {
		t.Fatalf("History(%q) = %v, want nil", name, err)
	}
	return got
}

func TestHistoryCountsEachSupervisionRecord(t *testing.T) {
	sup, clk := historySupervisor(t)
	first := clk.Now().UTC().Format(time.RFC3339Nano)
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running"})
	appendHistory(t, sup, "job.adopted", "daemon", map[string]any{"state": "running"})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 0})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 3})
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "reason": "lost"})
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"restarts": 1})
	appendHistory(t, sup, "job.stale", "daemon", map[string]any{"last_beat": first})
	appendHistory(t, sup, "job.refused", "daemon", map[string]any{"state": "dead", "reason": "x"})
	appendHistory(t, sup, "job.armed", "daemon", map[string]any{"trigger": "t"})
	clk.Advance(time.Minute)
	last := clk.Now().UTC().Format(time.RFC3339Nano)
	appendHistory(t, sup, "job.stopped", "daemon", map[string]any{"state": "stopped"})
	appendHistory(t, sup, "job.started", "other", map[string]any{"state": "running"})
	got := historyOf(t, sup, "daemon", time.Time{})
	want := JobHistory{Name: "daemon", Started: 2, Exited: 3, Unclean: 2, Restarted: 1, Stale: 1, Refused: 1, Stopped: 1, First: first, Last: last}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("History(daemon) = %+v, want [%+v]", got, want)
	}
}

func TestHistoryListsEveryConfiguredJob(t *testing.T) {
	sup, _ := historySupervisor(t)
	appendHistory(t, sup, "job.started", "other", map[string]any{"state": "running"})
	appendHistory(t, sup, "job.started", "removed", map[string]any{"state": "running"})
	got := historyOf(t, sup, "", time.Time{})
	if len(got) != 2 || got[0].Name != "daemon" || got[0].Started != 0 || got[0].First != "" || got[1].Name != "other" || got[1].Started != 1 {
		t.Fatalf("History(all) = %+v, want daemon with nothing counted, then other with one start", got)
	}
}

func TestHistoryCountsFromSinceOn(t *testing.T) {
	sup, clk := historySupervisor(t)
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running"})
	clk.Advance(time.Hour)
	since := clk.Now()
	appendHistory(t, sup, "job.exited", "daemon", map[string]any{"state": "dead", "code": 1})
	clk.Advance(time.Second)
	appendHistory(t, sup, "job.restarted", "daemon", map[string]any{"restarts": 1})
	got := historyOf(t, sup, "daemon", since)
	if got[0].Started != 0 || got[0].Exited != 1 || got[0].Restarted != 1 || got[0].First != since.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("History(since) = %+v, want the exit at since and the restart after it, not the start before", got[0])
	}
	if late := historyOf(t, sup, "daemon", clk.Now().Add(time.Nanosecond)); late[0] != (JobHistory{Name: "daemon"}) {
		t.Fatalf("History(after the last record) = %+v, want nothing counted", late[0])
	}
}

func TestHistoryWithoutAnEventLogCountsNothing(t *testing.T) {
	sup, _ := historySupervisor(t)
	got := historyOf(t, sup, "daemon", time.Time{})
	if len(got) != 1 || got[0] != (JobHistory{Name: "daemon"}) {
		t.Fatalf("History(no log) = %+v, want daemon with nothing counted", got)
	}
}

func TestHistoryRefusesAnUnknownJobAndAnEndedContext(t *testing.T) {
	sup, _ := historySupervisor(t)
	if _, err := sup.History(testContext(t), "missing", time.Time{}); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("History(missing) = %v, want ErrUnknownJob", err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	cancel()
	if _, err := sup.History(ctx, "daemon", time.Time{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("History(cancelled) = %v, want context.Canceled", err)
	}
}
