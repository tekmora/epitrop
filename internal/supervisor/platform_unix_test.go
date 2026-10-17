//go:build linux

package supervisor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"golang.org/x/sys/unix"
)

type recordedSignal struct {
	pid int
	sig unix.Signal
}

func TestShimClosesLockOnUnixLogOpenFailure(t *testing.T) {
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

func TestSignalProcessGroupRefusesUnsafeRecordTargets(t *testing.T) {
	rec := withRecordingSignaller(t)
	tests := []struct {
		name   string
		target jobSignalTarget
		want   string
	}{
		{name: "pid zero", target: testSignalTarget(0, os.Getpid()), want: "pid 0 is not safe"},
		{name: "pid one", target: testSignalTarget(1, os.Getpid()), want: "pid 1 is not safe"},
		{name: "own pid", target: testSignalTarget(os.Getpid(), os.Getpid()), want: "targets supervisor"},
	}
	if unix.Getpgrp() > 1 {
		tests = append(tests, struct {
			name   string
			target jobSignalTarget
			want   string
		}{name: "own process group", target: testSignalTarget(unix.Getpgrp(), os.Getpid()), want: "targets supervisor"})
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			err := signalProcessGroup(tests[i].target, "TERM")
			assertRefusedWithoutSignal(t, err, tests[i].want, rec)
		})
	}
}

func TestSignalProcessGroupRefusesParentMismatch(t *testing.T) {
	rec := withRecordingSignaller(t)
	cmd := startSleep(t, true)
	target := testSignalTarget(cmd.Process.Pid, 1)
	err := signalProcessGroup(target, "TERM")
	assertRefusedWithoutSignal(t, err, "does not match shim_pid 1", rec)
}

func TestSignalProcessGroupRefusesNonGroupLeader(t *testing.T) {
	rec := withRecordingSignaller(t)
	cmd := startSleep(t, false)
	target := testSignalTarget(cmd.Process.Pid, os.Getpid())
	err := signalProcessGroup(target, "TERM")
	assertRefusedWithoutSignal(t, err, "not a process-group leader", rec)
}

func TestSignalProcessGroupRefusesUnlockedMatchingParent(t *testing.T) {
	rec := withRecordingSignaller(t)
	cmd := startSleep(t, true)
	target := testSignalTarget(cmd.Process.Pid, os.Getpid())
	target.LockPath = filepath.Join(t.TempDir(), "free.lock")
	err := signalProcessGroup(target, "TERM")
	assertRefusedWithoutSignal(t, err, "job lock is not held", rec)
}

func TestSignalProcessGroupRefusesUnprovenShimAndLeavesDecoyAlive(t *testing.T) {
	rec := withRecordingSignaller(t)
	cmd := startSleep(t, true)
	lockPath := filepath.Join(t.TempDir(), "daemon.lock")
	lock, err := holdLock(lockPath)
	if err != nil {
		t.Fatalf("holdLock(%s) = %v, want nil", lockPath, err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Fatalf("Close(lock) = %v, want nil", err)
		}
	})
	target := testSignalTarget(cmd.Process.Pid, os.Getpid())
	target.LockPath = lockPath
	err = signalProcessGroup(target, "TERM")
	assertRefusedWithoutSignal(t, err, "__job-shim missing", rec)
	if !processExists(cmd.Process.Pid) {
		t.Fatalf("decoy pid %d exited, want alive after refused signal", cmd.Process.Pid)
	}
}

