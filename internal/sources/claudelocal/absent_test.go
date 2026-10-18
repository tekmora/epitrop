package claudelocal

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// TestFetchUnknownCeilingStatesEveryGap: without a rejection the window
// still counts its active agents, and every field it cannot derive is
// absent with a reason, never zero.
func TestFetchUnknownCeilingStatesEveryGap(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "p/s.jsonl", fixtureNow,
		usageLine(usageFixture{at: fixtureNow.Add(-5 * time.Minute), id: "m1", input: 10, agent: "a1"}),
		usageLine(usageFixture{at: fixtureNow.Add(-4 * time.Minute), id: "m2", input: 10, session: "s2"}),
	)
	record := fetchOK(t, dir)["claude-code/five_hour"]
	window := record.UsageWindow
	if window.ActiveAgents == nil || *window.ActiveAgents != 2 {
		t.Fatalf("active agents = %v, want 2", window.ActiveAgents)
	}
	if window.UsedPercent != nil || window.BurnPercentPerHour != nil || window.ResetsAt != nil {
		t.Fatalf("want no derived field without a ceiling, got %+v", window)
	}
	for _, key := range []string{absentUsedKey, absentBurnKey, absentResetKey} {
		if record.Absent[key] == "" {
			t.Fatalf("absent %s has no reason: %v", key, record.Absent)
		}
	}
}

// TestFetchUnknownCeilingKeepsTheLastReset: a rejection without priced
// usage before it calibrates nothing, but its reset time still stands.
func TestFetchUnknownCeilingKeepsTheLastReset(t *testing.T) {
	dir := t.TempDir()
	reset := fixtureNow.Add(2 * time.Hour)
	writeLog(t, dir, "p/s.jsonl", fixtureNow, rejectedLine(fixtureNow.Add(-time.Hour), "five_hour", reset))
	record := fetchOK(t, dir)["claude-code/five_hour"]
	window := record.UsageWindow
	if window.ResetsAt == nil || !window.ResetsAt.Equal(reset) {
		t.Fatalf("resets_at = %v, want %v", window.ResetsAt, reset)
	}
	if window.UsedPercent != nil || record.Absent[absentUsedKey] == "" || record.Absent[absentResetKey] != "" {
		t.Fatalf("want used_percent absent with reason and resets_at present, got %+v absent %v", window, record.Absent)
	}
	if window.ActiveAgents == nil || *window.ActiveAgents != 0 {
		t.Fatalf("active agents = %v, want a measured 0", window.ActiveAgents)
	}
}

// TestFetchRefusesMoreWindowsThanItsBound: rejections that name more window
// types than MaxRecords fail the source instead of dropping windows.
func TestFetchRefusesMoreWindowsThanItsBound(t *testing.T) {
	for _, tc := range []struct {
		extra int
		want  catalog.Status
	}{{MaxRecords - 2, catalog.StatusOK}, {MaxRecords - 1, catalog.StatusFail}} {
		dir := t.TempDir()
		lines := make([]string, 0, tc.extra)
		for i := 0; i < tc.extra; i++ {
			lines = append(lines, rejectedLine(fixtureNow.Add(-time.Hour), fmt.Sprintf("other_%02d", i), fixtureNow.Add(time.Hour)))
		}
		writeLog(t, dir, "p/s.jsonl", fixtureNow, lines...)
		res := Fetch(context.Background(), dir, fixtureNow, Options{})
		if res.Status != tc.want {
			t.Fatalf("%d extra windows: Status = %v, want %v: %s", tc.extra, res.Status, tc.want, res.Detail)
		}
		if tc.want == catalog.StatusFail && (len(res.Records) != 0 || !strings.Contains(res.Detail, "more than")) {
			t.Fatalf("a refused fetch keeps no records and says why: %d records, %q", len(res.Records), res.Detail)
		}
	}
}
