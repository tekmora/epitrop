package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestJobLogMaxBytesSchemaBounds(t *testing.T) {
	t.Run("schema_anyof_shape", func(t *testing.T) {
		assertJobLogMaxBytesSchemaShape(t)
	})
	t.Run("accepted_zero_min_and_max", func(t *testing.T) {
		assertJobLogMaxBytesAccepted(t)
	})
	t.Run("schema_refuses_below_min_and_above_max", func(t *testing.T) {
		assertJobLogMaxBytesSchemaRefused(t)
	})
}

func assertJobLogMaxBytesSchemaShape(t *testing.T) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(SchemaJSON(), &doc); err != nil {
		t.Fatalf("Unmarshal(schema) = %v, want nil", err)
	}
	rot := schemaChild(t, doc, "properties", "jobs", "items", "properties", "log_rotation")
	maxNode := schemaChild(t, rot, "properties", "max_bytes")
	if maxNode["type"] != "integer" || maxNode["default"] != float64(DefaultLogMaxBytes) {
		t.Fatalf("max_bytes type/default = %v/%v, want integer/%d", maxNode["type"], maxNode["default"], DefaultLogMaxBytes)
	}
	branches, ok := maxNode["anyOf"].([]any)
	if !ok || len(branches) != 2 {
		t.Fatalf("max_bytes anyOf = %#v, want 2 branches", maxNode["anyOf"])
	}
	assertJobLogZeroBranch(t, branches[0])
	assertJobLogRangeBranch(t, branches[1])
}

func assertJobLogZeroBranch(t *testing.T, raw any) {
	t.Helper()
	branch, ok := raw.(map[string]any)
	if !ok || branch["const"] != float64(0) {
		t.Fatalf("max_bytes anyOf branch 0 = %#v, want const 0", raw)
	}
}

func assertJobLogRangeBranch(t *testing.T, raw any) {
	t.Helper()
	branch, ok := raw.(map[string]any)
	if !ok || branch["minimum"] != float64(MinLogMaxBytes) || branch["maximum"] != float64(MaxLogMaxBytes) {
		t.Fatalf("max_bytes anyOf branch 1 = %#v, want %d..%d", raw, MinLogMaxBytes, MaxLogMaxBytes)
	}
}

func assertJobLogMaxBytesAccepted(t *testing.T) {
	t.Helper()
	cases := []struct {
		maxBytes int
		want     int
	}{
		{0, DefaultLogMaxBytes},
		{MinLogMaxBytes, MinLogMaxBytes},
		{MaxLogMaxBytes, MaxLogMaxBytes},
	}
	for i := 0; i < len(cases); i++ {
		body := fmt.Sprintf(`{"jobs": [{"name": "rot", "command": ["/bin/true"], "log_path": "/tmp/rot.log", "log_rotation": {"max_bytes": %d}, "sandbox": {"mode": "off", "reason": "test"}}]}`, cases[i].maxBytes)
		cfg, err := Load(testContext(t), writeConfig(t, body))
		if err != nil {
			t.Fatalf("Load(max_bytes=%d) = %v, want nil", cases[i].maxBytes, err)
		}
		if got := cfg.Jobs[0].LogRotation.MaxBytes; got != cases[i].want {
			t.Fatalf("Load(max_bytes=%d) MaxBytes = %d, want %d", cases[i].maxBytes, got, cases[i].want)
		}
	}
}

func assertJobLogMaxBytesSchemaRefused(t *testing.T) {
	t.Helper()
	refused := []int{-1, 1, MinLogMaxBytes - 1, MaxLogMaxBytes + 1}
	for i := 0; i < len(refused); i++ {
		body := fmt.Sprintf(`{"jobs": [{"name": "rot", "command": ["/bin/true"], "log_path": "/tmp/rot.log", "log_rotation": {"max_bytes": %d}, "sandbox": {"mode": "off", "reason": "test"}}]}`, refused[i])
		_, err := Load(testContext(t), writeConfig(t, body))
		if err == nil {
			t.Fatalf("Load(max_bytes=%d) = nil, want schema refusal", refused[i])
		}
		assertErrorContains(t, err, []string{"max_bytes", "config: validate /jobs/0/log_rotation/max_bytes:"})
	}
}

