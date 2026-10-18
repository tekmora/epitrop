//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gofrs/flock"
	"golang.org/x/sys/unix"
)

// heldLock is a job's lock: flock(2) through gofrs/flock, on a file opened close-on-exec, so
// the job the shim starts does not inherit it and the lock ends with the shim.
type heldLock struct {
	lock *flock.Flock
}

// errLockBusy marks a lock another process holds.
var errLockBusy = errors.New("lock is held")

var (
	processSignaller    = unix.Kill
	processGroupID      = unix.Getpgid
	procStatusReadFile  = os.ReadFile
	procCmdlineReadFile = os.ReadFile
	shimProcessProbe    = unix.Kill
)

func platformSupported() error {
	return nil
}

func holdLock(path string) (*heldLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("supervisor: mkdir lock dir: %w", err)
	}
	lock := flock.New(path, flock.SetFlag(os.O_CREATE|os.O_RDWR), flock.SetPermissions(0o600))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("supervisor: open lock %s: %w", path, err)
	}
	if !locked {
		return nil, fmt.Errorf("supervisor: flock %s: %w", path, errLockBusy)
	}
	return &heldLock{lock: lock}, nil
}

func lockHeld(path string) (bool, error) {
	lock, err := holdLock(path)
	if err == nil {
		return false, lock.Close()
	}
	if errors.Is(err, errLockBusy) {
		return true, nil
	}
	return false, err
}

func prepareProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true, Pdeathsig: unix.SIGKILL}
}

// errProcessGone marks a signal target that no longer exists. There is nothing to signal,
// so callers treat it like the ESRCH the signal itself would return: done, nothing sent.
var errProcessGone = errors.New("process is gone")

func signalProcessGroup(target jobSignalTarget, sig string) error {
	signal, err := signalFromName(sig)
	if err != nil {
		return err
	}
	if err = validateSignalTarget(target); err != nil {
		if errors.Is(err, errProcessGone) {
			return nil
		}
		return err
	}
	if err = processSignaller(-target.PID, signal); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("supervisor: signal %s pid %d: %w", sig, target.PID, err)
	}
	return nil
}

func killProcess(target jobSignalTarget) error {
	if err := validateSignalTarget(target); err != nil {
		if errors.Is(err, errProcessGone) {
			return nil
		}
		return err
	}
	return killStartedProcessGroup(target.PID)
}

func killStartedProcessGroup(pid int) error {
	return signalStartedGroup(pid, unix.SIGKILL, "KILL")
}

// childExitProbe asks the kernel whether pid, a child of this process, has exited as a whole
// and waits to be collected, without collecting it. child is false for a process that is not
// a child of this one, or was collected already.
var childExitProbe = waitChildExited

func waitChildExited(pid int) (exited bool, child bool, err error) {
	var info unix.Siginfo
	for i := 0; i < maxWaitRetries; i++ {
		err = unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			break
		}
	}
	if errors.Is(err, unix.ECHILD) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("supervisor: wait state of pid %d: %w", pid, err)
	}
	// With nothing to report the kernel leaves the signal number 0.
	return info.Signo == int32(unix.SIGCHLD), true, nil
}

// awaitExitUncollected blocks until pid, a child of this process, has exited, and leaves it
// uncollected. Until it is collected its process id and its process group stay what they
// were, so the group can still be checked and signalled.
func awaitExitUncollected(pid int) error {
	for i := 0; i < maxWaitRetries; i++ {
		err := unix.Waitid(unix.P_PID, pid, nil, unix.WEXITED|unix.WNOWAIT, nil)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("supervisor: wait for pid %d: %w", pid, err)
		}
	}
	return fmt.Errorf("supervisor: wait for pid %d: interrupted %d times", pid, maxWaitRetries)
}

// maxWaitRetries bounds how often a wait interrupted by a signal is taken up again.
const maxWaitRetries = 1 << 20

// signalStartedProcessGroup sends the named stop signal to the process group of a job the
// shim started itself. A group that is gone is no error.
func signalStartedProcessGroup(pid int, name string) error {
	sig, err := signalFromName(name)
	if err != nil {
		return err
	}
	return signalStartedGroup(pid, sig, name)
}

func signalStartedGroup(pid int, sig unix.Signal, name string) error {
	if err := validateStartedProcessGroup(pid); err != nil {
		if errors.Is(err, errProcessGone) {
			return nil
		}
		return err
	}
	if err := processSignaller(-pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("supervisor: SIG%s pid %d: %w", name, pid, err)
	}
	return nil
}

