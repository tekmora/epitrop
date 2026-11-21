//go:build linux

package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// deadlineJob is a job that runs until it is stopped, far longer than any bound in these tests. Its first process and a child both
// carry marker on their command lines, so what is left of the process group can be found.
// With trap set, the first process answers the stop signal by writing "stopped" and exiting.
func deadlineJob(t *testing.T, name string, trap bool) (config.JobConfig, string, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), name+"-marker")
	stopped := filepath.Join(t.TempDir(), name+"-stopped")
	script := `/bin/sh -c "sleep 300; :" "$0" & wait`
	if trap {
		script = `trap 'echo stopped > "$1"; exit 0' TERM; /bin/sh -c "sleep 300; :" "$0" & wait`
	}
	job := testJob(name, []string{"/bin/sh", "-c", script, marker, stopped})
	job.LogPath = filepath.Join(t.TempDir(), name+".log")
	job.MaxRuntimeSeconds = 1
	// No grace period unless a test sets one: testJob's default is a second.
	job.Stop.GraceSeconds = 0
	return job, marker, stopped
}

// waitForExited waits for the job.exited event of the job's current start. The bound is far
// above the deadline plus grace of any job in these tests; it only ends a failing run.
func waitForExited(t *testing.T, sup *Supervisor, name string) eventlog.Job {
	t.Helper()
	var job eventlog.Job
	exited := waitUntil(func() bool {
		job = eventlogJob(t, sup, name)
		return job.LastEvent == "job.exited"
	})
	if !exited {
		t.Fatalf("no job.exited for %s after %s; last event %q", name, testWaitBound, job.LastEvent)
	}
	return job
}

func assertGroupGone(t *testing.T, marker string) {
	t.Helper()
	var alive []int
	waitUntil(func() bool {
		alive = liveProcessesWithArg(t, marker)
		return len(alive) == 0
	})
	if len(alive) > 0 {
		t.Fatalf("processes %v of the job still run after its deadline, want the whole group gone", alive)
	}
}

// TestJobIsKilledAtItsDeadline: with no grace period the whole process group is killed when
// the job has run for max_runtime_seconds, and the exit is recorded with reason "deadline".
func TestJobIsKilledAtItsDeadline(t *testing.T) {
	job, marker, _ := deadlineJob(t, "overrun", false)
	sup := testSupervisor(t, job)
	began := time.Now()
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	exited := waitForExited(t, sup, job.Name)
	if exited.LastReason != "deadline" || exited.State != string(StateDead) || exited.LastExitCode == nil || *exited.LastExitCode == 0 {
		t.Fatalf("job.exited = %+v, want dead with reason deadline and a non-zero code", exited)
	}
	if ran := time.Since(began); ran < time.Second {
		t.Fatalf("job was stopped after %s, want it to run its full second", ran)
	}
	assertGroupGone(t, marker)
	waitForUnlock(t, sup, job)
	if sent := exited.LastExitCode; *sent != -1 {
		t.Fatalf("exit code = %d, want -1: with no grace period the job is killed, not asked", *sent)
	}
}

// TestJobGetsItsStopSignalAndGraceAtTheDeadline: with a grace period the job first gets its
// stop signal and may exit by itself; what it left running is killed all the same.
func TestJobGetsItsStopSignalAndGraceAtTheDeadline(t *testing.T) {
	job, marker, stopped := deadlineJob(t, "graceful", true)
	job.Stop.GraceSeconds = 5
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	exited := waitForExited(t, sup, job.Name)
	if exited.LastReason != "deadline" || exited.LastExitCode == nil || *exited.LastExitCode != 0 {
		t.Fatalf("job.exited = %+v, want reason deadline and the job's own exit code 0", exited)
	}
	if body, err := os.ReadFile(stopped); err != nil || strings.TrimSpace(string(body)) != "stopped" { // #nosec G304 -- stopped is the temporary marker written by the test job
		t.Fatalf("stop marker = %q, %v, want the job to have handled its stop signal", body, err)
	}
	assertGroupGone(t, marker)
}

// TestJobThatIgnoresItsStopSignalIsKilledAfterTheGrace: the grace period is a bound.
func TestJobThatIgnoresItsStopSignalIsKilledAfterTheGrace(t *testing.T) {
	job, marker, _ := deadlineJob(t, "stubborn", false)
	job.Command[2] = `trap '' TERM; /bin/sh -c "trap '' TERM; sleep 300; :" "$0" & wait`
	job.Stop.GraceSeconds = 1
	sup := testSupervisor(t, job)
	began := time.Now()
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	exited := waitForExited(t, sup, job.Name)
	if exited.LastReason != "deadline" || exited.LastExitCode == nil || *exited.LastExitCode == 0 {
		t.Fatalf("job.exited = %+v, want reason deadline and a kill", exited)
	}
	if ran := time.Since(began); ran < 2*time.Second {
		t.Fatalf("job was killed after %s, want its second of runtime and its second of grace first", ran)
	}
	assertGroupGone(t, marker)
}

// TestJobThatIgnoresItsStopSignalIsKilledAtOnceWithoutGrace: with no grace period there is
// no stop signal to ignore; the group is killed at the deadline.
func TestJobThatIgnoresItsStopSignalIsKilledAtOnceWithoutGrace(t *testing.T) {
	job, marker, _ := deadlineJob(t, "deaf", false)
	job.Command[2] = `trap '' TERM; /bin/sh -c "trap '' TERM; sleep 300; :" "$0" & wait`
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	exited := waitForExited(t, sup, job.Name)
	if exited.LastReason != "deadline" || exited.LastExitCode == nil || *exited.LastExitCode == 0 {
		t.Fatalf("job.exited = %+v, want reason deadline and a kill", exited)
	}
	assertGroupGone(t, marker)
}

