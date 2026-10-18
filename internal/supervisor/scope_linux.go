//go:build linux

package supervisor

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	scopeNamePrefix = "epitrop-job-"

	// The shim waits this long for systemd-run to place the job: 400 polls, 25 ms apart.
	// Measured, a job is in its scope 26 ms after systemd-run starts, one poll, both on a
	// workstation (systemd 262) and on a hosted CI runner (systemd 259), where the move goes
	// through the system manager. The 10 s are a margin for a loaded host, not an expected
	// wait; the scope probe of the tests logs the measured time on every run, so a drift
	// shows in the CI log.
	scopeConfirmPolls    = 400
	scopeConfirmInterval = 25 * time.Millisecond
)

var (
	procCgroupReadFile = os.ReadFile
	procExeReadlink    = os.Readlink
	scopeConfirmSleep  = time.Sleep
	startingImage      = os.Executable
)

// newScopeName names the systemd scope of one job start. The shim asks systemd-run for
// exactly this unit and then looks for it in the job's cgroup path. A name of its own is
// what tells a job that entered its scope from one left where the shim started it: the
// shim's own cgroup may itself be a scope.
func newScopeName() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("sandbox: scope name: %w", err)
	}
	return scopeNamePrefix + hex.EncodeToString(raw[:]) + ".scope", nil
}

// confirmJobScope waits until the started process sits in the cgroup of its own scope, and
// says why when it does not. systemd-run execs the job only after the user manager placed
// it in the scope, so the shim reads three things in this order: whether the process has
// exited, which program it is, and its cgroup. A process in the scope is confirmed. One
// that has exited, or runs a third program, outside the scope is refused at once: the job
// would run without its memory, CPU and task limits. While it is still systemd-run the shim
// waits, up to scopeConfirmPolls. It also waits while the process still shows the shim's
// own program: right after the start the kernel may name the parent's program for a moment
// before the new one (#228).
func confirmJobScope(pid int, scope string, runner string) error {
	if scope == "" {
		return nil
	}
	starter := ownImage()
	cgroup := ""
	for i := 0; i < scopeConfirmPolls; i++ {
		gone, err := processGone(pid)
		if err != nil {
			return fmt.Errorf("sandbox: scope %s: %w", scope, err)
		}
		image := processImage(pid)
		if cgroup, err = processCgroup(pid); err != nil {
			return fmt.Errorf("sandbox: scope %s: %w", scope, err)
		}
		if cgroupInScope(cgroup, scope) {
			return nil
		}
		if gone {
			return fmt.Errorf("sandbox: job exited in cgroup %s before it entered scope %s", cgroup, scope)
		}
		if thirdProgram(image, runner, starter) {
			return fmt.Errorf("sandbox: job runs as %s in cgroup %s, outside scope %s", image, cgroup, scope)
		}
		scopeConfirmSleep(scopeConfirmInterval)
	}
	return fmt.Errorf("sandbox: job is still in cgroup %s, not in scope %s, after %s", cgroup, scope, scopeConfirmPolls*scopeConfirmInterval)
}

// ownImage returns the program of this process, or "" when that cannot be read. Without it
// every program but the runner counts as a third one.
func ownImage() string {
	image, err := startingImage()
	if err != nil {
		return ""
	}
	return image
}

// thirdProgram reports whether image is a program the process runs once it left both the
// shim and the runner. An unreadable image is none.
func thirdProgram(image string, runner string, starter string) bool {
	return image != "" && image != runner && image != starter
}

// cgroupInScope reports whether a cgroup path ends in the scope unit. The kernel appends
// " (deleted)" to the path of an exited process whose cgroup was already removed.
func cgroupInScope(cgroup string, scope string) bool {
	return path.Base(strings.TrimSuffix(cgroup, " (deleted)")) == scope
}

// processCgroup returns the cgroup v2 path of pid. A host without the unified hierarchy has
// no such entry, and no scope that could carry the job's limits.
func processCgroup(pid int) (string, error) {
	body, err := procCgroupReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", fmt.Errorf("read cgroup of pid %d: %w", pid, err)
	}
	lines := strings.Split(string(body), "\n")
	for i := 0; i < len(lines); i++ {
		if cgroup, ok := strings.CutPrefix(lines[i], "0::"); ok {
			return cgroup, nil
		}
	}
	return "", fmt.Errorf("pid %d has no cgroup v2 entry", pid)
}

// processImage returns the program pid runs, or "" when that cannot be read.
func processImage(pid int) string {
	image, err := procExeReadlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return ""
	}
	return image
}

// processGone reports whether pid has exited. For a child of this process the kernel's wait
// interface answers exactly. The State line of /proc does not: it shows a zombie also while a
// process with several threads execs, and when its first thread has ended while others run.
// For any other process only its absence counts as an exit, and a zombie with one thread
// left, which is a process its parent has not collected yet.
func processGone(pid int) (bool, error) {
	exited, child, err := childExitProbe(pid)
	if err != nil {
		return false, err
	}
	if child {
		return exited, nil
	}
	body, err := procStatusReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read status of pid %d: %w", pid, err)
	}
	status := string(body)
	state := procStatusField(status, "State:")
	dead := strings.HasPrefix(state, "Z") || strings.HasPrefix(state, "X")
	return dead && procStatusField(status, "Threads:") == "1", nil
}

// procStatusField returns the value of one line of /proc/<pid>/status, or "".
func procStatusField(body string, key string) string {
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		if value, ok := strings.CutPrefix(lines[i], key); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
