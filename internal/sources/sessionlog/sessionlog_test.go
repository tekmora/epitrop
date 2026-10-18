package sessionlog

// Ported from cordanaLLM/praetor tribunus/internal/sources/sessionlog at 5d4acfb (EUPL-1.2).

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, body string, limits Limits) ([]string, int, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, body)
	var lines []string
	skipped, err := ScanLines(context.Background(), path, limits, func(line []byte) { lines = append(lines, string(line)) })
	return lines, skipped, err
}

var small = Limits{MaxFileBytes: 1 << 20, MaxLines: 100, MaxLineBytes: 16}

func TestScanLinesVisitsEachLine(t *testing.T) {
	lines, skipped, err := scan(t, "a\r\nbb\n\nccc", small)
	if err != nil || skipped != 0 || strings.Join(lines, ",") != "a,bb,ccc" {
		t.Fatalf("lines %q skipped %d err %v", lines, skipped, err)
	}
}

func TestScanLinesSkipsOverlongLinesWhole(t *testing.T) {
	fits := strings.Repeat("x", small.MaxLineBytes-1)
	tooLong := strings.Repeat("y", small.MaxLineBytes)
	lines, skipped, err := scan(t, fits+"\n"+tooLong+"\n"+strings.Repeat("z", 50)+"\nend", small)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 2 || strings.Join(lines, ",") != fits+",end" {
		t.Fatalf("want the fitting line and end, two skipped; got %q skipped %d", lines, skipped)
	}
	if _, skipped, err := scan(t, tooLong, small); err != nil || skipped != 1 {
		t.Fatalf("an overlong last line without newline must be skipped, skipped %d err %v", skipped, err)
	}
}

func TestScanLinesRefusesOversizedInput(t *testing.T) {
	if _, _, err := scan(t, strings.Repeat("a\n", 101), small); err == nil {
		t.Fatal("more lines than MaxLines must fail")
	}
	if _, _, err := scan(t, strings.Repeat("a\n", 100), small); err != nil {
		t.Fatalf("exactly MaxLines must pass: %v", err)
	}
	if _, _, err := scan(t, strings.Repeat("a", 33), Limits{MaxFileBytes: 32, MaxLines: 10, MaxLineBytes: 64}); err == nil {
		t.Fatal("a file above MaxFileBytes must fail")
	}
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeFile(t, path, "a\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanLines(ctx, path, small, func([]byte) {}); err == nil {
		t.Fatal("a canceled context must fail")
	}
}

func TestWalkJSONL(t *testing.T) {
	if found, err := WalkJSONL(context.Background(), filepath.Join(t.TempDir(), "missing"), 10, nil); found || err != nil {
		t.Fatalf("missing root: found %v err %v, want a skip", found, err)
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a", "one.jsonl"), "x\n")
	writeFile(t, filepath.Join(root, "a", "b", "two.jsonl"), "x\n")
	writeFile(t, filepath.Join(root, "notes.txt"), "x\n")
	var seen []string
	found, err := WalkJSONL(context.Background(), root, 3, func(path string, _ fs.FileInfo) error {
		seen = append(seen, filepath.Base(path))
		return nil
	})
	if err != nil || !found || strings.Join(seen, ",") != "two.jsonl,one.jsonl" && strings.Join(seen, ",") != "one.jsonl,two.jsonl" {
		t.Fatalf("found %v seen %v err %v", found, seen, err)
	}
	if _, err := WalkJSONL(context.Background(), root, 2, func(string, fs.FileInfo) error { return nil }); err == nil {
		t.Fatal("more files than maxFiles must fail")
	}
}
