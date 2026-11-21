package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// cliBuildTimeout bounds one build of the command. It is separate from the deadline a test
// gives the run: a cold build on a loaded runner can take longer than the run itself is
// allowed to.
const cliBuildTimeout = 5 * time.Minute

// cliTestVersion is set as main.version in the shared build, so the version test needs no
// build of its own.
const cliTestVersion = "v9.8.7"

// cliBuild holds the one build of the command that the tests running it share.
var cliBuild struct {
	once sync.Once
	dir  string
	bin  string
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if cliBuild.dir != "" {
		if err := os.RemoveAll(cliBuild.dir); err != nil {
			fmt.Fprintf(os.Stderr, "remove the command build: %v\n", err)
		}
	}
	os.Exit(code)
}

// buildCLI returns the path of the command, built once for this test process with
// cliTestVersion as its version.
func buildCLI(t *testing.T) string {
	t.Helper()
	cliBuild.once.Do(func() {
		cliBuild.bin, cliBuild.err = buildCLIOnce()
	})
	if cliBuild.err != nil {
		t.Fatal(cliBuild.err)
	}
	return cliBuild.bin
}

func buildCLIOnce() (string, error) {
	dir, err := os.MkdirTemp("", "epitropctl-test-")
	if err != nil {
		return "", fmt.Errorf("create the build directory: %w", err)
	}
	cliBuild.dir = dir
	ctx, cancel := context.WithTimeout(context.Background(), cliBuildTimeout)
	defer cancel()
	bin := filepath.Join(dir, "epitropctl")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.CommandContext(ctx, "go", "build", "-ldflags=-X main.version="+cliTestVersion, "-o", bin, ".").CombinedOutput() // #nosec G204 -- fixed go build writes the CLI into a temporary directory
	if err != nil {
		return "", fmt.Errorf("build epitropctl: %w\n%s", err, out)
	}
	return bin, nil
}

// runSystemCLI runs the built binary with a deadline and an isolated child environment.
func runSystemCLI(t *testing.T, bin string, args ...string) (string, string, int, error) {
	t.Helper()
	return runSystemCLIWithEnv(t, bin, nil, args...)
}

// runSystemCLIWithEnv runs the built binary with additional environment variables.
func runSystemCLIWithEnv(t *testing.T, bin string, extraEnv []string, args ...string) (string, string, int, error) {
	t.Helper()
	return runSystemCLIWithDeadline(t, bin, 30*time.Second, extraEnv, args...)
}

// runSystemCLIWithDeadline runs the built binary under deadline with additional environment variables.
func runSystemCLIWithDeadline(t *testing.T, bin string, deadline time.Duration, extraEnv []string, args ...string) (string, string, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...) // #nosec G204 -- callers pass the CLI built by buildCLI and fixture arguments
	cmd.Env = isolatedCLIEnv(t, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

func isolatedCLIEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	tempDir := t.TempDir()
	env := make([]string, 0, 16+len(extra))
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := kv[:eq]
		if strings.EqualFold(key, "PATH") {
			env = append(env, kv)
		} else if runtime.GOOS == "windows" {
			if strings.EqualFold(key, "SystemRoot") ||
				strings.EqualFold(key, "ComSpec") ||
				strings.EqualFold(key, "PATHEXT") ||
				strings.EqualFold(key, "WINDIR") {
				env = append(env, kv)
			}
		}
	}
	env = append(env,
		"HOME="+tempDir,
		"XDG_CONFIG_HOME="+filepath.Join(tempDir, "config"),
		"XDG_DATA_HOME="+filepath.Join(tempDir, "data"),
		"XDG_CACHE_HOME="+filepath.Join(tempDir, "cache"),
		"XDG_STATE_HOME="+filepath.Join(tempDir, "state"),
		"TMPDIR="+tempDir,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"LC_ALL=C",
	)
	if runtime.GOOS == "windows" {
		env = append(env, "TEMP="+tempDir, "TMP="+tempDir, "USERPROFILE="+tempDir)
	}
	env = append(env, extra...)
	return env
}

func TestIsolatedCLIEnv_Isolation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "sentinel-gh-token")
	t.Setenv("RELEASE_WATCH_TOKEN", "sentinel-rw-token")
	t.Setenv("API_KEY", "sentinel-api-key")

	env := isolatedCLIEnv(t, "EXTRA_SENTINEL=allowed-value")

	hasPath := false
	hasExtra := false
	for _, kv := range env {
		parts := strings.SplitN(kv, "=", 2)
		k := parts[0]
		if strings.Contains(strings.ToUpper(k), "TOKEN") || strings.Contains(strings.ToUpper(k), "KEY") {
			t.Errorf("isolatedCLIEnv leaked sensitive environment variable %q", kv)
		}
		if strings.EqualFold(k, "PATH") {
			hasPath = true
		}
		if kv == "EXTRA_SENTINEL=allowed-value" {
			hasExtra = true
		}
	}
	if !hasPath {
		t.Error("isolatedCLIEnv missing PATH")
	}
	if !hasExtra {
		t.Error("isolatedCLIEnv missing explicit extra variable EXTRA_SENTINEL")
	}
}
