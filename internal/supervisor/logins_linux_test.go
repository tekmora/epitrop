//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

// sandboxLogins are login files as shells, forges and coding clients keep them below HOME.
var sandboxLogins = []string{
	".claude/.credentials.json", ".claude.json", ".codex/auth.json", ".gemini/oauth_creds.json",
	".config/gh/hosts.yml", ".config/opencode/auth.json", ".ssh/id_ed25519", ".netrc",
	".git-credentials", ".docker/config.json", ".aws/credentials",
}

// loginHome plants every login file in a home directory of its own. It lies in the package
// directory, not below the temporary directories: those are empty tmpfs mounts inside the
// sandbox, which would hide a planted file from the job even if a bind exposed it.
func loginHome(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() = %v, want nil", err)
	}
	home, err := os.MkdirTemp(wd, ".login-home-")
	if err != nil {
		t.Fatalf("MkdirTemp() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(home); err != nil {
			t.Errorf("RemoveAll(%s) = %v, want nil", home, err)
		}
	})
	if home, err = filepath.EvalSymlinks(home); err != nil {
		t.Fatalf("EvalSymlinks(%s) = %v, want nil", home, err)
	}
	for i := 0; i < len(sandboxLogins); i++ {
		path := filepath.Join(home, filepath.FromSlash(sandboxLogins[i]))
		if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) = %v, want nil", path, err)
		}
		if err = os.WriteFile(path, []byte("planted\n"), 0o600); err != nil {
			t.Fatalf("WriteFile(%s) = %v, want nil", path, err)
		}
	}
	return home
}

// loadLoginJob loads a configuration whose job is given its own client folder alone, the
// way an operator writes it, and returns the job as the loader resolved it.
func loadLoginJob(t *testing.T, home string, workspace string, roots []string, inputs []string) (config.JobConfig, error) {
	t.Helper()
	dir := t.TempDir()
	doc := map[string]any{
		"sandbox_input_roots": roots,
		"event_log":           map[string]any{"dir": filepath.Join(dir, "events")},
		"jobs": []any{map[string]any{
			"name": "client", "command": []string{"/usr/bin/true"}, "log_path": filepath.Join(dir, "client.log"),
			"sandbox": map[string]any{"mode": "enforce", "workspace": workspace, "inputs": inputs},
		}},
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal the configuration: %v", err)
	}
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v, want nil", err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cfg, err := config.Load(ctx, path)
	if err != nil {
		return config.JobConfig{}, err
	}
	return cfg.Jobs[0], nil
}

// A job given its own client folder reads the login file in it and none of the others,
// which the loader refuses to hand it at all.
func TestSandboxedJobReadsOnlyItsOwnClientLogin(t *testing.T) {
	tools := rt03Tools(t, "none")
	home := loginHome(t)
	workspace := t.TempDir()
	own := filepath.Join(home, ".claude")
	roots := []string{workspace, own}
	for i := 1; i < len(sandboxLogins); i++ {
		other := filepath.Join(home, filepath.FromSlash(sandboxLogins[i]))
		if _, err := loadLoginJob(t, home, workspace, roots, []string{own, other}); err == nil || !strings.Contains(err.Error(), "inputs/1: "+other) {
			t.Errorf("Load(input %s) = %v, want it refused", other, err)
		}
	}
	job, err := loadLoginJob(t, home, workspace, roots, []string{own})
	if err != nil {
		t.Fatalf("Load(own client folder) = %v, want nil", err)
	}
	var script strings.Builder
	script.WriteString(": > reads.txt\n")
	for i := 0; i < len(sandboxLogins); i++ {
		fmt.Fprintf(&script, "if cat '%s' >/dev/null 2>&1; then echo '%s' >> reads.txt; fi\n", filepath.Join(home, sandboxLogins[i]), sandboxLogins[i])
	}
	built, err := buildSandboxCommand([]string{"/usr/bin/bash", "-c", script.String()}, job.Sandbox, sandboxBuildOptions{Tools: tools, Env: os.LookupEnv, Hosts: testHosts(t)})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	if !runRT03Command(t, built) {
		t.Fatal("the job failed, want it to run and record what it read")
	}
	reads, err := os.ReadFile(filepath.Join(workspace, "reads.txt"))
	if err != nil {
		t.Fatalf("ReadFile(reads.txt) = %v, want what the job read", err)
	}
	if got, want := string(reads), sandboxLogins[0]+"\n"; got != want {
		t.Fatalf("the job read %q, want its own client login alone: %q", got, want)
	}
}
