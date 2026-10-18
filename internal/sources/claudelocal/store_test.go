package claudelocal

// Ported from cordanaLLM/praetor tribunus/internal/sources/claudelocal at 5d4acfb (EUPL-1.2).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// calibrationLines: 10 units before a rejection in the window that reset at
// fixtureNow-1h, nothing after it.
func calibrationLines() []string {
	return []string{
		usageLine(usageFixture{at: fixtureNow.Add(-3 * time.Hour), id: "m1", input: million(2.5)}),
		rejectedLine(fixtureNow.Add(-2*time.Hour), "five_hour", fixtureNow.Add(-time.Hour)),
	}
}

func fetchAt(t *testing.T, logs string, at time.Time, opts Options) Result {
	t.Helper()
	res := Fetch(context.Background(), logs, at, opts)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail %q, want ok", res.Status, res.Detail)
	}
	return res
}

func fiveHour(t *testing.T, res Result) catalog.Record {
	t.Helper()
	for _, record := range res.Records {
		if record.ModelID == "claude-code/five_hour" {
			return record
		}
	}
	t.Fatal("no five_hour record")
	return catalog.Record{}
}

// laterLogs is a log folder written at `at`, holding units of usage 20
// minutes before it and no rejection.
func laterLogs(t *testing.T, at time.Time, units float64) string {
	t.Helper()
	dir := t.TempDir()
	writeLog(t, dir, "p/later.jsonl", at, usageLine(usageFixture{at: at.Add(-20 * time.Minute), id: "later", input: million(units / 4)}))
	return dir
}

func TestStoredCeilingSurvivesResetsAndRestarts(t *testing.T) {
	state := t.TempDir()
	first := t.TempDir()
	writeLog(t, first, "p/s.jsonl", fixtureNow, calibrationLines()...)
	fetchAt(t, first, fixtureNow, Options{StateDir: state})
	stored, err := loadStore(storePath(state, DefaultAccount), DefaultAccount)
	if err != nil {
		t.Fatal(err)
	}
	entry := stored.Ceilings["five_hour"]
	if entry.Ceiling != 10 || entry.SourceFile != "p/s.jsonl" || !entry.RejectedAt.Equal(fixtureNow.Add(-2*time.Hour)) || !entry.RecordedAt.Equal(fixtureNow) {
		t.Fatalf("stored entry %+v, want ceiling 10 from p/s.jsonl rejected at now-2h, recorded now", entry)
	}
	// Eight days on, the rejection has left the scanned logs; a new process
	// reads the store. The reset advances from the stored one in 5 h steps.
	later := fixtureNow.Add(8 * 24 * time.Hour)
	record := fiveHour(t, fetchAt(t, laterLogs(t, later, 4), later, Options{StateDir: state}))
	assertNear(t, "used_percent after eight days", record.UsageWindow.UsedPercent, 40)
	steps := later.Sub(fixtureNow.Add(-time.Hour))/(5*time.Hour) + 1
	if want := fixtureNow.Add(-time.Hour).Add(steps * 5 * time.Hour); !record.UsageWindow.ResetsAt.Equal(want) {
		t.Fatalf("reset %v, want %v", record.UsageWindow.ResetsAt, want)
	}
}

func TestStoredCeilingAge(t *testing.T) {
	state := t.TempDir()
	first := t.TempDir()
	writeLog(t, first, "p/s.jsonl", fixtureNow, calibrationLines()...)
	fetchAt(t, first, fixtureNow, Options{StateDir: state})
	rejectedAt := fixtureNow.Add(-2 * time.Hour)
	cases := []struct {
		name   string
		at     time.Time
		maxAge time.Duration
		known  bool
	}{
		{"exactly the default bound", rejectedAt.Add(DefaultMaxCeilingAge), 0, true},
		{"one second past the default bound", rejectedAt.Add(DefaultMaxCeilingAge + time.Second), 0, false},
		{"within a configured bound", rejectedAt.Add(time.Hour), 2 * time.Hour, true},
		{"past a configured bound", rejectedAt.Add(3 * time.Hour), 2 * time.Hour, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := fiveHour(t, fetchAt(t, laterLogs(t, tc.at, 1), tc.at, Options{StateDir: state, MaxCeilingAge: tc.maxAge}))
			if known := record.UsageWindow.UsedPercent != nil; known != tc.known {
				t.Fatalf("known = %v, want %v (absent %q)", known, tc.known, record.Absent["usage_window.used_percent"])
			}
			if !tc.known && !strings.Contains(record.Absent["usage_window.used_percent"], "unknown again") {
				t.Fatalf("a stale ceiling must say so: %q", record.Absent["usage_window.used_percent"])
			}
		})
	}
}

