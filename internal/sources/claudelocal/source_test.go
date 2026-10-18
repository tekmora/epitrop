package claudelocal

// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// Every fixture here is synthetic: hand-made lines in the shape of a Claude
// Code session log, never a copy of a real transcript.

var fixtureNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// secretBody is built at run time so the source tree holds no secret-shaped literal.
var secretBody = "sk-" + "ant-" + strings.Repeat("Q", 40) + " password=" + strings.Repeat("z", 12)

type usageFixture struct {
	at                time.Time
	id, model         string
	input, output     int64
	session, agent    string
	speed, contentTxt string
}

// usageLine is one assistant line (fixture text is ASCII, so %q quotes it as JSON); one million input tokens of claude-opus-5-5 cost 4.
func usageLine(u usageFixture) string {
	if u.model == "" {
		u.model = "claude-opus-5-5"
	}
	if u.session == "" {
		u.session = "s1"
	}
	if u.contentTxt == "" {
		u.contentTxt = "ordinary reply"
	}
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":"req-%s","sessionId":%q,"agentId":%q,"cwd":"C:\\work","message":{"id":%q,"model":%q,"role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"speed":%q}}}`,
		u.at.Format(time.RFC3339Nano), u.id, u.session, u.agent, u.id, u.model, u.contentTxt, u.input, u.output, u.speed)
}

// rejectedLine is a rejected request whose message is an error string, not an object.
func rejectedLine(at time.Time, limitType string, resetsAt time.Time) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"isApiErrorMessage":true,"message":"usage limit reached","quotaLimits":{"status":"rejected","rateLimitType":%q,"resetsAt":%d,"isUsingOverage":false}}`,
		at.Format(time.RFC3339Nano), limitType, resetsAt.Unix())
}

func writeLog(t *testing.T, dir, name string, modTime time.Time, lines ...string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func fetchOK(t *testing.T, dir string) map[string]catalog.Record {
	t.Helper()
	res := Fetch(context.Background(), dir, fixtureNow, Options{})
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail %q, want ok", res.Status, res.Detail)
	}
	byID := make(map[string]catalog.Record, len(res.Records))
	for _, record := range res.Records {
		if err := record.Validate(); err != nil {
			t.Fatalf("record %s invalid: %v", record.ModelID, err)
		}
		byID[record.ModelID] = record
	}
	return byID
}

func million(n float64) int64 { return int64(n * 1_000_000) }

