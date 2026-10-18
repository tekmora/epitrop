//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/golusoris/golusoris/core/clock"
	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/eventlog"
	"golang.org/x/sys/unix"
)

func TestHelperProcessShim(t *testing.T) {
	if os.Getenv("EPITROP_TEST_SHIM") != "1" {
		return
	}
	code := RunShim(argsAfterDash(os.Args))
	os.Exit(code)
}

func TestHelperProcessJob(t *testing.T) {
	if os.Getenv("EPITROP_TEST_JOB") == "" {
		return
	}
	runHelperJob()
	os.Exit(0)
}

func TestShimClosesLockWhenLogOpenFails(t *testing.T) {
	cfg := validShimConfig(t, "daemon")
	fileParent := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(fileParent, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(state-file) = %v, want nil", err)
	}
	cfg.LogPath = filepath.Join(fileParent, "daemon.log")
	err := errorWithoutPanic(t, "runShim", func() error { return runShim(cfg) })
	if err == nil || !strings.Contains(err.Error(), "mkdir log dir") {
		t.Fatalf("runShim(log) = %v, want mkdir log dir", err)
	}
	assertLockReleased(t, cfg.LockPath)
}

func TestStartStatusRunning(t *testing.T) {
	sup := testSupervisor(t, testJob("daemon", longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	st, err := sup.Status(testContext(t), "daemon")
	if err != nil {
		t.Fatalf("Status() = %v, want nil", err)
	}
	if st[0].State != StateRunning || st[0].PID == 0 {
		t.Fatalf("Status() = %+v, want running with pid", st)
	}
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start(already running) = %v, want nil", err)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 1 {
		t.Fatalf("job.started events = %d, want no duplicate start", starts)
	}
}

func TestShellMetacharactersRunLiterally(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "literal.log")
	job := testJob("literal", []string{os.Args[0], "-test.run=TestHelperProcessJob", "--", "$HOME;echo bad"})
	job.LogPath = logPath
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "echo-arg")
	if err := sup.Start(testContext(t), "literal"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	waitForState(t, sup, "literal", StateDead)
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log) = %v, want nil", err)
	}
	if string(body) != "$HOME;echo bad\n" {
		t.Fatalf("log = %q, want literal shell metacharacters", body)
	}
}

func TestKilledJobReportsDead(t *testing.T) {
	sup := testSupervisor(t, testJob("daemon", longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	running := statusOne(t, sup, "daemon")
	if err := killStartedProcessGroup(running.PID); err != nil {
		t.Fatalf("killStartedProcessGroup(%d) = %v, want nil", running.PID, err)
	}
	waitForState(t, sup, "daemon", StateDead)
}

func TestReplayAdoptsWithoutSecondStart(t *testing.T) {
	base := testRuntime(t)
	job := testJob("daemon", longJobCommand(t))
	first := testSupervisorAt(t, base, job)
	if err := first.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	before := statusOne(t, first, "daemon")
	second := testSupervisorAt(t, base, job)
	if err := second.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}
	after := statusOne(t, second, "daemon")
	if after.State != StateRunning || after.PID != before.PID {
		t.Fatalf("adopted status = %+v, want same running pid %d", after, before.PID)
	}
	if starts := countEvents(t, base.EventLogDir, "job.started"); starts != 1 {
		t.Fatalf("job.started events = %d, want exactly one", starts)
	}
	if adopted := countEvents(t, base.EventLogDir, "job.adopted"); adopted != 1 {
		t.Fatalf("job.adopted events = %d, want exactly one", adopted)
	}
}

func TestLinuxChildDoesNotInheritLockFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux /proc fd inspection required")
	}
	base := testRuntime(t)
	job := testJob("daemon", longJobCommand(t))
	sup := testSupervisorAt(t, base, job)
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	st := statusOne(t, sup, "daemon")
	hasLock, err := processHasOpenPath(st.PID, filepath.Join(base.EventLogDir, "jobs", "daemon.lock"))
	if err != nil {
		t.Fatalf("processHasOpenPath(%d) = %v, want nil", st.PID, err)
	}
	if hasLock {
		t.Fatalf("child pid %d holds job lock fd, want lock held only by shim", st.PID)
	}
}

func TestLinuxShimRemainsChildParent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux /proc status inspection required")
	}
	sup := testSupervisor(t, testJob("daemon", longJobCommand(t)))
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	st := statusOne(t, sup, "daemon")
	ppid, err := processParent(st.PID)
	if err != nil {
		t.Fatalf("processParent(%d) = %v, want nil", st.PID, err)
	}
	if ppid != st.ShimPID {
		t.Fatalf("child parent = %d, want shim pid %d", ppid, st.ShimPID)
	}
}

func TestLinuxKilledShimKillsJobBeforeLockFree(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux parent-death signal required")
	}
	base := testRuntime(t)
	job := testJob("daemon", longJobCommand(t))
	job.Restart = config.JobRestartConfig{Policy: "always", MaxRestarts: 1}
	first := testSupervisorAt(t, base, job)
	if err := first.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	before := statusOne(t, first, "daemon")
	assertSafeSignalPID(t, before.ShimPID)
	if err := syscall.Kill(before.ShimPID, syscall.SIGKILL); err != nil {
		t.Fatalf("SIGKILL shim %d = %v, want nil", before.ShimPID, err)
	}
	waitForProcessGone(t, before.PID)
	waitForUnlock(t, first, job)
	second := testSupervisorAt(t, base, job)
	checkReconcileAfterKilledShim(t, base, second, before)
}

func checkReconcileAfterKilledShim(t *testing.T, base testRuntimeConfig, second *Supervisor, before JobStatus) {
	t.Helper()
	if err := second.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(lost) = %v, want nil", err)
	}
	if exits := countEvents(t, base.EventLogDir, "job.exited"); exits != 1 {
		t.Fatalf("job.exited events = %d, want one lost record", exits)
	}
	if err := second.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(restart) = %v, want nil", err)
	}
	after := statusOne(t, second, "daemon")
	if after.State != StateRunning || after.PID == 0 || after.PID == before.PID {
		t.Fatalf("fresh status = %+v, want one new running child after pid %d", after, before.PID)
	}
	if starts := countEvents(t, base.EventLogDir, "job.started"); starts != 2 {
		t.Fatalf("job.started events = %d, want original plus one restart", starts)
	}
}

func TestWaitStartedRequiresCurrentStartedEvent(t *testing.T) {
	job := testJob("daemon", longJobCommand(t))
	job.Restart = config.JobRestartConfig{Policy: "always", MaxRestarts: 1}
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	before := eventlogJob(t, sup, "daemon")
	payload := map[string]any{"state": string(StateDead)}
	if err := sup.appendJobEvent(testContext(t), "job.restarted", "daemon", payload); err != nil {
		t.Fatalf("appendJobEvent(job.restarted) = %v, want nil", err)
	}
	nextShim := before.ShimPID + 1
	if startedBy(eventlogJob(t, sup, "daemon"), before, nextShim) {
		t.Fatalf("startedBy(next shim %d) = true after job.restarted only, want false", nextShim)
	}
}

func TestOnFailureRestartStopsAtMax(t *testing.T) {
	job := testJob("flap", []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.Restart = config.JobRestartConfig{Policy: "on-failure", MaxRestarts: 2}
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "exit-7")
	for i := 0; i < 4; i++ {
		if err := sup.Reconcile(testContext(t)); err != nil {
			t.Fatalf("Reconcile(%d) = %v, want nil", i, err)
		}
		waitForState(t, sup, "flap", StateDead)
	}
	if restarts := statusOne(t, sup, "flap").Restarts; restarts != 2 {
		t.Fatalf("restarts = %d, want 2", restarts)
	}
}

func TestReconcileStartsNewAlwaysJob(t *testing.T) {
	job := testJob("daemon", longJobCommand(t))
	sup := testSupervisor(t, job)
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}
	st := statusOne(t, sup, "daemon")
	if st.State != StateRunning || st.PID == 0 {
		t.Fatalf("status = %+v, want running new job", st)
	}
}

func TestNeverRestartDoesNotRestartFailure(t *testing.T) {
	job := testJob("once", []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.Restart = config.JobRestartConfig{Policy: "never", MaxRestarts: 2}
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "exit-7")
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(start) = %v, want nil", err)
	}
	waitForState(t, sup, "once", StateDead)
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile(no restart) = %v, want nil", err)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 1 {
		t.Fatalf("job.started events = %d, want no restart", starts)
	}
}

func TestReconcileSkipsNonAlwaysSchedule(t *testing.T) {
	job := testJob("event", longJobCommand(t))
	job.Schedule = "event"
	job.OnEvent = &config.JobEventTrigger{Type: "job.exited", TaskID: "other"}
	sup := testSupervisor(t, job)
	if err := sup.Reconcile(testContext(t)); err != nil {
		t.Fatalf("Reconcile() = %v, want nil", err)
	}
	if starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); starts != 0 {
		t.Fatalf("job.started events = %d, want none for event schedule", starts)
	}
}

func TestStopEscalatesAfterGrace(t *testing.T) {
	job := testJob("stubborn", []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.Stop = config.JobStopConfig{Signal: "TERM", GraceSeconds: 0}
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "ignore-term")
	if err := sup.Start(testContext(t), "stubborn"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if err := sup.Stop(testContext(t), "stubborn"); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	if got := statusOne(t, sup, "stubborn").State; got != StateStopped {
		t.Fatalf("state = %s, want stopped", got)
	}
}

func TestStopTermSignalsWholeProcessGroup(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "group.log")
	job := testJob("group", []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.LogPath = logPath
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "spawn-child")
	if err := sup.Start(testContext(t), "group"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	child := waitForChildPIDInLog(t, logPath)
	if err := sup.Stop(testContext(t), "group"); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	waitForProcessGone(t, child)
}

func TestStopEscalationKillsWholeProcessGroup(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "kill-group.log")
	job := testJob("killgroup", []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.LogPath = logPath
	job.Stop = config.JobStopConfig{Signal: "TERM", GraceSeconds: 0}
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "spawn-child-ignore-term")
	if err := sup.Start(testContext(t), "killgroup"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	child := waitForChildPIDInLog(t, logPath)
	if err := sup.Stop(testContext(t), "killgroup"); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	waitForProcessGone(t, child)
}

func TestUnknownJobRefused(t *testing.T) {
	sup := testSupervisor(t, testJob("known", longJobCommand(t)))
	err := sup.Start(testContext(t), "missing")
	if !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Start(missing) = %v, want ErrUnknownJob", err)
	}
}

func TestCheckJobRejectsRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		job  config.JobConfig
		want string
	}{
		{name: "name", job: config.JobConfig{Command: []string{"/bin/true"}, LogPath: "/tmp/job.log"}, want: "name is required"},
		{name: "command", job: config.JobConfig{Name: "daemon", LogPath: "/tmp/job.log"}, want: "command must have"},
		{name: "too many args", job: config.JobConfig{Name: "daemon", Command: manyArgs(config.MaxJobArgs + 1), LogPath: "/tmp/job.log"}, want: "command must have"},
		{name: "log path", job: config.JobConfig{Name: "daemon", Command: []string{"/bin/true"}}, want: "log_path is required"},
		{name: "sandbox", job: withJobSandbox(testJob("daemon", []string{"/bin/true"}), config.SandboxConfig{Mode: "off"}), want: "sandbox.reason: required when mode=off"},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			err := checkJob(tests[i].job)
			if err == nil || !strings.Contains(err.Error(), tests[i].want) {
				t.Fatalf("checkJob() = %v, want %q", err, tests[i].want)
			}
		})
	}
}

func TestCheckJobRejectsRestartAndStopBounds(t *testing.T) {
	tests := []struct {
		name string
		job  config.JobConfig
		want string
	}{
		{name: "negative restarts", job: boundedJob("daemon", -1, 0, 0), want: "restart.max_restarts"},
		{name: "too many restarts", job: boundedJob("daemon", config.MaxJobRestarts+1, 0, 0), want: "restart.max_restarts"},
		{name: "negative backoff", job: boundedJob("daemon", 0, -1, 0), want: "restart.backoff_seconds"},
		{name: "too much backoff", job: boundedJob("daemon", 0, config.MaxRestartBackoff+1, 0), want: "restart.backoff_seconds"},
		{name: "negative window", job: withRestartWindow(boundedJob("daemon", 0, 0, 0), -1), want: "restart.window_seconds"},
		{name: "too much window", job: withRestartWindow(boundedJob("daemon", 0, 0, 0), config.MaxRestartWindowSeconds+1), want: "restart.window_seconds"},
		{name: "negative grace", job: boundedJob("daemon", 0, 0, -1), want: "stop.grace_seconds"},
		{name: "too much grace", job: boundedJob("daemon", 0, 0, config.MaxStopGraceSeconds+1), want: "stop.grace_seconds"},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			err := checkJob(tests[i].job)
			if err == nil || !strings.Contains(err.Error(), tests[i].want) {
				t.Fatalf("checkJob() = %v, want %q", err, tests[i].want)
			}
		})
	}
}

func TestCheckJobAcceptsRestartAndStopMaxBounds(t *testing.T) {
	job := boundedJob("daemon", config.MaxJobRestarts, config.MaxRestartBackoff, config.MaxStopGraceSeconds)
	job.Restart.WindowSeconds = config.MaxRestartWindowSeconds
	if err := checkJob(job); err != nil {
		t.Fatalf("checkJob(max bounds) = %v, want nil", err)
	}
	tests := []struct {
		name string
		job  config.JobConfig
		want string
	}{
		{name: "restarts", job: boundedJob("daemon", config.MaxJobRestarts+1, 0, 0), want: "restart.max_restarts"},
		{name: "backoff", job: boundedJob("daemon", 0, config.MaxRestartBackoff+1, 0), want: "restart.backoff_seconds"},
		{name: "window", job: withRestartWindow(boundedJob("daemon", 0, 0, 0), config.MaxRestartWindowSeconds+1), want: "restart.window_seconds"},
		{name: "grace", job: boundedJob("daemon", 0, 0, config.MaxStopGraceSeconds+1), want: "stop.grace_seconds"},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			err := checkJob(tests[i].job)
			if err == nil || !strings.Contains(err.Error(), tests[i].want) {
				t.Fatalf("checkJob(%s Max+1) = %v, want %q", tests[i].name, err, tests[i].want)
			}
		})
	}
}

func TestCheckJobRestartWindowBounds(t *testing.T) {
	job := boundedJob("daemon", 0, 0, 0)
	job.Restart.WindowSeconds = config.MaxRestartWindowSeconds
	if err := checkJob(job); err != nil {
		t.Fatalf("checkJob(604800) = %v, want nil", err)
	}
	job.Restart.WindowSeconds = config.MaxRestartWindowSeconds + 1
	err := checkJob(job)
	if err == nil || !strings.Contains(err.Error(), "restart.window_seconds must be 0..604800") {
		t.Fatalf("checkJob(604801) = %v, want 'restart.window_seconds must be 0..604800'", err)
	}
	job.Restart.WindowSeconds = -1
	err = checkJob(job)
	if err == nil || !strings.Contains(err.Error(), "restart.window_seconds must be 0..604800") {
		t.Fatalf("checkJob(-1) = %v, want 'restart.window_seconds must be 0..604800'", err)
	}
}

func TestNormalizeConfigAppliesSupervisorJobDefaults(t *testing.T) {
	cfg := config.Default()
	cfg.Jobs = []config.JobConfig{{
		Name:    "daemon",
		Command: []string{"/bin/true"},
		LogPath: "/tmp/daemon.log",
	}}
	got := normalizeConfig(cfg).Jobs[0]
	if got.Schedule != "always" || got.Restart.Policy != "never" || got.Stop.Signal != "TERM" {
		t.Fatalf("normalizeConfig() job = %+v, want supervisor defaults", got)
	}
}

func TestCheckShimConfigRejectsRequiredFields(t *testing.T) {
	base := shimConfig{EventLogDir: "/state", SigningKey: "/key", Name: "job", LockPath: "/lock", RecordPath: "/record", LogPath: "/log", Command: []string{"/bin/true"}, Sandbox: config.SandboxConfig{Mode: "off", Reason: "unit test"}}
	tests := []struct {
		name string
		cfg  shimConfig
		want string
	}{
		{name: "identity", cfg: withShimName(base, ""), want: "event log, signing key and name are required"},
		{name: "paths", cfg: withShimLock(base, ""), want: "lock, record and log paths are required"},
		{name: "command", cfg: withShimCommand(base, nil), want: "command must have"},
		{name: "too many args", cfg: withShimCommand(base, manyArgs(config.MaxJobArgs+1)), want: "command must have"},
		{name: "sandbox", cfg: withShimSandbox(base, config.SandboxConfig{Mode: "off"}), want: "sandbox.reason: required when mode=off"},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			err := checkShimConfig(tests[i].cfg)
			if err == nil || !strings.Contains(err.Error(), tests[i].want) {
				t.Fatalf("checkShimConfig() = %v, want %q", err, tests[i].want)
			}
		})
	}
}

func TestCheckConfigRejectsMissingEventLogFields(t *testing.T) {
	cfg := config.Default()
	cfg.EventLog.Dir = ""
	if err := checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "event_log.dir is required") {
		t.Fatalf("checkConfig(no dir) = %v, want event_log.dir", err)
	}
	cfg = config.Default()
	cfg.EventLog.SigningKeyPath = ""
	if err := checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "event_log.signing_key_path is required") {
		t.Fatalf("checkConfig(no key) = %v, want signing key", err)
	}
	cfg = config.Default()
	cfg.EventLog.SigningKeyPath = "/tmp/key"
	cfg.Jobs = make([]config.JobConfig, config.MaxJobs+1)
	if err := checkConfig(cfg); err == nil || !strings.Contains(err.Error(), "jobs exceeds") {
		t.Fatalf("checkConfig(too many jobs) = %v, want jobs exceeds", err)
	}
}

func TestCheckConfigRejectsEveryJob(t *testing.T) {
	cfg := config.Default()
	cfg.EventLog.SigningKeyPath = "/tmp/key"
	cfg.Jobs = []config.JobConfig{
		testJob("first", []string{"/bin/true"}),
		{Name: "second", Command: []string{"/bin/true"}},
	}
	err := checkConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "jobs/1") || !strings.Contains(err.Error(), "log_path is required") {
		t.Fatalf("checkConfig(second bad job) = %v, want indexed job error", err)
	}
}

func TestSelectedJobsAndReadyContextBounds(t *testing.T) {
	sup := &Supervisor{cfg: config.Config{Jobs: []config.JobConfig{
		testJob("one", []string{"/bin/true"}),
		testJob("two", []string{"/bin/true"}),
	}}}
	jobs, err := sup.selectedJobs("two")
	if err != nil || len(jobs) != 1 || jobs[0].Name != "two" {
		t.Fatalf("selectedJobs(two) = %+v, %v, want only two", jobs, err)
	}
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{name: "nil", ctx: nil, want: "requires context"},
		{name: "no deadline", ctx: context.Background(), want: "requires context deadline"},
		{name: "done", ctx: canceledContext(t), want: "context:"},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			err := readyContext(tests[i].ctx, "unit")
			if err == nil || !strings.Contains(err.Error(), tests[i].want) {
				t.Fatalf("readyContext(%s) = %v, want %q", tests[i].name, err, tests[i].want)
			}
		})
	}
	if sleepContext(canceledContext(t), time.Second) {
		t.Fatal("sleepContext(canceled) = true, want false")
	}
}

func TestShimCommandUsesSeparatorAndDetachedCancel(t *testing.T) {
	sup := &Supervisor{
		cfg: config.Config{EventLog: config.EventLogConfig{
			Dir:            "/state",
			SigningKeyPath: "/key",
		}},
		shimCommand: []string{"/proc/self/exe", "-test.run=TestHelperProcessShim", "--"},
	}
	cmd := sup.shimCmd(testContext(t), testJob("daemon", []string{"/bin/echo", "ok"}))
	if cmd.Cancel != nil {
		t.Fatalf("shimCmd Cancel = %p, want nil", cmd.Cancel)
	}
	sep := lastArgIndex(cmd.Args, "--")
	if sep < 0 || sep+1 >= len(cmd.Args) || cmd.Args[sep+1] != "/bin/echo" {
		t.Fatalf("shim args = %q, want separator before job command", cmd.Args)
	}
}

