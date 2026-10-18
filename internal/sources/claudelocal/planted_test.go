package claudelocal

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// plantedSnapshot is the catalog's planted claude-local snapshot; the catalog
// package checks it against the schema, this test pins it to Fetch output.
var plantedSnapshot = filepath.Join("..", "..", "..", "catalog", "testdata", "claude-local.snapshot.json")

// TestFetchMatchesThePlantedSnapshot: one window with a calibrated ceiling and
// one without, written exactly as the planted snapshot holds them.
func TestFetchMatchesThePlantedSnapshot(t *testing.T) {
	dir := t.TempDir()
	previousReset := fixtureNow.Add(-time.Hour)
	writeLog(t, dir, "p/s.jsonl", fixtureNow,
		usageLine(usageFixture{at: fixtureNow.Add(-5 * time.Hour), id: "m1", input: million(1.5)}),
		usageLine(usageFixture{at: fixtureNow.Add(-3 * time.Hour), id: "m2", input: million(1)}),
		rejectedLine(fixtureNow.Add(-150*time.Minute), "five_hour", previousReset),
		usageLine(usageFixture{at: previousReset, id: "m3", input: million(0.125)}),
		usageLine(usageFixture{at: fixtureNow.Add(-30 * time.Minute), id: "m4", input: million(0.5), agent: "a1"}),
		usageLine(usageFixture{at: fixtureNow.Add(-10 * time.Minute), id: "m5", input: million(0.125), agent: "a2"}),
		usageLine(usageFixture{at: fixtureNow.Add(-5 * time.Minute), id: "m6", input: million(0.125), agent: "a3"}),
	)
	res := Fetch(context.Background(), dir, fixtureNow, Options{})
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, want ok: %s", res.Status, res.Detail)
	}
	snap := catalog.Snapshot{
		SchemaVersion: catalog.SnapshotSchemaVersion,
		GeneratedAt:   fixtureNow,
		Records:       res.Records,
		SourceRuns:    []catalog.SourceRun{{Source: SourceName, Status: res.Status, Count: res.Count}},
	}
	got, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	want, err := os.ReadFile(plantedSnapshot)
	if err != nil {
		t.Fatalf("read planted snapshot: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Fatalf("Fetch output differs from %s:\n%s", plantedSnapshot, got)
	}
}
