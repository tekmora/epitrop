package config

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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