func TestStoreKeepsTheNewestObservation(t *testing.T) {
	state := t.TempDir()
	path := storePath(state, DefaultAccount)
	seed := func(ceiling float64, rejectedAt time.Time) {
		t.Helper()
		store := &ceilingStore{Version: storeVersion, Account: DefaultAccount, Ceilings: map[string]storedCeiling{"five_hour": {
			Ceiling: ceiling, RejectedAt: rejectedAt, SourceFile: "old.jsonl", WindowReset: rejectedAt.Add(time.Hour), RecordedAt: rejectedAt}}}
		if err := saveStore(path, store); err != nil {
			t.Fatal(err)
		}
	}
	logs := t.TempDir()
	writeLog(t, logs, "p/s.jsonl", fixtureNow, calibrationLines()...)

	seed(20, fixtureNow.Add(-3*24*time.Hour))
	fetchAt(t, logs, fixtureNow, Options{StateDir: state})
	if store, err := loadStore(path, DefaultAccount); err != nil || store.Ceilings["five_hour"].Ceiling != 10 {
		t.Fatalf("a newer rejection in the logs must replace the stored ceiling: %+v %v", store, err)
	}

	seed(20, fixtureNow.Add(-30*time.Minute))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record := fiveHour(t, fetchAt(t, logs, fixtureNow, Options{StateDir: state}))
	assertNear(t, "used_percent against the newer stored ceiling", record.UsageWindow.UsedPercent, 50)
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Fatalf("an older observation must leave the store as it was (err %v)", err)
	}
}

func TestStoreWriteIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "default.json")
	if err := replaceFile(path, []byte("old store\n"), writeAndSync); err != nil {
		t.Fatal(err)
	}
	assertInterruptedWriteFails(t, path)
	assertLeftoverStagedFileReplaced(t, path)
}

func assertInterruptedWriteFails(t *testing.T, path string) {
	t.Helper()
	interrupted := func(f *os.File, data []byte) error {
		if _, err := f.Write(data[:len(data)/2]); err != nil {
			return err
		}
		return errors.New("simulated interruption")
	}
	if err := replaceFile(path, []byte(strings.Repeat("new store ", 50)), interrupted); err == nil {
		t.Fatal("an interrupted write must fail")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "old store\n" {
		t.Fatalf("the store must keep its old contents, got %q (%v)", data, err)
	}
	if _, err := os.Stat(path + pendingSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no staged file may remain after a failed write: %v", err)
	}
}

func assertLeftoverStagedFileReplaced(t *testing.T, path string) {
	t.Helper()
	// A staged file a killed process left behind is ignored and replaced.
	if err := os.WriteFile(path+pendingSuffix, []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(path, []byte("new store\n"), writeAndSync); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "new store\n" {
		t.Fatalf("got %q (%v)", data, err)
	}
	if _, err := os.Stat(path + pendingSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file must be renamed away: %v", err)
	}
}

func TestCorruptStoreIsRefused(t *testing.T) {
	valid := `{"version":1,"account":"default","ceilings":{"five_hour":{"ceiling_usd_equivalent":10,"rejected_at":"2026-10-13T10:00:00Z","source_file":"p/s.jsonl","window_reset":"2026-10-13T11:00:00Z","recorded_at":"2026-10-13T12:00:00Z"}}}`
	cases := map[string]string{
		"not json":           "not json",
		"truncated":          valid[:len(valid)/2],
		"unknown field":      strings.Replace(valid, `"version":1`, `"version":1,"extra":true`, 1),
		"data after value":   valid + `{}`,
		"wrong version":      strings.Replace(valid, `"version":1`, `"version":2`, 1),
		"other account":      strings.Replace(valid, `"account":"default"`, `"account":"other"`, 1),
		"zero ceiling":       strings.Replace(valid, `"ceiling_usd_equivalent":10`, `"ceiling_usd_equivalent":0`, 1),
		"negative ceiling":   strings.Replace(valid, `"ceiling_usd_equivalent":10`, `"ceiling_usd_equivalent":-1`, 1),
		"unknown window":     strings.Replace(valid, `"five_hour"`, `"monthly"`, 1),
		"no source file":     strings.Replace(valid, `"source_file":"p/s.jsonl"`, `"source_file":""`, 1),
		"missing timestamp":  strings.Replace(valid, `,"recorded_at":"2026-10-13T12:00:00Z"`, ``, 1),
		"oversized":          valid + strings.Repeat(" ", maxStoreBytes),
		"ceiling not number": strings.Replace(valid, `"ceiling_usd_equivalent":10`, `"ceiling_usd_equivalent":"10"`, 1),
	}
	logs := t.TempDir()
	writeLog(t, logs, "p/s.jsonl", fixtureNow, calibrationLines()...)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			checkCorruptStoreBody(t, logs, body)
		})
	}
	checkValidStoreLoads(t, logs, valid)
}