func TestStopDoesNotEscalateWhenTermStopsWithinGrace(t *testing.T) {
	rec := withPassthroughSignaller(t)
	job := testJob("gentle", []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.Stop = config.JobStopConfig{Signal: "TERM", GraceSeconds: 1}
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", "sleep")
	if err := sup.Start(testContext(t), "gentle"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	if err := sup.Stop(testContext(t), "gentle"); err != nil {
		t.Fatalf("Stop() = %v, want nil", err)
	}
	for i := 0; i < len(*rec); i++ {
		if (*rec)[i].sig == unix.SIGKILL {
			t.Fatalf("signals = %+v, want no SIGKILL after graceful TERM", *rec)
		}
	}
}

func TestHoldLockErrorAndStateBranches(t *testing.T) {
	checkHoldLockInvalidPathErrors(t)
	lockPath := filepath.Join(t.TempDir(), "daemon.lock")
	lock, err := holdLock(lockPath)
	if err != nil {
		t.Fatalf("holdLock(first) = %v, want nil", err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := holdLock(lockPath)
		done <- err
	}()
	select {
	case err = <-done:
		checkHoldLockContendedResult(t, err)
	case <-time.After(200 * time.Millisecond):
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock after blocked contender) = %v, want nil", closeErr)
		}
		err = <-done
		t.Fatalf("holdLock(contended) = %v after blocking wait, want nonblocking flock error", err)
	}
}

func checkHoldLockInvalidPathErrors(t *testing.T) {
	t.Helper()
	fileParent := filepath.Join(t.TempDir(), "state-file")
	if err := os.WriteFile(fileParent, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(state-file) = %v, want nil", err)
	}
	if _, err := holdLock(filepath.Join(fileParent, "daemon.lock")); err == nil || !strings.Contains(err.Error(), "mkdir lock dir") {
		t.Fatalf("holdLock(file parent) = %v, want mkdir error", err)
	}
	dirPath := t.TempDir()
	if _, err := holdLock(dirPath); err == nil || !strings.Contains(err.Error(), "open lock") {
		t.Fatalf("holdLock(directory) = %v, want open error", err)
	}
}

func checkHoldLockContendedResult(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "flock") {
		t.Fatalf("holdLock(contended) = %v, want flock error", err)
	}
}

func TestLockHeldReportsFreeHeldAndMissingParent(t *testing.T) {
	freePath := filepath.Join(t.TempDir(), "free.lock")
	held, err := lockHeld(freePath)
	if err != nil || held {
		t.Fatalf("lockHeld(free) = %v, %v, want false nil", held, err)
	}
	lockPath := filepath.Join(t.TempDir(), "held.lock")
	lock, err := holdLock(lockPath)
	if err != nil {
		t.Fatalf("holdLock(held) = %v, want nil", err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	held, err = lockHeld(lockPath)
	if err != nil || !held {
		t.Fatalf("lockHeld(held) = %v, %v, want true nil", held, err)
	}
}

func TestSignalValidationAndDeliveryErrors(t *testing.T) {
	rec := withRecordingSignaller(t)
	if _, err := signalFromName("INT"); err != nil {
		t.Fatalf("signalFromName(INT) = %v, want nil", err)
	}
	if _, err := signalFromName("HUP"); err != nil {
		t.Fatalf("signalFromName(HUP) = %v, want nil", err)
	}
	err := signalProcessGroup(testSignalTarget(0, os.Getpid()), "BOGUS")
	assertRefusedWithoutSignal(t, err, "unsupported signal", rec)
	checkSignalTargetValidation(t)
	checkArgHelpers(t)
}

func checkSignalTargetValidation(t *testing.T) {
	t.Helper()
	cmd := startSleep(t, true)
	target := testSignalTarget(cmd.Process.Pid, cmd.Process.Pid)
	err := validateSignalTarget(target)
	if err == nil || !strings.Contains(err.Error(), "pid equals shim_pid") {
		t.Fatalf("validateSignalTarget(pid equals shim) = %v, want refusal", err)
	}
	target.ShimPID = 99999999
	err = validateSignalTarget(target)
	if err == nil || !strings.Contains(err.Error(), "does not match shim_pid") {
		t.Fatalf("validateSignalTarget(bad shim parent) = %v, want parent refusal", err)
	}
	if _, err = linuxProcessParent(99999999); err == nil {
		t.Fatal("linuxProcessParent(missing) = nil, want error")
	}
}

func checkArgHelpers(t *testing.T) {
	t.Helper()
	args := []string{"__job-shim", "--name", "daemon", "--lock", "/lock", "--record", "/record"}
	if !hasArg(args, "__job-shim") || hasArg(args, "missing") {
		t.Fatalf("hasArg(%q) returned wrong membership", args)
	}
	if !hasArgPair(args, "--name", "daemon") || hasArgPair(args, "--name", "other") {
		t.Fatalf("hasArgPair(%q) returned wrong membership", args)
	}
	if hasArgPair([]string{"--name"}, "--name", "daemon") {
		t.Fatal("hasArgPair(key without value) = true, want false")
	}
	if !hasArgPair([]string{"__job-shim", "--name", "daemon"}, "--name", "daemon") {
		t.Fatal("hasArgPair(final pair) = false, want true")
	}
	if hasArgPair([]string{"--other", "daemon"}, "--name", "daemon") {
		t.Fatal("hasArgPair(value without key) = true, want false")
	}
}

func TestLinuxCmdlineShimChecksEveryRequiredArg(t *testing.T) {
	rec := withRecordingSignaller(t)
	target := testSignalTarget(123, 456)
	// A lock of this test's own: the helper's path is the same for every test process on the
	// machine, and two of them holding it at once would refuse each other.
	target.LockPath = filepath.Join(t.TempDir(), "daemon.lock")
	lock, err := holdLock(target.LockPath)
	if err != nil {
		t.Fatalf("holdLock(%s) = %v, want nil", target.LockPath, err)
	}
	t.Cleanup(func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	})
	withShimProbe(t, nil)
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "sentinel", args: []string{"--name", "daemon", "--lock", target.LockPath, "--record", target.RecordPath}, want: "__job-shim missing"},
		{name: "name", args: []string{"__job-shim", "--name", "other", "--lock", target.LockPath, "--record", target.RecordPath}, want: "--name mismatch"},
		{name: "lock", args: []string{"__job-shim", "--name", target.Name, "--lock", "/bad", "--record", target.RecordPath}, want: "--lock mismatch"},
		{name: "record", args: []string{"__job-shim", "--name", target.Name, "--lock", target.LockPath, "--record", "/bad"}, want: "--record mismatch"},
	}
	for i := 0; i < len(tests); i++ {
		t.Run(tests[i].name, func(t *testing.T) {
			withProcCmdline(t, tests[i].args)
			err := validateSignalShim(target)
			assertRefusedWithoutSignal(t, err, tests[i].want, rec)
		})
	}
}

