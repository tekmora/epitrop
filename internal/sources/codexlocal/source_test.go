package codexlocal

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/catalog"
)

var fixtureNow = time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)

func writeSession(t *testing.T, dir, name, body string, modTime time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	return path
}

const validLine = `{"timestamp":"2026-09-13T12:56:47.753Z","ordinal":1,"type":"event_msg","payload":{"type":"token_count","info":{},"rate_limits":{"limit_id":"codex","limit_name":null,"primary":{"used_percent":92.0,"window_minutes":10080,"resets_at":1789858494},"secondary":null,"credits":{"has_credits":false,"unlimited":false,"balance":"0"},"plan_type":"pro"}}}`

func TestUpstreamContractDocumentPinned(t *testing.T) {
	payload := mustReadFixture(t, "testdata/upstream/rate_limit_status_payload.rs")
	details := mustReadFixture(t, "testdata/upstream/rate_limit_status_details.rs")
	window := mustReadFixture(t, "testdata/upstream/rate_limit_window_snapshot.rs")
	for _, marker := range [][]byte{
		[]byte(`rename = "plan_type"`),
		[]byte(`rename = "primary_window"`),
		[]byte(`rename = "used_percent"`),
		[]byte(`rename = "reset_at"`),
	} {
		joined := append(append([]byte{}, payload...), details...)
		joined = append(joined, window...)
		if !bytes.Contains(joined, marker) {
			t.Fatalf("Codex upstream contract missing marker %q", marker)
		}
	}
}

func TestFetch_UpstreamFixtureContract(t *testing.T) {
	body := string(mustReadFixture(t, "testdata/fixtures/rate-limits.jsonl"))
	dir := t.TempDir()
	writeSession(t, dir, "valid.jsonl", body, fixtureNow)
	res := Fetch(context.Background(), dir, fixtureNow)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}

	mutated := strings.Replace(body, `"used_percent":42.0`, `"used_percent":"42.0"`, 1)
	dir = t.TempDir()
	writeSession(t, dir, "mutated.jsonl", mutated, fixtureNow)
	res = Fetch(context.Background(), dir, fixtureNow)
	if res.Status != catalog.StatusFail {
		t.Fatalf("Status = %v, detail = %q, want fail for retyped used_percent", res.Status, res.Detail)
	}
}

func TestFetch_Positive(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "old.jsonl", validLine, fixtureNow.Add(-time.Hour))
	// Newest file carries a later rate_limits reading than the older one.
	newer := strings.Replace(validLine, `"used_percent":92.0`, `"used_percent":45.5`, 1)
	writeSession(t, dir, "new.jsonl", validLine+"\n"+newer, fixtureNow)

	res := Fetch(context.Background(), dir, fixtureNow)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}
	if res.Count != 1 || len(res.Records) != 1 {
		t.Fatalf("got %d records, want 1", res.Count)
	}
	assertCodexRecord(t, res.Records[0])
}