func TestLoadJobLogPathsUnique(t *testing.T) {
	t.Run("distinct_and_rotated_neighbor_paths_load", func(t *testing.T) {
		assertDistinctJobLogPathsLoad(t)
	})
	t.Run("same_cleaned_path_refused", func(t *testing.T) {
		assertDuplicateJobLogPathsRefused(t)
	})
	t.Run("relative_and_absolute_same_file_refused", func(t *testing.T) {
		assertRelativeAndAbsoluteJobLogPaths(t)
	})
	t.Run("symlinked_directory_alias_refused", func(t *testing.T) {
		assertSymlinkedDirectoryAliasRefused(t)
	})
	t.Run("missing_directory_below_alias_refused", func(t *testing.T) {
		assertMissingDirectoryBelowAliasRefused(t)
	})
	t.Run("symlinked_file_alias_refused", func(t *testing.T) {
		assertSymlinkedFileAliasRefused(t)
	})
	t.Run("distinct_files_through_alias_load", func(t *testing.T) {
		assertDistinctFilesThroughAliasLoad(t)
	})
	t.Run("unreadable_directory_through_alias_refused", func(t *testing.T) {
		assertUnreadableDirectoryThroughAliasRefused(t)
	})
}

func assertSymlinkedDirectoryAliasRefused(t *testing.T) {
	t.Helper()
	_, realDir, aliasDir := newJobLogAliasDirs(t)
	assertSameJobLogFileRefused(t, filepath.Join(realDir, "a.log"), filepath.Join(aliasDir, "a.log"))
}

func assertMissingDirectoryBelowAliasRefused(t *testing.T) {
	t.Helper()
	_, realDir, aliasDir := newJobLogAliasDirs(t)
	assertSameJobLogFileRefused(t, filepath.Join(realDir, "new", "a.log"), filepath.Join(aliasDir, "new", "a.log"))
}

func assertSymlinkedFileAliasRefused(t *testing.T) {
	t.Helper()
	root, realDir, _ := newJobLogAliasDirs(t)
	realLog := filepath.Join(realDir, "a.log")
	if err := os.WriteFile(realLog, []byte("log\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) = %v, want nil", realLog, err)
	}
	linkLog := filepath.Join(root, "link.log")
	if err := os.Symlink(realLog, linkLog); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", linkLog, err)
	}
	assertSameJobLogFileRefused(t, realLog, linkLog)
}

func assertDistinctFilesThroughAliasLoad(t *testing.T) {
	t.Helper()
	_, realDir, aliasDir := newJobLogAliasDirs(t)
	firstPath := filepath.Join(aliasDir, "a.log")
	secondPath := filepath.Join(realDir, "b.log")
	if _, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig(firstPath, secondPath))); err != nil {
		t.Fatalf("Load(%q vs %q) = %v, want nil", firstPath, secondPath, err)
	}
}

func assertUnreadableDirectoryThroughAliasRefused(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads through mode 0000 directories")
	}
	root := newResolvedTempDir(t)
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) = %v, want nil", locked, err)
	}
	lockAlias := filepath.Join(root, "lockalias")
	if err := os.Symlink(locked, lockAlias); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", lockAlias, err)
	}
	lockTestDir(t, locked)
	assertSameJobLogFileRefused(t, filepath.Join(locked, "sub", "a.log"), filepath.Join(lockAlias, "sub", "a.log"))
}

func newResolvedTempDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temporary directory) = %v, want nil", err)
	}
	return root
}

func newJobLogAliasDirs(t *testing.T) (string, string, string) {
	t.Helper()
	root := newResolvedTempDir(t)
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatalf("MkdirAll(%s) = %v, want nil", realDir, err)
	}
	aliasDir := filepath.Join(root, "alias")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Fatalf("Symlink(%s) = %v, want nil", aliasDir, err)
	}
	return root, realDir, aliasDir
}

func assertSameJobLogFileRefused(t *testing.T, firstPath, secondPath string) {
	t.Helper()
	_, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig(firstPath, secondPath)))
	want := fmt.Sprintf("config: jobs/1/log_path: %q names the same file as jobs/0 (%q)", secondPath, firstPath)
	if err == nil || err.Error() != want {
		t.Fatalf("Load(%q vs %q) = %v, want %q", firstPath, secondPath, err, want)
	}
}