func TestSignalValidationPropagatesWrapperErrors(t *testing.T) {
	rec := withRecordingSignaller(t)
	target := testSignalTarget(123, 456)
	withProcessGroupID(t, unix.EPERM)
	err := signalProcessGroup(target, "TERM")
	assertRefusedWithoutSignal(t, err, "getpgid 123", rec)
	withProcStatusError(t, os.ErrPermission)
	target = testSignalTarget(os.Getpid()+100000, 456)
	err = validateSignalParent(target)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("validateSignalParent(read error) = %v, want permission error", err)
	}
	withShimProbe(t, unix.EPERM)
	err = validateSignalShim(target)
	if err == nil || !strings.Contains(err.Error(), "shim_pid 456 is not alive") {
		t.Fatalf("validateSignalShim(probe error) = %v, want shim liveness error", err)
	}
}

func TestLockAndProcReadersPropagateExactErrors(t *testing.T) {
	target := testSignalTarget(123, 456)
	target.LockPath = filepath.Join(t.TempDir(), "state-file", "daemon.lock")
	if err := os.WriteFile(filepath.Dir(target.LockPath), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(lock parent) = %v, want nil", err)
	}
	withShimProbe(t, nil)
	err := validateSignalShim(target)
	if err == nil || !strings.Contains(err.Error(), "mkdir lock dir") {
		t.Fatalf("validateSignalShim(lock error) = %v, want lock error", err)
	}
	checkProcReadersPropagateErrors(t, target)
	target.LockPath = filepath.Join(t.TempDir(), "daemon.lock")
	lock, err := holdLock(target.LockPath)
	if err != nil {
		t.Fatalf("holdLock(%s) = %v, want nil", target.LockPath, err)
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			t.Fatalf("Close(lock) = %v, want nil", closeErr)
		}
	}()
	if err = validateSignalShim(target); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("validateSignalShim(cmdline read error) = %v, want permission error", err)
	}
}

func checkProcReadersPropagateErrors(t *testing.T, target jobSignalTarget) {
	t.Helper()
	withProcStatusBody(t, "Name:\ttest\n")
	if _, err := linuxProcessParent(123); err == nil || !strings.Contains(err.Error(), "PPid missing") {
		t.Fatalf("linuxProcessParent(missing PPid) = %v, want missing PPid", err)
	}
	withProcCmdlineError(t, os.ErrPermission)
	if _, err := shimCmdlineMismatch(target); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("shimCmdlineMismatch(read error) = %v, want permission error", err)
	}
}

