//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"github.com/tekmora/epitrop/internal/egressproxy"
	"github.com/tekmora/epitrop/internal/eventlog"
)

// fakeScopeEnv makes the test binary stand in for systemd-run. See TestMain. With
// fakeStragglerEnv set to a file, the stand-in also leaves a process behind in the job's
// process group and writes its pid there.
const (
	fakeScopeEnv     = "EPITROP_TEST_FAKE_SCOPE"
	fakeStragglerEnv = "EPITROP_TEST_FAKE_SCOPE_STRAGGLER"
)

// TestMain lets the test binary be the scope command of the negative scope tests. Started
// with systemd-run's arguments and fakeScopeEnv set, it does what a systemd-run that reports
// success does, waits a moment and execs the command in place, but creates no scope. Asked by
// the preflight whether it knows scopeNoExpand, it answers as a current systemd-run does.
func TestMain(m *testing.M) {
	if os.Getenv(fakeScopeEnv) == "1" && slices.Equal(os.Args[1:], []string{scopeNoExpand, "--version"}) {
		fmt.Println("systemd 262 (stand-in)")
		os.Exit(0)
	}
	if os.Getenv(fakeScopeEnv) == "1" && len(os.Args) > 1 && os.Args[1] == "--user" {
		os.Exit(runFakeScopeCommand(os.Args))
	}
	// A sandbox with network=proxy runs the program of its shim as the relay; in these tests
	// that program is the test binary.
	if len(os.Args) > 1 && os.Args[1] == egressproxy.RelayCommand {
		os.Exit(egressproxy.RunRelay(os.Args[1:], os.Stderr))
	}
	code := m.Run()
	removeTestScratch()
	os.Exit(code)
}

// The stand-in keeps its main goroutine on the process's first thread, so that the exec
// below always comes from another thread. The kernel then ends the first thread before the
// new program takes over its process id, and for that moment /proc shows the process as a
// zombie: the case that once made the scope check take a live job for an exited one.
func init() {
	if os.Getenv(fakeScopeEnv) == "1" {
		runtime.LockOSThread()
	}
}

func runFakeScopeCommand(args []string) int {
	// The first "--" ends systemd-run's own arguments; pasta and bwrap carry later ones.
	sep := 0
	for sep < len(args) && args[sep] != "--" {
		sep++
	}
	if sep+1 >= len(args) {
		fmt.Fprintln(os.Stderr, "fake scope command: no command after --")
		return 2
	}
	time.Sleep(100 * time.Millisecond)
	if err := leaveFakeStraggler(os.Getenv(fakeStragglerEnv)); err != nil {
		fmt.Fprintf(os.Stderr, "fake scope command: straggler: %v\n", err)
		return 2
	}
	command := args[sep+1:]
	failed := make(chan error, 1)
	go func() {
		// #nosec G204 -- test stand-in for systemd-run; the argv comes from the sandbox builder.
		failed <- syscall.Exec(command[0], command, os.Environ())
	}()
	fmt.Fprintf(os.Stderr, "fake scope command: exec %s: %v\n", command[0], <-failed)
	return 127
}

// leaveFakeStraggler starts a process that is in the job's process group but neither under
// bwrap nor under a parent-death signal: only a kill of the whole group ends it.
func leaveFakeStraggler(pidFile string) error {
	if pidFile == "" {
		return nil
	}
	// No deadline: the stand-in execs the job right after, and the straggler must outlive it.
	straggler := exec.CommandContext(context.Background(), "/usr/bin/sleep", "30") // #nosec G204 -- fixed test stand-in.
	if err := straggler.Start(); err != nil {
		return err
	}
	return os.WriteFile(pidFile, []byte(strconv.Itoa(straggler.Process.Pid)), 0o600)
}

