// SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me>
//
// SPDX-License-Identifier: EUPL-1.2

package supervisor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteRecordInterrupted(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "job.json")
	oldBody := []byte("old record\n")
	if err := os.WriteFile(recordPath, oldBody, 0o600); err != nil {
		t.Fatalf("write old record: %v", err)
	}

	origSync := recordSync
	t.Cleanup(func() { recordSync = origSync })
	simErr := errors.New("simulated sync failure")
	recordSync = func(*os.File) error { return simErr }

	cfg := shimConfig{
		Name:       "test-job",
		RecordPath: recordPath,
		LogPath:    filepath.Join(dir, "job.log"),
	}

	err := writeRecord(cfg, 42, "2026-10-18T20:00:00Z")
	if err == nil {
		t.Fatal("writeRecord with failing sync = nil, want error")
	}
	if !errors.Is(err, simErr) {
		t.Fatalf("err = %v, want cause %v", err, simErr)
	}

	got, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !bytes.Equal(got, oldBody) {
		t.Fatalf("record content = %q, want old %q", got, oldBody)
	}

	assertNoLeftoverTemp(t, dir, "job.json")
}

func assertNoLeftoverTemp(t *testing.T, dir, wantFile string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != wantFile {
		t.Fatalf("dir entries = %v, want only %s", entries, wantFile)
	}
}

func TestWriteRecordReplacesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "job.json")
	if err := os.WriteFile(recordPath, []byte("old content\n"), 0o600); err != nil {
		t.Fatalf("write old record: %v", err)
	}

	cfg := shimConfig{
		Name:       "test-job",
		RecordPath: recordPath,
		LogPath:    filepath.Join(dir, "job.log"),
	}

	if err := writeRecord(cfg, 1234, "2026-10-18T21:00:00Z"); err != nil {
		t.Fatalf("writeRecord() = %v, want nil", err)
	}

	got, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if bytes.Equal(got, []byte("old content\n")) {
		t.Fatal("writeRecord did not replace content")
	}
	if !strings.Contains(string(got), `"pid":1234`) {
		t.Fatalf("record missing pid: %s", string(got))
	}

	assertRecordMode(t, recordPath)
}

func assertRecordMode(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat record: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("record mode = %o, want 0600", perm)
	}
}

func TestWriteRecordCreatesMissingDir(t *testing.T) {
	dir := t.TempDir()
	recordPath := filepath.Join(dir, "missing", "nested", "job.json")

	cfg := shimConfig{
		Name:       "nested-job",
		RecordPath: recordPath,
		LogPath:    filepath.Join(dir, "job.log"),
	}

	if err := writeRecord(cfg, 5678, "2026-10-18T22:00:00Z"); err != nil {
		t.Fatalf("writeRecord(missing dir) = %v, want nil", err)
	}

	got, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	if !strings.Contains(string(got), `"pid":5678`) {
		t.Fatalf("record missing pid: %s", string(got))
	}
}
