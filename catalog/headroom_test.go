package catalog

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

var headroomNow = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

const headroomMaxAge = 10 * time.Minute

// usageRecord is a codex-like usage record fetched age before headroomNow,
// whose window resets an hour after headroomNow.
func usageRecord(age time.Duration, used *float64) Record {
	minutes := 300
	resets := headroomNow.Add(time.Hour)
	return Record{
		ModelID:     "codex-cli/codex",
		AccessPath:  AccessSubscriptionCLI,
		UsageWindow: &UsageWindow{UsedPercent: used, WindowMinutes: &minutes, ResetsAt: &resets},
		Provenance:  Provenance{Source: "codex-local", FetchedAt: headroomNow.Add(-age), Kind: KindMeasured},
	}
}

func percent(v float64) *float64 { return &v }

func headroomSnapshot(records ...Record) Snapshot {
	return Snapshot{SchemaVersion: SnapshotSchemaVersion, GeneratedAt: headroomNow, Records: records}
}

func readOneForTest(t *testing.T, r Record) Headroom {
	t.Helper()
	got, err := ReadHeadroom(headroomSnapshot(r), headroomNow, headroomMaxAge)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadHeadroom() = %+v, %v, want one reading", got, err)
	}
	return got[0]
}

func TestReadHeadroomKnowsAFreshReading(t *testing.T) {
	model := validRecord()
	got, err := ReadHeadroom(headroomSnapshot(model, usageRecord(time.Minute, percent(42.5))), headroomNow, headroomMaxAge)
	if err != nil || len(got) != 1 {
		t.Fatalf("ReadHeadroom() = %+v, %v, want only the usage record read", got, err)
	}
	body, err := json.Marshal(got[0])
	want := `{"model_id":"codex-cli/codex","source":"codex-local","state":"known","used_percent":42.5,"window_minutes":300,` +
		`"resets_at":"2026-10-17T13:00:00Z","fetched_at":"2026-10-17T11:59:00Z"}`
	if err != nil || string(body) != want {
		t.Fatalf("reading = %s, %v, want %s", body, err, want)
	}
}

// An overrun window is real usage, not a broken reading.
func TestReadHeadroomKnowsAnOverrun(t *testing.T) {
	if over := readOneForTest(t, usageRecord(0, percent(130))); over.State != HeadroomKnown {
		t.Fatalf("reading at 130%% = %+v, want known", over)
	}
}

func TestReadHeadroomNeverReadsUnknownAsZero(t *testing.T) {
	past := headroomNow.Add(-time.Second)
	reset := usageRecord(time.Minute, percent(80))
	reset.UsageWindow.ResetsAt = &past
	cases := []struct {
		name   string
		record Record
		want   string
	}{
		{"stale", usageRecord(time.Hour, percent(10)), "stale: fetched 1h0m0s ago"},
		{"no share", usageRecord(time.Minute, nil), "no used_percent"},
		{"negative", usageRecord(time.Minute, percent(-1)), "out of range"},
		{"not a number", usageRecord(time.Minute, percent(math.NaN())), "out of range"},
		{"infinite", usageRecord(time.Minute, percent(math.Inf(1))), "out of range"},
		{"from the future", usageRecord(-time.Second, percent(10)), "after now"},
		{"window reset", reset, "the window reset at"},
	}
	for _, tc := range cases {
		h := readOneForTest(t, tc.record)
		if h.State != HeadroomUnknown || h.UsedPercent != nil || !strings.Contains(h.Reason, tc.want) {
			t.Errorf("%s: reading = %+v, want unknown with %q and no used share", tc.name, h, tc.want)
		}
		body, err := json.Marshal(h)
		if err != nil || strings.Contains(string(body), `"used_percent":`) {
			t.Errorf("%s: json = %s, %v, want no used_percent key", tc.name, body, err)
		}
	}
}

// At exactly the max age a reading is known; one nanosecond older it is not.
// A window that resets exactly now has reset.
func TestReadHeadroomBoundaries(t *testing.T) {
	if h := readOneForTest(t, usageRecord(headroomMaxAge, percent(1))); h.State != HeadroomKnown {
		t.Errorf("reading at the max age = %+v, want known", h)
	}
	if h := readOneForTest(t, usageRecord(headroomMaxAge+time.Nanosecond, percent(1))); h.State != HeadroomUnknown {
		t.Errorf("reading past the max age = %+v, want unknown", h)
	}
	if h := readOneForTest(t, usageRecord(0, percent(0))); h.State != HeadroomKnown || *h.UsedPercent != 0 {
		t.Errorf("fresh reading at 0%% = %+v, want known zero", h)
	}
	resetNow := usageRecord(time.Minute, percent(5))
	resetNow.UsageWindow.ResetsAt = &headroomNow
	if h := readOneForTest(t, resetNow); h.State != HeadroomUnknown {
		t.Errorf("reading whose window resets now = %+v, want unknown", h)
	}
}

func TestReadHeadroomRefusesABadRequest(t *testing.T) {
	for _, maxAge := range []time.Duration{0, -time.Second} {
		if _, err := ReadHeadroom(headroomSnapshot(), headroomNow, maxAge); err == nil || !strings.Contains(err.Error(), "not positive") {
			t.Errorf("ReadHeadroom(max age %s) = %v, want it refused", maxAge, err)
		}
	}
	bad := headroomSnapshot(usageRecord(0, percent(1)))
	bad.SchemaVersion = 0
	if _, err := ReadHeadroom(bad, headroomNow, headroomMaxAge); err == nil {
		t.Error("ReadHeadroom(invalid snapshot) = nil, want an error")
	}
	got, err := ReadHeadroom(headroomSnapshot(validRecord()), headroomNow, headroomMaxAge)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("ReadHeadroom(no usage records) = %#v, %v, want an empty list", got, err)
	}
}
