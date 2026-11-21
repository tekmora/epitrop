package atomicfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type replaceTestCase struct {
	name       string
	preCreate  bool
	oldBody    []byte
	body       []byte
	mode       os.FileMode
	assertMode bool
}

func TestReplaceSuccessTable(t *testing.T) {
	tests := []replaceTestCase{
		{
			name:       "new file created",
			preCreate:  false,
			body:       []byte("hello atomic world"),
			mode:       0o644,
			assertMode: true,
		},
		{
			name:       "replaces existing content",
			preCreate:  true,
			oldBody:    []byte("initial content that must be replaced"),
			body:       []byte("new content after atomic replacement"),
			mode:       0o600,
			assertMode: true,
		},
		{
			name:       "empty body works",
			preCreate:  false,
			body:       []byte{},
			mode:       0o600,
			assertMode: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runReplaceTestCase(t, tc)
		})
	}
}

func runReplaceTestCase(t *testing.T, tc replaceTestCase) {
	t.Helper()
	target := setupTestFile(t, tc.preCreate, tc.oldBody)
	if err := Replace(target, tc.body, tc.mode); err != nil {
		t.Fatalf("Replace() = %v, want nil", err)
	}
	got, err := os.ReadFile(target) // #nosec G304 -- target is the file created by this temporary-directory test
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, tc.body) {
		t.Fatalf("content = %q, want %q", got, tc.body)
	}
	if tc.assertMode {
		checkFileMode(t, target, tc.mode)
	}
}

func setupTestFile(t *testing.T, preCreate bool, oldBody []byte) string {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if preCreate {
		if err := os.WriteFile(target, oldBody, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	return target
}

func checkFileMode(t *testing.T, target string, wantMode os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("file mode bits not supported on windows")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if gotMode := info.Mode().Perm(); gotMode != wantMode {
		t.Fatalf("mode = %o, want %o", gotMode, wantMode)
	}
}

func TestMissingParentDirReturnsError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nonexistent", "nested", "file.txt")
	err := Replace(target, []byte("data"), 0o600)
	if err == nil {
		t.Fatal("Replace(missing parent dir) = nil, want error")
	}
	if _, statErr := os.Stat(target); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("Stat(%s) = %v, want ErrNotExist", target, statErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("found %d entries in dir, want 0", len(entries))
	}
}

func TestSyncHookFailingOnFileFailsAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	causeErr := errors.New("simulated-file-sync-failure")
	syncFn := func(f *os.File) error {
		return causeErr
	}
	err := ReplaceWithSync(target, []byte("data"), 0o600, syncFn)
	if err == nil {
		t.Fatal("ReplaceWithSync() = nil, want error")
	}
	if !errors.Is(err, causeErr) {
		t.Fatalf("err = %v, want cause %v", err, causeErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	for i := 0; i < len(entries); i++ {
		if strings.HasPrefix(entries[i].Name(), ".") {
			t.Fatalf("leftover temp file found: %s", entries[i].Name())
		}
	}
	if len(entries) != 0 {
		t.Fatalf("found %d entries in dir, want 0", len(entries))
	}
}

func TestSyncHookCountsTwoCallsOnSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	calls := 0
	syncFn := func(f *os.File) error {
		calls++
		return Sync(f)
	}
	if err := ReplaceWithSync(target, []byte("durable"), 0o600, syncFn); err != nil {
		t.Fatalf("ReplaceWithSync() = %v, want nil", err)
	}
	if calls != 2 {
		t.Fatalf("sync hook called %d times, want exactly 2 (file + dir)", calls)
	}
}

func TestRenameFailureLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "dir-target")
	if err := os.Mkdir(targetDir, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	childFile := filepath.Join(targetDir, "existing.txt")
	if err := os.WriteFile(childFile, []byte("stay"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	err := Replace(targetDir, []byte("payload"), 0o600)
	if err == nil {
		t.Fatal("Replace(existing dir) = nil, want error")
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	for i := 0; i < len(entries); i++ {
		if strings.HasPrefix(entries[i].Name(), ".") {
			t.Fatalf("leftover temp file found: %s", entries[i].Name())
		}
	}
	if len(entries) != 1 || entries[0].Name() != "dir-target" {
		t.Fatalf("dir entries = %v, want only dir-target", entries)
	}
}

func TestSyncHookFailingOnDirSyncReturnsError(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	dirSyncErr := errors.New("simulated-dir-sync-failure")
	calls := 0
	syncFn := func(f *os.File) error {
		calls++
		if calls == 2 {
			return dirSyncErr
		}
		return Sync(f)
	}
	err := ReplaceWithSync(target, []byte("data"), 0o600, syncFn)
	if err == nil {
		t.Fatal("ReplaceWithSync() = nil, want error")
	}
	if !errors.Is(err, dirSyncErr) {
		t.Fatalf("err = %v, want dir sync error %v", err, dirSyncErr)
	}
}

func TestReplaceWithSyncNilDefaultsToFsync(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "default-sync.txt")
	if err := ReplaceWithSync(target, []byte("data"), 0o600, nil); err != nil {
		t.Fatalf("ReplaceWithSync(nil syncFn) = %v, want nil", err)
	}
	got, err := os.ReadFile(target) // #nosec G304 -- target is the file created by this temporary-directory test
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "data" {
		t.Fatalf("content = %q, want %q", got, "data")
	}
}

func TestCloseWithCauseClosesFileAndReturnsCause(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tmp-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	cause := errors.New("initial-cause")
	gotErr := closeWithCause(f, cause)
	if !errors.Is(gotErr, cause) {
		t.Fatalf("closeWithCause() = %v, want cause %v", gotErr, cause)
	}
	if closeErr := f.Close(); closeErr == nil {
		t.Fatal("second Close() = nil, want error on already closed file")
	}
}

func TestCloseWithCauseJoinsCloseError(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tmp-*")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if closeErr := f.Close(); closeErr != nil {
		t.Fatalf("first Close: %v", closeErr)
	}
	cause := errors.New("cause-err")
	gotErr := closeWithCause(f, cause)
	if !errors.Is(gotErr, cause) || !strings.Contains(gotErr.Error(), "atomicfile: close") {
		t.Fatalf("closeWithCause() = %v, want joined close error", gotErr)
	}
}

func TestRemoveTempIgnoresMissing(t *testing.T) {
	cause := errors.New("my-cause")
	gotErr := removeTemp(filepath.Join(t.TempDir(), "nonexistent"), cause)
	if !errors.Is(gotErr, cause) || gotErr.Error() != cause.Error() {
		t.Fatalf("removeTemp() = %v, want the cause alone: %v", gotErr, cause)
	}
}

func TestSyncFlushesADirectoryOnEveryPlatform(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	t.Cleanup(func() {
		if cerr := dir.Close(); cerr != nil {
			t.Errorf("close dir: %v", cerr)
		}
	})
	if err = Sync(dir); err != nil {
		t.Fatalf("Sync(directory) = %v, want nil", err)
	}
}
