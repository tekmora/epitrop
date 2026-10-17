//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// unsafeSignalTarget reports whether a kill(2) target would reach more than one process
// group the test started. A negative target names the group -target, so it is bounded on its
// absolute value: kill(-1) reaches every process the user owns, kill(0) and kill(-pgrp) the
// test's own group. A mock process whose pid converts to 1 lands here too (#264).
func unsafeSignalTarget(target int) bool {
	abs := target
	if abs < 0 {
		abs = -abs
	}
	return abs <= 1 || abs == os.Getpid() || abs == syscall.Getpgrp()
}

// guardedKill sends sig to target through kill, unless the target is unsafe; then it
// returns an error and kill is never called.
func guardedKill(target int, sig unix.Signal, kill func(int, unix.Signal) error) error {
	if unsafeSignalTarget(target) {
		return fmt.Errorf("refuse signal %d to unsafe target %d", sig, target)
	}
	return kill(target, sig)
}

func TestGuardedKillRefusesUnsafeTargets(t *testing.T) {
	targets := []int{0, 1, -1, os.Getpid(), -os.Getpid(), syscall.Getpgrp(), -syscall.Getpgrp()}
	for _, target := range targets {
		sent := 0
		err := guardedKill(target, unix.SIGKILL, func(int, unix.Signal) error {
			sent++
			return nil
		})
		if err == nil {
			t.Errorf("guardedKill(%d) = nil, want refusal", target)
		}
		if sent != 0 {
			t.Errorf("guardedKill(%d) sent %d signals, want 0", target, sent)
		}
	}
}

func TestGuardedKillPassesBoundedTarget(t *testing.T) {
	// A fixed pid above 1 that is neither this process nor its group; the recorder stands in
	// for kill, so no signal is sent.
	target := 1 << 22
	var got []int
	err := guardedKill(-target, unix.SIGTERM, func(pid int, _ unix.Signal) error {
		got = append(got, pid)
		return nil
	})
	if err != nil {
		t.Fatalf("guardedKill(%d) = %v, want nil", -target, err)
	}
	if len(got) != 1 || got[0] != -target {
		t.Fatalf("kill calls = %v, want [%d]", got, -target)
	}
}