func TestShimCmdlineMismatchTrimsTrailingNULAndRefusesEmpty(t *testing.T) {
	target := testSignalTarget(123, 456)
	withProcCmdline(t, []string{"__job-shim", "--name", target.Name, "--lock", target.LockPath, "--record", target.RecordPath})
	if reason, err := shimCmdlineMismatch(target); err != nil || reason != "" {
		t.Fatalf("shimCmdlineMismatch(valid trailing NUL) = %q, %v, want match", reason, err)
	}
	withProcCmdline(t, nil)
	if reason, err := shimCmdlineMismatch(target); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("shimCmdlineMismatch(empty cmdline) = %q, %v, want empty-cmdline error", reason, err)
	}
}

func TestKillProcessRefusesInvalidTargetWithoutSignal(t *testing.T) {
	rec := withRecordingSignaller(t)
	target := testSignalTarget(0, os.Getpid())
	err := killProcess(target)
	assertRefusedWithoutSignal(t, err, "pid 0 is not safe", rec)
	cmd := startSleep(t, true)
	target = testSignalTarget(cmd.Process.Pid, cmd.Process.Pid)
	err = killProcess(target)
	assertRefusedWithoutSignal(t, err, "pid equals shim_pid", rec)
}

func TestSignalAndKillIgnoreMissingProcessGroup(t *testing.T) {
	job := testJob("daemon", longJobCommand(t))
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	target, err := sup.signalTarget(job)
	if err != nil {
		t.Fatalf("signalTarget() = %v, want nil", err)
	}
	old := processSignaller
	processSignaller = func(pid int, sig unix.Signal) error {
		if pid != -target.PID {
			t.Fatalf("signal pid = %d, want process group %d", pid, -target.PID)
		}
		return unix.ESRCH
	}
	t.Cleanup(func() {
		processSignaller = old
	})
	if err = signalProcessGroup(target, "TERM"); err != nil {
		t.Fatalf("signalProcessGroup(ESRCH) = %v, want nil", err)
	}
	if err = killProcess(target); err != nil {
		t.Fatalf("killProcess(ESRCH) = %v, want nil", err)
	}
}

func TestSignalAndKillReturnUnexpectedSignallerError(t *testing.T) {
	job := testJob("daemon", longJobCommand(t))
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), "daemon"); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	target, err := sup.signalTarget(job)
	if err != nil {
		t.Fatalf("signalTarget() = %v, want nil", err)
	}
	old := processSignaller
	processSignaller = func(pid int, sig unix.Signal) error {
		if pid != -target.PID {
			t.Fatalf("signal pid = %d, want process group %d", pid, -target.PID)
		}
		return unix.EPERM
	}
	t.Cleanup(func() {
		processSignaller = old
	})
	err = signalProcessGroup(target, "TERM")
	if err == nil || !strings.Contains(err.Error(), "signal TERM") {
		t.Fatalf("signalProcessGroup(EPERM) = %v, want signal error", err)
	}
	err = killProcess(target)
	if err == nil || !strings.Contains(err.Error(), "SIGKILL") {
		t.Fatalf("killProcess(EPERM) = %v, want SIGKILL error", err)
	}
	if err = killStartedProcessGroup(0); err == nil || !strings.Contains(err.Error(), "pid 0 is not safe") {
		t.Fatalf("killStartedProcessGroup(0) = %v, want safety error", err)
	}
}

func TestKillAndCollectJoinsKillFailure(t *testing.T) {
	cmd := startSleep(t, true)
	cause := errors.New("write failed")
	old := processSignaller
	processSignaller = func(pid int, sig unix.Signal) error {
		if pid != -cmd.Process.Pid {
			t.Fatalf("signal pid = %d, want process group %d", pid, -cmd.Process.Pid)
		}
		return unix.EPERM
	}
	t.Cleanup(func() { processSignaller = old })
	err := killAndCollect(cmd, cause)
	if err == nil || !strings.Contains(err.Error(), "write failed") || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("killAndCollect(EPERM) = %v, want joined cause and kill error", err)
	}
	if cmd.ProcessState != nil {
		t.Fatalf("killAndCollect(EPERM) waited for a job it could not kill, want no wait")
	}
}