// plantFakeScopeCommand puts the test binary first on PATH under the name systemd-run.
func plantFakeScopeCommand(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	dir := t.TempDir()
	if err = os.Symlink(self, filepath.Join(dir, "systemd-run")); err != nil {
		t.Fatalf("Symlink(systemd-run) = %v, want nil", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakeScopeEnv, "1")
}

func TestNewScopeNameIsOneUnitPerStart(t *testing.T) {
	valid := regexp.MustCompile(`^epitrop-job-[0-9a-f]{16}\.scope$`)
	first, err := newScopeName()
	if err != nil || !valid.MatchString(first) {
		t.Fatalf("newScopeName() = %q, %v, want %s", first, err, valid)
	}
	second, err := newScopeName()
	if err != nil || second == first {
		t.Fatalf("newScopeName() twice = %q, %q, %v, want two names", first, second, err)
	}
}

func TestCgroupInScope(t *testing.T) {
	const scope = "epitrop-job-0123456789abcdef.scope"
	const app = "/user.slice/user-1000.slice/user@1000.service/app.slice/"
	cases := []struct {
		name   string
		cgroup string
		want   bool
	}{
		{"in the scope", app + scope, true},
		{"exited, scope removed", app + scope + " (deleted)", true},
		{"the service the job was started from", "/system.slice/hosted-compute-agent.service", false},
		{"another job's scope", app + "epitrop-job-fedcba9876543210.scope", false},
		{"a scope the shim itself runs in", app + "session-3.scope", false},
		{"a transient scope with another name", app + "run-p12-i34.scope", false},
		{"below the scope", app + scope + "/sub", false},
		{"scope name as a prefix", app + scope + ".d", false},
		{"root", "/", false},
		{"empty", "", false},
	}
	for i := 0; i < len(cases); i++ {
		if got := cgroupInScope(cases[i].cgroup, scope); got != cases[i].want {
			t.Errorf("cgroupInScope(%s: %q) = %v, want %v", cases[i].name, cases[i].cgroup, got, cases[i].want)
		}
	}
}

// scopeWorld scripts what the shim reads about one process, poll by poll. The last entry of
// each list repeats.
type scopeWorld struct {
	cgroups []string
	states  []string
	images  []string
	reads   int
	sleeps  int
}

func pick(values []string, i int) string {
	if i >= len(values) {
		i = len(values) - 1
	}
	return values[i]
}

// starter is the program of the process that starts the job in the scope tests: the shim.
// Right after the start the kernel may still show it as the job's program (#228).
const starter = "/opt/epitrop/bin/epitrop"

// notAChild makes the process under test one that is not a child of the test process, so
// that a made-up process id never reaches the kernel's wait interface.
func notAChild(t *testing.T) {
	t.Helper()
	old := childExitProbe
	t.Cleanup(func() { childExitProbe = old })
	childExitProbe = func(int) (bool, bool, error) { return false, false, nil }
}

func (w *scopeWorld) install(t *testing.T) {
	t.Helper()
	oldCgroup, oldStatus, oldExe, oldSleep, oldStarter := procCgroupReadFile, procStatusReadFile, procExeReadlink, scopeConfirmSleep, startingImage
	t.Cleanup(func() {
		procCgroupReadFile, procStatusReadFile, procExeReadlink, scopeConfirmSleep, startingImage = oldCgroup, oldStatus, oldExe, oldSleep, oldStarter
	})
	startingImage = func() (string, error) { return starter, nil }
	notAChild(t)
	procStatusReadFile = func(string) ([]byte, error) {
		state := pick(w.states, w.sleeps)
		if state == "missing" {
			return nil, fs.ErrNotExist
		}
		return []byte("Name:\tjob\nState:\t" + state + "\nPPid:\t7\nThreads:\t1\n"), nil
	}
	procExeReadlink = func(string) (string, error) {
		image := pick(w.images, w.sleeps)
		if image == "" {
			return "", fs.ErrNotExist
		}
		return image, nil
	}
	procCgroupReadFile = func(string) ([]byte, error) {
		w.reads++
		return []byte("0::" + pick(w.cgroups, w.sleeps) + "\n"), nil
	}
	scopeConfirmSleep = func(time.Duration) { w.sleeps++ }
}

func TestConfirmJobScope(t *testing.T) {
	const scope = "epitrop-job-0123456789abcdef.scope"
	const runner = "/usr/bin/systemd-run"
	const outside = "/system.slice/hosted-compute-agent.service"
	const inside = "/user.slice/user-1001.slice/user@1001.service/app.slice/" + scope
	cases := []struct {
		name      string
		world     scopeWorld
		wantErr   string
		wantReads int
	}{
		{"in the scope at once", scopeWorld{cgroups: []string{inside}, states: []string{"S (sleeping)"}, images: []string{"/usr/bin/env"}}, "", 1},
		{"enters the scope while still systemd-run", scopeWorld{cgroups: []string{outside, outside, inside}, states: []string{"R (running)"}, images: []string{runner}}, "", 3},
		{"enters the scope on the last poll", scopeWorld{cgroups: append(repeat(outside, scopeConfirmPolls-1), inside), states: []string{"S (sleeping)"}, images: []string{runner}}, "", scopeConfirmPolls},
		{"exited inside a scope already removed", scopeWorld{cgroups: []string{inside + " (deleted)"}, states: []string{"Z (zombie)"}, images: []string{""}}, "", 1},
		{"never leaves the cgroup it was started in", scopeWorld{cgroups: []string{outside}, states: []string{"S (sleeping)"}, images: []string{runner}}, "is still in cgroup " + outside + ", not in scope " + scope, scopeConfirmPolls},
		{"runs the job outside the scope", scopeWorld{cgroups: []string{outside}, states: []string{"S (sleeping)"}, images: []string{runner, runner, "/usr/bin/env"}}, "runs as /usr/bin/env in cgroup " + outside + ", outside scope " + scope, 3},
		{"exited outside the scope", scopeWorld{cgroups: []string{outside}, states: []string{"S (sleeping)", "Z (zombie)"}, images: []string{runner, ""}}, "exited in cgroup " + outside + " before it entered scope " + scope, 2},
		{"already reaped", scopeWorld{cgroups: []string{outside}, states: []string{"missing"}, images: []string{""}}, "exited in cgroup " + outside, 1},
		{"still the starting program, then systemd-run", scopeWorld{cgroups: []string{outside, outside, inside}, states: []string{"R (running)"}, images: []string{starter, runner, "/usr/bin/env"}}, "", 3},
		{"stays the starting program outside the scope", scopeWorld{cgroups: []string{outside}, states: []string{"R (running)"}, images: []string{starter}}, "is still in cgroup " + outside, scopeConfirmPolls},
		{"the starting program, then another outside the scope", scopeWorld{cgroups: []string{outside}, states: []string{"R (running)"}, images: []string{starter, "/usr/bin/env"}}, "runs as /usr/bin/env in cgroup " + outside, 2},
		{"program unreadable outside the scope", scopeWorld{cgroups: []string{outside}, states: []string{"S (sleeping)"}, images: []string{""}}, "is still in cgroup " + outside, scopeConfirmPolls},
	}
	for i := 0; i < len(cases); i++ {
		tc := cases[i]
		t.Run(tc.name, func(t *testing.T) {
			tc.world.install(t)
			err := confirmJobScope(4242, scope, runner)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("confirmJobScope() = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("confirmJobScope() = %v, want refusal holding %q", err, tc.wantErr)
			}
			if tc.world.reads != tc.wantReads {
				t.Fatalf("confirmJobScope() read the cgroup %d times, want %d", tc.world.reads, tc.wantReads)
			}
		})
	}
}

func repeat(value string, n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = value
	}
	return out
}

