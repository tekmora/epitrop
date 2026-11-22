package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

func TestLogRotationConcurrentJobAndEpitropLinesNeverInterleave(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "concurrent.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 512, KeepFiles: 100})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	const count = 150
	var jobInput strings.Builder
	for i := 0; i < count; i++ {
		fmt.Fprintf(&jobInput, "job-line-%03d: payload-alpha-beta\n", i)
	}
	adapter := log.lineWriter()
	var wg sync.WaitGroup
	var jobErr, epiErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		jobErr = copyJobLines(&smallChunkReader{data: []byte(jobInput.String()), step: 7}, log)
	}()
	go func() {
		defer wg.Done()
		epiErr = writeEpitropBurst(adapter, count)
	}()
	wg.Wait()
	if err = errors.Join(jobErr, epiErr); err != nil {
		t.Fatalf("concurrent write error = %v, want nil", err)
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	assertNoInterleavedLines(t, readOrderedLogLines(t, logPath, 100), count)
}

func writeEpitropBurst(w io.Writer, count int) error {
	for i := 0; i < count; i++ {
		msg := fmt.Sprintf("epitrop: event-%03d notice", i)
		if i%2 == 0 {
			msg += "\n"
		}
		if _, err := w.Write([]byte(msg)); err != nil {
			return err
		}
	}
	return nil
}

func assertNoInterleavedLines(t *testing.T, lines []string, count int) {
	t.Helper()
	jobSeen := make(map[string]int, count)
	epiSeen := make(map[string]int, count)
	for i := 0; i < len(lines); i++ {
		switch {
		case strings.HasPrefix(lines[i], "job-line-") && strings.HasSuffix(lines[i], ": payload-alpha-beta"):
			jobSeen[lines[i]]++
		case strings.HasPrefix(lines[i], "epitrop: event-") && strings.HasSuffix(lines[i], " notice"):
			epiSeen[lines[i]]++
		default:
			t.Fatalf("corrupted or interleaved line %d: %q", i, lines[i])
		}
	}
	if len(jobSeen) != count || len(epiSeen) != count || len(lines) != 2*count {
		t.Fatalf("counts job=%d epitrop=%d total=%d, want %d %d %d", len(jobSeen), len(epiSeen), len(lines), count, count, 2*count)
	}
}

func TestLogRotationHeartbeatOnPartialLine(t *testing.T) {
	t.Run("partial_chunk_moves_mtime_without_writing_partial_line", func(t *testing.T) {
		assertPartialChunkTouchesLog(t)
	})
	t.Run("newline_chunk_leaves_pending_empty", func(t *testing.T) {
		assertNewlineChunkLeavesPendingEmpty(t)
	})
	t.Run("touch_failure_wraps_and_preserves_first_error", func(t *testing.T) {
		assertTouchErrorWrappedAndKept(t)
	})
}

func assertPartialChunkTouchesLog(t *testing.T) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "heartbeat.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 4096, KeepFiles: 3})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	defer func() {
		if closeErr := log.Close(); closeErr != nil {
			t.Errorf("Close() = %v, want nil", closeErr)
		}
	}()
	past := time.Now().Add(-time.Hour)
	if err = os.Chtimes(logPath, past, past); err != nil {
		t.Fatalf("Chtimes(1h back) = %v, want nil", err)
	}
	before := time.Now().Add(-5 * time.Second)
	splitter := newLineSplitter(log)
	if err = splitter.writeChunk([]byte("progress 50%\r")); err != nil {
		t.Fatalf("writeChunk(no newline) = %v, want nil", err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("Stat(%s) = %v, want nil", logPath, err)
	}
	if info.ModTime().Before(before) || time.Since(info.ModTime()) > 10*time.Second {
		t.Fatalf("mtime = %v, want within a few seconds of now (after %v)", info.ModTime(), before)
	}
	if got := mustReadLogFile(t, logPath); len(got) != 0 {
		t.Fatalf("active log content = %q, want empty (no partial line written)", got)
	}
}

func assertNewlineChunkLeavesPendingEmpty(t *testing.T) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "exact-nl.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 4096, KeepFiles: 3})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	splitter := newLineSplitter(log)
	if err = splitter.writeChunk([]byte("exact-line\n")); err != nil {
		t.Fatalf("writeChunk(newline) = %v, want nil", err)
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if len(splitter.pending) != 0 {
		t.Fatalf("pending len = %d, want 0", len(splitter.pending))
	}
	if got := string(mustReadLogFile(t, logPath)); got != "exact-line\n" {
		t.Fatalf("active log content = %q, want %q", got, "exact-line\n")
	}
}

func assertTouchErrorWrappedAndKept(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	sink, err := openAppendLog(filepath.Join(dir, "sink.log"))
	if err != nil {
		t.Fatalf("openAppendLog(sink) = %v, want nil", err)
	}
	broken := &rotatingLog{
		path:      filepath.Join(dir, "missing", "job.log"),
		maxBytes:  4096,
		keepFiles: 3,
		file:      sink,
	}
	defer func() {
		if closeErr := broken.Close(); closeErr != nil {
			t.Errorf("Close() = %v, want nil", closeErr)
		}
	}()
	err = copyJobLines(&smallChunkReader{data: []byte("progress\r"), step: 9}, broken)
	if err == nil || !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "job-shim: touch log") {
		t.Fatalf("copyJobLines(touch error) = %v, want wrapped os.ErrNotExist with context", err)
	}
}