// TestKillAndCollectLeavesNoZombie: a job the shim started and then gives up is killed with
// its group and collected, so that nothing of it is left, not even its exit status. The job
// here would exit with status 0 by itself after a second: only a kill ends it by a signal.
func TestKillAndCollectLeavesNoZombie(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 1")
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start(sh) = %v, want nil", err)
	}
	cause := errors.New("record failed")
	if err := killAndCollect(cmd, cause); !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatalf("killAndCollect() = %v, want the cause and nothing added", err)
	}
	checkKilledJobState(t, cmd)
	// A job that was collected already cannot be collected again: that is reported, joined
	// with the cause.
	err := killAndCollect(cmd, cause)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "job-shim: collect the killed job: ") {
		t.Fatalf("killAndCollect(collected job) = %v, want the cause joined with the wait error", err)
	}
}

func checkKilledJobState(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.ProcessState == nil {
		t.Fatalf("killAndCollect() did not collect the job, want its exit status taken")
	}
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("job ended as %v, want it killed, not left to finish", cmd.ProcessState)
	}
	if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("/proc entry of the killed job: %v, want it gone", err)
	}
}

func TestLockCloseErrorPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad-close.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(%s) = %v, want nil", path, err)
	}
	if err = file.Close(); err != nil {
		t.Fatalf("Close(preclose) = %v, want nil", err)
	}
	lock := &heldLock{file: file}
	if err = lock.Close(); err == nil || !strings.Contains(err.Error(), "unlock") {
		t.Fatalf("heldLock.Close(preclosed) = %v, want unlock error", err)
	}
	cause := errors.New("cause")
	err = closeHeldLock(file, cause)
	if err == nil || !strings.Contains(err.Error(), "cause") || !strings.Contains(err.Error(), "close lock") {
		t.Fatalf("closeHeldLock(preclosed) = %v, want joined close error", err)
	}
}

func withRecordingSignaller(t *testing.T) *[]recordedSignal {
	t.Helper()
	records := []recordedSignal{}
	old := processSignaller
	processSignaller = func(pid int, sig unix.Signal) error {
		records = append(records, recordedSignal{pid: pid, sig: sig})
		return nil
	}
	t.Cleanup(func() {
		processSignaller = old
	})
	return &records
}

func withPassthroughSignaller(t *testing.T) *[]recordedSignal {
	t.Helper()
	records := []recordedSignal{}
	old := processSignaller
	processSignaller = func(pid int, sig unix.Signal) error {
		records = append(records, recordedSignal{pid: pid, sig: sig})
		if err := guardedKill(pid, sig, unix.Kill); err != nil {
			t.Errorf("passthrough signaller: %v", err)
			return err
		}
		return nil
	}
	t.Cleanup(func() {
		processSignaller = old
	})
	return &records
}

func withProcessGroupID(t *testing.T, err error) {
	t.Helper()
	old := processGroupID
	processGroupID = func(pid int) (int, error) {
		if err != nil {
			return 0, err
		}
		return pid, nil
	}
	t.Cleanup(func() { processGroupID = old })
}

func withShimProbe(t *testing.T, err error) {
	t.Helper()
	old := shimProcessProbe
	shimProcessProbe = func(pid int, sig unix.Signal) error {
		return err
	}
	t.Cleanup(func() { shimProcessProbe = old })
}

func withProcStatusError(t *testing.T, err error) {
	t.Helper()
	old := procStatusReadFile
	procStatusReadFile = func(path string) ([]byte, error) {
		return nil, err
	}
	t.Cleanup(func() { procStatusReadFile = old })
}

func withProcStatusBody(t *testing.T, body string) {
	t.Helper()
	old := procStatusReadFile
	procStatusReadFile = func(path string) ([]byte, error) {
		return []byte(body), nil
	}
	t.Cleanup(func() { procStatusReadFile = old })
}

func withProcCmdline(t *testing.T, args []string) {
	t.Helper()
	old := procCmdlineReadFile
	procCmdlineReadFile = func(path string) ([]byte, error) {
		return []byte(strings.Join(args, "\x00") + "\x00"), nil
	}
	t.Cleanup(func() { procCmdlineReadFile = old })
}