func TestConfirmJobScopeWithoutAScopeReadsNothing(t *testing.T) {
	world := scopeWorld{cgroups: []string{"/"}, states: []string{"S (sleeping)"}, images: []string{"/usr/bin/true"}}
	world.install(t)
	if err := confirmJobScope(4242, "", ""); err != nil || world.reads != 0 {
		t.Fatalf("confirmJobScope(no scope) = %v after %d reads, want nil and 0", err, world.reads)
	}
}

func TestConfirmJobScopeRefusesUnreadableState(t *testing.T) {
	world := scopeWorld{cgroups: []string{"/"}, states: []string{"S (sleeping)"}, images: []string{"/usr/bin/systemd-run"}}
	world.install(t)
	procCgroupReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if err := confirmJobScope(4242, "x.scope", "/usr/bin/systemd-run"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("confirmJobScope(cgroup unreadable) = %v, want the read error", err)
	}
	procCgroupReadFile = func(string) ([]byte, error) { return []byte("1:name=systemd:/x.scope\n"), nil }
	if err := confirmJobScope(4242, "x.scope", "/usr/bin/systemd-run"); err == nil || !strings.Contains(err.Error(), "no cgroup v2 entry") {
		t.Fatalf("confirmJobScope(cgroup v1 only) = %v, want refusal", err)
	}
	procStatusReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if err := confirmJobScope(4242, "x.scope", "/usr/bin/systemd-run"); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("confirmJobScope(status unreadable) = %v, want the read error", err)
	}
}

