package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

func TestLogRotationKeepsBoundedFilesAndOrder(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "job.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 128, KeepFiles: 20})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	var want []string
	for i := 0; i < 40; i++ {
		line := fmt.Sprintf("entry-%03d-payload\n", i)
		want = append(want, strings.TrimSuffix(line, "\n"))
		if err = log.writeLine([]byte(line)); err != nil {
			t.Fatalf("writeLine(%d) = %v, want nil", i, err)
		}
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	assertLogFileMode(t, logPath)
	files := listLogFiles(t, dir, "job.log")
	if len(files) < 4 || len(files) > 21 {
		t.Fatalf("log file count = %d, want rotated files within 4..21", len(files))
	}
	got := readOrderedLogLines(t, logPath, 20)
	if !slices.Equal(got, want) {
		t.Fatalf("lines across kept files = %d, want all %d in order", len(got), len(want))
	}
}

func TestLogRotationBoundaries(t *testing.T) {
	t.Run("exact_max_bytes_stays_and_one_more_byte_rotates", func(t *testing.T) {
		assertExactBoundAndOneMoreByte(t)
	})
	t.Run("line_of_exact_max_bytes_stays_whole", func(t *testing.T) {
		assertExactMaxBytesLineWhole(t)
	})
	t.Run("line_of_max_bytes_plus_one_cuts_into_two_pieces", func(t *testing.T) {
		assertMaxBytesPlusOneCutIntoTwo(t)
	})
	t.Run("trailing_partial_line_and_chunked_reads_never_split_lines", func(t *testing.T) {
		assertChunkedReadsAndTrailingPartial(t)
	})
}

func assertExactBoundAndOneMoreByte(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "exact.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 64, KeepFiles: 3})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	half := []byte(strings.Repeat("a", 31) + "\n")
	for i := 0; i < 2; i++ {
		if err = log.writeLine(half); err != nil {
			t.Fatalf("writeLine(half %d) = %v, want nil", i, err)
		}
	}
	if got := listLogFiles(t, dir, "exact.log"); len(got) != 1 {
		t.Fatalf("files at exact max_bytes = %v, want only active exact.log", got)
	}
	if err = log.writeLine([]byte("z")); err != nil {
		t.Fatalf("writeLine(one more byte) = %v, want nil", err)
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if got := string(mustReadLogFile(t, logPath+".1")); got != string(half)+string(half) {
		t.Fatalf("exact.log.1 = %q, want 64-byte full content", got)
	}
	if got := string(mustReadLogFile(t, logPath)); got != "z" {
		t.Fatalf("exact.log = %q, want single trailing byte", got)
	}
}

func assertExactMaxBytesLineWhole(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "whole.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 64, KeepFiles: 3})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	fullLine := []byte(strings.Repeat("m", 63) + "\n")
	if err = log.writeLine([]byte("prefix\n")); err != nil {
		t.Fatalf("writeLine(prefix) = %v, want nil", err)
	}
	if err = log.writeLine(fullLine); err != nil {
		t.Fatalf("writeLine(fullLine) = %v, want nil", err)
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if got := string(mustReadLogFile(t, logPath+".1")); got != "prefix\n" {
		t.Fatalf("whole.log.1 = %q, want prefix\\n", got)
	}
	if got := mustReadLogFile(t, logPath); !bytes.Equal(got, fullLine) {
		t.Fatalf("whole.log len = %d, want whole 64-byte line", len(got))
	}
}

func assertMaxBytesPlusOneCutIntoTwo(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "overlong.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 64, KeepFiles: 3})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	overlong := strings.Repeat("x", 64) + "\n"
	if err = copyJobLines(&smallChunkReader{data: []byte(overlong), step: 13}, log); err != nil {
		t.Fatalf("copyJobLines(overlong) = %v, want nil", err)
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if got := listLogFiles(t, dir, "overlong.log"); len(got) != 2 {
		t.Fatalf("files = %v, want 2 pieces", got)
	}
	if got := string(mustReadLogFile(t, logPath+".1")); got != strings.Repeat("x", 64) {
		t.Fatalf("piece 1 len = %d, want 64 x bytes", len(got))
	}
	if got := string(mustReadLogFile(t, logPath)); got != "\n" {
		t.Fatalf("piece 2 = %q, want trailing newline", got)
	}
}

func assertChunkedReadsAndTrailingPartial(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "chunked.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 70, KeepFiles: 10})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	var input strings.Builder
	var want []string
	for i := 0; i < 12; i++ {
		line := fmt.Sprintf("chunked-line-%02d-abcdef", i)
		want = append(want, line)
		input.WriteString(line + "\n")
	}
	input.WriteString("tail-without-newline")
	if err = copyJobLines(&smallChunkReader{data: []byte(input.String()), step: 5}, log); err != nil {
		t.Fatalf("copyJobLines() = %v, want nil", err)
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	assertEveryRotatedFileEndsWithNewline(t, logPath, 10)
	got := readOrderedLogRaw(t, logPath, 10)
	if got != input.String() {
		t.Fatalf("reconstructed stream len = %d, want exact %d bytes", len(got), input.Len())
	}
	lines := strings.Split(strings.TrimSuffix(got, "tail-without-newline"), "\n")
	if !slices.Equal(lines[:len(lines)-1], want) {
		t.Fatalf("lines = %v, want %v", lines[:len(lines)-1], want)
	}
}

func TestLogRotationDropsOldestAndPreservesAcceptedOnFailure(t *testing.T) {
	t.Run("keep_files_drops_only_oldest", func(t *testing.T) {
		assertKeepFilesDropsOnlyOldest(t)
	})
	t.Run("open_and_rename_failures_wrap_error_and_keep_accepted_lines", func(t *testing.T) {
		assertFailuresPreserveAcceptedLines(t)
	})
}

func assertKeepFilesDropsOnlyOldest(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "cap.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 32, KeepFiles: 2})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	for i := 0; i < 5; i++ {
		line := fmt.Sprintf("record-number-%02d-pad\n", i)
		if err = log.writeLine([]byte(line)); err != nil {
			t.Fatalf("writeLine(%d) = %v, want nil", i, err)
		}
	}
	if err = log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
	if files := listLogFiles(t, dir, "cap.log"); len(files) != 3 {
		t.Fatalf("files = %v, want at most keep_files+1 (3)", files)
	}
	want := []string{"record-number-02-pad", "record-number-03-pad", "record-number-04-pad"}
	if got := readOrderedLogLines(t, logPath, 2); !slices.Equal(got, want) {
		t.Fatalf("kept lines = %v, want newest 3 %v", got, want)
	}
}

func assertFailuresPreserveAcceptedLines(t *testing.T) {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(blocker) = %v, want nil", err)
	}
	if _, err := openRotatingLog(filepath.Join(blocker, "job.log"), config.JobLogRotation{}); err == nil {
		t.Fatal("openRotatingLog(path under file) = nil, want wrapped error")
	}
	assertBlockedRenameKeepsAccepted(t)
	assertReadOnlyDirKeepsAccepted(t)
}

func assertBlockedRenameKeepsAccepted(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "job.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 40, KeepFiles: 2})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	defer func() {
		if closeErr := log.Close(); closeErr != nil {
			t.Errorf("Close() = %v, want nil", closeErr)
		}
	}()
	accepted := "accepted-line-one\naccepted-line-two\n"
	if err = log.writeLine([]byte("accepted-line-one\n")); err != nil {
		t.Fatalf("writeLine(1) = %v, want nil", err)
	}
	if err = log.writeLine([]byte("accepted-line-two\n")); err != nil {
		t.Fatalf("writeLine(2) = %v, want nil", err)
	}
	if err = os.WriteFile(logPath+".2", []byte("older-accepted-line\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(.2) = %v, want nil", err)
	}
	if err = os.MkdirAll(filepath.Join(logPath+".3", "nonempty"), 0o700); err != nil {
		t.Fatalf("MkdirAll(blocker dir) = %v, want nil", err)
	}
	err = log.writeLine([]byte("triggers-rotation-and-fails\n"))
	var linkErr *os.LinkError
	if err == nil || !errors.As(err, &linkErr) {
		t.Fatalf("writeLine(blocked rename) = %v, want wrapped *os.LinkError", err)
	}
	if got := string(mustReadLogFile(t, logPath)); got != accepted {
		t.Fatalf("active log after failed rename = %q, want %q preserved", got, accepted)
	}
	if got := string(mustReadLogFile(t, logPath+".2")); got != "older-accepted-line\n" {
		t.Fatalf("rotated .2 after failed rename = %q, want older line preserved", got)
	}
}

func assertReadOnlyDirKeepsAccepted(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ro.log")
	log, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 32, KeepFiles: 2})
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	if err = log.writeLine([]byte("kept-before-chmod\n")); err != nil {
		t.Fatalf("writeLine(before chmod) = %v, want nil", err)
	}
	if err = os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- read-only directory simulates rename permission failure
		t.Fatalf("Chmod(0500) = %v, want nil", err)
	}
	t.Cleanup(func() {
		if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil { // #nosec G302 -- restore temp dir permissions for cleanup
			t.Errorf("Chmod(0700) = %v, want nil", chmodErr)
		}
	})
	err = log.writeLine([]byte("second-line-rotates\n"))
	if err == nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("writeLine(read-only dir) = %v, want wrapped permission error", err)
	}
	if closeErr := log.Close(); closeErr != nil {
		t.Fatalf("Close() = %v, want nil", closeErr)
	}
	if got := string(mustReadLogFile(t, logPath)); got != "kept-before-chmod\n" {
		t.Fatalf("ro.log = %q, want accepted line preserved", got)
	}
}

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

