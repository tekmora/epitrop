//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

// readOnlyTestSandbox is an enforced sandbox whose workspace holds meta/hk/pc and declares
// meta/hk read-only.
func readOnlyTestSandbox(t *testing.T) config.SandboxConfig {
	t.Helper()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(workspace) = %v, want nil", err)
	}
	if err = os.MkdirAll(filepath.Join(ws, "meta", "hk"), 0o700); err != nil {
		t.Fatalf("MkdirAll(meta/hk) = %v, want nil", err)
	}
	writeRT03File(t, filepath.Join(ws, "meta", "hk", "pc"), "orig\n")
	return config.SandboxConfig{Mode: "enforce", Workspace: ws, Network: "none", MemoryMax: "256M", CPUWeight: 100, TasksMax: 128, ReadOnly: []string{"meta/hk"}}
}

func TestAppendReadOnlyBindsPinsEachAncestorOnce(t *testing.T) {
	got := appendReadOnlyBinds(nil, "/ws", []string{"/ws/a/b/c", "/ws/a/d", "/ws/e"})
	want := []string{
		"--bind", "/ws/a", "/ws/a",
		"--bind", "/ws/a/b", "/ws/a/b",
		"--ro-bind", "/ws/a/b/c", "/ws/a/b/c",
		"--ro-bind", "/ws/a/d", "/ws/a/d",
		"--ro-bind", "/ws/e", "/ws/e",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("appendReadOnlyBinds() = %q, want %q", got, want)
	}
	if got = appendReadOnlyBinds(nil, "/ws", nil); len(got) != 0 {
		t.Fatalf("appendReadOnlyBinds(none) = %q, want nothing", got)
	}
}

// The read-only binds follow the writable workspace, which they cover, and the pins come
// before the binds they protect.
func TestBuildSandboxCommandBindsReadOnlyOverTheWorkspace(t *testing.T) {
	sandbox := readOnlyTestSandbox(t)
	built, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, sandboxBuildOptions{Tools: sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"}, Env: os.LookupEnv})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	argv := strings.Join(built.Argv, " ")
	ws := sandbox.Workspace
	order := []string{
		"--bind " + ws + " " + ws + " ",
		"--bind " + ws + "/meta " + ws + "/meta ",
		"--ro-bind " + ws + "/meta/hk " + ws + "/meta/hk ",
	}
	last := -1
	for i := 0; i < len(order); i++ {
		at := strings.Index(argv, order[i])
		if at <= last {
			t.Fatalf("argv = %q, want %q after the previous bind", argv, order[i])
		}
		last = at
	}
}

// A read-only path swapped for a symlink out of the workspace after the load is refused at
// the start, before bwrap would bind the target read-only and the job could read it.
func TestBuildSandboxCommandRefusesAReadOnlyPathSwappedOut(t *testing.T) {
	sandbox := readOnlyTestSandbox(t)
	hooks := filepath.Join(sandbox.Workspace, "meta", "hk")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatalf("RemoveAll(meta/hk) = %v, want nil", err)
	}
	if err := os.Symlink(t.TempDir(), hooks); err != nil {
		t.Fatalf("Symlink(meta/hk) = %v, want nil", err)
	}
	_, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, sandboxBuildOptions{Tools: sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"}, Env: os.LookupEnv})
	if err == nil || !strings.Contains(err.Error(), "read_only/0") || !strings.Contains(err.Error(), "not below the workspace") {
		t.Fatalf("buildSandboxCommand(meta/hk now a symlink out) = %v, want the path refused", err)
	}
}

func TestShimArgsCarryTheReadOnlyPaths(t *testing.T) {
	sandbox := readOnlyTestSandbox(t)
	sandbox.ReadOnly = []string{"meta/hk", "meta/pc"}
	parsed, err := parseShimArgs(shimArgs(t, withShimSandbox(validShimConfig(t, "read-only-args"), sandbox)))
	if err != nil || !slices.Equal(parsed.Sandbox.ReadOnly, sandbox.ReadOnly) {
		t.Fatalf("parseShimArgs() = %q, %v, want %q in order", parsed.Sandbox.ReadOnly, err, sandbox.ReadOnly)
	}
	sandbox.ReadOnly = nil
	if args := sandboxFlagArgs(sandbox); slices.Contains(args, "--sandbox-read-only") {
		t.Fatalf("sandboxFlagArgs(no read-only paths) = %q, want no --sandbox-read-only", args)
	}
}

// The job cannot write, add to, empty or move a read-only directory, nor move its parent
// aside to make a writable one of the same name; the rest of the workspace, the parent
// included, stays writable.
func TestSandboxedJobCannotChangeItsReadOnlyPaths(t *testing.T) {
	tools := rt03Tools(t, "none")
	sandbox := readOnlyTestSandbox(t)
	script := `if echo changed > meta/hk/pc 2>/dev/null; then echo wrote >> writes.txt; fi
if touch meta/hk/new 2>/dev/null; then echo created >> writes.txt; fi
if rm -f meta/hk/pc 2>/dev/null && [ ! -e meta/hk/pc ]; then echo removed >> writes.txt; fi
if mv meta/hk meta/moved 2>/dev/null; then echo moved >> writes.txt; fi
if mv meta aside 2>/dev/null; then echo parent-moved >> writes.txt; mkdir -p meta/hk && echo planted > meta/hk/pc; fi
echo free > free.txt
echo beside > meta/beside.txt
: >> writes.txt`
	built, err := buildSandboxCommand([]string{"/usr/bin/bash", "-c", script}, sandbox, sandboxBuildOptions{Tools: tools, Env: os.LookupEnv})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	if !runRT03Command(t, built) {
		t.Fatal("the job failed, want it to run and record what it could do")
	}
	ws := sandbox.Workspace
	if got := readWorkspaceFile(t, ws, "writes.txt"); got != "" {
		t.Errorf("the job changed its read-only path: %q, want nothing", got)
	}
	if got := readWorkspaceFile(t, ws, "meta/hk/pc"); got != "orig\n" {
		t.Errorf("meta/hk/pc = %q, want it unchanged", got)
	}
	for _, name := range []string{"meta/hk/new", "meta/moved", "aside"} {
		if _, err = os.Lstat(filepath.Join(ws, name)); !os.IsNotExist(err) {
			t.Errorf("Lstat(%s) = %v, want it absent", name, err)
		}
	}
	if readWorkspaceFile(t, ws, "free.txt") != "free\n" || readWorkspaceFile(t, ws, "meta/beside.txt") != "beside\n" {
		t.Error("the job could not write the rest of its workspace, want it writable")
	}
}

func readWorkspaceFile(t *testing.T, ws string, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(ws, name)) // #nosec G304 -- path inside the test's own workspace.
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", name, err)
	}
	return string(body)
}