// TestProcessGoneCountsAZombie uses a real zombie: a child that has exited and is not yet
// collected still answers signal 0, which is why the tests no longer ask that way.
func TestProcessGoneCountsAZombie(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/usr/bin/sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(sleep) = %v, want nil", err)
	}
	pid := cmd.Process.Pid
	checkRunningChildState(t, pid)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill(child) = %v, want nil", err)
	}
	checkZombieChildState(t, pid)
	checkCollectedChildState(t, cmd, pid)
}

func checkRunningChildState(t *testing.T, pid int) {
	t.Helper()
	if gone, err := processGone(pid); err != nil || gone {
		t.Fatalf("processGone(running child) = %v, %v, want false", gone, err)
	}
	if !processExists(pid) {
		t.Fatalf("processExists(running child) = false, want true")
	}
}

func checkZombieChildState(t *testing.T, pid int) {
	t.Helper()
	zombie := false
	for i := 0; i < 200 && !zombie; i++ {
		body, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
		zombie = err == nil && strings.HasPrefix(procStatusField(string(body), "State:"), "Z")
		time.Sleep(5 * time.Millisecond)
	}
	if !zombie {
		t.Fatalf("child %d never became a zombie", pid)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("signal 0 to zombie %d = %v, want nil: the zombie must still be in the process table here", pid, err)
	}
	if gone, err := processGone(pid); err != nil || !gone {
		t.Fatalf("processGone(zombie) = %v, %v, want true", gone, err)
	}
	if processExists(pid) {
		t.Fatalf("processExists(zombie) = true, want false")
	}
}

func checkCollectedChildState(t *testing.T, cmd *exec.Cmd, pid int) {
	t.Helper()
	var killed *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &killed) {
		t.Fatalf("Wait(killed child) = %v, want its kill status: processGone must not collect the child", err)
	}
	if gone, err := processGone(pid); err != nil || !gone {
		t.Fatalf("processGone(collected child) = %v, %v, want true", gone, err)
	}
}

func TestProcessGoneReportsUnreadableState(t *testing.T) {
	old := procStatusReadFile
	t.Cleanup(func() { procStatusReadFile = old })
	notAChild(t)
	procStatusReadFile = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	if gone, err := processGone(4242); gone || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("processGone(unreadable) = %v, %v, want false and the read error", gone, err)
	}
	if shimExited(4242) {
		t.Fatalf("shimExited(unreadable) = true, want a shim of unknown state taken as alive")
	}
	procStatusReadFile = func(string) ([]byte, error) { return nil, syscall.ESRCH }
	if gone, err := processGone(4242); !gone || err != nil {
		t.Fatalf("processGone(ESRCH) = %v, %v, want true", gone, err)
	}
	procStatusReadFile = func(string) ([]byte, error) { return []byte("Name:\tx\nState:\tX (dead)\nThreads:\t1\n"), nil }
	if gone, err := processGone(4242); !gone || err != nil {
		t.Fatalf("processGone(dead) = %v, %v, want true", gone, err)
	}
	checkProcessGoneThreadsLeft(t)
	checkProcessGoneKernelOverrides(t)
}

func checkProcessGoneThreadsLeft(t *testing.T) {
	t.Helper()
	for _, status := range []string{"State:\tZ (zombie)\nThreads:\t3\n", "State:\tZ (zombie)\nThreads:\t0\n", "State:\tZ (zombie)\n", "State:\tX (dead)\nThreads:\t2\n"} {
		procStatusReadFile = func(string) ([]byte, error) { return []byte("Name:\tx\n" + status), nil }
		if gone, err := processGone(4242); gone || err != nil {
			t.Fatalf("processGone(%q) = %v, %v, want false: threads are left", status, gone, err)
		}
	}
}

func checkProcessGoneKernelOverrides(t *testing.T) {
	t.Helper()
	procStatusReadFile = func(string) ([]byte, error) { return []byte("Name:\tx\nState:\tZ (zombie)\nThreads:\t1\n"), nil }
	childExitProbe = func(int) (bool, bool, error) { return false, true, nil }
	if gone, err := processGone(4242); gone || err != nil {
		t.Fatalf("processGone(child the kernel calls running) = %v, %v, want false", gone, err)
	}
	childExitProbe = func(int) (bool, bool, error) { return true, true, nil }
	procStatusReadFile = func(string) ([]byte, error) { return []byte("Name:\tx\nState:\tS (sleeping)\nThreads:\t4\n"), nil }
	if gone, err := processGone(4242); !gone || err != nil {
		t.Fatalf("processGone(child the kernel calls exited) = %v, %v, want true", gone, err)
	}
	childExitProbe = func(int) (bool, bool, error) { return false, false, fs.ErrPermission }
	if gone, err := processGone(4242); gone || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("processGone(wait state unreadable) = %v, %v, want false and the error", gone, err)
	}
}