func checkCorruptStoreBody(t *testing.T, logs, body string) {
	t.Helper()
	state := t.TempDir()
	path := storePath(state, DefaultAccount)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Fetch(context.Background(), logs, fixtureNow, Options{StateDir: state})
	if res.Status != catalog.StatusFail || len(res.Records) != 0 {
		t.Fatalf("Status = %v, %d records, want a refusal: %q", res.Status, len(res.Records), res.Detail)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != body {
		t.Fatal("a refused store must be left as it was")
	}
}

func checkValidStoreLoads(t *testing.T, logs, valid string) {
	t.Helper()
	state := t.TempDir()
	path := storePath(state, DefaultAccount)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := Fetch(context.Background(), logs, fixtureNow, Options{StateDir: state}); res.Status != catalog.StatusOK {
		t.Fatalf("the valid store must load: %q", res.Detail)
	}
}

func TestStoreOptionsAreChecked(t *testing.T) {
	logs := t.TempDir()
	writeLog(t, logs, "p/s.jsonl", fixtureNow, calibrationLines()...)
	for name, opts := range map[string]Options{
		"account with a path separator": {StateDir: t.TempDir(), Account: "../x"},
		"upper-case account":            {StateDir: t.TempDir(), Account: "Main"},
		"negative maximum age":          {MaxCeilingAge: -time.Hour},
	} {
		if res := Fetch(context.Background(), logs, fixtureNow, opts); res.Status != catalog.StatusFail {
			t.Fatalf("%s: Status = %v, want fail", name, res.Status)
		}
	}
	if res := fetchAt(t, logs, fixtureNow, Options{StateDir: t.TempDir(), Account: "max-a.2"}); len(res.Records) == 0 {
		t.Fatal("a valid account name must be accepted")
	}
}

func TestCountLimitNames(t *testing.T) {
	dir := t.TempDir()
	quota := func(limitType, status string) string {
		return `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","message":"x","quotaLimits":{"status":"` + status + `","rateLimitType":"` + limitType + `","resetsAt":1}}`
	}
	writeLog(t, dir, "p/old.jsonl", fixtureNow.Add(-90*24*time.Hour), quota("five_hour", "rejected"), quota("five_hour", "rejected"))
	writeLog(t, dir, "p/new.jsonl", fixtureNow, quota("seven_day", "allowed_warning"), quota("Five Hour!", "rejected"),
		usageLine(usageFixture{at: fixtureNow, id: "m1", input: 10, contentTxt: secretBody}))
	counts, err := CountLimitNames(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 || counts.QuotaLines != 4 {
		t.Fatalf("files %d quota lines %d, want 2 and 4 (old files count too)", counts.Files, counts.QuotaLines)
	}
	if got := strings.Join(SortedKeys(counts.Types), ","); got != "five_hour,<other>,seven_day" {
		t.Fatalf("types %v (%s)", counts.Types, got)
	}
	if counts.Statuses["rejected"] != 3 || counts.Statuses["allowed_warning"] != 1 {
		t.Fatalf("statuses %v", counts.Statuses)
	}
	if _, err := CountLimitNames(context.Background(), filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("a missing folder counts nothing and is no error: %v", err)
	}
}
