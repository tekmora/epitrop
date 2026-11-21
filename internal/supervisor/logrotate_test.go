package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

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
	t.Run("read_only_dir_keeps_accepted_lines", func(t *testing.T) {
		assertReadOnlyDirKeepsAccepted(t)
	})
}

func TestLogRotationOpenPrunesStaleBackups(t *testing.T) {
	t.Run("removes_above_keep_files_and_keeps_active_kept_and_unrelated", func(t *testing.T) {
		assertOpenPrunesAboveKeepFiles(t)
	})
	t.Run("skips_directory_and_symlink_backups", func(t *testing.T) {
		assertOpenSkipsNonRegularStalePaths(t)
	})
	t.Run("removes_max_keep_plus_one_and_leaves_higher_index", func(t *testing.T) {
		assertOpenPrunesAtMaxKeepBound(t)
	})
	t.Run("stale_remove_error_fails_open_with_context", func(t *testing.T) {
		assertOpenFailsWhenStaleRemoveFails(t)
	})
}

func assertOpenPrunesAboveKeepFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath, otherPath := filepath.Join(dir, "job.log"), filepath.Join(dir, "other.log")
	mustWriteRawFile(t, logPath, "active\n")
	mustWriteRawFile(t, otherPath, "unrelated\n")
	for i := 0; i < 5; i++ {
		mustWriteRawFile(t, rotatedLogPath(logPath, i+1), fmt.Sprintf("backup-%d\n", i+1))
	}
	mustCloseRotatingLog(t, mustOpenRotatingLog(t, logPath, config.JobLogRotation{MaxBytes: 4096, KeepFiles: 2}))
	for i := 0; i < 3; i++ {
		assertPathMissing(t, rotatedLogPath(logPath, i+3))
	}
	assertFileString(t, logPath, "active\n")
	assertFileString(t, logPath+".1", "backup-1\n")
	assertFileString(t, logPath+".2", "backup-2\n")
	assertFileString(t, otherPath, "unrelated\n")
}

func assertOpenSkipsNonRegularStalePaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath, target := filepath.Join(dir, "job.log"), filepath.Join(dir, "target.txt")
	mustWriteRawFile(t, logPath+".3", "regular-stale\n")
	mustWriteRawFile(t, target, "target\n")
	if err := os.Mkdir(logPath+".4", 0o700); err != nil {
		t.Fatalf("Mkdir(.4) = %v, want nil", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(target, logPath+".5"); err != nil {
			t.Fatalf("Symlink(.5) = %v, want nil", err)
		}
	}
	mustCloseRotatingLog(t, mustOpenRotatingLog(t, logPath, config.JobLogRotation{MaxBytes: 4096, KeepFiles: 2}))
	assertPathMissing(t, logPath+".3")
	if info, err := os.Lstat(logPath + ".4"); err != nil || !info.IsDir() {
		t.Fatalf("Lstat(.4) = %v, %v, want directory kept", info, err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Lstat(logPath + ".5"); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("Lstat(.5) = %v, %v, want symlink kept", info, err)
		}
	}
}

func assertOpenPrunesAtMaxKeepBound(t *testing.T) {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "bound.log")
	indices := []int{100, 101, 102}
	for i := 0; i < len(indices); i++ {
		mustWriteRawFile(t, rotatedLogPath(logPath, indices[i]), fmt.Sprintf("b-%d\n", indices[i]))
	}
	mustCloseRotatingLog(t, mustOpenRotatingLog(t, logPath, config.JobLogRotation{MaxBytes: 4096, KeepFiles: config.MaxLogKeepFiles}))
	assertPathMissing(t, rotatedLogPath(logPath, 101))
	assertFileString(t, rotatedLogPath(logPath, 100), "b-100\n")
	assertFileString(t, rotatedLogPath(logPath, 102), "b-102\n")
}