func validateStartedProcessGroup(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("supervisor: refuse signal: pid %d is not safe", pid)
	}
	if pid == os.Getpid() || pid == unix.Getpgrp() {
		return fmt.Errorf("supervisor: refuse signal: pid %d targets supervisor", pid)
	}
	pgid, err := processGroupID(pid)
	if errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("supervisor: pid %d: %w", pid, errProcessGone)
	}
	if err != nil {
		return fmt.Errorf("supervisor: refuse signal: getpgid %d: %w", pid, err)
	}
	if pgid != pid {
		return fmt.Errorf("supervisor: refuse signal: pid %d is not a process-group leader", pid)
	}
	return nil
}

func validateSignalTarget(target jobSignalTarget) error {
	if err := validateStartedProcessGroup(target.PID); err != nil {
		return signalRecordError(target, err)
	}
	if target.PID == target.ShimPID {
		return signalRecordReason(target, "pid equals shim_pid")
	}
	if err := validateSignalParent(target); err != nil {
		return err
	}
	return validateSignalShim(target)
}

func validateSignalParent(target jobSignalTarget) error {
	parent, err := linuxProcessParent(target.PID)
	// A process that ends between the open and the read of its status file gives ESRCH,
	// not a missing file.
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return signalRecordError(target, fmt.Errorf("pid %d: %w", target.PID, errProcessGone))
	}
	if err != nil {
		return signalRecordError(target, err)
	}
	if parent != target.ShimPID {
		return signalRecordReason(target, fmt.Sprintf("parent pid %d does not match shim_pid %d", parent, target.ShimPID))
	}
	return nil
}

func validateSignalShim(target jobSignalTarget) error {
	if err := shimProcessProbe(target.ShimPID, 0); err != nil {
		return signalRecordError(target, fmt.Errorf("shim_pid %d is not alive: %w", target.ShimPID, err))
	}
	held, err := lockHeld(target.LockPath)
	if err != nil {
		return signalRecordError(target, err)
	}
	if !held {
		return signalRecordReason(target, "job lock is not held")
	}
	reason, err := shimCmdlineMismatch(target)
	if err != nil {
		return signalRecordError(target, err)
	}
	if reason != "" {
		return signalRecordReason(target, reason)
	}
	return nil
}

func linuxProcessParent(pid int) (int, error) {
	body, err := procStatusReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, err
	}
	parent := procStatusField(string(body), "PPid:")
	if parent == "" {
		return 0, fmt.Errorf("PPid missing for %d", pid)
	}
	return strconv.Atoi(parent)
}

// shimCmdlineMismatch reads the shim's command line once and returns why it is not this
// job's shim, or "" when it is. An unreadable or empty command line (a zombie or a kernel
// thread) is an error, never a match.
func shimCmdlineMismatch(target jobSignalTarget) (string, error) {
	body, err := procCmdlineReadFile(filepath.Join("/proc", strconv.Itoa(target.ShimPID), "cmdline"))
	if err != nil {
		return "", err
	}
	// strings.Split always returns at least one element; it is "" only for an empty line.
	args := strings.Split(strings.TrimRight(string(body), "\x00"), "\x00")
	if args[0] == "" {
		return "", fmt.Errorf("cmdline of shim_pid %d is empty", target.ShimPID)
	}
	if !hasArg(args, "__job-shim") {
		return "__job-shim missing", nil
	}
	if !hasArgPair(args, "--name", target.Name) {
		return "--name mismatch", nil
	}
	if !hasArgPair(args, "--lock", target.LockPath) {
		return "--lock mismatch", nil
	}
	if !hasArgPair(args, "--record", target.RecordPath) {
		return "--record mismatch", nil
	}
	return "", nil
}

func hasArg(args []string, want string) bool {
	for i := 0; i < len(args); i++ {
		if args[i] == want {
			return true
		}
	}
	return false
}

func hasArgPair(args []string, key string, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func signalRecordError(target jobSignalTarget, err error) error {
	return fmt.Errorf("supervisor: record %s: refuse signal for %s: %w", target.RecordPath, target.Name, err)
}

func signalRecordReason(target jobSignalTarget, reason string) error {
	return fmt.Errorf("supervisor: record %s: refuse signal for %s: %s", target.RecordPath, target.Name, reason)
}

func signalFromName(name string) (unix.Signal, error) {
	switch name {
	case "TERM":
		return unix.SIGTERM, nil
	case "INT":
		return unix.SIGINT, nil
	case "HUP":
		return unix.SIGHUP, nil
	}
	return 0, fmt.Errorf("supervisor: unsupported signal %q", name)
}

func (l *heldLock) Close() error {
	if err := l.lock.Unlock(); err != nil {
		return fmt.Errorf("supervisor: unlock %s: %w", l.lock.Path(), err)
	}
	return nil
}