func TestSupervisorLockHeldReportsErrorsAndHeldState(t *testing.T) {
	sup := &Supervisor{cfg: config.Config{EventLog: config.EventLogConfig{Dir: t.TempDir()}}}
	lock, err := holdLock(sup.lockPath("held"))
	if err != nil {
		t.Fatalf("holdLock(held) = %v, want nil", err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	held, err := lockHeld(sup.lockPath("held"))
	if err != nil || !held {
		t.Fatal("lockHeld(held) = false, want true")
	}
	filePath := filepath.Join(t.TempDir(), "state-file")
	if err = os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(state-file) = %v, want nil", err)
	}
	sup.cfg.EventLog.Dir = t.TempDir()
	held, err = lockHeld(sup.lockPath("free"))
	if err != nil || held {
		t.Fatal("lockHeld(free) = true, want false")
	}
	sup.cfg.EventLog.Dir = filePath
	if _, err = lockHeld(sup.lockPath("bad")); err == nil || !strings.Contains(err.Error(), "mkdir lock dir") {
		t.Fatalf("lockHeld(error path) = %v, want mkdir lock dir", err)
	}
}

func TestWaitStoppedBoundaries(t *testing.T) {
	sup := &Supervisor{cfg: config.Config{EventLog: config.EventLogConfig{Dir: t.TempDir()}}}
	job := testJob("free", []string{"/bin/true"})
	job.Stop.GraceSeconds = -1
	if !sup.waitStopped(testContext(t), job) {
		t.Fatal("waitStopped(free negative grace) = false, want true")
	}
	lock, err := holdLock(sup.lockPath("held"))
	if err != nil {
		t.Fatalf("holdLock(held) = %v, want nil", err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	job.Name = "held"
	job.Stop.GraceSeconds = 0
	if sup.waitStopped(testContext(t), job) {
		t.Fatal("waitStopped(held grace zero) = true, want false")
	}
	if sup.waitStopped(canceledContext(t), job) {
		t.Fatal("waitStopped(canceled) = true, want false")
	}
}

func TestWaitStoppedZeroGraceReturnsWithoutSleep(t *testing.T) {
	sup := &Supervisor{cfg: config.Config{EventLog: config.EventLogConfig{Dir: t.TempDir()}}}
	job := testJob("held", []string{"/bin/true"})
	lock, err := holdLock(sup.lockPath(job.Name))
	if err != nil {
		t.Fatalf("holdLock(held) = %v, want nil", err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	job.Stop.GraceSeconds = 0
	started := time.Now()
	if sup.waitStopped(testContext(t), job) {
		t.Fatal("waitStopped(held zero grace) = true, want false")
	}
	if elapsed := time.Since(started); elapsed >= 8*time.Millisecond {
		t.Fatalf("waitStopped zero grace elapsed = %s, want no sleep", elapsed)
	}
}

func TestRunShimReturnCodes(t *testing.T) {
	if code := RunShim([]string{"-bad"}); code != 2 {
		t.Fatalf("RunShim(parse error) = %d, want 2", code)
	}
	cfg := shimArgs(t, shimConfig{
		EventLogDir: t.TempDir(),
		SigningKey:  filepath.Join(t.TempDir(), "missing-key"),
		Name:        "daemon",
		LockPath:    filepath.Join(t.TempDir(), "daemon.lock"),
		RecordPath:  filepath.Join(t.TempDir(), "daemon.json"),
		LogPath:     filepath.Join(t.TempDir(), "daemon.log"),
		Command:     []string{"/bin/true"},
		Sandbox:     config.SandboxConfig{Mode: "off", Reason: "unit test"},
	})
	if code := RunShim(cfg); code != 1 {
		t.Fatalf("RunShim(run error) = %d, want 1", code)
	}
}

func TestParseShimArgsStripsSentinelsAndRejectsFields(t *testing.T) {
	base := shimConfig{
		EventLogDir: "/state",
		SigningKey:  "/key",
		Name:        "daemon",
		LockPath:    "/lock",
		RecordPath:  "/record",
		LogPath:     "/log",
		Command:     []string{"/bin/true"},
		Sandbox:     config.SandboxConfig{Mode: "off", Reason: "unit test"},
	}
	checkParseShimArgsCommand(t, base)
	checkParseShimArgsRejectsMissingFields(t, base)
}

func checkParseShimArgsCommand(t *testing.T, base shimConfig) {
	t.Helper()
	cfg, err := parseShimArgs(shimArgs(t, base))
	if err != nil || len(cfg.Command) != 1 || cfg.Command[0] != "/bin/true" {
		t.Fatalf("parseShimArgs(valid) = %+v, %v, want stripped command", cfg, err)
	}
	cfg, err = parseShimArgs(shimArgs(t, withShimCommand(base, []string{"--", "/bin/true"})))
	if err != nil || len(cfg.Command) != 1 || cfg.Command[0] != "/bin/true" {
		t.Fatalf("parseShimArgs(double separator) = %+v, %v, want stripped command", cfg, err)
	}
	if _, err = parseShimArgs([]string{"-bad"}); err == nil || !strings.Contains(err.Error(), "parse") {
		t.Fatalf("parseShimArgs(bad flag) = %v, want parse error", err)
	}
}

func checkParseShimArgsRejectsMissingFields(t *testing.T, base shimConfig) {
	t.Helper()
	tests := []struct {
		name string
		cfg  shimConfig
		want string
	}{
		{name: "event log", cfg: withShimEventLog(base, ""), want: "event log, signing key and name"},
		{name: "signing key", cfg: withShimSigningKey(base, ""), want: "event log, signing key and name"},
		{name: "record", cfg: withShimRecord(base, ""), want: "lock, record and log paths"},
		{name: "log", cfg: withShimLog(base, ""), want: "lock, record and log paths"},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			_, err := parseShimArgs(shimArgs(t, tests[i].cfg))
			if err == nil || !strings.Contains(err.Error(), tests[i].want) {
				t.Fatalf("parseShimArgs(%s) = %v, want %q", tests[i].name, err, tests[i].want)
			}
		})
	}
}

func TestRunShimRejectsLockLogStartAndRecordErrors(t *testing.T) {
	cfg := validShimConfig(t, "daemon")
	fileParent := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(fileParent, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(state-file) = %v, want nil", err)
	}
	cases := []struct {
		name string
		cfg  shimConfig
		want string
	}{
		{name: "lock", cfg: withShimLock(cfg, filepath.Join(fileParent, "daemon.lock")), want: "mkdir lock dir"},
		{name: "log", cfg: withShimLog(cfg, filepath.Join(fileParent, "daemon.log")), want: "mkdir log dir"},
		{name: "start", cfg: withShimCommand(cfg, []string{filepath.Join(t.TempDir(), "missing-command")}), want: "start"},
		{name: "record", cfg: withShimRecord(cfg, filepath.Join(fileParent, "daemon.json")), want: "mkdir state dir"},
	}
	for i := 0; i < len(cases); i++ {
		t.Run(cases[i].name, func(t *testing.T) {
			err := errorWithoutPanic(t, "runShim", func() error { return runShim(cases[i].cfg) })
			if err == nil || !strings.Contains(err.Error(), cases[i].want) {
				t.Fatalf("runShim(%s) = %v, want %q", cases[i].name, err, cases[i].want)
			}
			if cases[i].name != "lock" {
				assertLockReleased(t, cases[i].cfg.LockPath)
			}
		})
	}
}

func TestShimEventAppendAndCleanupErrors(t *testing.T) {
	cfg := validShimConfig(t, "daemon")
	writer := appendFailingWriter(t)
	if err := appendStarted(writer, cfg, 1234, time.Now().UTC().Format(time.RFC3339Nano)); err == nil {
		t.Fatal("appendStarted(failing writer) = nil, want append error")
	}
	cmd := exec.CommandContext(testContext(t), "true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(true) = %v, want nil", err)
	}
	err := waitAndRecordExit(testContext(t), writer, cfg, cmd)
	if err == nil {
		t.Fatal("waitAndRecordExit(failing writer) = nil, want append error")
	}
	preclosed := preclosedFile(t)
	stderr := captureStderr(t, func() { closeLockForShim(&heldLock{file: preclosed}) })
	if !strings.Contains(stderr, "close job lock") {
		t.Fatalf("closeLockForShim stderr = %q, want close job lock", stderr)
	}
	stderr = captureStderr(t, func() { closeLogForShim(preclosedFile(t)) })
	if !strings.Contains(stderr, "close job log") {
		t.Fatalf("closeLogForShim stderr = %q, want close job log", stderr)
	}
}

func TestShimErrorHelpers(t *testing.T) {
	if got := exitCode(nil); got != 0 {
		t.Fatalf("exitCode(nil) = %d, want 0", got)
	}
	if got := exitCode(errors.New("plain")); got != -1 {
		t.Fatalf("exitCode(plain) = %d, want -1", got)
	}
	path := filepath.Join(t.TempDir(), "append.log")
	checkOpenAppendLogWrites(t, path)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) = %v, want nil", path, err)
	}
	if string(body) != "one\ntwo\n" {
		t.Fatalf("append log = %q, want both writes", body)
	}
	if _, err = openAppendLog(t.TempDir()); err == nil || !strings.Contains(err.Error(), "open log") {
		t.Fatalf("openAppendLog(directory) = %v, want open error", err)
	}
}

func checkOpenAppendLogWrites(t *testing.T, path string) {
	t.Helper()
	first, err := openAppendLog(path)
	if err != nil {
		t.Fatalf("openAppendLog(first) = %v, want nil", err)
	}
	if _, err = first.WriteString("one\n"); err != nil {
		t.Fatalf("WriteString(first) = %v, want nil", err)
	}
	if err = first.Close(); err != nil {
		t.Fatalf("Close(first) = %v, want nil", err)
	}
	second, err := openAppendLog(path)
	if err != nil {
		t.Fatalf("openAppendLog(second) = %v, want nil", err)
	}
	if _, err = second.WriteString("two\n"); err != nil {
		t.Fatalf("WriteString(second) = %v, want nil", err)
	}
	if err = second.Close(); err != nil {
		t.Fatalf("Close(second) = %v, want nil", err)
	}
}