func assertNear(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestFetchWithoutLogsSkips(t *testing.T) {
	if res := Fetch(context.Background(), filepath.Join(t.TempDir(), "missing"), fixtureNow, Options{}); res.Status != catalog.StatusSkip {
		t.Fatalf("missing directory: Status = %v, want skip", res.Status)
	}
	dir := t.TempDir()
	writeLog(t, dir, "notes.txt", fixtureNow, "not a session log")
	if res := Fetch(context.Background(), dir, fixtureNow, Options{}); res.Status != catalog.StatusSkip {
		t.Fatalf("no *.jsonl: Status = %v, want skip", res.Status)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writeLog(t, dir, "p/s.jsonl", fixtureNow, usageLine(usageFixture{at: fixtureNow, id: "m1", input: 10}))
	if res := Fetch(ctx, dir, fixtureNow, Options{}); res.Status != catalog.StatusFail {
		t.Fatalf("canceled context: Status = %v, want fail", res.Status)
	}
}

func TestFetchWithoutRejectionLeavesCeilingUnknown(t *testing.T) {
	dir := t.TempDir()
	writeLog(t, dir, "p/s.jsonl", fixtureNow, usageLine(usageFixture{at: fixtureNow.Add(-time.Hour), id: "m1", input: million(1)}))
	records := fetchOK(t, dir)
	if len(records) != 2 {
		t.Fatalf("want the five_hour and seven_day records, got %d", len(records))
	}
	for id, minutes := range map[string]int{"claude-code/five_hour": 300, "claude-code/seven_day": 10080} {
		record := records[id]
		window := record.UsageWindow
		if window == nil || window.UsedPercent != nil || window.WindowMinutes == nil || *window.WindowMinutes != minutes {
			t.Fatalf("%s: want a %d-minute window without used_percent, got %+v", id, minutes, window)
		}
		if !strings.Contains(record.Absent["usage_window.used_percent"], "ceiling is unknown") {
			t.Fatalf("%s: the missing used_percent must say why: %q", id, record.Absent["usage_window.used_percent"])
		}
	}
}

// TestFetchRejectionCalibratesAndWindowRollsOver: the last window ended an
// hour ago with a rejection at 10 units; the current window holds 4 units.
func TestFetchRejectionCalibratesAndWindowRollsOver(t *testing.T) {
	dir := t.TempDir()
	previousReset := fixtureNow.Add(-time.Hour)
	writeLog(t, dir, "p/s.jsonl", fixtureNow,
		usageLine(usageFixture{at: fixtureNow.Add(-5 * time.Hour), id: "m1", input: million(1.5)}),
		usageLine(usageFixture{at: fixtureNow.Add(-3 * time.Hour), id: "m2", input: million(1)}),
		rejectedLine(fixtureNow.Add(-150*time.Minute), "five_hour", previousReset),
		rejectedLine(fixtureNow.Add(-140*time.Minute), "five_hour", previousReset),
		usageLine(usageFixture{at: previousReset, id: "m3", input: million(0.125)}),
		usageLine(usageFixture{at: fixtureNow.Add(-30 * time.Minute), id: "m4", input: million(0.5), agent: "a1"}),
		usageLine(usageFixture{at: fixtureNow.Add(-10 * time.Minute), id: "m5", input: million(0.125), agent: "a2"}),
		usageLine(usageFixture{at: fixtureNow.Add(-5 * time.Minute), id: "m6", input: million(0.125), agent: "a3"}),
	)
	window := fetchOK(t, dir)["claude-code/five_hour"].UsageWindow
	// Ceiling: 6 + 4 = 10 units held at the first rejection. Current window
	// [now-1h, now]: 0.5 (at its first instant) + 2 + 0.5 + 0.5 = 3.5 units.
	assertNear(t, "used_percent", window.UsedPercent, 35)
	// Burn: 1 unit in the last 15 minutes = 10% of the ceiling = 40%/h.
	assertNear(t, "burn_percent_per_hour", window.BurnPercentPerHour, 40)
	if window.ActiveAgents == nil || *window.ActiveAgents != 2 {
		t.Fatalf("active agents in the last 15 minutes = %v, want 2", window.ActiveAgents)
	}
	if want := previousReset.Add(5 * time.Hour); window.ResetsAt == nil || !window.ResetsAt.Equal(want) {
		t.Fatalf("reset = %v, want the last reported reset advanced one step to %v", window.ResetsAt, want)
	}
}

func TestCurrentResetAdvancesInWholeSteps(t *testing.T) {
	step := 5 * time.Hour
	cases := map[string]struct{ last, want time.Time }{
		"future reset kept":         {fixtureNow.Add(time.Minute), fixtureNow.Add(time.Minute)},
		"reset exactly now":         {fixtureNow, fixtureNow.Add(step)},
		"one step back":             {fixtureNow.Add(-time.Hour), fixtureNow.Add(4 * time.Hour)},
		"three steps back":          {fixtureNow.Add(-11 * time.Hour), fixtureNow.Add(4 * time.Hour)},
		"exactly two steps back":    {fixtureNow.Add(-2 * step), fixtureNow.Add(step)},
		"one nanosecond in future":  {fixtureNow.Add(time.Nanosecond), fixtureNow.Add(time.Nanosecond)},
		"one nanosecond before now": {fixtureNow.Add(-time.Nanosecond), fixtureNow.Add(step - time.Nanosecond)},
	}
	for name, tc := range cases {
		if got := currentReset(tc.last, step, fixtureNow); !got.Equal(tc.want) {
			t.Fatalf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestFetchWeeklyAndPerModelWindows(t *testing.T) {
	dir := t.TempDir()
	weeklyReset := fixtureNow.Add(24 * time.Hour)
	writeLog(t, dir, "p/s.jsonl", fixtureNow,
		usageLine(usageFixture{at: fixtureNow.Add(-5 * 24 * time.Hour), id: "m1", input: million(5)}),
		rejectedLine(fixtureNow.Add(-time.Hour), "seven_day_opus", weeklyReset),
		usageLine(usageFixture{at: fixtureNow.Add(-30 * time.Minute), id: "m2", model: "claude-sonnet-5-5", input: million(50)}),
	)
	records := fetchOK(t, dir)
	if len(records) != 3 {
		t.Fatalf("want five_hour, seven_day and seven_day_opus, got %d records", len(records))
	}
	opus := records["claude-code/seven_day_opus"].UsageWindow
	// Only opus usage counts toward the opus sub-limit: 20 units at the
	// rejection and 20 now; the 100 sonnet units after it do not count.
	assertNear(t, "seven_day_opus used_percent", opus.UsedPercent, 100)
	if opus.WindowMinutes == nil || *opus.WindowMinutes != 10080 {
		t.Fatalf("seven_day_opus is a weekly window: %+v", opus)
	}
	for _, id := range []string{"claude-code/five_hour", "claude-code/seven_day"} {
		if records[id].UsageWindow.UsedPercent != nil {
			t.Fatalf("%s must stay unknown: a seven_day_opus rejection calibrates only its own window", id)
		}
	}
}

func TestFetchRefusesWhatItCannotMeasure(t *testing.T) {
	cases := map[string]struct {
		lines  []string
		record string
		absent string
	}{
		"unpriced model in the window": {
			[]string{
				usageLine(usageFixture{at: fixtureNow.Add(-4 * time.Hour), id: "m1", input: million(1)}),
				rejectedLine(fixtureNow.Add(-3*time.Hour), "five_hour", fixtureNow.Add(time.Hour)),
				usageLine(usageFixture{at: fixtureNow.Add(-time.Hour), id: "m2", model: "claude-unknown-9", input: 10}),
			},
			"claude-code/five_hour", "usage_window.used_percent",
		},
		"weekly window older than the scanned logs": {
			[]string{
				usageLine(usageFixture{at: fixtureNow.Add(-6 * 24 * time.Hour), id: "m1", input: million(1)}),
				rejectedLine(fixtureNow.Add(-6*24*time.Hour), "seven_day", fixtureNow.Add(-time.Hour)),
			},
			"claude-code/seven_day", "usage_window.used_percent",
		},
		"unknown window type": {
			[]string{rejectedLine(fixtureNow.Add(-time.Hour), "monthly", fixtureNow.Add(time.Hour))},
			"claude-code/monthly", "usage_window",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeLog(t, dir, "p/s.jsonl", fixtureNow, tc.lines...)
			record := fetchOK(t, dir)[tc.record]
			if record.UsageWindow == nil || record.UsageWindow.UsedPercent != nil || record.Absent[tc.absent] == "" {
				t.Fatalf("want no used_percent and a stated %s gap, got %+v absent %v", tc.absent, record.UsageWindow, record.Absent)
			}
		})
	}
}

func TestFetchSkipsFilesOlderThanTheWeeklyWindow(t *testing.T) {
	lines := []string{
		usageLine(usageFixture{at: fixtureNow.Add(-4 * time.Hour), id: "m1", input: million(1)}),
		rejectedLine(fixtureNow.Add(-3*time.Hour), "five_hour", fixtureNow.Add(time.Hour)),
	}
	old := t.TempDir()
	writeLog(t, old, "p/s.jsonl", fixtureNow.Add(-weeklyWindow-time.Second), lines...)
	if fetchOK(t, old)["claude-code/five_hour"].UsageWindow.UsedPercent != nil {
		t.Fatal("a file last written before the weekly window must not be read")
	}
	recent := t.TempDir()
	writeLog(t, recent, "p/s.jsonl", fixtureNow.Add(-weeklyWindow+time.Second), lines...)
	assertNear(t, "used_percent", fetchOK(t, recent)["claude-code/five_hour"].UsageWindow.UsedPercent, 100)
}

func TestFetchCountsOneResponseOnceAndKeepsNoContent(t *testing.T) {
	dir := t.TempDir()
	repeated := usageFixture{at: fixtureNow.Add(-time.Hour), id: "m3", input: million(1), contentTxt: secretBody}
	writeLog(t, dir, "p/s.jsonl", fixtureNow,
		usageLine(usageFixture{at: fixtureNow.Add(-4 * time.Hour), id: "m1", input: million(2), contentTxt: secretBody}),
		rejectedLine(fixtureNow.Add(-3*time.Hour), "five_hour", fixtureNow.Add(-2*time.Hour)),
		usageLine(repeated), usageLine(repeated), usageLine(repeated),
		usageLine(usageFixture{at: fixtureNow.Add(-time.Minute), id: "m4", model: "<synthetic>", contentTxt: secretBody}),
	)
	writeLog(t, dir, "p/subagents/agent-1.jsonl", fixtureNow, usageLine(repeated))
	res := Fetch(context.Background(), dir, fixtureNow, Options{})
	// Ceiling 8 units; m3, logged four times in two files, counts once: 4 of 8.
	assertNear(t, "used_percent", fetchOK(t, dir)["claude-code/five_hour"].UsageWindow.UsedPercent, 50)
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{secretBody[:20], "password", "ordinary reply", `C:\work`} {
		if strings.Contains(string(data), leak) || strings.Contains(res.Detail, leak) {
			t.Fatalf("message content or metadata %q leaked into the result", leak)
		}
	}
}

func TestCostOf(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		u      usage
		want   float64
		priced bool
	}{
		{"input and output", "claude-opus-5-5", usage{InputTokens: 1e6, OutputTokens: 1e6}, 24, true},
		{"cache write split by TTL", "claude-opus-5-5", usage{CacheCreationInputTokens: 2e6, CacheCreation: &cacheCreation{Ephemeral5m: 1e6, Ephemeral1h: 1e6}}, 5 + 8, true},
		{"cache write without split is five-minute", "claude-sonnet-5-5", usage{CacheCreationInputTokens: 1e6}, 2.5, true},
		{"cache read", "claude-fable-5-1", usage{CacheReadInputTokens: 1e6}, 0.25, true},
		{"fast mode doubles", "claude-opus-5-5", usage{InputTokens: 1e6, Speed: "fast"}, 8, true},
		{"dated snapshot id", "claude-haiku-4-5-20251001", usage{InputTokens: 1e6}, 1, true},
		{"no tokens costs nothing", "<synthetic>", usage{}, 0, true},
		{"unknown model is unpriced", "claude-opus-9", usage{InputTokens: 1}, 0, false},
		{"prefix of a known id is not that id", "claude-opus", usage{InputTokens: 1}, 0, false},
	}
	for _, tc := range cases {
		got, priced := costOf(tc.model, tc.u)
		if priced != tc.priced || math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("%s: got %v priced %v, want %v priced %v", tc.name, got, priced, tc.want, tc.priced)
		}
	}
}

// TestFetchTakesTheSmallestObservedCeiling: two past windows hit the limit
// at 12 and at 8 units; the current one holds 4, which is half of 8.
func TestFetchTakesTheSmallestObservedCeiling(t *testing.T) {
	dir := t.TempDir()
	first, second := fixtureNow.Add(-6*time.Hour), fixtureNow.Add(-time.Hour)
	writeLog(t, dir, "p/s.jsonl", fixtureNow,
		usageLine(usageFixture{at: first.Add(-4 * time.Hour), id: "m1", input: million(3)}),
		rejectedLine(first.Add(-3*time.Hour), "five_hour", first),
		usageLine(usageFixture{at: second.Add(-4 * time.Hour), id: "m2", input: million(2)}),
		rejectedLine(second.Add(-3*time.Hour), "five_hour", second),
		usageLine(usageFixture{at: fixtureNow.Add(-20 * time.Minute), id: "m3", input: million(1)}),
	)
	assertNear(t, "used_percent", fetchOK(t, dir)["claude-code/five_hour"].UsageWindow.UsedPercent, 50)
}
