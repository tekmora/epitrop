package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// headroomTestSnapshot holds one model record without a usage window and one
// key budget record at 25% fetched fetchedAge before now.
func headroomTestSnapshot(now time.Time, fetchedAge time.Duration) catalog.Snapshot {
	snap := sampleSnapshot()
	used, minutes := 25.0, 43200
	snap.Records = append(snap.Records, catalog.Record{
		ModelID:     "litellm-key/agent-lane",
		AccessPath:  catalog.AccessGateway,
		UsageWindow: &catalog.UsageWindow{UsedPercent: &used, WindowMinutes: &minutes},
		Provenance:  catalog.Provenance{Source: "litellm-key-budget", FetchedAt: now.Add(-fetchedAge), Kind: catalog.KindMeasured},
	})
	return snap
}

func decodeHeadroom(t *testing.T, body []byte) []catalog.Headroom {
	t.Helper()
	var got []catalog.Headroom
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("headroom output %s: %v", body, err)
	}
	return got
}

func TestWriteHeadroomReadsTheUsageWindows(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := writeHeadroom(&out, headroomTestSnapshot(now, time.Minute), now, 10*time.Minute); err != nil {
		t.Fatalf("writeHeadroom() = %v", err)
	}
	got := decodeHeadroom(t, out.Bytes())
	if len(got) != 1 || got[0].State != catalog.HeadroomKnown || *got[0].UsedPercent != 25 || got[0].ModelID != "litellm-key/agent-lane" {
		t.Fatalf("headroom = %+v, want the key budget known at 25%%", got)
	}
}

func TestWriteHeadroomPrintsAStaleReadingAsUnknown(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := writeHeadroom(&out, headroomTestSnapshot(now, time.Hour), now, 10*time.Minute); err != nil {
		t.Fatalf("writeHeadroom() = %v", err)
	}
	got := decodeHeadroom(t, out.Bytes())
	if len(got) != 1 || got[0].State != catalog.HeadroomUnknown || got[0].UsedPercent != nil || bytes.Contains(out.Bytes(), []byte(`"used_percent":`)) {
		t.Fatalf("headroom = %s, want unknown without a used share", out.Bytes())
	}
}

// A snapshot without usage windows prints an empty list, not null.
func TestWriteHeadroomPrintsAnEmptyList(t *testing.T) {
	var out bytes.Buffer
	if err := writeHeadroom(&out, sampleSnapshot(), time.Now(), time.Minute); err != nil {
		t.Fatalf("writeHeadroom() = %v", err)
	}
	if strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("headroom = %q, want []", out.String())
	}
}

func TestWriteHeadroomReportsAWriteFailure(t *testing.T) {
	if err := writeHeadroom(failingWriter{}, sampleSnapshot(), time.Now(), time.Minute); err == nil || !strings.Contains(err.Error(), "write refused") {
		t.Fatalf("writeHeadroom(failing writer) = %v, want the write error", err)
	}
}

func TestRunHeadroomRefusesABadRequest(t *testing.T) {
	data, err := sampleSnapshot().Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for _, args := range [][]string{
		{"--in=" + path},
		{"--in=" + path, "--max-age=0s"},
		{"--in=" + path, "--max-age=-1m"},
		{"--in=" + path, "--max-age=soon"},
		{"--in=" + filepath.Join(t.TempDir(), "missing.json"), "--max-age=1m"},
	} {
		if err := runHeadroom(args); err == nil {
			t.Errorf("runHeadroom(%q) = nil, want an error", args)
		}
	}
	if err := runHeadroom([]string{"--in=" + path, "--max-age=1ns"}); err != nil {
		t.Errorf("runHeadroom(--max-age=1ns) = %v, want nil", err)
	}
}
