//go:build linux

package supervisor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tekmora/epitrop/internal/config"
)

// testHosts returns the path to a temporary hosts file for testing and registers
// cleanup to remove the directory when the test completes.
func testHosts(t *testing.T) string {
	t.Helper()
	hosts, err := startJobHosts(config.SandboxConfig{Mode: "enforce"})
	if err != nil {
		t.Fatalf("startJobHosts() = %v, want nil", err)
	}
	t.Cleanup(func() {
		stopHostsForShim(hosts, io.Discard)
	})
	return hosts.path
}

// TestBwrapArgsBindsHostsReadOnlyOnceForEachNetworkMode tests positive conditions on bwrap
// arguments: /etc/hosts must be bound read-only exactly once for each network mode,
// source must be opts.Hosts, never /etc/hosts, and /etc/hosts must never be bound writable.
func TestBwrapArgsBindsHostsReadOnlyOnceForEachNetworkMode(t *testing.T) {
	networks := []string{"none", "egress", "proxy"}
	proxy := sandboxProxy{Socket: "/tmp/proxy.sock", Relay: "/usr/bin/epitropctl"}
	tools := sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"}

	for i := 0; i < len(networks); i++ {
		net := networks[i]
		t.Run(net, func(t *testing.T) {
			ws := t.TempDir()
			sandbox := config.SandboxConfig{
				Mode:      "enforce",
				Workspace: ws,
				Network:   net,
			}
			hostsPath := filepath.Join(t.TempDir(), "hosts")
			if hostsPath == "/etc/hosts" {
				t.Fatalf("hostsPath is /etc/hosts, want a private per-job file")
			}

			// Test bwrapBaseArgs.
			baseArgs := bwrapBaseArgs(tools.Bwrap, sandbox, hostsPath)
			assertHostsBoundReadOnlyOnce(t, baseArgs, hostsPath)

			// Test full bwrapArgs.
			fullArgs := bwrapArgs("/usr/bin/true", []string{"/usr/bin/true"}, sandbox, tools, proxy, nil, hostsPath)
			assertHostsBoundReadOnlyOnce(t, fullArgs, hostsPath)
		})
	}
}

// assertHostsBoundReadOnlyOnce verifies /etc/hosts is bound read-only exactly once with the
// expected source, and is never bound writable.
func assertHostsBoundReadOnlyOnce(t *testing.T, args []string, wantSource string) {
	t.Helper()
	var roBinds int
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--ro-bind" && args[i+2] == "/etc/hosts" {
			roBinds++
			if args[i+1] != wantSource {
				t.Errorf("--ro-bind /etc/hosts source = %q, want %q", args[i+1], wantSource)
			}
			if args[i+1] == "/etc/hosts" {
				t.Errorf("--ro-bind /etc/hosts binds host's /etc/hosts, want private per-job source")
			}
		}
		if args[i] == "--bind" && args[i+2] == "/etc/hosts" {
			t.Errorf("found writable --bind for /etc/hosts, want only read-only")
		}
	}
	if roBinds != 1 {
		t.Errorf("/etc/hosts bound %d times, want exactly once; args = %v", roBinds, args)
	}
}

// TestBuildSandboxCommandRefusesEmptyOrRelativeHosts tests negative conditions on buildSandboxCommand:
// empty Hosts -> error; relative Hosts -> error.
func TestBuildSandboxCommandRefusesEmptyOrRelativeHosts(t *testing.T) {
	tools := sandboxTools{Bwrap: "/usr/bin/bwrap", Env: "/usr/bin/env"}
	sandbox := config.SandboxConfig{
		Mode:      "enforce",
		Workspace: t.TempDir(),
		Network:   "none",
	}

	// Empty Hosts -> error.
	opts := sandboxBuildOptions{Tools: tools, Hosts: ""}
	if _, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, opts); err == nil || !strings.Contains(err.Error(), "hosts") {
		t.Errorf("buildSandboxCommand(empty Hosts) = %v, want error naming hosts", err)
	}

	// Relative Hosts -> error.
	for _, rel := range []string{"hosts", "relative/hosts", "./hosts", "../hosts"} {
		opts = sandboxBuildOptions{Tools: tools, Hosts: rel}
		if _, err := buildSandboxCommand([]string{"/usr/bin/true"}, sandbox, opts); err == nil || !strings.Contains(err.Error(), "hosts") || !strings.Contains(err.Error(), "relative") {
			t.Errorf("buildSandboxCommand(Hosts=%q) = %v, want error naming hosts and relative", rel, err)
		}
	}
}