func TestStateHelpersRejectBadInputs(t *testing.T) {
	sup := &Supervisor{cfg: config.Config{EventLog: config.EventLogConfig{Dir: filepath.Join(t.TempDir(), "state")}}}
	if err := os.WriteFile(sup.cfg.EventLog.Dir, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(state) = %v, want nil", err)
	}
	if _, err := sup.replayState(testContext(t)); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("replayState(file dir) = %v, want stat error", err)
	}
	st, err := JobReducer(eventlog.State{}, eventlog.Record{
		Type:    "job.adopted",
		TaskID:  "daemon",
		Payload: []byte(`{"state":"running","pid":12,"shim_pid":11,"since":"now"}`),
	})
	if err != nil || st.Jobs["daemon"].LastEvent != "job.adopted" {
		t.Fatalf("JobReducer(adopted) = %+v, %v, want adopted event", st, err)
	}
	badPayload := eventlog.Record{Type: "job.started", TaskID: "daemon", Payload: []byte(`{`)}
	if _, err = JobReducer(eventlog.State{}, badPayload); err == nil {
		t.Fatal("JobReducer(bad started payload) = nil, want error")
	}
	badPayload.Type = "job.exited"
	if _, err = JobReducer(eventlog.State{}, badPayload); err == nil {
		t.Fatal("JobReducer(bad exited payload) = nil, want error")
	}
}

