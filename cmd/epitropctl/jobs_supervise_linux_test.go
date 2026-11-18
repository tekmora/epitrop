package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/supervisor"
)

func newEndsSupervisor(t *testing.T, stderr io.Writer) *supervisor.Supervisor {
	t.Helper()
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
	sup, err := supervisor.New(cfg, supervisor.Options{
		Clock:        clock.NewFake(),
		ShimCommand:  []string{os.Args[0], "-test.run=TestHelperProcessShim", "--"},
		PollInterval: 100 * time.Millisecond,
		Stderr:       stderr,
	})
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Setenv("EPITROP_TEST_SHIM", "1")
	return sup
}

// A job that ran and ended with no restart left leaves nothing alive: supervise says so on
// its stderr and the command exits 3, the status a host unit can name in
// RestartPreventExitStatus.
func TestJobsSuperviseExitsThreeWhenNoJobIsAlive(t *testing.T) {
	var stderr bytes.Buffer
	sup := newEndsSupervisor(t, &stderr)
	err := runLoadedJobsAction(testContext(t), sup, "supervise", "")
	if !errors.Is(err, supervisor.ErrNoJobAlive) {
		t.Fatalf("runLoadedJobsAction(supervise) = %v, want an error wrapping ErrNoJobAlive", err)
	}
	var exitCoder interface{ ExitCode() int }
	if !errors.As(err, &exitCoder) || exitCoder.ExitCode() != 3 {
		t.Fatalf("runLoadedJobsAction(supervise) = %#v, want an error with ExitCode() 3", err)
	}
	line := stderr.String()
	if !strings.HasPrefix(line, "epitrop supervisor: finding: no job alive: ends dead (exit 0,") || !strings.HasSuffix(line, "; supervise exits 3\n") || strings.Count(line, "\n") != 1 {
		t.Fatalf("stderr = %q, want one finding line naming ends with exit 0", line)
	}
}

type blockedSuperviseWriter struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (w blockedSuperviseWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return len(p), nil
}

func TestJobsSuperviseExitsZeroWhenCanceledDuringFindingWrite(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	sup := newEndsSupervisor(t, blockedSuperviseWriter{entered: entered, release: release})
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runLoadedJobsAction(ctx, sup, "supervise", "") }()
	select {
	case <-entered:
		cancel()
	case err := <-done:
		t.Fatalf("supervise ended before finding Write: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("supervise did not reach finding Write within 10s")
	}
	err := <-done
	var stderr bytes.Buffer
	if err != nil || commandExit(err, &stderr) != 0 || stderr.String() != "" {
		t.Fatalf("supervise err = %v, exit = %d, stderr = %q; want nil, 0, empty", err, commandExit(err, &stderr), stderr.String())
	}
}

func TestCommandExitReportsNoJobAliveOnce(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantOutput bool
	}{
		{"already reported", supervisor.ErrNoJobAlive, false},
		{"not written", fmt.Errorf("%w: %w", supervisor.ErrNoJobAlive, supervisor.ErrFindingLineNotWritten), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := fmt.Errorf("command: %w", superviseEnd(tc.err))
			if code := commandExit(err, &stderr); code != 3 {
				t.Fatalf("commandExit = %d, want 3", code)
			}
			got := stderr.String()
			if (!tc.wantOutput && got != "") || (tc.wantOutput && !regexp.MustCompile(`\AError: [^\n]+\n\z`).MatchString(got)) {
				t.Fatalf("stderr = %q, want Error line = %t", got, tc.wantOutput)
			}
		})
	}
}

type forbiddenFindingRetryWriter struct{ t *testing.T }

func (w forbiddenFindingRetryWriter) Write([]byte) (int, error) {
	w.t.Error("commandExit wrote to stderr after a timed-out finding")
	return 0, errors.New("unexpected retry")
}

func TestCommandExitDoesNotRetryTimedOutFinding(t *testing.T) {
	err := fmt.Errorf("%w: %w: %w", supervisor.ErrNoJobAlive, supervisor.ErrFindingLineNotWritten, supervisor.ErrWriteTimedOut)
	if code := commandExit(superviseEnd(err), forbiddenFindingRetryWriter{t: t}); code != 3 {
		t.Fatalf("commandExit = %d, want 3", code)
	}
}
