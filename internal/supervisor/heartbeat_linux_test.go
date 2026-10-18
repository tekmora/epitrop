//go:build linux

package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"golang.org/x/sys/unix"

	"github.com/tekmora/epitrop/internal/config"
)

// heartbeatSupervisor starts a sleeping job whose heartbeat may be maxAge seconds old.
func heartbeatSupervisor(t *testing.T, maxAge int, policy string) (*Supervisor, *clockwork.FakeClock) {
	t.Helper()
	clk := clockwork.NewFakeClock()
	job := testJob("hung", longJobCommand(t))
	job.Heartbeat.MaxAgeSeconds = maxAge
	job.Restart.Policy, job.Restart.MaxRestarts = policy, 1
	sup := testSupervisorWithClock(t, testRuntime(t), clk, job)
	if err := sup.Start(testContext(t), "hung"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	return sup, clk
}

func setBeat(t *testing.T, sup *Supervisor, at time.Time) {
	t.Helper()
	if err := os.Chtimes(sup.cfg.Jobs[0].LogPath, at, at); err != nil {
		t.Fatalf("Chtimes(log) = %v, want nil", err)
	}
}

// A job that holds its lock and writes nothing for longer than its heartbeat allows is
// hung: Status says so, and Reconcile records job.stale and ends it through the stop path
// without a job.stopped record, so its restart policy starts it again.
func TestHungJobIsEndedAndRestartedOnAStaleHeartbeat(t *testing.T) {
	sup, clk := heartbeatSupervisor(t, 5, "on-failure")
	stale := staleAfterSilence(t, sup, clk)
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(stale) = %v, want nil", err)
	}
	assertEndedNotStopped(t, sup)
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(after stale) = %v, want nil", err)
	}
	restarted := statusOne(t, sup, "hung")
	if restarted.State != StateRunning || restarted.ShimPID == stale.ShimPID || restarted.Restarts != 1 {
		t.Fatalf("Status(after restart) = %+v, want a new running copy with 1 restart", restarted)
	}
	stopTestJob(t, sup, "hung")
}

// staleAfterSilence: the fresh job is not stale; after a silent minute it is, with its
// start as the last beat.
func staleAfterSilence(t *testing.T, sup *Supervisor, clk *clockwork.FakeClock) JobStatus {
	t.Helper()
	if fresh := statusOne(t, sup, "hung"); fresh.State != StateRunning || fresh.Stale {
		t.Fatalf("Status(fresh) = %+v, want running and not stale", fresh)
	}
	clk.Advance(time.Minute)
	stale := statusOne(t, sup, "hung")
	if stale.State != StateRunning || !stale.Stale || stale.LastBeat != stale.Since {
		t.Fatalf("Status(silent a minute) = %+v, want running, stale, last beat at its start", stale)
	}
	return stale
}

// assertEndedNotStopped: the stale job is dead, one job.stale and no job.stopped were
// recorded, and the shim recorded its exit.
func assertEndedNotStopped(t *testing.T, sup *Supervisor) {
	t.Helper()
	waitForState(t, sup, "hung", StateDead)
	dir := sup.cfg.EventLog.Dir
	if n := countEvents(t, dir, "job.stale"); n != 1 {
		t.Fatalf("job.stale events = %d, want 1", n)
	}
	if n := countEvents(t, dir, "job.stopped"); n != 0 {
		t.Fatalf("job.stopped events = %d, want 0: a stale job is ended, not stopped", n)
	}
	if !waitUntil(func() bool { return eventlogJob(t, sup, "hung").LastEvent == "job.exited" }) {
		t.Fatalf("last event = %q, want job.exited from the shim", eventlogJob(t, sup, "hung").LastEvent)
	}
}

// A write to the job's log is a beat; the age is measured from the newest one, and an age
// of exactly max_age_seconds is not stale yet.
func TestLogWriteIsAHeartbeat(t *testing.T) {
	sup, clk := heartbeatSupervisor(t, 5, "never")
	clk.Advance(time.Hour)
	setBeat(t, sup, clk.Now().Add(-5*time.Second))
	if st := statusOne(t, sup, "hung"); st.Stale {
		t.Fatalf("Status(beat 5 s ago, max 5) = %+v, want not stale", st)
	}
	setBeat(t, sup, clk.Now().Add(-6*time.Second))
	st := statusOne(t, sup, "hung")
	if !st.Stale || st.LastBeat != clk.Now().Add(-6*time.Second).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("Status(beat 6 s ago, max 5) = %+v, want stale with that beat", st)
	}
	setBeat(t, sup, clk.Now())
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(fresh beat) = %v, want nil", err)
	}
	if st = statusOne(t, sup, "hung"); st.State != StateRunning || st.Stale || countEvents(t, sup.cfg.EventLog.Dir, "job.stale") != 0 {
		t.Fatalf("Status(fresh beat) = %+v, want running, not stale, no job.stale", st)
	}
	stopTestJob(t, sup, "hung")
}

// Without a heartbeat a silent job is never stale.
func TestNoHeartbeatNeverStale(t *testing.T) {
	sup, clk := heartbeatSupervisor(t, 0, "never")
	clk.Advance(365 * 24 * time.Hour)
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}
	if st := statusOne(t, sup, "hung"); st.State != StateRunning || st.Stale || st.LastBeat != "" {
		t.Fatalf("Status(no heartbeat) = %+v, want running, never stale, no beat", st)
	}
	stopTestJob(t, sup, "hung")
}

