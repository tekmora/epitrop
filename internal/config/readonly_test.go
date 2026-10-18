package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// readOnlyWorkspace makes a resolved workspace with a hooks directory two levels down, a
// docs directory, the home's directory, a symlink inside, one to the workspace itself and
// one out of it. It returns the workspace and the directory outside.
func readOnlyWorkspace(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the sandbox does not run on Windows")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(temporary directory) = %v, want nil", err)
	}
	ws, outside := filepath.Join(root, "ws"), filepath.Join(root, "outside")
	for _, dir := range []string{filepath.Join(ws, "meta", "hk"), filepath.Join(ws, "docs"), filepath.Join(ws, SandboxHomeTop), outside} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("MkdirAll(%s) = %v, want nil", dir, err)
		}
	}
	links := map[string]string{"link-in": filepath.Join(ws, "docs"), "self": ws, "out": outside}
	for name, target := range links {
		if err = os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Fatalf("Symlink(%s) = %v, want nil", name, err)
		}
	}
	return ws, outside
}

// The shape check reads the configuration's slash-separated form on every platform.
func TestValidateSandboxReadOnlyShape(t *testing.T) {
	for _, ok := range [][]string{nil, {"meta/hk"}, {"a/b/c", ".slot"}} {
		if err := validateSandboxReadOnly(SandboxConfig{Mode: "enforce", ReadOnly: ok}); err != nil {
			t.Errorf("validateSandboxReadOnly(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "/x", "a/", "a//b", "a/./b", "a/../b"} {
		err := validateSandboxReadOnly(SandboxConfig{Mode: "enforce", ReadOnly: []string{bad}})
		if err == nil || !strings.Contains(err.Error(), "no clean relative path") {
			t.Errorf("validateSandboxReadOnly(%q) = %v, want it refused", bad, err)
		}
	}
}

func TestResolveSandboxReadOnlyAcceptsPathsBelowTheWorkspace(t *testing.T) {
	ws, _ := readOnlyWorkspace(t)
	sandbox := SandboxConfig{Mode: "enforce", Workspace: ws, ReadOnly: []string{"meta/hk", "link-in", SandboxHomeTop}}
	got, err := ResolveSandboxReadOnly(sandbox)
	want := []string{filepath.Join(ws, "meta", "hk"), filepath.Join(ws, "docs"), filepath.Join(ws, SandboxHomeTop)}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("ResolveSandboxReadOnly() = %q, %v, want %q", got, err, want)
	}
	if got, err = ResolveSandboxReadOnly(SandboxConfig{Mode: "enforce", Workspace: ws}); err != nil || len(got) != 0 {
		t.Fatalf("ResolveSandboxReadOnly(none) = %q, %v, want nothing", got, err)
	}
}

func TestResolveSandboxReadOnlyRefusesPathsOutOfPlace(t *testing.T) {
	ws, _ := readOnlyWorkspace(t)
	cases := []struct {
		name     string
		mode     string
		readOnly []string
		home     bool
		want     string
	}{
		{"absolute", "enforce", []string{ws + "/meta"}, false, "no clean relative path"},
		{"parent", "enforce", []string{"../outside"}, false, "no clean relative path"},
		{"the workspace", "enforce", []string{"."}, false, "no clean relative path"},
		{"empty", "enforce", []string{""}, false, "no clean relative path"},
		{"unclean", "enforce", []string{"meta/../docs"}, false, "no clean relative path"},
		{"trailing slash", "enforce", []string{"meta/"}, false, "no clean relative path"},
		{"missing", "enforce", []string{"meta/gone"}, false, "no such file"},
		{"symlink out", "enforce", []string{"out"}, false, "not below the workspace"},
		{"symlink to the workspace", "enforce", []string{"self"}, false, "not below the workspace"},
		{"nested", "enforce", []string{"meta", "meta/hk"}, false, "overlaps the read-only path"},
		{"twice", "enforce", []string{"docs", "link-in"}, false, "overlaps the read-only path"},
		{"home's directory", "enforce", []string{SandboxHomeTop}, true, "overlaps the home's directory"},
		{"sandbox off", "off", []string{"meta/hk"}, false, "forbidden unless mode=enforce"},
	}
	for _, tc := range cases {
		sandbox := SandboxConfig{Mode: tc.mode, Workspace: ws, ReadOnly: tc.readOnly}
		if tc.home {
			sandbox.Home = &SandboxHome{Source: ws, Env: "CODEX_HOME"}
		}
		got, err := ResolveSandboxReadOnly(sandbox)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "read_only") {
			t.Errorf("%s: ResolveSandboxReadOnly(%q) = %q, %v, want an error with %q", tc.name, tc.readOnly, got, err, tc.want)
		}
	}
}