func TestReadRecordAndAppendErrors(t *testing.T) {
	if _, ok := readJobRecord(filepath.Join(t.TempDir(), "missing.json")); ok {
		t.Fatal("readJobRecord(missing) ok = true, want false")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{`), 0o600); err != nil {
		t.Fatalf("WriteFile(bad record) = %v, want nil", err)
	}
	if _, ok := readJobRecord(bad); ok {
		t.Fatal("readJobRecord(invalid) ok = true, want false")
	}
	sup := &Supervisor{cfg: config.Config{EventLog: config.EventLogConfig{Dir: t.TempDir()}}}
	err := sup.appendJobEvent(testContext(t), "job.bad", "daemon", map[string]any{"bad": make(chan int)})
	if err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("appendJobEvent(unmarshalable) = %v, want marshal error", err)
	}
	err = sup.appendJobEvent(testContext(t), "job.bad", "daemon", map[string]any{"state": "dead"})
	if err == nil || !strings.Contains(err.Error(), "signer is required") {
		t.Fatalf("appendJobEvent(no signer) = %v, want signer error", err)
	}
}

func TestRestartAndShouldRestartBoundaries(t *testing.T) {
	job := testJob("daemon", []string{"/bin/true"})
	job.Restart = config.JobRestartConfig{Policy: "always", MaxRestarts: 2, BackoffSeconds: 1}
	sup := &Supervisor{cfg: config.Config{EventLog: config.EventLogConfig{Dir: t.TempDir()}}}
	err := sup.restartJob(canceledContext(t), job)
	if err == nil || !strings.Contains(err.Error(), "signer is required") {
		t.Fatalf("restartJob(canceled) = %v, want append signer error", err)
	}
	rt := testRuntime(t)
	t.Setenv("EPITROP_TEST_JOB", "sleep")
	sup = testSupervisorAt(t, rt, withRestartCommand(job, []string{os.Args[0], "-test.run=TestHelperProcessJob"}))
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	err = sup.restartJob(ctx, sup.cfg.Jobs[0])
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restartJob(backoff canceled) = %v, want deadline before restart", err)
	}
	zero := 0
	tests := []struct {
		name string
		job  config.JobConfig
		st   eventlog.Job
		want bool
	}{
		{name: "max", job: job, st: eventlog.Job{Restarts: 2}, want: false},
		{name: "always dead", job: job, st: eventlog.Job{State: string(StateDead)}, want: true},
		{name: "always stopped", job: job, st: eventlog.Job{State: string(StateStopped)}, want: false},
		{name: "never", job: withRestartPolicy(job, "never"), st: eventlog.Job{}, want: false},
		{name: "failure nil code", job: withRestartPolicy(job, "on-failure"), st: eventlog.Job{}, want: true},
		{name: "failure zero", job: withRestartPolicy(job, "on-failure"), st: eventlog.Job{LastExitCode: &zero}, want: false},
		{name: "failure lost", job: withRestartPolicy(job, "on-failure"), st: eventlog.Job{LastExitCode: &zero, LastReason: "lost"}, want: true},
	}
	fixedNow := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			if got := shouldRestart(tests[i].job, tests[i].st, fixedNow); got != tests[i].want {
				t.Fatalf("shouldRestart(%s) = %v, want %v", tests[i].name, got, tests[i].want)
			}
		})
	}
}

func TestShouldRestartCountsOnlyRestartsInsideTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	baseJob := testJob("daemon", []string{"/bin/true"})
	baseJob.Restart = config.JobRestartConfig{Policy: "always"}

	withWindowAndCap := func(window, cap int) config.JobConfig {
		j := baseJob
		j.Restart.WindowSeconds = window
		j.Restart.MaxRestarts = cap
		return j
	}

	twoHoursAgo := now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	tenMinAgo := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	exact3600Ago := now.Add(-3600 * time.Second).UTC().Format(time.RFC3339)
	sec3599Ago := now.Add(-3599 * time.Second).UTC().Format(time.RFC3339)

	tests := []struct {
		name string
		job  config.JobConfig
		st   eventlog.Job
		want bool
	}{
		{name: "positive old restarts", job: withWindowAndCap(3600, 2), st: eventlog.Job{RestartTimes: []string{twoHoursAgo, twoHoursAgo}}, want: true},
		{name: "negative recent restarts", job: withWindowAndCap(3600, 2), st: eventlog.Job{RestartTimes: []string{tenMinAgo, tenMinAgo}}, want: false},
		{name: "boundary exact window allowed", job: withWindowAndCap(3600, 1), st: eventlog.Job{RestartTimes: []string{exact3600Ago}}, want: true},
		{name: "boundary inside window refused", job: withWindowAndCap(3600, 1), st: eventlog.Job{RestartTimes: []string{sec3599Ago}}, want: false},
		{name: "window zero lifetime cap kept", job: withWindowAndCap(0, 2), st: eventlog.Job{Restarts: 2, RestartTimes: []string{twoHoursAgo, twoHoursAgo}}, want: false},
		{name: "unparseable time counts towards window cap", job: withWindowAndCap(3600, 1), st: eventlog.Job{RestartTimes: []string{"unparseable-time"}}, want: false},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			if got := shouldRestart(tests[i].job, tests[i].st, now); got != tests[i].want {
				t.Fatalf("shouldRestart(%s) = %v, want %v", tests[i].name, got, tests[i].want)
			}
		})
	}
}

func TestReduceJobRestartedKeepsTheNewestTimes(t *testing.T) {
	st := eventlog.State{Jobs: map[string]eventlog.Job{}}
	baseTime := time.Date(2026, 10, 18, 10, 0, 0, 0, time.UTC)
	var ninthTime string
	var earlySlice []string
	var err error

	for i := 1; i <= 40; i++ {
		recTime := baseTime.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
		if i == 9 {
			ninthTime = recTime
		}
		rec := eventlog.Record{Seq: uint64(i), Type: "job.restarted", TaskID: "daemon", Time: recTime}
		st, err = JobReducer(st, rec)
		if err != nil {
			t.Fatalf("JobReducer(seq=%d) = %v, want nil", i, err)
		}
		if i == 20 {
			earlySlice = st.Jobs["daemon"].RestartTimes
		}
	}

	job := st.Jobs["daemon"]
	if job.Restarts != 40 || len(job.RestartTimes) != 32 {
		t.Fatalf("job.Restarts = %d, len(RestartTimes) = %d, want 40 and 32", job.Restarts, len(job.RestartTimes))
	}
	if job.RestartTimes[0] != ninthTime {
		t.Fatalf("job.RestartTimes[0] = %q, want %q", job.RestartTimes[0], ninthTime)
	}
	if len(earlySlice) != 20 || earlySlice[19] != baseTime.Add(20*time.Minute).UTC().Format(time.RFC3339) {
		t.Fatalf("earlySlice modified: len = %d, want 20", len(earlySlice))
	}
	assertRestartTimesBranch(t, eventlog.Job{Name: "daemon", RestartTimes: earlySlice})
}

// assertRestartTimesBranch reduces two records from one earlier State; the
// second must not overwrite the time the first one appended.
func assertRestartTimesBranch(t *testing.T, base eventlog.Job) {
	t.Helper()
	at := func(seq uint64, ts string) eventlog.State {
		st := eventlog.State{Jobs: map[string]eventlog.Job{"daemon": base}}
		next, err := JobReducer(st, eventlog.Record{Seq: seq, Type: "job.restarted", TaskID: "daemon", Time: ts})
		if err != nil {
			t.Fatalf("JobReducer(seq=%d) = %v, want nil", seq, err)
		}
		return next
	}
	first := at(41, "2026-10-18T12:00:00Z")
	at(42, "2026-10-18T13:00:00Z")
	times := first.Jobs["daemon"].RestartTimes
	if got := times[len(times)-1]; got != "2026-10-18T12:00:00Z" {
		t.Fatalf("first branch newest time = %q after a second branch, want 2026-10-18T12:00:00Z", got)
	}
}

func TestAdoptIfFreshStartOnlyAdoptsStartedEvent(t *testing.T) {
	rt := testRuntime(t)
	job := testJob("daemon", longJobCommand(t))
	sup := testSupervisorAt(t, rt, job)
	status := JobStatus{Name: "daemon", State: StateRunning, PID: 123, ShimPID: 122, Since: "now"}
	st := eventlog.Job{LastEvent: "job.adopted"}
	if err := sup.adoptIfFreshStart(testContext(t), job, st, status); err != nil {
		t.Fatalf("adoptIfFreshStart(adopted) = %v, want nil", err)
	}
	if adopted := countEvents(t, rt.EventLogDir, "job.adopted"); adopted != 0 {
		t.Fatalf("job.adopted events = %d, want none for non-started event", adopted)
	}
}

func TestReplayStateReturnsContextWhenTransientRetryCanceled(t *testing.T) {
	rt := testRuntime(t)
	sup := testSupervisorAt(t, rt, testJob("daemon", []string{"/bin/true"}))
	if err := sup.appendJobEvent(testContext(t), "job.stopped", "daemon", map[string]any{"state": string(StateStopped)}); err != nil {
		t.Fatalf("appendJobEvent(seed) = %v, want nil", err)
	}
	if err := os.Remove(filepath.Join(rt.EventLogDir, "events", "HEAD.json")); err != nil {
		t.Fatalf("Remove(HEAD.json) = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err := sup.replayState(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("replayState(transient canceled) = %v, want deadline", err)
	}
}

func TestSupervisorConstructorAndReadyErrors(t *testing.T) {
	cfg := config.Default()
	if _, err := New(cfg, Options{}); err == nil || !strings.Contains(err.Error(), "signing_key_path") {
		t.Fatalf("New(no signing key) = %v, want config error", err)
	}
	cfg.EventLog.SigningKeyPath = filepath.Join(t.TempDir(), "missing.hex")
	if _, err := New(cfg, Options{}); err == nil {
		t.Fatal("New(missing key) = nil, want signer error")
	}
	rt := testRuntime(t)
	cfg = config.Default()
	cfg.EventLog.Dir = rt.EventLogDir
	cfg.EventLog.SigningKeyPath = rt.KeyPath
	cfg.EventLog.PublicKey = rt.PublicKey
	sup, err := New(cfg, Options{})
	if err != nil {
		t.Fatalf("New(defaults) = %v, want nil", err)
	}
	if sup.clk == nil || sup.pollInterval != defaultPollInterval || len(sup.shimCommand) == 0 {
		t.Fatalf("New(defaults) = clk %v poll %s shim %q, want defaults", sup.clk, sup.pollInterval, sup.shimCommand)
	}
	assertReadyError(t, "Start", func(ctx context.Context) error { return sup.Start(ctx, "daemon") })
	assertReadyError(t, "Status", func(ctx context.Context) error {
		_, err := sup.Status(ctx, "daemon")
		return err
	})
	assertReadyError(t, "Stop", func(ctx context.Context) error { return sup.Stop(ctx, "daemon") })
	assertReadyError(t, "Supervise", sup.Supervise)
	assertReadyError(t, "Reconcile", sup.Reconcile)
}

func TestSuperviseReadyAndDeadlineErrors(t *testing.T) {
	sup := testSupervisor(t)
	if err := sup.Supervise(context.Background()); err == nil || !strings.Contains(err.Error(), "supervise requires context deadline") {
		t.Fatalf("Supervise(no deadline) = %v, want supervise-specific deadline error", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	// The deadline can land in the poll sleep (ctx.Err()) or in the next Reconcile, which
	// wraps it; both are the same outcome, so the contract is errors.Is.
	if err := sup.Supervise(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Supervise(deadline) = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

func TestSupervisePropagatesReconcileStartError(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", longJobCommand(t)))
	sup.shimCommand = []string{filepath.Join(t.TempDir(), "missing-shim")}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sup.Supervise(ctx)
	if err == nil || !strings.Contains(err.Error(), "start shim") {
		t.Fatalf("Supervise(bad shim) = %v, want reconcile start-shim error", err)
	}
}

func TestSuperviseOpenEventLogErrorWrapped(t *testing.T) {
	sup := testSupervisor(t)
	sup.cfg.EventLog.Dir = ""
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sup.Supervise(ctx)
	if err == nil {
		t.Fatal("Supervise() with empty dir = nil, want error")
	}
	wantPrefix := fmt.Sprintf("supervisor: open event log %s:", sup.cfg.EventLog.Dir)
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("Supervise() error = %q, want prefix %q", err.Error(), wantPrefix)
	}
}

func TestSuperviseRepairTailErrorWrapped(t *testing.T) {
	sup := testSupervisor(t)
	if err := sup.appendJobEvent(testContext(t), "job.stopped", "probe", map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("append seed = %v, want nil", err)
	}
	corruptEventLog(t, sup.cfg.EventLog.Dir)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sup.Supervise(ctx)
	if err == nil {
		t.Fatal("Supervise() with corrupt log = nil, want error")
	}
	wantPrefix := fmt.Sprintf("supervisor: repair event log tail %s:", sup.cfg.EventLog.Dir)
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("Supervise() error = %q, want prefix %q", err.Error(), wantPrefix)
	}
}

func TestReconcilePropagatesStartAndReplayErrors(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", longJobCommand(t)))
	sup.shimCommand = []string{filepath.Join(t.TempDir(), "missing-shim")}
	if err := sup.Reconcile(testContext(t)); err == nil || !strings.Contains(err.Error(), "start shim") {
		t.Fatalf("Reconcile(bad shim) = %v, want start-shim error", err)
	}
	corrupt, _, counter := countingShimSupervisor(t, "corrupt")
	if err := corrupt.appendJobEvent(testContext(t), "job.stopped", "corrupt", map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("append seed = %v, want nil", err)
	}
	corruptEventLog(t, corrupt.cfg.EventLog.Dir)
	if err := corrupt.Reconcile(testContext(t)); err == nil {
		t.Fatal("Reconcile(corrupt log) = nil, want replay error")
	}
	time.Sleep(50 * time.Millisecond)
	if c := countProbeStarts(counter); c != 0 {
		t.Fatalf("Reconcile(corrupt log) spawned %d shims, want 0", c)
	}
}

func TestReadyContextRequiresDeadline(t *testing.T) {
	if err := readyContext(context.Background(), "probe"); err == nil || !strings.Contains(err.Error(), "probe requires context deadline") {
		t.Fatalf("readyContext(no deadline) = %v, want deadline error", err)
	}
}

func TestStartRefusesCorruptEventLogWithoutSpawning(t *testing.T) {
	sup, _, counter := countingShimSupervisor(t, "corrupt")
	if err := sup.appendJobEvent(testContext(t), "job.stopped", "corrupt", map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("append seed = %v, want nil", err)
	}
	corruptEventLog(t, sup.cfg.EventLog.Dir)
	if err := sup.Start(testContext(t), "corrupt"); err == nil {
		t.Fatal("Start(corrupt log) = nil, want replay error")
	}
	if c := countProbeStarts(counter); c != 0 {
		t.Fatalf("Start(corrupt log) spawned %d shims, want 0", c)
	}
}

func TestWaitStartedReportsReplayError(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", longJobCommand(t)))
	if err := sup.appendJobEvent(testContext(t), "job.stopped", "probe", map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("append seed = %v, want nil", err)
	}
	corruptEventLog(t, sup.cfg.EventLog.Dir)
	// The wait follows the shim, so the shim must be one that is known to have exited: any
	// fixed number may be a live process on the machine that runs the test.
	err := sup.waitStarted(testContext(t), "probe", eventlog.Job{}, exitedPID(t))
	if err == nil || !strings.Contains(err.Error(), "probe did not start: ") || !strings.Contains(err.Error(), "eventlog: seq 1 file ") {
		t.Fatalf("waitStarted(corrupt log, exited shim) = %v, want did-not-start wrapping the replay error", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitStarted(corrupt log, exited shim) = %v, want it to end with the shim, not at the deadline", err)
	}
}

// exitedPID returns the process id of a child that has exited and been collected.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run(/bin/true) = %v, want nil", err)
	}
	return cmd.Process.Pid
}

func TestStartStatusStopReconcileErrorPaths(t *testing.T) {
	bad := testSupervisor(t, testJob("bad", []string{"/bin/true"}))
	bad.cfg.EventLog.Dir = filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(bad.cfg.EventLog.Dir, []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(state-file) = %v, want nil", err)
	}
	checkBadEventLogDirErrorPaths(t, bad)
	checkMissingShimErrorPaths(t)
}

func checkBadEventLogDirErrorPaths(t *testing.T, bad *Supervisor) {
	t.Helper()
	if err := bad.Start(testContext(t), "bad"); err == nil {
		t.Fatal("Start(lock error) = nil, want error")
	}
	if _, err := bad.Status(testContext(t), "missing"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Status(missing) = %v, want ErrUnknownJob", err)
	}
	if _, err := bad.Status(testContext(t), "bad"); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("Status(replay error) = %v, want stat error", err)
	}
	if err := bad.Reconcile(testContext(t)); err == nil || !strings.Contains(err.Error(), "stat") {
		t.Fatalf("Reconcile(replay error) = %v, want stat error", err)
	}
	if err := bad.Stop(testContext(t), "missing"); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("Stop(missing) = %v, want ErrUnknownJob", err)
	}
}

func checkMissingShimErrorPaths(t *testing.T) {
	t.Helper()
	missingShim := testSupervisor(t, testJob("one", []string{"/bin/true"}), testJob("two", []string{"/bin/true"}))
	missingShim.shimCommand = []string{filepath.Join(t.TempDir(), "missing-shim")}
	if err := missingShim.Start(testContext(t), ""); err == nil || !strings.Contains(err.Error(), "start shim") {
		t.Fatalf("Start(all missing shim) = %v, want start shim error", err)
	}
	if err := missingShim.Stop(testContext(t), ""); err != nil {
		t.Fatalf("Stop(all not running) = %v, want nil", err)
	}
}

func TestStopAllPropagatesStoppedEventError(t *testing.T) {
	rt := testRuntime(t)
	sup := testSupervisorAt(t, rt, testJob("probe", longJobCommand(t)))
	if err := os.WriteFile(filepath.Join(rt.EventLogDir, "events"), []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(events file) = %v, want nil", err)
	}
	if err := sup.Stop(testContext(t), "probe"); err == nil {
		t.Fatal("Stop(named) = nil, want append error precondition")
	}
	if err := sup.Stop(testContext(t), ""); err == nil {
		t.Fatal("Stop(all) = nil, want append error propagated")
	}
}

func TestStopPropagatesSignalTargetAndSignalErrors(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", longJobCommand(t)))
	if err := sup.Start(testContext(t), "probe"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	failNextJobRecordRead(t)
	if err := sup.Stop(testContext(t), "probe"); err == nil || !strings.Contains(err.Error(), "missing or invalid") {
		t.Fatalf("Stop(record read failure) = %v, want signal target error", err)
	}
	rec := withStopSignaller(t, unixEPERMOnAnySignal)
	err := sup.Stop(testContext(t), "probe")
	if err == nil || !strings.Contains(err.Error(), "signal TERM") {
		t.Fatalf("Stop(signal EPERM) = %v, want signal error", err)
	}
	if len(*rec) != 1 {
		t.Fatalf("recorded signals = %+v, want one TERM", *rec)
	}
}

func TestStopPropagatesKillError(t *testing.T) {
	job := testJob("probe", []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.Stop.GraceSeconds = 0
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "ignore-term")
	if err := sup.Start(testContext(t), "probe"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	rec := withStopSignaller(t, unixEPERMAfterTERM)
	err := sup.Stop(testContext(t), "probe")
	if err == nil || !strings.Contains(err.Error(), "SIGKILL pid") {
		t.Fatalf("Stop(kill EPERM) = %v, want kill error", err)
	}
	if len(*rec) != 2 {
		t.Fatalf("recorded signals = %+v, want TERM then SIGKILL", *rec)
	}
}

func TestStopReportsHeldLockAfterDroppedSignals(t *testing.T) {
	job := testJob("probe", longJobCommand(t))
	job.Stop.GraceSeconds = 0
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), "probe"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	rec := withRecordingSignaller(t)
	err := sup.Stop(testContext(t), "probe")
	if err == nil || !strings.Contains(err.Error(), "lock stayed held") {
		t.Fatalf("Stop(dropped signals) = %v, want held-lock error", err)
	}
	if len(*rec) != 2 {
		t.Fatalf("recorded signals = %+v, want TERM and SIGKILL", *rec)
	}
}

func TestStartAllStopAllAndWaitStartedBoundaries(t *testing.T) {
	rt := testRuntime(t)
	sup := testSupervisorAt(t, rt,
		testJob("one", longJobCommand(t)),
		testJob("two", longJobCommand(t)),
	)
	if err := sup.Start(testContext(t), ""); err != nil {
		t.Fatalf("Start(all) = %v, want nil", err)
	}
	for _, name := range []string{"one", "two"} {
		if got := statusOne(t, sup, name).State; got != StateRunning {
			t.Fatalf("Status(%s) = %s, want running", name, got)
		}
	}
	if err := sup.Stop(testContext(t), ""); err != nil {
		t.Fatalf("Stop(all) = %v, want nil", err)
	}
	for _, name := range []string{"one", "two"} {
		if got := statusOne(t, sup, name).State; got != StateStopped {
			t.Fatalf("Status(%s) = %s, want stopped", name, got)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	for ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	if err := sup.waitStarted(ctx, "missing", eventlog.Job{}, 0); err == nil {
		t.Fatal("waitStarted(expired missing) = nil, want error")
	}
}

func TestStartAlreadyRunningDoesNotSpawnSecondShim(t *testing.T) {
	sup, _, counter := countingShimSupervisor(t, "probe")
	if err := sup.Start(testContext(t), "probe"); err != nil {
		t.Fatalf("Start#1 = %v, want nil", err)
	}
	if err := sup.Start(testContext(t), "probe"); err != nil {
		t.Fatalf("Start#2 = %v, want nil", err)
	}
	time.Sleep(50 * time.Millisecond)
	if c := countProbeStarts(counter); c != 1 {
		t.Fatalf("shim spawned %d times for running job, want 1", c)
	}
}

func TestStartReturnsLockErrorBeforeShimStart(t *testing.T) {
	rt := testRuntime(t)
	sup := testSupervisorAt(t, rt, testJob("probe", longJobCommand(t)))
	if err := os.WriteFile(filepath.Join(rt.EventLogDir, "jobs"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(jobs file) = %v, want nil", err)
	}
	err := sup.Start(testContext(t), "probe")
	if err == nil || !strings.Contains(err.Error(), "mkdir lock dir") {
		t.Fatalf("Start(lock dir blocked) = %v, want lock error", err)
	}
}

func TestStartReturnsWaitStartedTimeout(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", []string{"/bin/true"}))
	sup.shimCommand = []string{"/bin/true"}
	err := sup.Start(testContext(t), "probe")
	if err == nil || !strings.Contains(err.Error(), "did not start") {
		t.Fatalf("Start(shim exits immediately) = %v, want did-not-start error", err)
	}
}

func TestStartedByChecksShimPIDAndStartTime(t *testing.T) {
	started := eventlog.Job{State: string(StateRunning), PID: 20, ShimPID: 19, Since: "t2", LastEvent: "job.started"}
	exited := started
	exited.State, exited.LastEvent = "exited", "job.exited"
	cases := []struct {
		name   string
		job    eventlog.Job
		before eventlog.Job
		shim   int
		want   bool
	}{
		{name: "new shim started", job: started, before: eventlog.Job{ShimPID: 7, Since: "t1"}, shim: 19, want: true},
		{name: "new shim started then exited", job: exited, before: eventlog.Job{ShimPID: 7, Since: "t1"}, shim: 19, want: true},
		{name: "first start ever", job: started, before: eventlog.Job{}, shim: 19, want: true},
		{name: "other shim's start", job: started, before: eventlog.Job{}, shim: 18, want: false},
		{name: "reused pid, old record", job: started, before: started, shim: 19, want: false},
		{name: "reused pid, new start", job: started, before: eventlog.Job{ShimPID: 19, Since: "t1"}, shim: 19, want: true},
		{name: "no start time", job: eventlog.Job{ShimPID: 19}, before: eventlog.Job{}, shim: 19, want: false},
		{name: "record without start time", job: eventlog.Job{ShimPID: 19}, before: eventlog.Job{ShimPID: 7, Since: "t1"}, shim: 19, want: false},
	}
	for i := 0; i < len(cases); i++ {
		if got := startedBy(cases[i].job, cases[i].before, cases[i].shim); got != cases[i].want {
			t.Errorf("startedBy(%s) = %v, want %v", cases[i].name, got, cases[i].want)
		}
	}
}

func TestRunShimReturnsAfterStartedAppendError(t *testing.T) {
	t.Setenv("EPITROP_TEST_JOB", "sleep")
	cfg := validShimConfig(t, "probe")
	cfg.Command = []string{os.Args[0], "-test.run=TestHelperProcessJob"}
	if err := os.WriteFile(filepath.Join(cfg.EventLogDir, "events"), []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(events file) = %v, want nil", err)
	}
	done := make(chan error, 1)
	go func() { done <- runShim(cfg) }()
	t.Cleanup(func() { cleanupRecordProcessGroup(t, cfg.RecordPath) })
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("runShim(started append error) = nil, want append error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runShim kept waiting after started append failure, want immediate return")
	}
}

func TestStartAndStopOperationalErrors(t *testing.T) {
	sup := testSupervisor(t, testJob("daemon", []string{"/bin/true"}))
	sup.shimCommand = []string{filepath.Join(t.TempDir(), "missing-shim")}
	if err := sup.Start(testContext(t), "daemon"); err == nil || !strings.Contains(err.Error(), "start shim") {
		t.Fatalf("Start(missing shim) = %v, want start shim error", err)
	}
	stopped := testSupervisor(t, testJob("stopped", []string{"/bin/true"}))
	if err := stopped.Stop(testContext(t), "stopped"); err != nil {
		t.Fatalf("Stop(not running) = %v, want nil", err)
	}
	if got := statusOne(t, stopped, "stopped").State; got != StateStopped {
		t.Fatalf("Status(stopped) = %s, want stopped", got)
	}
	if _, err := stopped.signalTarget(testJob("missing-record", []string{"/bin/true"})); err == nil {
		t.Fatal("signalTarget(missing record) = nil, want error")
	}
}

// testWaitBound ends a wait in a run that fails; it is no claim about speed. It is twice what
// the code allows one start, 15 s for the record and 10 s for the scope, so a host that is
// slow but inside the code's own bounds does not fail a test.
const (
	testWaitBound    = 2 * (maxStartPolls*startPollInterval + scopeConfirmPolls*scopeConfirmInterval)
	testPollInterval = 10 * time.Millisecond
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testWaitBound)
	t.Cleanup(cancel)
	return ctx
}

// waitUntil polls cond until it holds, for at most testWaitBound, and reports whether it held.
func waitUntil(cond func() bool) bool {
	for i := 0; i < int(testWaitBound/testPollInterval); i++ {
		if cond() {
			return true
		}
		time.Sleep(testPollInterval)
	}
	return cond()
}

var (
	testScratchOnce sync.Once
	testScratchDir  string
)

// testScratch is one directory per test process for the paths that helpers hand out without a
// *testing.T: the default log of a test job, and the lock and record of a signal target. A
// fixed path under the system's temp directory is shared by every test run on the machine,
// so two runs at once probe the same lock file. TestMain removes the directory.
func testScratch() string {
	testScratchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "epitrop-supervisor-scratch-")
		if err != nil {
			panic(fmt.Sprintf("supervisor tests: scratch directory: %v", err))
		}
		testScratchDir = dir
	})
	return testScratchDir
}

// removeTestScratch removes the scratch directory if this process made one.
func removeTestScratch() {
	if testScratchDir == "" {
		return
	}
	if err := os.RemoveAll(testScratchDir); err != nil {
		fmt.Fprintf(os.Stderr, "supervisor tests: remove %s: %v\n", testScratchDir, err)
	}
}

// untilStopped is the script of a test job that runs until its test stops it: body, a pause,
// and again. It still ends by itself after about ten minutes, the default timeout of go test.
// A test binary that is killed runs no cleanup, and a job whose shim a test killed has nobody
// left to end it; a loop without an end then stays behind for good.
func untilStopped(body string, pause time.Duration) string {
	return boundedLoop(body, pause, int(10*time.Minute/pause))
}

// boundedLoop is a shell loop that runs body and a pause for a number of rounds.
func boundedLoop(body string, pause time.Duration, rounds int) string {
	seconds := strconv.FormatFloat(pause.Seconds(), 'f', -1, 64)
	return fmt.Sprintf(`i=0; while [ "$i" -lt %d ]; do %s sleep %s; i=$((i+1)); done`, rounds, body, seconds)
}

func canceledContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	cancel()
	return ctx
}

func testSupervisor(t *testing.T, jobs ...config.JobConfig) *Supervisor {
	t.Helper()
	return testSupervisorAt(t, testRuntime(t), jobs...)
}

func testSupervisorAt(t *testing.T, rt testRuntimeConfig, jobs ...config.JobConfig) *Supervisor {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("supervisor tests require Linux flock, process groups and parent-death signals")
	}
	cfg := config.Default()
	cfg.EventLog.Dir = rt.EventLogDir
	cfg.EventLog.SigningKeyPath = rt.KeyPath
	cfg.EventLog.PublicKey = rt.PublicKey
	cfg.Jobs = jobs
	sup, err := New(cfg, Options{
		Clock:        clock.NewFake(),
		ShimCommand:  []string{os.Args[0], "-test.run=TestHelperProcessShim", "--"},
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New() = %v, want nil", err)
	}
	t.Setenv("EPITROP_TEST_SHIM", "1")
	t.Cleanup(func() {
		// The test's own context has ended by now.
		ctx, cancel := context.WithTimeout(context.Background(), testWaitBound)
		defer cancel()
		if err := sup.Stop(ctx, ""); err != nil {
			t.Logf("cleanup Stop(all) = %v", err)
		}
		endLeftoverSandboxedJobs(ctx, t, sup)
		waitForShimWaiters(t, sup, 0)
	})
	return sup
}

// waitForShimWaiters waits until the supervisor has collected its shims down to want. A shim
// that has recorded its start still appends job.exited, with a sync to disk, before it
// exits, so the wait is as long as that append may take, not a guess at a fast disk.
func waitForShimWaiters(t *testing.T, sup *Supervisor, want int32) {
	t.Helper()
	if !waitUntil(func() bool { return sup.shimWaiters.Load() == want }) {
		t.Fatalf("shim waiters = %d after %s, want %d: a shim was not collected", sup.shimWaiters.Load(), testWaitBound, want)
	}
}

// endLeftoverSandboxedJobs kills a sandboxed job that is still alive when its test ends: one
// whose shim the test killed on purpose, or one that Stop could not reach. The jobs come from
// the test's own event log.
func endLeftoverSandboxedJobs(ctx context.Context, t *testing.T, sup *Supervisor) {
	t.Helper()
	state, err := sup.replayState(ctx)
	if err != nil {
		// A test that broke its own event log has no record to go by.
		return
	}
	jobs := sup.cfg.Jobs
	for i := 0; i < len(jobs); i++ {
		pid := state.Jobs[jobs[i].Name].PID
		if !leftoverJob(pid) {
			continue
		}
		// The job may end between the check and the signal; that is no failure of the test.
		if err := killStartedProcessGroup(pid); err != nil {
			t.Logf("cleanup kill leftover job %s (group %d) = %v", jobs[i].Name, pid, err)
		}
	}
}

// leftoverJob reports whether pid is a sandboxed job that may be killed with its group: a
// process that leads a group other than the test's own and sits in a job scope, a cgroup
// only a shim asks for. A process id alone proves nothing: the job may have ended and its
// id may belong to something else by now.
func leftoverJob(pid int) bool {
	if pid <= 1 || pid == os.Getpid() {
		return false
	}
	pgid, err := unix.Getpgid(pid)
	if err != nil || pgid != pid || pgid == unix.Getpgrp() {
		return false
	}
	cgroup, err := processCgroup(pid)
	if err != nil {
		return false
	}
	unit := path.Base(strings.TrimSuffix(cgroup, " (deleted)"))
	return strings.HasPrefix(unit, scopeNamePrefix) && strings.HasSuffix(unit, ".scope")
}

func countingShimSupervisor(t *testing.T, name string) (*Supervisor, testRuntimeConfig, string) {
	t.Helper()
	rt := testRuntime(t)
	sup := testSupervisorAt(t, rt, testJob(name, longJobCommand(t)))
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv("EPITROP_PROBE_COUNT", counter)
	sup.shimCommand = []string{
		"/bin/sh", "-c",
		`echo x >> "$EPITROP_PROBE_COUNT"; exec "$@"`,
		"sh", os.Args[0], "-test.run=TestHelperProcessShim", "--",
	}
	return sup, rt, counter
}

func countProbeStarts(path string) int {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(body), "x")
}

func corruptEventLog(t *testing.T, dir string) {
	t.Helper()
	events := filepath.Join(dir, "events")
	changed := 0
	err := filepath.WalkDir(events, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || filepath.Base(path) == "HEAD.json" {
			return walkErr
		}
		changed++
		return os.WriteFile(path, []byte("garbage"), 0o600)
	})
	if err != nil {
		t.Fatalf("WalkDir(%s) = %v, want nil", events, err)
	}
	if changed == 0 {
		t.Fatalf("corruptEventLog(%s) changed 0 records, want at least one", events)
	}
}

type testRuntimeConfig struct {
	EventLogDir string
	KeyPath     string
	PublicKey   string
}

func testRuntime(t *testing.T) testRuntimeConfig {
	t.Helper()
	root := os.Getenv("GOCACHE")
	if root == "" {
		root = t.TempDir()
	}
	base, err := os.MkdirTemp(root, "epitrop-supervisor-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp(/var/tmp) = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Fatalf("RemoveAll(%s) = %v, want nil", base, err)
		}
	})
	keyPath := filepath.Join(base, "seed.hex")
	seed := bytes.Repeat([]byte{3}, 32)
	if err = os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatalf("WriteFile(key) = %v, want nil", err)
	}
	cfg := config.Default()
	cfg.EventLog.Dir = filepath.Join(base, "state")
	cfg.EventLog.SigningKeyPath = keyPath
	cfg.EventLog.PublicKey = ""
	if err = os.MkdirAll(filepath.Join(cfg.EventLog.Dir, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll(state/.git) = %v, want nil", err)
	}
	sup, err := New(cfg, Options{Clock: clock.NewFake(), ShimCommand: []string{os.Args[0], "-test.run=TestHelperProcessShim", "--"}})
	if err != nil {
		t.Fatalf("New(runtime) = %v, want nil", err)
	}
	return testRuntimeConfig{EventLogDir: cfg.EventLog.Dir, KeyPath: keyPath, PublicKey: sup.PublicKey()}
}

func testJob(name string, command []string) config.JobConfig {
	return config.JobConfig{
		Name:     name,
		Command:  command,
		LogPath:  filepath.Join(testScratch(), name+".log"),
		Schedule: "always",
		Restart:  config.JobRestartConfig{Policy: "never"},
		Stop:     config.JobStopConfig{Signal: "TERM", GraceSeconds: 1},
		Sandbox:  config.SandboxConfig{Mode: "off", Reason: "supervisor unit test"},
	}
}

func boundedJob(name string, restarts int, backoff int, grace int) config.JobConfig {
	job := testJob(name, []string{"/bin/true"})
	job.Restart.MaxRestarts = restarts
	job.Restart.BackoffSeconds = backoff
	job.Stop.GraceSeconds = grace
	return job
}

func withRestartWindow(job config.JobConfig, window int) config.JobConfig {
	job.Restart.WindowSeconds = window
	return job
}

func manyArgs(count int) []string {
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, "arg"+strconv.Itoa(i))
	}
	return out
}

func lastArgIndex(args []string, want string) int {
	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == want {
			return i
		}
	}
	return -1
}

func shimArgs(t *testing.T, cfg shimConfig) []string {
	t.Helper()
	args := []string{
		"__job-shim",
		"--event-log-dir", cfg.EventLogDir,
		"--signing-key", cfg.SigningKey,
		"--name", cfg.Name,
		"--lock", cfg.LockPath,
		"--record", cfg.RecordPath,
		"--log", cfg.LogPath,
	}
	args = append(args, sandboxFlagArgs(cfg.Sandbox)...)
	args = append(args, "--")
	return append(args, cfg.Command...)
}

func withShimEventLog(cfg shimConfig, value string) shimConfig {
	cfg.EventLogDir = value
	return cfg
}

func withShimSigningKey(cfg shimConfig, value string) shimConfig {
	cfg.SigningKey = value
	return cfg
}

func withShimRecord(cfg shimConfig, value string) shimConfig {
	cfg.RecordPath = value
	return cfg
}

func withShimLog(cfg shimConfig, value string) shimConfig {
	cfg.LogPath = value
	return cfg
}

func withRestartPolicy(job config.JobConfig, policy string) config.JobConfig {
	job.Restart.Policy = policy
	return job
}

func withRestartCommand(job config.JobConfig, command []string) config.JobConfig {
	job.Command = command
	return job
}

func assertReadyError(t *testing.T, op string, call func(context.Context) error) {
	t.Helper()
	if err := call(nil); err == nil || !strings.Contains(err.Error(), "requires context") {
		t.Fatalf("%s(nil context) = %v, want ready error", op, err)
	}
	if err := call(context.Background()); err == nil || !strings.Contains(err.Error(), "requires context deadline") {
		t.Fatalf("%s(no deadline) = %v, want deadline error", op, err)
	}
	if err := call(canceledContext(t)); err == nil || !strings.Contains(err.Error(), "context:") {
		t.Fatalf("%s(canceled context) = %v, want canceled error", op, err)
	}
}

func assertLockReleased(t *testing.T, path string) {
	t.Helper()
	held, err := lockHeld(path)
	if err != nil || held {
		t.Fatalf("lockHeld(%s) = %v, %v, want released", path, held, err)
	}
}

func failNextJobRecordRead(t *testing.T) {
	t.Helper()
	old := jobRecordReadFile
	reads := 0
	jobRecordReadFile = func(path string) ([]byte, error) {
		reads++
		if reads == 2 {
			return nil, os.ErrNotExist
		}
		return old(path)
	}
	t.Cleanup(func() { jobRecordReadFile = old })
}

func withStopSignaller(t *testing.T, fail func(int, unix.Signal) error) *[]recordedSignal {
	t.Helper()
	records := []recordedSignal{}
	old := processSignaller
	processSignaller = func(pid int, sig unix.Signal) error {
		records = append(records, recordedSignal{pid: pid, sig: sig})
		return fail(pid, sig)
	}
	t.Cleanup(func() { processSignaller = old })
	return &records
}

func unixEPERMOnAnySignal(pid int, sig unix.Signal) error {
	return unix.EPERM
}

func unixEPERMAfterTERM(pid int, sig unix.Signal) error {
	if sig == unix.SIGKILL {
		return unix.EPERM
	}
	return nil
}

func withShimName(cfg shimConfig, name string) shimConfig {
	cfg.Name = name
	return cfg
}

func withShimLock(cfg shimConfig, path string) shimConfig {
	cfg.LockPath = path
	return cfg
}

func withShimCommand(cfg shimConfig, command []string) shimConfig {
	cfg.Command = command
	return cfg
}

func validShimConfig(t *testing.T, name string) shimConfig {
	t.Helper()
	rt := testRuntime(t)
	return shimConfig{
		EventLogDir: rt.EventLogDir,
		SigningKey:  rt.KeyPath,
		Name:        name,
		LockPath:    filepath.Join(rt.EventLogDir, "jobs", name+".lock"),
		RecordPath:  filepath.Join(rt.EventLogDir, "jobs", name+".json"),
		LogPath:     filepath.Join(t.TempDir(), name+".log"),
		Command:     []string{"/bin/true"},
		Sandbox:     config.SandboxConfig{Mode: "off", Reason: "unit test"},
	}
}

func appendFailingWriter(t *testing.T) *eventlog.Writer {
	t.Helper()
	rt := testRuntime(t)
	clk := clock.NewFake()
	signer, err := eventlog.NewSignerFromKeyFile(testContext(t), rt.KeyPath, rt.EventLogDir, clk)
	if err != nil {
		t.Fatalf("NewSignerFromKeyFile() = %v, want nil", err)
	}
	writer, err := eventlog.Open(rt.EventLogDir, signer, eventlog.Limits{Clock: clk})
	if err != nil {
		t.Fatalf("eventlog.Open() = %v, want nil", err)
	}
	if err = os.WriteFile(filepath.Join(rt.EventLogDir, "events"), []byte("file"), 0o600); err != nil {
		t.Fatalf("WriteFile(events file) = %v, want nil", err)
	}
	return writer
}

func preclosedFile(t *testing.T) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "preclosed")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(%s) = %v, want nil", path, err)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("Close(preclosed) = %v, want nil", err)
	}
	return file
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe(stderr) = %v, want nil", err)
	}
	os.Stderr = writer
	defer func() { os.Stderr = old }()
	fn()
	if err = writer.Close(); err != nil {
		t.Fatalf("Close(stderr writer) = %v, want nil", err)
	}
	var buf bytes.Buffer
	if _, err = io.Copy(&buf, reader); err != nil {
		t.Fatalf("Copy(stderr) = %v, want nil", err)
	}
	if err = reader.Close(); err != nil {
		t.Fatalf("Close(stderr reader) = %v, want nil", err)
	}
	return buf.String()
}

func errorWithoutPanic(t *testing.T, name string, call func() error) (err error) {
	t.Helper()
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("%s panicked: %v, want returned error", name, recovered)
		}
	}()
	return call()
}

func assertSafeSignalPID(t *testing.T, pid int) {
	t.Helper()
	if unsafeSignalTarget(pid) {
		t.Fatalf("refuse unsafe signal pid %d", pid)
	}
}

func cleanupRecordProcessGroup(t *testing.T, recordPath string) {
	t.Helper()
	rec, ok := readJobRecord(recordPath)
	if !ok {
		return
	}
	assertSafeSignalPID(t, rec.PID)
	if err := killStartedProcessGroup(rec.PID); err != nil {
		t.Logf("cleanup kill process group %d = %v", rec.PID, err)
	}
}

func longJobCommand(t *testing.T) []string {
	t.Helper()
	t.Setenv("EPITROP_TEST_JOB", "sleep")
	return []string{os.Args[0], "-test.run=TestHelperProcessJob"}
}

func statusOne(t *testing.T, sup *Supervisor, name string) JobStatus {
	t.Helper()
	st, err := sup.Status(testContext(t), name)
	if err != nil {
		t.Fatalf("Status(%s) = %v, want nil", name, err)
	}
	return st[0]
}

func eventlogJob(t *testing.T, sup *Supervisor, name string) eventlog.Job {
	t.Helper()
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatalf("replayState() = %v, want nil", err)
	}
	return state.Jobs[name]
}

func waitForState(t *testing.T, sup *Supervisor, name string, want JobRunState) {
	t.Helper()
	if !waitUntil(func() bool { return statusOne(t, sup, name).State == want }) {
		t.Fatalf("state for %s did not become %s within %s", name, want, testWaitBound)
	}
}

func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()
	if waitUntil(func() bool { return !processExists(pid) }) {
		return
	}
	status, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		status = []byte(err.Error())
	}
	exe, exeErr := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	t.Fatalf("pid %d still exists after %s: exe=%q (%v) status=%q", pid, testWaitBound, exe, exeErr, firstLines(string(status), 8))
}

func firstLines(text string, n int) string {
	lines := strings.SplitN(text, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "; ")
}

func waitForChildPIDInLog(t *testing.T, path string) int {
	t.Helper()
	pid := 0
	found := waitUntil(func() bool {
		body, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		var ok bool
		pid, ok = childPIDFromLog(string(body))
		return ok
	})
	if !found {
		t.Fatalf("child pid did not appear in %s within %s", path, testWaitBound)
	}
	return pid
}

func childPIDFromLog(body string) (int, bool) {
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "child=") {
			pid, err := strconv.Atoi(strings.TrimPrefix(lines[i], "child="))
			return pid, err == nil
		}
	}
	return 0, false
}

// waitForUnlock waits for the job's lock to be free. The shim holds it until its last append
// to the event log is on disk.
func waitForUnlock(t *testing.T, sup *Supervisor, job config.JobConfig) {
	t.Helper()
	free := waitUntil(func() bool {
		running, err := sup.isRunning(job)
		return err == nil && !running
	})
	if !free {
		t.Fatalf("lock for %s stayed held for %s", job.Name, testWaitBound)
	}
}

func countEvents(t *testing.T, dir string, typ string) int {
	t.Helper()
	events := filepath.Join(dir, "events")
	count := 0
	err := filepath.WalkDir(events, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return walkErr
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), `"type":"`+typ+`"`) {
			count++
		}
		return nil
	})
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("WalkDir(events) = %v, want nil", err)
	}
	return count
}

func processHasOpenPath(pid int, want string) (bool, error) {
	fdDir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return false, err
	}
	for i := 0; i < len(entries); i++ {
		target, err := os.Readlink(filepath.Join(fdDir, entries[i].Name()))
		if err != nil {
			continue
		}
		if target == want {
			return true, nil
		}
	}
	return false, nil
}

// processExists reads the process state itself, past the seams the tests plant. A zombie
// has exited; signal 0 would still reach it for as long as its parent leaves it uncollected.
func processExists(pid int) bool {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return false
	}
	status := string(body)
	dead := strings.Contains(status, "\nState:\tZ") || strings.Contains(status, "\nState:\tX")
	// A zombie line with threads left is a running process: see processGone.
	return !dead || !strings.Contains(status, "\nThreads:\t1\n")
}

func processParent(pid int) (int, error) {
	body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(body), "\n")
	for i := 0; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "PPid:") {
			return strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(lines[i], "PPid:")))
		}
	}
	return 0, fmt.Errorf("PPid missing for %d", pid)
}

func runHelperJob() {
	switch os.Getenv("EPITROP_TEST_JOB") {
	case "echo-arg":
		fmt.Println(strings.Join(argsAfterDash(os.Args), " "))
	case "exit-7":
		os.Exit(7)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(30 * time.Second)
	case "spawn-child":
		runChildJob(false)
	case "spawn-child-ignore-term":
		runChildJob(true)
	default:
		time.Sleep(30 * time.Second)
	}
}

func runChildJob(ignoreTerm bool) {
	if ignoreTerm {
		signal.Ignore(syscall.SIGTERM)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	if err := cmd.Start(); err != nil {
		fmt.Printf("child-error=%v\n", err)
		return
	}
	fmt.Printf("child=%d\n", cmd.Process.Pid)
	if err := cmd.Wait(); err != nil {
		return
	}
}

func argsAfterDash(args []string) []string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" && i+1 < len(args) {
			return args[i+1:]
		}
	}
	return nil
}

func TestStatusReportsSandboxTheJobStartedUnder(t *testing.T) {
	job := testJob("sandbox-status", []string{"/bin/sh", "-c", untilStopped("", time.Second)})
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	sup.cfg.Jobs[0].Sandbox.Reason = "changed after start"
	if got := statusOne(t, sup, job.Name).Sandbox; got != "off: supervisor unit test" {
		t.Fatalf("running Sandbox = %q, want the mode recorded at start", got)
	}
	if err := sup.Stop(testContext(t), job.Name); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	if got := statusOne(t, sup, job.Name).Sandbox; got != "off: changed after start" {
		t.Fatalf("stopped Sandbox = %q, want the configured mode", got)
	}
}

func TestRunningSandboxIsUnknownUntilThisStartIsReplayed(t *testing.T) {
	rec := jobRecord{StartedAt: "2026-10-10T08:00:00Z"}
	if got := runningSandbox(rec, eventlog.Job{Since: "2026-10-10T07:00:00Z", Sandbox: "enforce/none"}); got != "unknown" {
		t.Fatalf("runningSandbox(previous start) = %q, want unknown", got)
	}
	if got := runningSandbox(rec, eventlog.Job{Since: rec.StartedAt}); got != "unknown" {
		t.Fatalf("runningSandbox(no recorded mode) = %q, want unknown", got)
	}
	if got := runningSandbox(rec, eventlog.Job{Since: rec.StartedAt, Sandbox: "enforce/egress"}); got != "enforce/egress" {
		t.Fatalf("runningSandbox(this start) = %q, want enforce/egress", got)
	}
}

func withJobSandbox(job config.JobConfig, sandbox config.SandboxConfig) config.JobConfig {
	job.Sandbox = sandbox
	return job
}

func withShimSandbox(cfg shimConfig, sandbox config.SandboxConfig) shimConfig {
	cfg.Sandbox = sandbox
	return cfg
}
