package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/supervisor"
)

// A job that ran and ended with no restart left leaves nothing alive: supervise says so on
// its stderr and the command exits 3, the status a host unit can name in
// RestartPreventExitStatus.
func TestJobsSuperviseExitsThreeWhenNoJobIsAlive(t *testing.T) {
	base := jobsTempDir(t)
	keyPath := filepath.Join(base, "seed.hex")
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(bytes.Repeat([]byte{6}, 32))), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v, want nil", err)
	}
	cfg := config.Default()
	cfg.EventLog.Dir = filepath.Join(base, "state")
	cfg.EventLog.SigningKeyPath = keyPath
	cfg.Jobs = []config.JobConfig{{
		Name:     "ends",
		Command:  []string{"/bin/true"},
		LogPath:  filepath.Join(base, "ends.log"),
		Schedule: "always",
		Restart:  config.JobRestartConfig{Policy: "never"},
		Sandbox:  config.SandboxConfig{Mode: "off", Reason: "supervise exit status unit test"},
	}}
	if err := os.MkdirAll(filepath.Join(cfg.EventLog.Dir, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll(state/.git) = %v, want nil", err)
	}
	var stderr bytes.Buffer
	sup, err := supervisor.New(cfg, supervisor.Options{
		Clock:        clock.NewFake(),
		ShimCommand:  []string{os.Args[0], "-test.run=TestHelperProcessShim", "--"},
		PollInterval: 10 * time.Millisecond,
		Stderr:       &stderr,
	})
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Setenv("EPITROP_TEST_SHIM", "1")

	err = runLoadedJobsAction(testContext(t), sup, "supervise", "")
	if !errors.Is(err, supervisor.ErrNoJobAlive) {
		t.Fatalf("runLoadedJobsAction(supervise) = %v, want an error wrapping ErrNoJobAlive", err)
	}
	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) || exitCoder.ExitCode() != 3 {
		t.Fatalf("runLoadedJobsAction(supervise) = %#v, want an error with ExitCode() 3", err)
	}
	line := stderr.String()
	if !strings.HasPrefix(line, "epitrop supervisor: finding: no job alive: ends dead (") || !strings.HasSuffix(line, "; supervise exits 3\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("stderr = %q, want one finding line naming ends", line)
	}
}