// TestPrepareJobHostsFileModeContentAndCleanup tests the boundary condition:
// hosts writer makes a 0444 file with exact content in a 0700 dir, and after shim
// cleanup the dir is gone.
func TestPrepareJobHostsFileModeContentAndCleanup(t *testing.T) {
	sandbox := config.SandboxConfig{
		Mode:      "enforce",
		Workspace: t.TempDir(),
		Network:   "none",
	}

	hosts, err := startJobHosts(sandbox)
	if err != nil {
		t.Fatalf("startJobHosts() = %v, want nil", err)
	}

	// Directory must be mode 0700.
	dirInfo, err := os.Stat(hosts.dir)
	if err != nil {
		t.Fatalf("Stat(%s) = %v, want nil", hosts.dir, err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("hosts dir perm = %#o, want 0700", perm)
	}

	// File must be mode 0444.
	fileInfo, err := os.Stat(hosts.path)
	if err != nil {
		t.Fatalf("Stat(%s) = %v, want nil", hosts.path, err)
	}
	if perm := fileInfo.Mode().Perm(); perm != 0o444 {
		t.Errorf("hosts file perm = %#o, want 0444", perm)
	}

	// Content must be exact sandboxHostsContent.
	body, err := os.ReadFile(hosts.path)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", hosts.path, err)
	}
	want := "127.0.0.1 localhost\n::1 localhost\n"
	if string(body) != want {
		t.Errorf("hosts file content = %q, want %q", string(body), want)
	}

	// After shim cleanup the directory is gone.
	var logBuf bytes.Buffer
	stopHostsForShim(hosts, &logBuf)
	if _, err := os.Stat(hosts.dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(hosts.dir) after cleanup = %v, want ErrNotExist", err)
	}
}

// TestSandboxedJobReadsLocalhostOnly tests that a real sandboxed job running inside bwrap
// sees only the two localhost lines in /etc/hosts, and leaves no .slot directory in workspace.
func TestSandboxedJobReadsLocalhostOnly(t *testing.T) {
	tools := rt03Tools(t, "none")
	sandbox := config.SandboxConfig{
		Mode:      "enforce",
		Workspace: t.TempDir(),
		Network:   "none",
		MemoryMax: "256M",
		CPUWeight: 100,
		TasksMax:  128,
	}

	hosts := testHosts(t)
	script := `cat /etc/hosts > "$PWD/hosts.txt"`
	built, err := buildSandboxCommand([]string{"/usr/bin/bash", "-c", script}, sandbox, sandboxBuildOptions{
		Tools: tools,
		Env:   os.LookupEnv,
		Hosts: hosts,
	})
	if err != nil {
		t.Fatalf("buildSandboxCommand() = %v, want nil", err)
	}
	if !runRT03Command(t, built) {
		t.Fatal("the sandboxed job failed, want it to run")
	}

	outPath := filepath.Join(sandbox.Workspace, "hosts.txt")
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("ReadFile(hosts.txt) = %v, want nil", err)
	}
	want := "127.0.0.1 localhost\n::1 localhost\n"
	if string(body) != want {
		t.Errorf("cat /etc/hosts inside sandbox = %q, want %q", string(body), want)
	}

	// Plain sandboxed job without home has no .slot in workspace.
	slotDir := filepath.Join(sandbox.Workspace, ".slot")
	if _, err := os.Stat(slotDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace has %s: %v, want no .slot directory", slotDir, err)
	}
}