type smallChunkReader struct {
	data []byte
	pos  int
	step int
}

func (r *smallChunkReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	end := min(r.pos+r.step, len(r.data), r.pos+len(p))
	n := copy(p, r.data[r.pos:end])
	r.pos += n
	return n, nil
}

func assertLogFileMode(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s) = %v, want nil", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("log mode = %o, want 0600", perm)
	}
}

func listLogFiles(t *testing.T, dir, prefix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) = %v, want nil", dir, err)
	}
	var names []string
	for i := 0; i < len(entries); i++ {
		name := entries[i].Name()
		if name == prefix || strings.HasPrefix(name, prefix+".") {
			names = append(names, name)
		}
	}
	return names
}

func mustReadLogFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path) // #nosec G304 -- test reads temporary rotated log file
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", path, err)
	}
	return body
}

func assertEveryRotatedFileEndsWithNewline(t *testing.T, logPath string, maxKeep int) {
	t.Helper()
	for i := 1; i <= maxKeep; i++ {
		body, err := os.ReadFile(rotatedLogPath(logPath, i)) // #nosec G304 -- test reads temporary rotated log file
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("ReadFile(rotated %d) = %v, want nil", i, err)
		}
		if len(body) == 0 || body[len(body)-1] != '\n' {
			t.Fatalf("rotated file .%d does not end on a line boundary: %q", i, body)
		}
	}
}

func readOrderedLogRaw(t *testing.T, logPath string, maxKeep int) string {
	t.Helper()
	var out strings.Builder
	for step := 0; step < maxKeep; step++ {
		idx := maxKeep - step
		body, err := os.ReadFile(rotatedLogPath(logPath, idx)) // #nosec G304 -- test reads temporary rotated log file
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("ReadFile(rotated %d) = %v, want nil", idx, err)
		}
		out.Write(body)
	}
	body, err := os.ReadFile(logPath) // #nosec G304 -- test reads temporary active log file
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", logPath, err)
	}
	out.Write(body)
	return out.String()
}

func readOrderedLogLines(t *testing.T, logPath string, maxKeep int) []string {
	t.Helper()
	raw := readOrderedLogRaw(t, logPath, maxKeep)
	if raw == "" {
		return nil
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Fatalf("concatenated log missing trailing newline: %q", raw)
	}
	return strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
}
