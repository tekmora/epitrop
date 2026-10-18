package main

// Ported from cordanaLLM/praetor tribunus/cmd/tribunusctl at 5d4acfb (EUPL-1.2).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeLimitsCommand_Positive(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "proj", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"assistant","timestamp":"2026-10-18T08:00:00Z","quotaLimits":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1789858494}}
{"type":"assistant","timestamp":"2026-10-18T08:05:00Z","quotaLimits":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1789858494}}
`
	if err := os.WriteFile(logPath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := runClaudeLimits([]string{"--claude-projects-dir=" + dir}, &buf); err != nil {
		t.Fatalf("runClaudeLimits() = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "files 1, quotaLimits lines 2") {
		t.Fatalf("unexpected summary: %s", out)
	}
	if !strings.Contains(out, "rateLimitType five_hour 2") {
		t.Fatalf("missing five_hour counts: %s", out)
	}
	if !strings.Contains(out, "status rejected 2") {
		t.Fatalf("missing rejected status: %s", out)
	}
}

func TestClaudeLimitsCommand_Negative(t *testing.T) {
	var buf bytes.Buffer
	if err := runClaudeLimits([]string{"--bad-flag"}, &buf); err == nil {
		t.Fatal("runClaudeLimits() with bad flag = nil, want error")
	}
}