func stopTestJob(t *testing.T, sup *Supervisor, name string) {
	t.Helper()
	if err := sup.Stop(testContext(t), name); err != nil {
		t.Fatalf("Stop(%s) = %v, want nil", name, err)
	}
}

// startLookalike starts a process whose command line is the job's shim command line, with
// a child of its own, and writes the job record that names both, as a stale record of a
// dead shim would after its pids were reused. It holds no job lock.
func startLookalike(t *testing.T, sup *Supervisor, name string) (shim *exec.Cmd, child int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	shim = exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcessJob", "--", "__job-shim",
		"--name", name, "--lock", sup.lockPath(name), "--record", sup.recordPath(name))
	shim.Env = append(os.Environ(), "EPITROP_TEST_JOB=spawn-child")
	shim.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	out, err := shim.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() = %v, want nil", err)
	}
	if err = shim.Start(); err != nil {
		t.Fatalf("start lookalike = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := unix.Kill(-shim.Process.Pid, unix.SIGKILL); err != nil {
			t.Logf("kill lookalike group %d = %v", shim.Process.Pid, err)
		}
		if err := shim.Wait(); err != nil {
			t.Logf("wait lookalike = %v", err)
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "child=") {
		t.Fatalf("lookalike output = %q, %v, want child=<pid>", line, err)
	}
	if child, err = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "child="))); err != nil {
		t.Fatalf("lookalike child pid %q: %v", line, err)
	}
	writeLookalikeRecord(t, sup, name, child, shim.Process.Pid)
	return shim, child
}

func writeLookalikeRecord(t *testing.T, sup *Supervisor, name string, pid int, shimPID int) {
	t.Helper()
	body, err := json.Marshal(jobRecord{Name: name, PID: pid, ShimPID: shimPID, StartedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatalf("Marshal(record) = %v, want nil", err)
	}
	if err = os.MkdirAll(filepath.Dir(sup.recordPath(name)), 0o700); err != nil {
		t.Fatalf("MkdirAll(jobs) = %v, want nil", err)
	}
	if err = os.WriteFile(sup.recordPath(name), body, 0o600); err != nil {
		t.Fatalf("WriteFile(record) = %v, want nil", err)
	}
}

// The planted self-match of #21: a live process that looks like the job's shim by its
// command line, named by the job's record, but holding no lock, is not the job. Status
// reports the job dead, Stop signals nothing, and Reconcile starts a real copy rather than
// adopting the lookalike.
func TestLookalikeShimIsNeverTheJob(t *testing.T) {
	realSignaller := processSignaller
	rec := withRecordingSignaller(t)
	sup := testSupervisor(t, testJob("daemon", longJobCommand(t)))
	shim, child := startLookalike(t, sup, "daemon")
	assertLookalikeUntouched(t, sup, rec, shim.Process.Pid, child)
	processSignaller = realSignaller
	sup = testSupervisor(t, testJob("daemon", longJobCommand(t)))
	writeLookalikeRecord(t, sup, "daemon", child, shim.Process.Pid)
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(lookalike) = %v, want nil", err)
	}
	if n := countEvents(t, sup.cfg.EventLog.Dir, "job.adopted"); n != 0 {
		t.Fatalf("job.adopted events = %d, want 0", n)
	}
	st := statusOne(t, sup, "daemon")
	if st.State != StateRunning || st.ShimPID == shim.Process.Pid || st.PID == child {
		t.Fatalf("Status(after reconcile) = %+v, want a real copy, not the lookalike %d/%d", st, shim.Process.Pid, child)
	}
}

// assertLookalikeUntouched: Status reports the job dead, and Stop sends no signal and leaves
// the lookalike and its child alive.
func assertLookalikeUntouched(t *testing.T, sup *Supervisor, rec *[]recordedSignal, shimPID, child int) {
	t.Helper()
	if st := statusOne(t, sup, "daemon"); st.State != StateDead || st.PID != 0 {
		t.Fatalf("Status(lookalike) = %+v, want dead", st)
	}
	if err := sup.Stop(testContext(t), "daemon"); err != nil {
		t.Fatalf("Stop(lookalike) = %v, want nil", err)
	}
	if len(*rec) != 0 || !processExists(child) || !processExists(shimPID) {
		t.Fatalf("signals = %+v, lookalike alive = %v/%v; want no signal and both alive", *rec, processExists(shimPID), processExists(child))
	}
}

func TestHeartbeatBound(t *testing.T) {
	job := testJob("bounds", []string{"/bin/true"})
	for _, age := range []int{-1, config.MaxHeartbeatSeconds + 1} {
		job.Heartbeat.MaxAgeSeconds = age
		if err := checkJobBounds(job); err == nil || !strings.Contains(err.Error(), "heartbeat.max_age_seconds must be 0..86400") {
			t.Fatalf("checkJobBounds(heartbeat %d) = %v, want refusal", age, err)
		}
	}
	job.Heartbeat.MaxAgeSeconds = config.MaxHeartbeatSeconds
	if err := checkJobBounds(job); err != nil {
		t.Fatalf("checkJobBounds(heartbeat at the bound) = %v, want nil", err)
	}
}
