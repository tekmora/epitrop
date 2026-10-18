package codexlocal

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

const burnResetsAt = 1789858494

func burnLine(at time.Time, limitID string, used float64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","rate_limits":{"limit_id":%q,"primary":{"used_percent":%g,"window_minutes":10080,"resets_at":%d},"plan_type":"pro"}}}`,
		at.UTC().Format(time.RFC3339Nano), limitID, used, burnResetsAt)
}

func burnFetch(t *testing.T, now time.Time, files map[string][]string) (Result, catalog.Record) {
	t.Helper()
	dir := t.TempDir()
	for name, lines := range files {
		writeSession(t, dir, name, strings.Join(lines, "\n")+"\n", now)
	}
	res := Fetch(context.Background(), dir, now)
	if len(res.Records) != 1 {
		t.Fatalf("Fetch() = %+v, want one record", res)
	}
	return res, res.Records[0]
}

func burnNow() time.Time {
	return time.Now().UTC().Truncate(time.Second)
}

func TestFetchBurnAndAgents_Positive(t *testing.T) {
	now := burnNow()
	res, rec := burnFetch(t, now, map[string][]string{
		"a.jsonl": {burnLine(now.Add(-20*time.Minute), "codex", 30), burnLine(now.Add(-10*time.Minute), "codex", 40), burnLine(now.Add(-5*time.Minute), "codex", 41)},
		"b.jsonl": {burnLine(now.Add(-2*time.Minute), "codex", 42.5)},
		"c.jsonl": {burnLine(now.Add(-30*time.Minute), "codex", 20)},
		"d.jsonl": {burnLine(now.Add(-3*time.Minute), "other", 99)},
	})
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail %q, want ok", res.Status, res.Detail)
	}
	uw := rec.UsageWindow
	if uw.BurnPercentPerHour == nil || *uw.BurnPercentPerHour != 18.75 {
		t.Fatalf("burn = %v, want 18.75 (2.5 points over 8 minutes)", uw.BurnPercentPerHour)
	}
	if uw.ActiveAgents == nil || *uw.ActiveAgents != 2 {
		t.Fatalf("active agents = %v, want 2 (c is outside the window, d reports another limit)", uw.ActiveAgents)
	}
	if _, ok := rec.Absent[absentBurnKey]; ok {
		t.Fatalf("Absent names burn although it was measured: %q", rec.Absent[absentBurnKey])
	}
}

func assertBurnAbsent(t *testing.T, rec catalog.Record, what string) {
	t.Helper()
	if rec.UsageWindow.BurnPercentPerHour != nil || rec.Absent[absentBurnKey] == "" {
		t.Fatalf("%s: burn = %v, absent %q, want absent with a reason", what, rec.UsageWindow.BurnPercentPerHour, rec.Absent[absentBurnKey])
	}
}

func assertBothAbsent(t *testing.T, rec catalog.Record, what, reasonPart string) {
	t.Helper()
	assertBurnAbsent(t, rec, what)
	if rec.UsageWindow.ActiveAgents != nil || !strings.Contains(rec.Absent[absentAgentsKey], reasonPart) {
		t.Fatalf("%s: agents = %v, absent %q, want absent naming %q", what, rec.UsageWindow.ActiveAgents, rec.Absent[absentAgentsKey], reasonPart)
	}
}

func assertAgents(t *testing.T, rec catalog.Record, want int, what string) {
	t.Helper()
	if a := rec.UsageWindow.ActiveAgents; a == nil || *a != want {
		t.Fatalf("%s: agents = %v, want %d", what, a, want)
	}
}

func recentFiles(now time.Time, n int) map[string][]string {
	files := make(map[string][]string, n)
	for i := range n {
		files[fmt.Sprintf("s%03d.jsonl", i)] = []string{burnLine(now.Add(-time.Duration(i+1)*time.Second), "codex", float64(i))}
	}
	return files
}

func TestFetchBurnAndAgents_NegativeFallingUse(t *testing.T) {
	now := burnNow()
	_, rec := burnFetch(t, now, map[string][]string{
		"a.jsonl": {burnLine(now.Add(-10*time.Minute), "codex", 50), burnLine(now.Add(-time.Minute), "codex", 40)},
	})
	assertBurnAbsent(t, rec, "falling used_percent")
}

func TestFetchBurnAndAgents_NegativeNoTimestamp(t *testing.T) {
	now := burnNow()
	noStamp := strings.Replace(burnLine(now.Add(-time.Minute), "codex", 40), `"timestamp"`, `"time"`, 1)
	res, rec := burnFetch(t, now, map[string][]string{
		"a.jsonl": {burnLine(now.Add(-10*time.Minute), "codex", 39), noStamp},
	})
	if res.Status != catalog.StatusDegraded || !strings.Contains(res.Detail, "timestamp") {
		t.Fatalf("line without timestamp: status %v detail %q, want degraded naming the timestamp", res.Status, res.Detail)
	}
	assertBothAbsent(t, rec, "line without timestamp", "could not be read")
}

func TestFetchBurnAndAgents_NegativeTooManySessions(t *testing.T) {
	now := burnNow()
	res, rec := burnFetch(t, now, recentFiles(now, maxRecentSessions+1))
	if res.Status != catalog.StatusOK {
		t.Fatalf("one past maxRecentSessions: status %v, want ok (a bound, not a failure)", res.Status)
	}
	assertBothAbsent(t, rec, "one past maxRecentSessions", "more than")
}

func TestFetchBurnAndAgents_BoundaryWindowEdges(t *testing.T) {
	now := burnNow()
	_, rec := burnFetch(t, now, map[string][]string{
		"a.jsonl": {burnLine(now.Add(-burnWindow), "codex", 40), burnLine(now, "codex", 40)},
	})
	if b := rec.UsageWindow.BurnPercentPerHour; b == nil || *b != 0 {
		t.Fatalf("samples at exactly now-%s and now with no rise: burn = %v, want a measured 0", burnWindow, b)
	}

	_, rec = burnFetch(t, now, map[string][]string{
		"a.jsonl": {burnLine(now.Add(-burnWindow-time.Nanosecond), "codex", 30), burnLine(now, "codex", 40)},
	})
	assertBurnAbsent(t, rec, "one sample just outside the window")
	assertAgents(t, rec, 1, "one sample in the window")
}

func TestFetchBurnAndAgents_BoundarySessionBound(t *testing.T) {
	now := burnNow()
	_, rec := burnFetch(t, now, recentFiles(now, maxRecentSessions))
	assertAgents(t, rec, maxRecentSessions, "exactly maxRecentSessions files")
}

func TestFetchBurnAndAgents_BoundaryNoRecentSession(t *testing.T) {
	now := burnNow()
	old := now.Add(-time.Hour)
	dir := t.TempDir()
	writeSession(t, dir, "old.jsonl", burnLine(old, "codex", 5)+"\n", old)
	rec := Fetch(context.Background(), dir, now).Records[0]
	assertAgents(t, rec, 0, "no session in the window")
	assertBurnAbsent(t, rec, "no session in the window")
}
