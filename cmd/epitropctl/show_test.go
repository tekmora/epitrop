package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

// failingWriter always refuses a Write, so a test can force renderTable's
// write-failure path without a real broken pipe.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func sampleSnapshot() catalog.Snapshot {
	price := 30.0
	ctx := int64(8192)
	return catalog.Snapshot{
		SchemaVersion: catalog.SnapshotSchemaVersion,
		GeneratedAt:   time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
		Records: []catalog.Record{{
			ModelID:       "vendor-a/model-1",
			AccessPath:    catalog.AccessAPI,
			ContextWindow: &ctx,
			PriceInPerM:   &price,
			Provenance: catalog.Provenance{
				Source:    "public-catalog:openrouter",
				FetchedAt: time.Now(),
				Kind:      catalog.KindDeclared,
			},
		}},
		SourceRuns: []catalog.SourceRun{
			{Source: "public-catalog", Status: catalog.StatusDegraded, Count: 1},
		},
	}
}

func TestReadSnapshotFile_Positive(t *testing.T) {
	snap := sampleSnapshot()
	data, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	got, err := readSnapshotFile(path)
	if err != nil {
		t.Fatalf("readSnapshotFile() = %v", err)
	}
	if len(got.Records) != 1 || got.Records[0].ModelID != "vendor-a/model-1" {
		t.Fatalf("readSnapshotFile() = %+v", got)
	}
}

func TestReadSnapshotFile_Negative(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := readSnapshotFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
			t.Fatal("readSnapshotFile() = nil error, want failure for a missing file")
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.json")
		if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
			t.Fatalf("setup: %v", err)
		}
		if _, err := readSnapshotFile(path); err == nil {
			t.Fatal("readSnapshotFile() = nil error, want failure for malformed JSON")
		}
	})
}

func TestReadSnapshotFileRejectsUnsupportedSchemaVersion(t *testing.T) {
	cases := map[string]string{
		"missing":     `{"generated_at":"2026-09-18T12:00:00Z","records":[],"source_runs":[]}`,
		"unsupported": `{"schema_version":99,"generated_at":"2026-09-18T12:00:00Z","records":[],"source_runs":[]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "snap.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("setup: %v", err)
			}
			_, err := readSnapshotFile(path)
			if err == nil {
				t.Fatal("readSnapshotFile() = nil error, want schema version failure")
			}
			if msg := err.Error(); !strings.Contains(msg, "re-run epitropctl sync") {
				t.Fatalf("error = %q, want guidance to re-run epitropctl sync", msg)
			}
		})
	}
}

// TestReadSnapshotFile_Boundary confirms the byte bound is enforced: a file
// one byte over maxSnapshotReadBytes must be rejected outright.
func TestReadSnapshotFile_Boundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	f, err := os.Create(path) // #nosec G304 -- path is big.json under t.TempDir
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := f.Truncate(maxSnapshotReadBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := readSnapshotFile(path); err == nil {
		t.Fatal("readSnapshotFile() = nil error, want failure for a file over the byte bound")
	}
}

func TestRenderTable_Positive(t *testing.T) {
	var buf bytes.Buffer
	if err := renderTable(&buf, sampleSnapshot()); err != nil {
		t.Fatalf("renderTable() error = %v, want nil", err)
	}
	out := buf.String()
	if !strings.Contains(out, "vendor-a/model-1") {
		t.Fatalf("renderTable() output missing model id: %s", out)
	}
	if !strings.Contains(out, "public-catalog") {
		t.Fatalf("renderTable() output missing source run line: %s", out)
	}
	if !strings.Contains(out, "degraded") {
		t.Fatalf("renderTable() output missing degraded status: %s", out)
	}
}

// TestRenderTable_Negative confirms a write failure surfaces as an error
// instead of being silently discarded.
func TestRenderTable_Negative(t *testing.T) {
	if err := renderTable(failingWriter{}, sampleSnapshot()); err == nil {
		t.Fatal("renderTable() = nil error, want failure when the writer refuses every write")
	}
}

// TestRenderTable_Boundary confirms an empty snapshot renders a header and
// a zero-record summary line instead of panicking or printing nothing.
func TestRenderTable_Boundary(t *testing.T) {
	var buf bytes.Buffer
	if err := renderTable(&buf, catalog.Snapshot{GeneratedAt: time.Now()}); err != nil {
		t.Fatalf("renderTable() error = %v, want nil", err)
	}
	if !strings.Contains(buf.String(), "0 records") {
		t.Fatalf("renderTable() = %q, want a 0 records summary line", buf.String())
	}
}

func TestFormatPtr_Negative(t *testing.T) {
	if got := formatInt64Ptr(nil); got != "-" {
		t.Fatalf("formatInt64Ptr(nil) = %q, want -", got)
	}
	if got := formatFloatPtr(nil); got != "-" {
		t.Fatalf("formatFloatPtr(nil) = %q, want -", got)
	}
}

func TestRunShow_Positive(t *testing.T) {
	snap := sampleSnapshot()
	data, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v", err)
	}
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := runShow([]string{"--in=" + path}); err != nil {
		t.Fatalf("runShow() = %v", err)
	}
}