func assertCodexRecord(t *testing.T, rec catalog.Record) {
	t.Helper()
	if rec.ModelID != "codex-cli/codex" {
		t.Fatalf("ModelID = %q, want codex-cli/codex", rec.ModelID)
	}
	if rec.UsageWindow == nil || rec.UsageWindow.UsedPercent == nil || *rec.UsageWindow.UsedPercent != 45.5 {
		t.Fatalf("UsageWindow = %+v, want used_percent from the newest file's last line (45.5)", rec.UsageWindow)
	}
	if rec.Provenance.Kind != catalog.KindMeasured || rec.Provenance.Source != SourceName {
		t.Fatalf("Provenance = %+v", rec.Provenance)
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func mustReadFixture(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func TestFetch_Negative(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		res := Fetch(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), fixtureNow)
		if res.Status != catalog.StatusSkip {
			t.Fatalf("Status = %v, want skip for a missing sessions directory", res.Status)
		}
	})

	t.Run("no jsonl files", func(t *testing.T) {
		dir := t.TempDir()
		writeSession(t, dir, "notes.txt", "irrelevant", fixtureNow)
		res := Fetch(context.Background(), dir, fixtureNow)
		if res.Status != catalog.StatusSkip {
			t.Fatalf("Status = %v, want skip when no *.jsonl files exist", res.Status)
		}
	})

	t.Run("malformed newest session is not silently guessed", func(t *testing.T) {
		dir := t.TempDir()
		writeSession(t, dir, "broken.jsonl", "not json at all\n{\"payload\":", fixtureNow)
		res := Fetch(context.Background(), dir, fixtureNow)
		// Malformed lines are skipped individually; a file with none parseable
		// yields a skip (no rate_limits found), never a fabricated record.
		if res.Status != catalog.StatusSkip {
			t.Fatalf("Status = %v, detail=%q, want skip for a file with no parseable rate_limits", res.Status, res.Detail)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		dir := t.TempDir()
		writeSession(t, dir, "new.jsonl", validLine, fixtureNow)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res := Fetch(ctx, dir, fixtureNow)
		if res.Status != catalog.StatusFail {
			t.Fatalf("Status = %v, want fail for an already-canceled context", res.Status)
		}
	})
}

// TestFetch_Boundary covers a session with rate_limits present but no
// primary window on every line (secondary-only), which must skip rather
// than fabricate.
func TestFetch_Boundary(t *testing.T) {
	dir := t.TempDir()
	line := `{"timestamp":"2026-09-13T12:56:47.753Z","ordinal":1,"type":"event_msg","payload":{"rate_limits":{"limit_id":"codex","primary":null,"secondary":null,"plan_type":"pro"}}}`
	writeSession(t, dir, "new.jsonl", line, fixtureNow)

	res := Fetch(context.Background(), dir, fixtureNow)
	if res.Status != catalog.StatusSkip {
		t.Fatalf("Status = %v, detail=%q, want skip when rate_limits.primary is null", res.Status, res.Detail)
	}
}

// TestFetch_SessionEndsWithNullPrimary reproduces a real shape found live on
// this workstation on 2026-09-18: the newest session's very last
// rate_limits line has primary:null (a session-end event), while an
// earlier line in the same file has a real reading. Fetch must report that
// earlier reading, not skip -- the file has usable data, it just is not on
// the last line.
func TestFetch_SessionEndsWithNullPrimary(t *testing.T) {
	dir := t.TempDir()
	nullPrimaryLine := `{"timestamp":"2026-09-13T13:13:28.673Z","ordinal":2,"type":"event_msg","payload":{"rate_limits":{"limit_id":"codex","primary":null,"secondary":null,"plan_type":"pro"}}}`
	writeSession(t, dir, "new.jsonl", validLine+"\n"+nullPrimaryLine, fixtureNow)

	res := Fetch(context.Background(), dir, fixtureNow)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok using the earlier non-null reading", res.Status, res.Detail)
	}
	if res.Records[0].UsageWindow == nil || res.Records[0].UsageWindow.UsedPercent == nil || *res.Records[0].UsageWindow.UsedPercent != 92.0 {
		t.Fatalf("UsageWindow = %+v, want the earlier line's used_percent=92.0", res.Records[0].UsageWindow)
	}
}