func assertOpenFailsWhenStaleRemoveFails(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("skipping read-only directory test: Windows lacks POSIX directory modes and root (euid 0) ignores them")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ro-open.log")
	mustWriteRawFile(t, logPath, "existing\n")
	mustWriteRawFile(t, logPath+".3", "stale\n")
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- read-only directory simulates stale backup unlink permission failure
		t.Fatalf("Chmod(0500) = %v, want nil", err)
	}
	t.Cleanup(func() {
		if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil { // #nosec G302 -- restore temp dir permissions for cleanup
			t.Errorf("Chmod(0700) = %v, want nil", chmodErr)
		}
	})
	_, err := openRotatingLog(logPath, config.JobLogRotation{MaxBytes: 4096, KeepFiles: 2})
	if err == nil || !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "remove stale log") {
		t.Fatalf("openRotatingLog(read-only stale remove) = %v, want wrapped os.ErrPermission with context", err)
	}
}

func mustWriteRawFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v, want nil", path, err)
	}
}

func assertFileString(t *testing.T, path, want string) {
	t.Helper()
	if got := string(mustReadLogFile(t, path)); got != want {
		t.Fatalf("ReadFile(%s) = %q, want %q", path, got, want)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Lstat(%s) = %v, want os.ErrNotExist", path, err)
	}
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
}

func assertBlockedRenameKeepsAccepted(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "job.log")
	log := mustOpenRotatingLog(t, logPath, config.JobLogRotation{MaxBytes: 40, KeepFiles: 2})
	defer func() {
		if closeErr := log.Close(); closeErr != nil {
			t.Errorf("Close() = %v, want nil", closeErr)
		}
	}()
	accepted := "accepted-line-one\naccepted-line-two\n"
	mustWriteLine(t, log, "1", "accepted-line-one\n")
	mustWriteLine(t, log, "2", "accepted-line-two\n")
	mustWriteRawFile(t, logPath+".1", "recent-accepted-line\n")
	mustWriteRawFile(t, logPath+".2", "older-accepted-line\n")
	if err := os.MkdirAll(filepath.Join(logPath+".3", "nonempty"), 0o700); err != nil {
		t.Fatalf("MkdirAll(blocker dir) = %v, want nil", err)
	}
	err := log.writeLine([]byte("triggers-rotation-and-fails\n"))
	var linkErr *os.LinkError
	if err == nil || !errors.As(err, &linkErr) {
		t.Fatalf("writeLine(blocked rename) = %v, want wrapped *os.LinkError", err)
	}
	assertFileString(t, logPath, accepted)
	assertFileString(t, logPath+".1", "recent-accepted-line\n")
	assertFileString(t, logPath+".2", "older-accepted-line\n")
}

func assertReadOnlyDirKeepsAccepted(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("skipping read-only directory test: Windows lacks POSIX directory modes and root (euid 0) ignores them")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ro.log")
	log := mustOpenRotatingLog(t, logPath, config.JobLogRotation{MaxBytes: 32, KeepFiles: 2})
	mustWriteLine(t, log, "before chmod", "kept-before-chmod\n")
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- read-only directory simulates rename permission failure
		t.Fatalf("Chmod(0500) = %v, want nil", err)
	}
	t.Cleanup(func() {
		if chmodErr := os.Chmod(dir, 0o700); chmodErr != nil { // #nosec G302 -- restore temp dir permissions for cleanup
			t.Errorf("Chmod(0700) = %v, want nil", chmodErr)
		}
	})
	err := log.writeLine([]byte("second-line-rotates\n"))
	if err == nil || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("writeLine(read-only dir) = %v, want wrapped permission error", err)
	}
	mustCloseRotatingLog(t, log)
	if got := string(mustReadLogFile(t, logPath)); got != "kept-before-chmod\n" {
		t.Fatalf("ro.log = %q, want accepted line preserved", got)
	}
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

func mustOpenRotatingLog(t *testing.T, path string, rot config.JobLogRotation) *rotatingLog {
	t.Helper()
	log, err := openRotatingLog(path, rot)
	if err != nil {
		t.Fatalf("openRotatingLog() = %v, want nil", err)
	}
	return log
}

func mustWriteLine(t *testing.T, log *rotatingLog, label, line string) {
	t.Helper()
	if err := log.writeLine([]byte(line)); err != nil {
		t.Fatalf("writeLine(%s) = %v, want nil", label, err)
	}
}

func mustCloseRotatingLog(t *testing.T, log *rotatingLog) {
	t.Helper()
	if err := log.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
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
