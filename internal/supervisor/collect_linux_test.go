//go:build linux

package supervisor

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// zombieChildOfThisProcess reports whether pid is a zombie whose parent is the test process:
// a shim the supervisor under test started and did not collect. A pid that is gone, or
// belongs to another process by now, is not one.
func zombieChildOfThisProcess(pid int) bool {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	status := string(body)
	return strings.HasPrefix(procStatusField(status, "State:"), "Z") && procStatusField(status, "PPid:") == strconv.Itoa(os.Getpid())
}

// TestSupervisorCollectsItsShims: every job start runs one shim as a child of the supervisor.
// After the shims have exited none of them may be left as a zombie, however many starts
// the supervisor has made.
func TestSupervisorCollectsItsShims(t *testing.T) {
	job := testJob("flap", []string{"/bin/true"})
	sup := testSupervisor(t, job)
	const starts = 5
	shims := make([]int, 0, starts)
	for i := 0; i < starts; i++ {
		if err := sup.Start(testContext(t), job.Name); err != nil {
			t.Fatalf("Start(%d) = %v, want nil", i, err)
		}
		shims = append(shims, eventlogJob(t, sup, job.Name).ShimPID)
		waitForShimWaiters(t, sup, 0)
	}
	// The bound only ends a run in which a shim is never collected.
	left := len(shims)
	waitUntil(func() bool {
		left = 0
		for j := 0; j < len(shims); j++ {
			if zombieChildOfThisProcess(shims[j]) {
				left++
			}
		}
		return left == 0
	})
	if left > 0 {
		t.Fatalf("%d of %d shims %v are zombies of the supervisor, want every shim collected", left, starts, shims)
	}
	waitForShimWaiters(t, sup, 0)
}

func TestStartRefusesAShimBeyondTheWaiterBound(t *testing.T) {
	job := testJob("bound", []string{"/bin/true"})
	sup := testSupervisor(t, job)
	sup.shimWaiters.Store(maxShimWaiters)
	err := sup.Start(testContext(t), job.Name)
	if err == nil || !strings.Contains(err.Error(), "shims are still uncollected") {
		t.Fatalf("Start(at the waiter bound) = %v, want refusal", err)
	}
	if waiting := sup.shimWaiters.Load(); waiting != maxShimWaiters {
		t.Fatalf("shim waiters = %d after a refused start, want %d unchanged", waiting, maxShimWaiters)
	}
	if recorded := eventlogJob(t, sup, job.Name); recorded.ShimPID != 0 {
		t.Fatalf("event log holds a start by shim %d after a refused start, want none", recorded.ShimPID)
	}
	// One below the bound is the last start that is allowed.
	sup.shimWaiters.Store(maxShimWaiters - 1)
	if err = sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start(one below the waiter bound) = %v, want nil", err)
	}
	waitForShimWaiters(t, sup, maxShimWaiters-1)
	sup.shimWaiters.Store(0)
}

func TestStartGivesTheWaiterBackWhenTheShimCannotStart(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", []string{"/bin/true"}))
	sup.shimCommand = []string{filepath.Join(t.TempDir(), "missing-shim")}
	err := sup.Start(testContext(t), "probe")
	if err == nil || !strings.Contains(err.Error(), "start shim") {
		t.Fatalf("Start(missing shim) = %v, want start-shim error", err)
	}
	if waiting := sup.shimWaiters.Load(); waiting != 0 {
		t.Fatalf("shim waiters = %d after a shim that could not start, want 0", waiting)
	}
}

func TestCollectShimReportsOnlyAWaitThatFails(t *testing.T) {
	var reported []string
	var stderr bytes.Buffer
	old := reportShimError
	t.Cleanup(func() { reportShimError = old })
	reportShimError = func(w io.Writer, timeout time.Duration, name string, err error) {
		if timeout != defaultWriteTimeout {
			t.Errorf("reportShimError timeout = %s, want %s", timeout, defaultWriteTimeout)
		}
		if w != io.Writer(&stderr) {
			t.Errorf("reportShimError writer = %v, want the supervisor's stderr", w)
		}
		reported = append(reported, fmt.Sprintf("%s: %v", name, err))
	}
	sup := &Supervisor{stderr: &stderr, writeTimeout: defaultWriteTimeout}

	failed := exec.CommandContext(t.Context(), "/bin/false")
	if err := failed.Start(); err != nil {
		t.Fatalf("Start(false) = %v, want nil", err)
	}
	sup.shimWaiters.Store(2)
	sup.collectShim(failed, "refused")
	if len(reported) != 0 {
		t.Fatalf("collectShim(exit status 1) reported %q, want nothing: the shim reports its own failures", reported)
	}
	if zombieChildOfThisProcess(failed.Process.Pid) {
		t.Fatalf("shim %d is a zombie after collectShim, want it collected", failed.Process.Pid)
	}

	neverStarted := exec.CommandContext(t.Context(), "/bin/true")
	sup.collectShim(neverStarted, "lost")
	if len(reported) != 1 || !strings.HasPrefix(reported[0], "lost: ") || strings.HasSuffix(reported[0], "<nil>") {
		t.Fatalf("collectShim(wait fails) reported %q, want one wait error named for the job", reported)
	}
	if waiting := sup.shimWaiters.Load(); waiting != 0 {
		t.Fatalf("shim waiters = %d after two collections, want 0", waiting)
	}
}

// TestSupervisorCleanupReapsAllShims: after a test using testSupervisor finishes and its
// cleanup runs, all shims started by the supervisor have been reaped and none remain.
func TestSupervisorCleanupReapsAllShims(t *testing.T) {
	job := testJob("cleanup", longJobCommand(t))
	var sup *Supervisor
	var shimPID int
	t.Run("supervised", func(t *testing.T) {
		sup = testSupervisor(t, job)
		if err := sup.Start(testContext(t), job.Name); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		shimPID = eventlogJob(t, sup, job.Name).ShimPID
	})
	if waiting := sup.shimWaiters.Load(); waiting != 0 {
		t.Fatalf("shim waiters = %d after supervisor cleanup, want 0: a shim was left uncollected", waiting)
	}
	if zombieChildOfThisProcess(shimPID) {
		comm, ppid, state := zombieChildDiagnostic(shimPID)
		t.Fatalf("shim %d (%s) is a zombie child after supervisor cleanup (PPid %s, State %s), want collected", shimPID, comm, ppid, state)
	}
}