// TestProcessGoneIsNotFooledByAnExecWithThreads: a child with several threads that execs
// from a thread other than its first is shown as a zombie by /proc for a moment. It has not
// exited, and processGone must not say so at any point.
func TestProcessGoneIsNotFooledByAnExecWithThreads(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() = %v, want nil", err)
	}
	sawZombieLine := 0
	for run := 0; run < 5; run++ {
		sawZombieLine += runExecWithThreadsIteration(t, self)
	}
	// Without this the test would prove nothing on a machine where the moment is never seen.
	t.Logf("/proc showed the running child as a zombie on %d polls over 5 runs", sawZombieLine)
	if sawZombieLine == 0 {
		t.Skipf("/proc never showed the zombie line during the exec; the case was not exercised here")
	}
}

func runExecWithThreadsIteration(t *testing.T, self string) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), self, "--user", "--", "/usr/bin/sleep", "30") // #nosec G204 -- the test binary as the stand-in scope command.
	cmd.Env = append(os.Environ(), fakeScopeEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(stand-in) = %v, want nil", err)
	}
	pid := cmd.Process.Pid
	status := filepath.Join("/proc", strconv.Itoa(pid), "status")
	sawZombie, becameSleep := pollProcessDuringExec(t, pid, status)
	if !becameSleep {
		t.Fatalf("stand-in %d never became sleep", pid)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("Kill(stand-in) = %v, want nil", err)
	}
	if err := awaitExitUncollected(pid); err != nil {
		t.Fatalf("awaitExitUncollected(killed stand-in) = %v, want nil", err)
	}
	if gone, goneErr := processGone(pid); !gone || goneErr != nil {
		t.Fatalf("processGone(killed, uncollected child) = %v, %v, want true", gone, goneErr)
	}
	// The check must leave the child for its parent to collect: a kill status, not "no child".
	var killed *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &killed) {
		t.Fatalf("Wait(killed stand-in) = %v, want its kill status: processGone must not collect the child", err)
	}
	return sawZombie
}

func pollProcessDuringExec(t *testing.T, pid int, status string) (int, bool) {
	t.Helper()
	sawZombie := 0
	execed := time.Time{}
	for i := 0; i < 2_000_000 && (execed.IsZero() || time.Since(execed) < 20*time.Millisecond); i++ {
		gone, goneErr := processGone(pid)
		if gone || goneErr != nil {
			t.Fatalf("processGone(child that only execs) = %v, %v on poll %d, want false: it never exited", gone, goneErr, i)
		}
		if body, readErr := os.ReadFile(status); readErr == nil && strings.HasPrefix(procStatusField(string(body), "State:"), "Z") {
			sawZombie++
		}
		if execed.IsZero() && strings.HasSuffix(processImage(pid), "sleep") {
			execed = time.Now()
		}
	}
	return sawZombie, !execed.IsZero()
}

// TestScopeProbeRefusesAScopeCommandThatCreatesNoScope is the negative case of the probe the
// sandbox integration tests depend on: a scope command that exits 0 without a scope is no
// scope, and the tests must not run against a job outside any scope.
func TestScopeProbeRefusesAScopeCommandThatCreatesNoScope(t *testing.T) {
	plantFakeScopeCommand(t)
	_, err := systemdUserScopeProbe(t)
	if err == nil || !strings.Contains(err.Error(), "no systemd user scope for a job started from cgroup ") || !strings.Contains(err.Error(), "outside scope "+scopeNamePrefix) {
		t.Fatalf("systemdUserScopeProbe(scope command that creates no scope) = %v, want the reason", err)
	}
}

// stopRecorder records what reportMissingScope does with a test.
type stopRecorder struct {
	skipped string
	failed  string
}

func (r *stopRecorder) Helper() {}

func (r *stopRecorder) Skipf(format string, args ...any) { r.skipped = fmt.Sprintf(format, args...) }

func (r *stopRecorder) Fatalf(format string, args ...any) { r.failed = fmt.Sprintf(format, args...) }

