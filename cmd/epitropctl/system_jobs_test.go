package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSystemJobs_Positive(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the supervisor runs on Linux only")
	}
	bin := buildCLI(t)
	cfgPath := writeJobsConfig(t, "valid-key")

	stdoutStart, stderrStart, codeStart, errStart := runSystemCLI(t, bin, "jobs", "start", "--config="+cfgPath, "idle")
	if errStart != nil || codeStart != 0 {
		t.Fatalf("jobs start code = %d, err = %v: %s\n%s", codeStart, errStart, stdoutStart, stderrStart)
	}

	checkJobLogFile(t, cfgPath)

	waitForJobStatus(t, bin, cfgPath, "idle", "idle\tdead\t0\t\t0\toff: jobs cli unit test\n")
}

func checkJobLogFile(t *testing.T, cfgPath string) {
	t.Helper()
	logPath := filepath.Join(filepath.Dir(cfgPath), "idle.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("job log file missing: %v", err)
	}
}

func TestSystemJobs_Negative(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the supervisor runs on Linux only")
	}
	bin := buildCLI(t)
	cfgPath := writeJobsConfig(t, "valid-key")

	t.Run("start unknown job", func(t *testing.T) {
		_, stderr, code, err := runSystemCLI(t, bin, "jobs", "start", "--config="+cfgPath, "missing-job")
		if err == nil || code != 1 {
			t.Fatalf("jobs start unknown code = %d, err = %v, want 1", code, err)
		}
		if !strings.Contains(stderr, "unknown job") {
			t.Fatalf("stderr = %q, want unknown job error", stderr)
		}
	})

	t.Run("status missing config", func(t *testing.T) {
		_, stderr, code, err := runSystemCLI(t, bin, "jobs", "status")
		if err == nil || code != 1 {
			t.Fatalf("jobs status missing --config code = %d, err = %v, want 1", code, err)
		}
		if !strings.Contains(stderr, "jobs status: --config is required") {
			t.Fatalf("stderr = %q, want --config is required error", stderr)
		}
	})
}