func TestFetch_EmptyLimitIDFails(t *testing.T) {
	dir := t.TempDir()
	line := `{"timestamp":"2026-09-13T12:56:47.753Z","ordinal":1,"type":"event_msg","payload":{"rate_limits":{"limit_id":"","primary":{"used_percent":10.0,"window_minutes":10080,"resets_at":1789858494},"plan_type":"pro"}}}`
	writeSession(t, dir, "new.jsonl", line, fixtureNow)

	res := Fetch(context.Background(), dir, fixtureNow)
	if res.Status != catalog.StatusFail {
		t.Fatalf("Status = %v, detail = %q, want fail", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "limit_id") {
		t.Fatalf("Detail = %q, want limit_id reason", res.Detail)
	}
}

func TestDefaultSessionsDir(t *testing.T) {
	t.Run("resolves under HOME", func(t *testing.T) {
		tempHome := t.TempDir()
		t.Setenv("HOME", tempHome)
		t.Setenv("USERPROFILE", tempHome)
		got, err := DefaultSessionsDir()
		if err != nil {
			t.Fatalf("DefaultSessionsDir() = %v, want nil", err)
		}
		want := filepath.Join(tempHome, ".codex", "sessions")
		if got != want {
			t.Fatalf("DefaultSessionsDir() = %q, want %q", got, want)
		}
	})

	t.Run("fails when the home directory is unset", func(t *testing.T) {
		t.Setenv("HOME", "")
		t.Setenv("USERPROFILE", "")
		_, err := DefaultSessionsDir()
		if err == nil || !strings.Contains(err.Error(), "resolve home directory") {
			t.Fatalf("DefaultSessionsDir() with empty HOME = %v, want error", err)
		}
	})
}

func TestCloseSessionFile(t *testing.T) {
	t.Run("closes open file and preserves cause", testCloseSessionFilePreservesCause)
	t.Run("joins close error on already closed file", testCloseSessionFileJoinsError)
	t.Run("nil cause with already closed file", testCloseSessionFileNilCause)
}

func testCloseSessionFilePreservesCause(t *testing.T) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "session-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	cause := errors.New("open-cause")
	got := closeSessionFile(f, cause)
	if !errors.Is(got, cause) {
		t.Fatalf("closeSessionFile() = %v, want cause %v", got, cause)
	}
	if err = f.Close(); err == nil {
		t.Fatal("second Close() = nil, want error since file was closed")
	}
}

func testCloseSessionFileJoinsError(t *testing.T) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "session-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if err = f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cause := errors.New("prior-cause")
	got := closeSessionFile(f, cause)
	if !errors.Is(got, cause) {
		t.Fatalf("closeSessionFile() = %v, want cause %v", got, cause)
	}
	if !errors.Is(got, os.ErrClosed) {
		t.Fatalf("closeSessionFile() = %v, want joined os.ErrClosed", got)
	}
}

func testCloseSessionFileNilCause(t *testing.T) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "session-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if err = f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := closeSessionFile(f, nil)
	if !errors.Is(got, os.ErrClosed) {
		t.Fatalf("closeSessionFile() = %v, want os.ErrClosed", got)
	}
}

type fakeDirEntry struct{}

func (fakeDirEntry) Name() string               { return "file.txt" }
func (fakeDirEntry) IsDir() bool                { return false }
func (fakeDirEntry) Type() fs.FileMode          { return 0 }
func (fakeDirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrInvalid }

func TestSessionScan_FilesScannedCap(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	scan := &sessionScan{root: root}
	entry := fakeDirEntry{}

	for i := 0; i < maxSessionFilesScanned; i++ {
		if err := scan.consider(ctx, path, entry, nil); err != nil {
			t.Fatalf("consider(%d) = %v, want nil error", i, err)
		}
	}
	if scan.scanned != maxSessionFilesScanned {
		t.Fatalf("scan.scanned = %d, want %d", scan.scanned, maxSessionFilesScanned)
	}

	err := scan.consider(ctx, path, entry, nil)
	if err == nil || !strings.Contains(err.Error(), "files scanned") {
		t.Fatalf("consider(max+1) = %v, want error with 'files scanned'", err)
	}
}

func TestOpenRegularSessionFile_SizeCap(t *testing.T) {
	t.Run("exactly maxSessionFileBytes opens", testOpenRegularSessionFileExactCap)
	t.Run("maxSessionFileBytes+1 fails with exceeds", testOpenRegularSessionFileOverCap)
}

