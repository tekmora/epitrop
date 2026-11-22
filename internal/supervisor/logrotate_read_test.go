package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readTempFile reads path via an os.Root scoped to the parent directory.
func readTempFile(path string) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, fmt.Errorf("open root %s: %w", filepath.Dir(path), err)
	}
	body, readErr := root.ReadFile(filepath.Base(path))
	if closeErr := root.Close(); closeErr != nil && readErr == nil {
		return nil, fmt.Errorf("close root %s: %w", path, closeErr)
	}
	return body, readErr
}

// makeDirReadOnly sets dir to 0500 and registers a cleanup to restore 0700.
func makeDirReadOnly(t *testing.T, dir string) {
	t.Helper()
	if err := chmodTempDir(dir, 0o500); err != nil {
		t.Fatalf("chmodTempDir(%s, 0500) = %v, want nil", dir, err)
	}
	t.Cleanup(func() {
		if err := chmodTempDir(dir, 0o700); err != nil {
			t.Errorf("chmodTempDir(%s, 0700) = %v, want nil", dir, err)
		}
	})
}

// chmodTempDir sets file mode permissions on a test directory.
func chmodTempDir(dir string, mode os.FileMode) error {
	if err := os.Chmod(dir, mode); err != nil { // #nosec G302 -- callers set a test temp directory to 0o500 and restore 0o700
		return fmt.Errorf("chmod %s %04o: %w", dir, mode, err)
	}
	return nil
}

func mustReadLogFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := readTempFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", path, err)
	}
	return body
}

func assertEveryRotatedFileEndsWithNewline(t *testing.T, logPath string, maxKeep int) {
	t.Helper()
	for i := 1; i <= maxKeep; i++ {
		body, err := readTempFile(rotatedLogPath(logPath, i))
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
		body, err := readTempFile(rotatedLogPath(logPath, idx))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("ReadFile(rotated %d) = %v, want nil", idx, err)
		}
		out.Write(body)
	}
	body, err := readTempFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", logPath, err)
	}
	out.Write(body)
	return out.String()
}