// TestMissingScopeFailsUnderCIAndSkipsElsewhere: the hosted runner provides a user scope, so
// a run there without one has lost the sandbox tests and must not pass.
func TestMissingScopeFailsUnderCIAndSkipsElsewhere(t *testing.T) {
	cause := errors.New("no scope here")
	underCI := &stopRecorder{}
	reportMissingScope(underCI, true, cause)
	if underCI.skipped != "" || !strings.Contains(underCI.failed, "no systemd user scope under CI") || !strings.Contains(underCI.failed, "no scope here") {
		t.Fatalf("under CI: skipped %q failed %q, want a failure with the reason and no skip", underCI.skipped, underCI.failed)
	}
	elsewhere := &stopRecorder{}
	reportMissingScope(elsewhere, false, cause)
	if elsewhere.failed != "" || !strings.Contains(elsewhere.skipped, "skipped: no scope here") {
		t.Fatalf("outside CI: skipped %q failed %q, want a skip with the reason and no failure", elsewhere.skipped, elsewhere.failed)
	}
}

// TestSandboxedJobOutsideItsScopeIsRefused is the negative case of the scope check: the
// scope command reports success and starts the job, but the job stays in the cgroup it was
// started from. The shim must kill it, record no start and say why in the job log.
func TestSandboxedJobOutsideItsScopeIsRefused(t *testing.T) {
	plantFakeScopeCommand(t)
	job := sandboxedSupervisorJob(t, "sandbox-noscope", untilStopped(`printf . >> "$PWD/beat";`, 50*time.Millisecond))
	stragglerFile := filepath.Join(t.TempDir(), "straggler.pid")
	t.Setenv(fakeStragglerEnv, stragglerFile)
	job.Sandbox.EnvAllow = []string{fakeScopeEnv, fakeStragglerEnv}
	requireSandboxToolchain(t, job.Sandbox)
	beat := filepath.Join(job.Sandbox.Workspace, "beat")
	sup := testSupervisor(t, job)
	err := sup.Start(testContext(t), job.Name)
	checkSandboxedJobRefusedLogs(t, job, err)
	checkSandboxedJobReplayedRefusal(t, sup, job.Name)
	checkSandboxedJobKilledRecord(t, sup, job.Name)
	checkSandboxedJobStragglerKilled(t, sup, job, stragglerFile, beat)
}

func checkSandboxedJobRefusedLogs(t *testing.T, job config.JobConfig, err error) {
	t.Helper()
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "sandbox-noscope: sandbox: job ") || !strings.Contains(err.Error(), "scope "+scopeNamePrefix) {
		t.Fatalf("Start(job outside its scope) = %v, want ErrRefused naming the job and the scope", err)
	}
	log, err := os.ReadFile(job.LogPath)
	if err != nil {
		t.Fatalf("ReadFile(job log) = %v, want nil", err)
	}
	if !strings.Contains(string(log), "epitrop: job-shim: sandbox-noscope refused: sandbox: job ") || !strings.Contains(string(log), "scope "+scopeNamePrefix) {
		t.Fatalf("job log = %q, want the refusal naming the scope", log)
	}
	t.Logf("refusal in the job log: %s", strings.TrimSpace(string(log)))
}

func checkSandboxedJobReplayedRefusal(t *testing.T, sup *Supervisor, jobName string) {
	t.Helper()
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatalf("replayState() = %v, want nil", err)
	}
	recorded := state.Jobs[jobName]
	if recorded.Since != "" || recorded.ShimPID != 0 || recorded.PID != 0 {
		t.Fatalf("event log holds %+v for the refused job, want no record of a start", recorded)
	}
	if recorded.LastEvent != "job.refused" || recorded.State != string(StateDead) || recorded.RefusedShimPID == 0 || recorded.RefusedAt == "" || !strings.Contains(recorded.LastReason, "scope "+scopeNamePrefix) {
		t.Fatalf("event log holds %+v for the refused job, want job.refused with the shim, the time and the reason", recorded)
	}
}

func checkSandboxedJobKilledRecord(t *testing.T, sup *Supervisor, jobName string) {
	t.Helper()
	rec, ok := readJobRecord(sup.recordPath(jobName))
	if !ok {
		t.Fatalf("job record %s missing, want the record of the killed job", sup.recordPath(jobName))
	}
	waitForProcessGone(t, rec.PID)
}