func lockTestDir(t *testing.T, dir string) {
	t.Helper()
	if err := chmodTestDir(dir, 0o000); err != nil {
		t.Fatalf("chmodTestDir(%s, 0000) = %v, want nil", dir, err)
	}
	t.Cleanup(func() {
		if err := chmodTestDir(dir, 0o700); err != nil {
			t.Errorf("chmodTestDir(%s, 0700) = %v, want nil", dir, err)
		}
	})
}

// chmodTestDir sets file mode permissions on a test directory.
func chmodTestDir(dir string, mode os.FileMode) error {
	if err := os.Chmod(dir, mode); err != nil { // #nosec G302 -- callers set a test temp directory to 0o000 and restore 0o700
		return fmt.Errorf("chmod %s %04o: %w", dir, mode, err)
	}
	return nil
}

func assertRelativeAndAbsoluteJobLogPaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	if _, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig("a.log", "b.log"))); err != nil {
		t.Fatalf("Load(relative distinct) = %v, want nil", err)
	}
	absA := filepath.Join(dir, "a.log")
	_, err := Load(testContext(t), writeConfig(t, twoJobsLogPathConfig("a.log", absA)))
	want := fmt.Sprintf("config: jobs/1/log_path: %q names the same file as jobs/0 (%q)", absA, "a.log")
	if err == nil || err.Error() != want {
		t.Fatalf("Load(relative vs absolute) = %v, want %q", err, want)
	}
}

func assertDistinctJobLogPathsLoad(t *testing.T) {
	t.Helper()
	pairs := [][2]string{
		{"/var/log/a.log", "/var/log/b.log"},
		{"/var/log/a.log", "/var/log/a.log.1"},
	}
	for i := 0; i < len(pairs); i++ {
		body := twoJobsLogPathConfig(pairs[i][0], pairs[i][1])
		if _, err := Load(testContext(t), writeConfig(t, body)); err != nil {
			t.Fatalf("Load(%q vs %q) = %v, want nil", pairs[i][0], pairs[i][1], err)
		}
	}
}

func assertDuplicateJobLogPathsRefused(t *testing.T) {
	t.Helper()
	secondSpellings := []string{
		"/var/log/a.log",
		"/var/log//a.log",
		"/var/log/./a.log",
		"/var/log/x/../a.log",
	}
	for i := 0; i < len(secondSpellings); i++ {
		body := twoJobsLogPathConfig("/var/log/a.log", secondSpellings[i])
		_, err := Load(testContext(t), writeConfig(t, body))
		want := fmt.Sprintf("config: jobs/1/log_path: %q names the same file as jobs/0 (%q)", secondSpellings[i], "/var/log/a.log")
		if err == nil || err.Error() != want {
			t.Fatalf("Load(%q) = %v, want %q", secondSpellings[i], err, want)
		}
	}
	threeJobs := `{"jobs": [
		{"name": "job-zero", "command": ["/bin/true"], "log_path": "/var/log/zero.log", "sandbox": {"mode": "off", "reason": "test"}},
		{"name": "job-one", "command": ["/bin/true"], "log_path": "/var/log/a.log", "sandbox": {"mode": "off", "reason": "test"}},
		{"name": "job-two", "command": ["/bin/true"], "log_path": "/var/log/./a.log", "sandbox": {"mode": "off", "reason": "test"}}
	]}`
	_, err := Load(testContext(t), writeConfig(t, threeJobs))
	wantThree := `config: jobs/2/log_path: "/var/log/./a.log" names the same file as jobs/1 ("/var/log/a.log")`
	if err == nil || !strings.Contains(err.Error(), wantThree) {
		t.Fatalf("Load(three jobs) = %v, want %q", err, wantThree)
	}
}

func twoJobsLogPathConfig(firstPath, secondPath string) string {
	return fmt.Sprintf(`{"jobs": [
		{"name": "job-a", "command": ["/bin/true"], "log_path": %q, "sandbox": {"mode": "off", "reason": "test"}},
		{"name": "job-b", "command": ["/bin/true"], "log_path": %q, "sandbox": {"mode": "off", "reason": "test"}}
	]}`, firstPath, secondPath)
}