// TestJobThatEndsBeforeItsDeadlineHasNoReason: the deadline changes nothing for a job that
// finishes in time.
func TestJobThatEndsBeforeItsDeadlineHasNoReason(t *testing.T) {
	job := testJob("intime", []string{"/bin/true"})
	job.MaxRuntimeSeconds = 30
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	exited := waitForExited(t, sup, job.Name)
	if exited.LastReason != "" || exited.LastExitCode == nil || *exited.LastExitCode != 0 {
		t.Fatalf("job.exited = %+v, want code 0 and no reason", exited)
	}
}

func TestJobDeadlineBounds(t *testing.T) {
	if got := jobDeadline(shimConfig{}); got != maxJobRuntime {
		t.Fatalf("jobDeadline(none) = %s, want the longest runtime %s", got, maxJobRuntime)
	}
	if got := jobDeadline(shimConfig{MaxRuntimeSeconds: 90}); got != 90*time.Second {
		t.Fatalf("jobDeadline(90) = %s, want 1m30s", got)
	}
	if maxJobRuntime != time.Duration(config.MaxJobRuntimeSeconds)*time.Second {
		t.Fatalf("maxJobRuntime = %s, config allows %d s: the two bounds must be one", maxJobRuntime, config.MaxJobRuntimeSeconds)
	}
	base := validShimConfig(t, "bounds")
	checkShimConfigBoundsCases(t, base)
	checkJobRuntimeBounds(t)
}

func checkShimConfigBoundsCases(t *testing.T, base shimConfig) {
	t.Helper()
	cases := []struct {
		name string
		edit func(*shimConfig)
		want string
	}{
		{"runtime at the bound", func(c *shimConfig) { c.MaxRuntimeSeconds = config.MaxJobRuntimeSeconds }, ""},
		{"runtime above the bound", func(c *shimConfig) { c.MaxRuntimeSeconds = config.MaxJobRuntimeSeconds + 1 }, "max runtime must be"},
		{"negative runtime", func(c *shimConfig) { c.MaxRuntimeSeconds = -1 }, "max runtime must be"},
		{"grace at the bound", func(c *shimConfig) { c.StopGraceSeconds = config.MaxStopGraceSeconds }, ""},
		{"grace above the bound", func(c *shimConfig) { c.StopGraceSeconds = config.MaxStopGraceSeconds + 1 }, "stop grace must be"},
		{"negative grace", func(c *shimConfig) { c.StopGraceSeconds = -1 }, "stop grace must be"},
		{"unknown stop signal", func(c *shimConfig) { c.StopSignal = "KILL" }, "stop signal"},
		{"known stop signal", func(c *shimConfig) { c.StopSignal = "HUP" }, ""},
	}
	for i := 0; i < len(cases); i++ {
		cfg := base
		cases[i].edit(&cfg)
		err := checkShimConfig(cfg)
		if cases[i].want == "" && err != nil {
			t.Errorf("checkShimConfig(%s) = %v, want nil", cases[i].name, err)
		}
		if cases[i].want != "" && (err == nil || !strings.Contains(err.Error(), cases[i].want)) {
			t.Errorf("checkShimConfig(%s) = %v, want %q", cases[i].name, err, cases[i].want)
		}
	}
}

func checkJobRuntimeBounds(t *testing.T) {
	t.Helper()
	job := testJob("bounds", []string{"/bin/true"})
	job.MaxRuntimeSeconds = config.MaxJobRuntimeSeconds + 1
	if err := checkJobBounds(job); err == nil || !strings.Contains(err.Error(), "max_runtime_seconds must be") {
		t.Fatalf("checkJobBounds(runtime above the bound) = %v, want refusal", err)
	}
	job.MaxRuntimeSeconds = -1
	if err := checkJobBounds(job); err == nil {
		t.Fatalf("checkJobBounds(negative runtime) = nil, want refusal")
	}
	job.MaxRuntimeSeconds = config.MaxJobRuntimeSeconds
	if err := checkJobBounds(job); err != nil {
		t.Fatalf("checkJobBounds(runtime at the bound) = %v, want nil", err)
	}
}

// TestShimGetsTheJobsDeadlineAndStopPolicy: the supervisor hands the shim what the job's
// config says, and the shim reads it back.
func TestShimGetsTheJobsDeadlineAndStopPolicy(t *testing.T) {
	job := testJob("handover", []string{"/bin/true"})
	job.MaxRuntimeSeconds, job.Stop.Signal, job.Stop.GraceSeconds = 120, "INT", 7
	sup := testSupervisor(t, job)
	cmd := sup.shimCmd(testContext(t), job)
	sep := lastArgIndex(cmd.Args, "__job-shim")
	if sep < 0 {
		t.Fatalf("shim args = %q, want the shim sentinel", cmd.Args)
	}
	cfg, err := parseShimArgs(cmd.Args[sep:])
	if err != nil {
		t.Fatalf("parseShimArgs(%q) = %v, want nil", cmd.Args[sep:], err)
	}
	if cfg.MaxRuntimeSeconds != 120 || cfg.StopSignal != "INT" || cfg.StopGraceSeconds != 7 {
		t.Fatalf("shim config = runtime %d signal %q grace %d, want 120 INT 7", cfg.MaxRuntimeSeconds, cfg.StopSignal, cfg.StopGraceSeconds)
	}
}