func withProcCmdlineError(t *testing.T, err error) {
	t.Helper()
	old := procCmdlineReadFile
	procCmdlineReadFile = func(path string) ([]byte, error) {
		return nil, err
	}
	t.Cleanup(func() { procCmdlineReadFile = old })
}

func testSignalTarget(pid int, shimPID int) jobSignalTarget {
	return jobSignalTarget{
		Name:       "daemon",
		PID:        pid,
		ShimPID:    shimPID,
		RecordPath: filepath.Join(testScratch(), fmt.Sprintf("daemon-%d.json", pid)),
		LockPath:   filepath.Join(testScratch(), fmt.Sprintf("daemon-%d.lock", pid)),
	}
}

func assertRefusedWithoutSignal(t *testing.T, err error, want string, records *[]recordedSignal) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("signalProcessGroup() = %v, want %q", err, want)
	}
	if len(*records) != 0 {
		t.Fatalf("recorded signals = %+v, want none", *records)
	}
}

func startSleep(t *testing.T, setpgid bool) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: setpgid}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep = %v, want nil", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if err := cmd.Process.Kill(); err != nil {
				t.Logf("cleanup kill sleep pid %d = %v", cmd.Process.Pid, err)
			}
		}
		done := make(chan error, 1)
		go func() {
			done <- cmd.Wait()
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("sleep pid %d did not exit after cleanup kill", cmd.Process.Pid)
		}
	})
	return cmd
}

// TestWaitStoppedCanceledContextProbesOnce pins that a canceled context ends the wait
// after the first lock probe. The lock stays held throughout, so the result does not
// depend on timing; inotify counts the probes, because each one opens the lock file.
func TestWaitStoppedCanceledContextProbesOnce(t *testing.T) {
	job := testJob("held", []string{"/bin/true"})
	job.Stop.GraceSeconds = 300
	sup := testSupervisor(t, job)
	lockPath := sup.lockPath(job.Name)
	lock, err := holdLock(lockPath)
	if err != nil {
		t.Fatalf("holdLock(%s) = %v, want nil", lockPath, err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Errorf("Close(lock) = %v, want nil", err)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := true
	opens := countFileOpens(t, lockPath, func() { stopped = sup.waitStopped(ctx, job) })
	if stopped {
		t.Fatal("waitStopped(canceled ctx, held lock) = true, want false")
	}
	if opens != 1 {
		t.Fatalf("waitStopped(canceled ctx) probed the lock %d times, want 1", opens)
	}
}

// countFileOpens returns how often path is opened while fn runs. A queue overflow
// counts as more opens than any caller expects.
func countFileOpens(t *testing.T, path string, fn func()) int {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatalf("InotifyInit1() = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := unix.Close(fd); err != nil {
			t.Errorf("Close(inotify) = %v, want nil", err)
		}
	})
	// Watch closes too: the kernel merges identical consecutive unread events, so with
	// IN_OPEN alone any number of probes would read back as one.
	if _, err = unix.InotifyAddWatch(fd, path, unix.IN_OPEN|unix.IN_CLOSE); err != nil {
		t.Fatalf("InotifyAddWatch(%s) = %v, want nil", path, err)
	}
	fn()
	buf := make([]byte, 64*1024)
	opens := 0
	for i := 0; i < 1024; i++ {
		n, err := unix.Read(fd, buf)
		if errors.Is(err, unix.EAGAIN) {
			return opens
		}
		if err != nil {
			t.Fatalf("Read(inotify) = %v, want nil or EAGAIN", err)
		}
		opens += countInotifyOpens(buf[:n])
	}
	t.Fatalf("inotify queue still had events after 1024 reads")
	return opens
}

// countInotifyOpens counts IN_OPEN events in one read of inotify events.
func countInotifyOpens(events []byte) int {
	opens := 0
	for off := 0; off+unix.SizeofInotifyEvent <= len(events); {
		mask := binary.NativeEndian.Uint32(events[off+4:])
		nameLen := int(binary.NativeEndian.Uint32(events[off+12:]))
		if mask&unix.IN_Q_OVERFLOW != 0 {
			return 1 << 20
		}
		if mask&unix.IN_OPEN != 0 {
			opens++
		}
		off += unix.SizeofInotifyEvent + nameLen
	}
	return opens
}