func TestResolveSandboxReadOnlyCountBoundary(t *testing.T) {
	ws, _ := readOnlyWorkspace(t)
	paths := make([]string, 0, MaxSandboxReadOnly+1)
	for i := 0; i <= MaxSandboxReadOnly; i++ {
		name := fmt.Sprintf("p%02d", i)
		if err := os.Mkdir(filepath.Join(ws, name), 0o700); err != nil {
			t.Fatalf("Mkdir(%s) = %v, want nil", name, err)
		}
		paths = append(paths, name)
	}
	sandbox := SandboxConfig{Mode: "enforce", Workspace: ws, ReadOnly: paths[:MaxSandboxReadOnly]}
	if got, err := ResolveSandboxReadOnly(sandbox); err != nil || len(got) != MaxSandboxReadOnly {
		t.Fatalf("ResolveSandboxReadOnly(%d paths) = %d, %v, want all", MaxSandboxReadOnly, len(got), err)
	}
	sandbox.ReadOnly = paths
	if _, err := ResolveSandboxReadOnly(sandbox); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d", MaxSandboxReadOnly)) {
		t.Fatalf("ResolveSandboxReadOnly(%d paths) = %v, want the count refused", len(paths), err)
	}
}

// The loader resolves the read-only paths against the resolved workspace: a missing one
// stops the load, and the configuration keeps the relative values.
func TestLoadChecksTheReadOnlyPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the sandbox does not run on Windows")
	}
	w := newSandboxPathWorld(t)
	ws := filepath.Join(w.root, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "meta", "hk"), 0o700); err != nil {
		t.Fatalf("MkdirAll(meta/hk) = %v, want nil", err)
	}
	cfg, err := w.loadReadOnly(t, ws, []string{"meta/hk"})
	if err != nil || len(cfg.Jobs) != 1 || !slices.Equal(cfg.Jobs[0].Sandbox.ReadOnly, []string{"meta/hk"}) {
		t.Fatalf("Load(read_only meta/hk) = %+v, %v, want it kept relative", cfg.Jobs, err)
	}
	for _, readOnly := range [][]string{{"meta/gone"}, {"/etc"}, {"../conf"}} {
		if _, err = w.loadReadOnly(t, ws, readOnly); err == nil || !strings.Contains(err.Error(), "read_only/0") {
			t.Errorf("Load(read_only %q) = %v, want it refused", readOnly, err)
		}
	}
}

func (w sandboxPathWorld) loadReadOnly(t *testing.T, workspace string, readOnly []string) (Config, error) {
	t.Helper()
	sandbox, err := json.Marshal(map[string]any{"workspace": workspace, "read_only": readOnly})
	if err != nil {
		t.Fatalf("marshal sandbox: %v", err)
	}
	body := fmt.Sprintf(`{"sandbox_input_roots": [%q], "event_log": {"dir": %q, "signing_key_path": %q}, "jobs": [{"name": "daemon", "command": ["/bin/true"], "log_path": "/tmp/daemon.log", "sandbox": %s}]}`, filepath.Join(w.root, "ws"), w.events, w.key, sandbox)
	path := filepath.Join(w.confDir, "config.json")
	if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile(config) = %v, want nil", err)
	}
	return Load(testContext(t), path)
}
