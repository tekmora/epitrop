//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// endlessJobForms are shell forms of a loop or a wait without an end. Each is spelled in two
// halves, so that this file does not hold what it searches for.
var endlessJobForms = []string{"while " + "true", "while " + ":", "sleep " + "infinity", "tail " + "-f"}

// endlessJobForm returns the form found in source, or "".
func endlessJobForm(source string) string {
	for i := 0; i < len(endlessJobForms); i++ {
		if strings.Contains(source, endlessJobForms[i]) {
			return endlessJobForms[i]
		}
	}
	return ""
}

// A test job without an end stays behind for good when its test fails before stopping it and
// its shim is gone: one such job ran for five hours before it was found. Every test job that
// runs until it is stopped is written with untilStopped.
func TestNoTestJobRunsWithoutAnEnd(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("Glob(*_test.go) = %d files, %v; want the test sources of this package", len(files), err)
	}
	for i := 0; i < len(files); i++ {
		body, err := os.ReadFile(files[i])
		if err != nil {
			t.Fatalf("ReadFile(%s) = %v, want nil", files[i], err)
		}
		lines := strings.Split(string(body), "\n")
		for n := 0; n < len(lines); n++ {
			if form := endlessJobForm(lines[n]); form != "" {
				t.Errorf("%s:%d: a test job with %q has no end; use untilStopped", files[i], n+1, form)
			}
		}
	}
}

func TestEndlessJobFormFindsWhatItIsMeantToFind(t *testing.T) {
	planted := []string{
		"while " + "true; do sleep 1; done",
		"trap : TERM; while " + ":; do sleep 1; done",
		"exec sleep " + "infinity",
		"tail " + "-f /dev/null",
	}
	for i := 0; i < len(planted); i++ {
		if endlessJobForm(planted[i]) == "" {
			t.Errorf("endlessJobForm(%q) = \"\", want the endless form reported", planted[i])
		}
	}
	bounded := []string{untilStopped("", time.Second), "sleep 30", `i=0; while [ "$i" -lt 3 ]; do i=$((i+1)); done`}
	for i := 0; i < len(bounded); i++ {
		if form := endlessJobForm(bounded[i]); form != "" {
			t.Errorf("endlessJobForm(%q) = %q, want \"\": the script ends by itself", bounded[i], form)
		}
	}
}

func TestUntilStoppedGivesALoopOfTenMinutes(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		pause time.Duration
		want  string
	}{
		{"one second, no body", "", time.Second, `i=0; while [ "$i" -lt 600 ]; do  sleep 1; i=$((i+1)); done`},
		{"a beat every 50 ms", "printf . >> beat;", 50 * time.Millisecond, `i=0; while [ "$i" -lt 12000 ]; do printf . >> beat; sleep 0.05; i=$((i+1)); done`},
	}
	for i := 0; i < len(cases); i++ {
		if got := untilStopped(cases[i].body, cases[i].pause); got != cases[i].want {
			t.Errorf("%s: untilStopped() = %q, want %q", cases[i].name, got, cases[i].want)
		}
	}
}

// The loop has to end by itself: the same script with three rounds runs to its end.
func TestBoundedLoopEndsByItself(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "rounds")
	script := boundedLoop("printf . >> "+marker+";", time.Millisecond, 3)
	if out, err := exec.CommandContext(testContext(t), "/bin/sh", "-c", script).CombinedOutput(); err != nil { // #nosec G204 -- script is a fixed bounded loop writing a t.TempDir marker
		t.Fatalf("sh -c %q = %v (%s), want it to end with exit 0", script, err, out)
	}
	body, err := os.ReadFile(marker) // #nosec G304 -- marker is constructed under t.TempDir
	if err != nil || string(body) != "..." {
		t.Fatalf("rounds = %q, %v; want three", body, err)
	}
}

// sleeper starts a process that the test ends again. With leader it leads its own process
// group, as a job does.
func sleeper(t *testing.T, leader bool) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: leader}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("Start(sleep) = %v, want nil", err)
	}
	t.Cleanup(func() {
		cancel()
		// The cancel kills the process; Wait then reports that exit and nothing else.
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			t.Errorf("Wait(sleep) = %v, want the exit of the cancelled process", err)
		}
	})
	return cmd.Process.Pid
}

// Nothing but a job in its scope is a leftover job. The job itself is the case of
// TestCleanupEndsASandboxedJobLeftRunning.
func TestLeftoverJobIsNothingOutsideAJobScope(t *testing.T) {
	cases := []struct {
		name string
		pid  int
	}{
		{"a group leader outside a job scope", sleeper(t, true)},
		{"a process of the test's own group", sleeper(t, false)},
		{"pid 0", 0},
		{"pid 1", 1},
		{"the test itself", os.Getpid()},
		{"a process that has exited", exitedPID(t)},
	}
	for i := 0; i < len(cases); i++ {
		if leftoverJob(cases[i].pid) {
			t.Errorf("%s: leftoverJob(%d) = true, want false", cases[i].name, cases[i].pid)
		}
	}
}

// The cleanup of a test ends a sandboxed job that nothing else ended. Here the job runs under
// a live shim and the cleanup is called without the Stop that goes before it.
func TestCleanupEndsASandboxedJobLeftRunning(t *testing.T) {
	requireSystemdUserScope(t)
	job := sandboxedSupervisorJob(t, "sandbox-left", untilStopped("", time.Second))
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	pid := statusOne(t, sup, job.Name).PID
	if !leftoverJob(pid) {
		t.Fatalf("leftoverJob(%d) = false, want the running job recognised", pid)
	}
	// A process inside the job's scope that does not lead the group is not the job: a signal
	// to "its" group would go to whatever group carries that number.
	child := 0
	if !waitUntil(func() bool { child = childOf(t, pid); return child != 0 }) {
		t.Fatalf("job %d has no child after %s, want the sandboxed command under it", pid, testWaitBound)
	}
	if leftoverJob(child) {
		t.Fatalf("leftoverJob(%d) = true for a child of the job, want false", child)
	}
	endLeftoverSandboxedJobs(testContext(t), t, sup)
	waitForProcessGone(t, pid)
	if exited := waitForExited(t, sup, job.Name); exited.State != string(StateDead) {
		t.Fatalf("job after the cleanup = %+v, want it dead", exited)
	}
}

// childOf returns a child process of parent, or 0 when it has none.
func childOf(t *testing.T, parent int) int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("ReadDir(/proc) = %v, want nil", err)
	}
	for i := 0; i < len(entries); i++ {
		pid, convErr := strconv.Atoi(entries[i].Name())
		if convErr != nil {
			continue
		}
		if ppid, parentErr := linuxProcessParent(pid); parentErr == nil && ppid == parent {
			return pid
		}
	}
	return 0
}