// TestValidateSignalShimProbesWithSignalZero pins that the liveness probe is signal 0
// (an existence check): any real signal here would hit the shim during validation.
func TestValidateSignalShimProbesWithSignalZero(t *testing.T) {
	target := testSignalTarget(123, 456)
	var probed []unix.Signal
	old := shimProcessProbe
	shimProcessProbe = func(pid int, sig unix.Signal) error {
		probed = append(probed, sig)
		return unix.ESRCH
	}
	t.Cleanup(func() { shimProcessProbe = old })
	if err := validateSignalShim(target); err == nil || !strings.Contains(err.Error(), "is not alive") {
		t.Fatalf("validateSignalShim(dead shim) = %v, want not-alive error", err)
	}
	if len(probed) != 1 || probed[0] != 0 {
		t.Fatalf("shim probe signals = %v, want exactly [0]", probed)
	}
}

// TestSignalAndKillTreatVanishedTargetAsDone pins that a job which exits on its own while
// Stop runs is not an error: nothing is signalled and the call succeeds.
func TestSignalAndKillTreatVanishedTargetAsDone(t *testing.T) {
	rec := withRecordingSignaller(t)
	cmd := exec.CommandContext(testContext(t), "/bin/true")
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run(/bin/true) = %v, want nil", err)
	}
	gone := cmd.Process.Pid
	target := testSignalTarget(gone, os.Getpid())
	if err := signalProcessGroup(target, "TERM"); err != nil {
		t.Fatalf("signalProcessGroup(vanished pid %d) = %v, want nil", gone, err)
	}
	if err := killProcess(target); err != nil {
		t.Fatalf("killProcess(vanished pid %d) = %v, want nil", gone, err)
	}
	if err := killStartedProcessGroup(gone); err != nil {
		t.Fatalf("killStartedProcessGroup(vanished pid %d) = %v, want nil", gone, err)
	}
	if len(*rec) != 0 {
		t.Fatalf("signals = %+v, want none for a vanished target", *rec)
	}
}

// TestValidateSignalParentTreatsMissingProcStatusAsGone pins that a target whose
// /proc entry disappeared between checks is treated as gone, and nothing is signalled.
// The entry is missing for a process that ended before the status file was opened
// (ENOENT), and the read fails with ESRCH for one that ended after the open.
func TestValidateSignalParentTreatsMissingProcStatusAsGone(t *testing.T) {
	gone := []struct {
		name string
		err  error
	}{
		{name: "ended before the open", err: os.ErrNotExist},
		{name: "ended between the open and the read", err: &os.PathError{Op: "read", Path: "/proc/1/status", Err: unix.ESRCH}},
	}
	for i := 0; i < len(gone); i++ {
		t.Run(gone[i].name, func(t *testing.T) {
			rec := withRecordingSignaller(t)
			cmd := startSleep(t, true)
			withProcStatusError(t, gone[i].err)
			target := testSignalTarget(cmd.Process.Pid, os.Getpid())
			if err := signalProcessGroup(target, "TERM"); err != nil {
				t.Fatalf("signalProcessGroup(status gone) = %v, want nil", err)
			}
			if err := killProcess(target); err != nil {
				t.Fatalf("killProcess(status gone) = %v, want nil", err)
			}
			if len(*rec) != 0 {
				t.Fatalf("signals = %+v, want none when the target's /proc entry is gone", *rec)
			}
			if !processExists(cmd.Process.Pid) {
				t.Fatalf("sleep pid %d exited, want it untouched", cmd.Process.Pid)
			}
		})
	}
}

// TestValidateSignalParentRefusesAStatusItCannotRead pins the other side: an error that
// does not say the process is gone refuses the signal and names the record.
func TestValidateSignalParentRefusesAStatusItCannotRead(t *testing.T) {
	rec := withRecordingSignaller(t)
	cmd := startSleep(t, true)
	withProcStatusError(t, &os.PathError{Op: "read", Path: "/proc/1/status", Err: unix.EIO})
	target := testSignalTarget(cmd.Process.Pid, os.Getpid())
	err := signalProcessGroup(target, "TERM")
	assertRefusedWithoutSignal(t, err, "input/output error", rec)
}