func checkSandboxedJobStragglerKilled(t *testing.T, sup *Supervisor, job config.JobConfig, stragglerFile string, beat string) {
	t.Helper()
	// The whole process group dies, not only the process the parent-death signal reaches.
	straggler, err := os.ReadFile(stragglerFile)
	if err != nil {
		t.Fatalf("ReadFile(straggler pid) = %v, want the pid the scope command left behind", err)
	}
	stragglerPID, err := strconv.Atoi(string(straggler))
	if err != nil {
		t.Fatalf("straggler pid %q = %v, want a pid", straggler, err)
	}
	waitForProcessGone(t, stragglerPID)
	waitForUnlock(t, sup, job)
	first := fileSize(beat)
	time.Sleep(300 * time.Millisecond)
	if fileSize(beat) != first {
		t.Fatalf("heartbeat %s still growing after the refusal: the job outlived it", beat)
	}
}

func startedPayload(shimPID int) map[string]any {
	return map[string]any{"state": string(StateRunning), "pid": 4242, "shim_pid": shimPID, "since": "2026-10-10T00:00:00Z", "sandbox": "enforce/none"}
}

// TestWaitStartedCountsALateStartByALiveShim: a shim that is alive and records its start
// late, as one waiting for systemd-run to place the job does, still started the job.
func TestWaitStartedCountsALateStartByALiveShim(t *testing.T) {
	sup := testSupervisor(t, testJob("slow", []string{"/bin/true"}))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	shim := os.Getpid()
	appended := make(chan error, 1)
	go func() {
		time.Sleep(2 * time.Second)
		appended <- sup.appendJobEvent(ctx, "job.started", "slow", startedPayload(shim))
	}()
	if err := sup.waitStarted(ctx, "slow", eventlog.Job{}, shim); err != nil {
		t.Fatalf("waitStarted(start recorded after 2 s by a live shim) = %v, want nil", err)
	}
	if err := <-appended; err != nil {
		t.Fatalf("appendJobEvent(job.started) = %v, want nil", err)
	}
}