func testOpenRegularSessionFileExactCap(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exact.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Truncate(path, maxSessionFileBytes); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	opened, err := openRegularSessionFile(path)
	if err != nil {
		t.Fatalf("openRegularSessionFile(cap) = %v, want nil error", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("Close opened: %v", err)
	}
}

func testOpenRegularSessionFileOverCap(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "over.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.Truncate(path, maxSessionFileBytes+1); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	_, err = openRegularSessionFile(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("openRegularSessionFile(cap+1) err = %v, want error with 'exceeds'", err)
	}
}

func TestScanLatestRateLimits_MaxScanLines(t *testing.T) {
	ctx := context.Background()
	t.Run("maxScanLines passes", func(t *testing.T) {
		s := bufio.NewScanner(strings.NewReader(strings.Repeat("\n", maxScanLines)))
		if _, err := scanLatestRateLimits(ctx, "session.jsonl", s); err != nil {
			t.Fatalf("scanLatestRateLimits(maxScanLines) = %v, want nil error", err)
		}
	})

	t.Run("maxScanLines+1 fails", func(t *testing.T) {
		s := bufio.NewScanner(strings.NewReader(strings.Repeat("\n", maxScanLines+1)))
		_, err := scanLatestRateLimits(ctx, "session.jsonl", s)
		if err == nil || !strings.Contains(err.Error(), "lines scanned") {
			t.Fatalf("scanLatestRateLimits(maxScanLines+1) = %v, want 'lines scanned'", err)
		}
	})
}

func TestLatestRateLimits_MaxLineBytes(t *testing.T) {
	ctx := context.Background()

	t.Run("longest line scans", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "max.jsonl")
		line := make([]byte, maxLineBytes)
		for i := 0; i < maxLineBytes-1; i++ {
			line[i] = ' '
		}
		line[maxLineBytes-1] = '\n'
		if err := os.WriteFile(path, line, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := latestRateLimits(ctx, path); err != nil {
			t.Fatalf("latestRateLimits() = %v, want nil error", err)
		}
	})

	t.Run("one byte longer fails with bufio.ErrTooLong", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "over.jsonl")
		line := make([]byte, maxLineBytes+1)
		for i := 0; i < maxLineBytes; i++ {
			line[i] = ' '
		}
		line[maxLineBytes] = '\n'
		if err := os.WriteFile(path, line, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		_, err := latestRateLimits(ctx, path)
		if !errors.Is(err, bufio.ErrTooLong) {
			t.Fatalf("latestRateLimits() err = %v, want bufio.ErrTooLong", err)
		}
	})
}

// TestDataSync_DocBounds pins caps to docs/data-sync.md ("Sources" table:
// codex-local: 20,000 files walked, 128 MiB per file, 500,000 lines, 4 MiB per line; one record).
func TestDataSync_DocBounds(t *testing.T) {
	tests := []struct {
		name string
		got  any
		want any
	}{
		{"maxSessionFilesScanned", maxSessionFilesScanned, 20000},
		{"maxSessionFileBytes", maxSessionFileBytes, 128 << 20},
		{"maxScanLines", maxScanLines, 500000},
		{"maxLineBytes", maxLineBytes, 4 << 20},
		{"MaxRecords", MaxRecords, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestFetch_StampsInjectedNow(t *testing.T) {
	fixed := time.Date(2026, 4, 1, 10, 20, 30, 0, time.UTC)
	dir := t.TempDir()
	writeSession(t, dir, "new.jsonl", validLine, fixed)
	res := Fetch(context.Background(), dir, fixed)
	if res.Status != catalog.StatusOK {
		t.Fatalf("Status = %v, detail = %q, want ok", res.Status, res.Detail)
	}
	if len(res.Records) != 1 {
		t.Fatalf("Records len = %d, want 1", len(res.Records))
	}
	if !res.Records[0].Provenance.FetchedAt.Equal(fixed) {
		t.Fatalf("FetchedAt = %v, want %v", res.Records[0].Provenance.FetchedAt, fixed)
	}
}
