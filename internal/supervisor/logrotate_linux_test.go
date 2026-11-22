//go:build linux

package supervisor

import (
	"fmt"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

func TestLogRotationShimRotatesJobLog(t *testing.T) {
	t.Run("every_numbered_line_once_across_kept_files", func(t *testing.T) {
		runShimNumberedRotation(t, 6, true)
	})
	t.Run("caps_files_at_keep_files_plus_active", func(t *testing.T) {
		runShimNumberedRotation(t, 2, false)
	})
}

func runShimNumberedRotation(t *testing.T, keepFiles int, expectAll bool) {
	t.Helper()
	const totalLines = 450
	dir := t.TempDir()
	logPath := filepath.Join(dir, "fast.log")
	script := fmt.Sprintf(`i=0; while [ "$i" -lt %d ]; do printf 'shim-rot-line-%%04d-payload-0123456789\n' "$i"; i=$((i+1)); done`, totalLines)
	job := testJob("fast-rot", []string{"/bin/sh", "-c", script})
	job.Schedule = "once"
	job.LogPath = logPath
	job.LogRotation = config.JobLogRotation{MaxBytes: 4096, KeepFiles: keepFiles}
	sup := testSupervisor(t, job)
	if err := sup.StartOnce(testContext(t), job.Name); err != nil {
		t.Fatalf("StartOnce() = %v, want nil", err)
	}
	exited := waitForExited(t, sup, job.Name)
	waitForUnlock(t, sup, job)
	if exited.LastExitCode == nil || *exited.LastExitCode != 0 {
		t.Fatalf("job.exited = %+v, want exit code 0", exited)
	}
	files := listLogFiles(t, dir, "fast.log")
	if len(files) < 3 || len(files) > keepFiles+1 {
		t.Fatalf("files = %v (count %d), want 3..%d", files, len(files), keepFiles+1)
	}
	assertEveryRotatedFileEndsWithNewline(t, logPath, keepFiles+2)
	assertNumberedShimLines(t, readOrderedLogLines(t, logPath, keepFiles), totalLines, expectAll)
}

func assertNumberedShimLines(t *testing.T, got []string, total int, expectAll bool) {
	t.Helper()
	want := make([]string, 0, total)
	for i := 0; i < total; i++ {
		want = append(want, fmt.Sprintf("shim-rot-line-%04d-payload-0123456789", i))
	}
	if expectAll {
		if !slices.Equal(got, want) {
			t.Fatalf("kept lines = %d, want all %d numbered lines once in order", len(got), total)
		}
		return
	}
	if len(got) == 0 || len(got) >= total {
		t.Fatalf("kept lines with pruned rotation = %d, want non-empty suffix shorter than %d", len(got), total)
	}
	wantSuffix := want[total-len(got):]
	if !slices.Equal(got, wantSuffix) {
		t.Fatalf("kept suffix mismatch: first=%q last=%q, want first=%q last=%q", got[0], got[len(got)-1], wantSuffix[0], wantSuffix[len(wantSuffix)-1])
	}
}

func TestLogRotationShimGrandchildPipeDrainIsBounded(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "grandchild.log")
	script := `echo before-fork; sleep 30 & echo "child=$!"; echo after-fork`
	job := testJob("grandchild", []string{"/bin/sh", "-c", script})
	job.Schedule = "once"
	job.LogPath = logPath
	job.Stop.GraceSeconds = 1
	job.LogRotation = config.JobLogRotation{MaxBytes: 4096, KeepFiles: 3}
	sup := testSupervisor(t, job)
	t.Cleanup(func() { cleanupForkedSleepFromLog(t, logPath) })

	began := time.Now()
	if err := sup.StartOnce(testContext(t), job.Name); err != nil {
		t.Fatalf("StartOnce() = %v, want nil", err)
	}
	exited := waitForExited(t, sup, job.Name)
	waitForUnlock(t, sup, job)
	elapsed := time.Since(began)
	if exited.LastExitCode == nil || *exited.LastExitCode != 0 {
		t.Fatalf("job.exited = %+v, want exit code 0", exited)
	}
	if elapsed >= 8*time.Second {
		t.Fatalf("shim returned after %s with grandchild holding pipe, want bounded drain near 1s", elapsed)
	}
	lines := readOrderedLogLines(t, logPath, 3)
	if !slices.Contains(lines, "before-fork") || !slices.Contains(lines, "after-fork") {
		t.Fatalf("log lines = %v, want before-fork and after-fork flushed before drain close", lines)
	}
}

func cleanupForkedSleepFromLog(t *testing.T, logPath string) {
	t.Helper()
	raw := string(mustReadLogFile(t, logPath))
	pid, ok := childPIDFromLog(raw)
	if !ok || pid <= 1 {
		return
	}
	assertSafeSignalPID(t, pid)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Logf("cleanup kill grandchild %d = %v", pid, err)
	}
}
