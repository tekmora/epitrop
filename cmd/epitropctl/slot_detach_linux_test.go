//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/slot"
)

// detachWaitPolls bounds the wait for a detached run's report: 360 polls, half a second
// apart.
const detachWaitPolls = 360

// systemctlUser asks the user manager about a unit and returns what it printed.
func systemctlUser(ctx context.Context, args ...string) string {
	out, err := exec.CommandContext(ctx, "systemctl", append([]string{"--user"}, args...)...).Output() // #nosec G204 -- test callers select actions for their generated user unit
	if err != nil && len(out) == 0 {
		return err.Error()
	}
	return strings.TrimSpace(string(out))
}

// skipWithoutSandbox skips where the host cannot start what the test needs, except under
// CI, where the user manager and the sandbox are part of the machine.
func skipWithoutSandbox(t *testing.T, what string, detail string) {
	t.Helper()
	if os.Getenv("CI") == "true" {
		t.Fatalf("under CI: %s: %s", what, detail)
	}
	t.Skipf("skipped: %s here: %s", what, detail)
}

// A detached run goes on after the caller is gone: the command returns from inside a scope
// of its own, the scope ends, and the runner, a service of the user manager with the
// template's limits, takes the work of the run and goes away.
func TestSlotRunDetachedOutlivesItsCaller(t *testing.T) {
	bin := buildCLI(t)
	script := "sleep 5\n" + standInDone
	run := newSlotRun(t, func(run slotRun) string {
		command, err := json.Marshal([]string{"/usr/bin/sh", "-c", fmt.Sprintf(script, slotTestUnit, run.outside, run.out)})
		if err != nil {
			t.Fatalf("Marshal(command) = %v, want nil", err)
		}
		return fmt.Sprintf(`{"name": "slot-1", "command": %s, "max_runtime_seconds": 120, "stop": {"signal": "TERM", "grace_seconds": 1}, "sandbox": {"network": "none", "memory_max": "512M", "memory_high": "384M", "tasks_max": 128}}`, command)
	})
	service := slotServiceName(run.out)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		systemctlUser(ctx, "stop", service)
	})
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("rand.Read() = %v, want nil", err)
	}
	caller := "epitrop-test-caller-" + hex.EncodeToString(suffix) + ".scope"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute+30*time.Second)
	defer cancel()
	callerArgs := append([]string{"--user", "--scope", "--quiet", "--collect", "--expand-environment=no", "--unit=" + caller, "--", bin, "slot", "run", "--detach"}, run.args[1:]...)
	printed, err := exec.CommandContext(ctx, "systemd-run", callerArgs...).CombinedOutput() // #nosec G204 -- argv starts the test-built CLI in the generated user scope
	if err != nil {
		skipWithoutSandbox(t, "the caller's scope or the runner's service did not start", fmt.Sprintf("%v: %s", err, printed))
	}
	checkDetachedOutputAndReportAbsence(t, printed, service, run.out)
	waitScopeEnded(ctx, t, caller)
	checkDetachedServiceLimits(t, ctx, service)
	report := waitDetachedReport(ctx, t, run.out)
	if report.Outcome == slot.OutcomeRefused {
		skipWithoutSandbox(t, "the sandbox did not start", report.Reason)
	}
	checkDetachedFinalReport(t, report)
}

func checkDetachedOutputAndReportAbsence(t *testing.T, printed []byte, service string, runOut string) {
	t.Helper()
	if want := "service\t" + service + "\n"; !strings.Contains(string(printed), want) {
		t.Fatalf("slot run --detach printed %q, want %q", printed, want)
	}
	if _, statErr := os.Lstat(filepath.Join(runOut, slot.ReportFile)); !os.IsNotExist(statErr) {
		t.Fatalf("the report stood when the command returned (%v): the caller waited for the run", statErr)
	}
}

func checkDetachedServiceLimits(t *testing.T, ctx context.Context, service string) {
	t.Helper()
	limits := systemctlUser(ctx, "show", "--property=MemoryMax", "--property=MemoryHigh", service)
	if !strings.Contains(limits, "MemoryMax=536870912") || !strings.Contains(limits, "MemoryHigh=402653184") {
		t.Fatalf("service %s has %q, want MemoryMax=512M and MemoryHigh=384M", service, limits)
	}
}

func checkDetachedFinalReport(t *testing.T, report slot.Report) {
	t.Helper()
	if report.Outcome != slot.OutcomeDone || report.Status != "review" || !report.Published {
		t.Fatalf("report = %+v, want a done run published after its caller ended", report)
	}
	if want := "brief starts with # Unit; wrote outside the workspace: no"; report.Summary != want {
		t.Fatalf("summary = %q, want %q", report.Summary, want)
	}
}

// waitScopeEnded waits until the user manager has let a scope go. The scope ends with its
// last process, and the manager notes that a moment after the command returned; a scope
// that stays active holds a process the command left behind.
func waitScopeEnded(ctx context.Context, t *testing.T, scope string) {
	t.Helper()
	for i := 0; i < 40; i++ {
		if state := systemctlUser(ctx, "is-active", scope); state != "active" && state != "deactivating" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("caller's scope %s is still active, want it ended:\n%s", scope, systemctlUser(ctx, "status", "--no-pager", scope))
}

// waitDetachedReport waits for the report a detached run writes when it ends, and for its
// service to be gone.
func waitDetachedReport(ctx context.Context, t *testing.T, out string) slot.Report {
	t.Helper()
	service := slotServiceName(out)
	for i := 0; i < detachWaitPolls; i++ {
		body, err := os.ReadFile(filepath.Join(out, slot.ReportFile)) // #nosec G304 -- out is the test-created slot output directory
		state := systemctlUser(ctx, "is-active", service)
		if err == nil && state != "active" && state != "activating" && state != "deactivating" {
			var report slot.Report
			if jsonErr := json.Unmarshal(body, &report); jsonErr != nil {
				t.Fatalf("Unmarshal(report) = %v, want nil", jsonErr)
			}
			return report
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for the report of %s: %v", service, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("no report from %s after %d polls; journalctl --user -u %s", service, detachWaitPolls, service)
	return slot.Report{}
}