func TestLogRotationSupervisorBoundsAndShimFlags(t *testing.T) {
	base := config.JobConfig{
		Name:    "rot",
		Command: []string{"/bin/true"},
		LogPath: "/tmp/rot.log",
		Sandbox: config.SandboxConfig{Mode: "off", Reason: "unit test"},
	}
	norm := normalizeJob(base)
	if norm.LogRotation.MaxBytes != config.DefaultLogMaxBytes || norm.LogRotation.KeepFiles != config.DefaultLogKeepFiles {
		t.Fatalf("normalizeJob(zero) = %+v, want %d and %d", norm.LogRotation, config.DefaultLogMaxBytes, config.DefaultLogKeepFiles)
	}
	assertLogRotationBounds(t, base)
	assertShimRotationFlags(t, base)
	assertShimRotationBounds(t)
}

func assertShimRotationBounds(t *testing.T) {
	t.Helper()
	if err := checkShimLogRotation(config.JobLogRotation{MaxBytes: config.MaxLogMaxBytes, KeepFiles: config.MaxLogKeepFiles}); err != nil {
		t.Fatalf("checkShimLogRotation(max) = %v, want nil", err)
	}
	bad := []struct {
		rot  config.JobLogRotation
		want string
	}{
		{config.JobLogRotation{MaxBytes: config.MinLogMaxBytes - 1}, "job-shim: log_rotation.max_bytes must be 4096..1073741824"},
		{config.JobLogRotation{MaxBytes: config.MaxLogMaxBytes + 1}, "job-shim: log_rotation.max_bytes must be 4096..1073741824"},
		{config.JobLogRotation{KeepFiles: config.MaxLogKeepFiles + 1}, "job-shim: log_rotation.keep_files must be 1..100"},
	}
	for i := 0; i < len(bad); i++ {
		if err := checkShimLogRotation(bad[i].rot); err == nil || err.Error() != bad[i].want {
			t.Fatalf("checkShimLogRotation(%+v) = %v, want %q", bad[i].rot, err, bad[i].want)
		}
	}
}

func assertLogRotationBounds(t *testing.T, base config.JobConfig) {
	t.Helper()
	bad := []struct {
		rot  config.JobLogRotation
		want string
	}{
		{config.JobLogRotation{MaxBytes: 4095}, "log_rotation.max_bytes must be 4096..1073741824"},
		{config.JobLogRotation{MaxBytes: 1073741825}, "log_rotation.max_bytes must be 4096..1073741824"},
		{config.JobLogRotation{MaxBytes: -1}, "log_rotation.max_bytes must be 4096..1073741824"},
		{config.JobLogRotation{KeepFiles: -1}, "log_rotation.keep_files must be 1..100"},
		{config.JobLogRotation{KeepFiles: 101}, "log_rotation.keep_files must be 1..100"},
	}
	for i := 0; i < len(bad); i++ {
		job := base
		job.LogRotation = bad[i].rot
		if err := checkJobBounds(job); err == nil || !strings.Contains(err.Error(), bad[i].want) {
			t.Fatalf("checkJobBounds(%+v) = %v, want %q", bad[i].rot, err, bad[i].want)
		}
	}
	good := []config.JobLogRotation{{}, {MaxBytes: 4096, KeepFiles: 1}, {MaxBytes: 1073741824, KeepFiles: 100}}
	for i := 0; i < len(good); i++ {
		job := base
		job.LogRotation = good[i]
		if err := checkJobBounds(job); err != nil {
			t.Fatalf("checkJobBounds(%+v) = %v, want nil", good[i], err)
		}
	}
}

func assertShimRotationFlags(t *testing.T, job config.JobConfig) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sup := &Supervisor{
		cfg:         config.Config{EventLog: config.EventLogConfig{Dir: "/state", SigningKeyPath: "/key"}},
		shimCommand: []string{"/bin/epitropctl"},
	}
	job.LogRotation = config.JobLogRotation{MaxBytes: 8192, KeepFiles: 9}
	args := sup.shimCmd(ctx, job).Args
	parsed, err := parseShimArgs(args[1:])
	if err != nil {
		t.Fatalf("parseShimArgs(with flags) = %v, want nil", err)
	}
	if parsed.LogRotation != job.LogRotation {
		t.Fatalf("parsed LogRotation = %+v, want %+v", parsed.LogRotation, job.LogRotation)
	}
	withoutFlags := stripLogRotationFlags(args[1:])
	defaulted, err := parseShimArgs(withoutFlags)
	if err != nil {
		t.Fatalf("parseShimArgs(without flags) = %v, want nil", err)
	}
	wantDef := config.JobLogRotation{MaxBytes: config.DefaultLogMaxBytes, KeepFiles: config.DefaultLogKeepFiles}
	if defaulted.LogRotation != wantDef {
		t.Fatalf("defaulted LogRotation = %+v, want %+v", defaulted.LogRotation, wantDef)
	}
}

func stripLogRotationFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--log-max-bytes" || args[i] == "--log-keep-files" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}