// TestWaitStartedEndsWhenTheShimHasExited: an exited shim records nothing more, so the wait
// ends with it and says so, long before the poll bound.
func TestWaitStartedEndsWhenTheShimHasExited(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", []string{"/bin/true"}))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	shim := exec.CommandContext(ctx, "/usr/bin/true")
	if err := shim.Start(); err != nil {
		t.Fatalf("Start(true) = %v, want nil", err)
	}
	began := time.Now()
	err := sup.waitStarted(ctx, "probe", eventlog.Job{}, shim.Process.Pid)
	if err == nil || !strings.Contains(err.Error(), "probe did not start: its shim exited without recording a start") {
		t.Fatalf("waitStarted(exited shim) = %v, want did-not-start naming the exited shim", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("waitStarted(exited shim) took %s, want it to end with the shim", took)
	}
	if err = shim.Wait(); err != nil {
		t.Fatalf("Wait(true) = %v, want nil", err)
	}
}

// TestWaitStartedKeepsTheReplayErrorWhenTheContextEnds: while the shim is alive the wait goes
// on, whatever the replay returns. When the caller's context ends it, the error names both
// the context and the replay that was still failing. The context is ended from inside the
// wait, after its second replay, so the test does not depend on how fast the machine is.
func TestWaitStartedKeepsTheReplayErrorWhenTheContextEnds(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", []string{"/bin/true"}))
	if err := sup.appendJobEvent(testContext(t), "job.stopped", "probe", map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("append seed = %v, want nil", err)
	}
	corruptEventLog(t, sup.cfg.EventLog.Dir)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	old := procStatusReadFile
	t.Cleanup(func() { procStatusReadFile = old })
	notAChild(t)
	checks := 0
	procStatusReadFile = func(string) ([]byte, error) {
		checks++
		if checks == 2 {
			cancel()
		}
		return []byte("Name:\tshim\nState:\tS (sleeping)\n"), nil
	}
	err := sup.waitStarted(ctx, "probe", eventlog.Job{}, 4244)
	if !errors.Is(err, context.Canceled) || checks != 2 {
		t.Fatalf("waitStarted(corrupt log, live shim) = %v after %d checks, want the ended context after 2", err, checks)
	}
	if !strings.Contains(err.Error(), "probe did not start: ") || !strings.Contains(err.Error(), "last replay: ") || !strings.Contains(err.Error(), "eventlog: seq 1 file ") {
		t.Fatalf("waitStarted(corrupt log, live shim) = %v, want did-not-start with the last replay error", err)
	}
	var both interface{ Unwrap() []error }
	if !errors.As(err, &both) || len(both.Unwrap()) != 2 {
		t.Fatalf("waitStarted(corrupt log, live shim) = %v, want it to wrap the context's end and the replay error", err)
	}
}

// TestLastReplayError: the deadline can fall inside a replay as well as between two. A replay
// it cut short must not replace what the last complete replay said about the log.
func TestLastReplayError(t *testing.T) {
	broken := errors.New("log is broken")
	cut := fmt.Errorf("replay: %w", context.DeadlineExceeded)
	cases := []struct {
		name      string
		previous  error
		replayErr error
		ctxErr    error
		want      error
	}{
		{"a complete failing replay is kept", nil, broken, nil, broken},
		{"a complete clean replay clears the last failure", broken, nil, nil, nil},
		{"a replay cut short keeps the last failure", broken, cut, context.DeadlineExceeded, broken},
		{"a replay cut short keeps a clean result", nil, cut, context.DeadlineExceeded, nil},
		{"a result that arrives with the context's end is not trusted", broken, nil, context.Canceled, broken},
	}
	for i := 0; i < len(cases); i++ {
		if got := lastReplayError(cases[i].previous, cases[i].replayErr, cases[i].ctxErr); !errors.Is(got, cases[i].want) || (got == nil) != (cases[i].want == nil) {
			t.Errorf("lastReplayError(%s) = %v, want %v", cases[i].name, got, cases[i].want)
		}
	}
}

// TestWaitStartedNamesOnlyTheContextWhenReplaysWork: with nothing wrong in the log, the
// error of an ended wait is the context's alone.
func TestWaitStartedNamesOnlyTheContextWhenReplaysWork(t *testing.T) {
	sup := testSupervisor(t, testJob("probe", []string{"/bin/true"}))
	// One record, so that every replay reads the log and checks its context.
	if err := sup.appendJobEvent(testContext(t), "job.stopped", "probe", map[string]any{"state": "stopped"}); err != nil {
		t.Fatalf("append seed = %v, want nil", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	err := sup.waitStarted(ctx, "probe", eventlog.Job{}, os.Getpid())
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "last replay") {
		t.Fatalf("waitStarted(clean log, live shim) = %v, want did-not-start with the deadline only", err)
	}
	if got, want := err.Error(), "supervisor: probe did not start: context deadline exceeded"; got != want {
		t.Fatalf("waitStarted(clean log, live shim) = %q, want %q", got, want)
	}
	// A replay that failed only because the context had ended adds nothing to say.
	err = sup.waitStarted(canceledContext(t), "probe", eventlog.Job{}, os.Getpid())
	if got, want := fmt.Sprint(err), "supervisor: probe did not start: context canceled"; got != want {
		t.Fatalf("waitStarted(canceled context, live shim) = %q, want %q", got, want)
	}
}

// TestWaitStartedReplaysOnceMoreAfterTheShimExited: a shim can record its start and exit
// between a replay and the check that finds it gone. That start must still count.
func TestWaitStartedReplaysOnceMoreAfterTheShimExited(t *testing.T) {
	sup := testSupervisor(t, testJob("fast", []string{"/bin/true"}))
	ctx := testContext(t)
	old := procStatusReadFile
	t.Cleanup(func() { procStatusReadFile = old })
	notAChild(t)
	checks := 0
	procStatusReadFile = func(string) ([]byte, error) {
		checks++
		if err := sup.appendJobEvent(ctx, "job.started", "fast", startedPayload(4243)); err != nil {
			t.Errorf("appendJobEvent(job.started) = %v, want nil", err)
		}
		return nil, fs.ErrNotExist
	}
	if err := sup.waitStarted(ctx, "fast", eventlog.Job{}, 4243); err != nil || checks != 1 {
		t.Fatalf("waitStarted(start recorded as the shim exited) = %v after %d checks, want nil after 1", err, checks)
	}
}

// requireSandboxToolchain skips where the sandbox preflight itself would stop the shim, so
// a test of a later refusal cannot pass or fail for that reason.
func requireSandboxToolchain(t *testing.T, sandbox config.SandboxConfig) {
	t.Helper()
	if _, err := sandboxToolchain(sandbox, sandboxTools{}); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("sandboxToolchain() = %v under CI, want the installed toolchain", err)
		}
		t.Skipf("sandbox test skipped: toolchain unavailable: %v", err)
	}
}
