//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/tekmora/epitrop/internal/eventlog"
	"golang.org/x/sys/unix"
)

func TestStopAutomaticRestartWithDeadRecord(t *testing.T) {
	job := stopJob("always", 1, 0, longJobCommand(t))
	sup, _ := stopSupervisor(t, job)
	seedInterruptedAlwaysJob(t, sup, "2026-11-18T08:00:00Z")
	oldPID, oldShim := exitedShimPID(t), exitedShimPID(t)
	writeStaleRecord(t, sup, job.Name, oldPID, oldShim)
	reconcileOnce(t, sup, "restart with dead record")
	fresh, ok := readJobRecord(sup.recordPath(job.Name))
	if !ok || overlapsOldPIDs(fresh.PID, fresh.ShimPID, oldPID, oldShim) {
		t.Fatalf("fresh record = %+v, valid %t; old PIDs %d/%d", fresh, ok, oldPID, oldShim)
	}
	held, err := lockHeld(sup.lockPath(job.Name))
	if err != nil || !held {
		t.Fatalf("lockHeld = %t, %v, want held", held, err)
	}
	starts := countEvents(t, sup.cfg.EventLog.Dir, "job.started")
	signals := withPassthroughSignaller(t)
	if err := sup.Stop(testContext(t), job.Name); err != nil {
		t.Fatalf("Stop(automatic restart) = %v, want nil", err)
	}
	assertFreshSignals(t, *signals, fresh.PID)
	if got := eventlogJob(t, sup, job.Name); got.LastEvent != "job.stopped" {
		t.Fatalf("last event = %s, want job.stopped", got.LastEvent)
	}
	if got := countEvents(t, sup.cfg.EventLog.Dir, "job.started"); got != starts {
		t.Fatalf("job.started count = %d, want %d", got, starts)
	}
}

func overlapsOldPIDs(pid, shim, oldPID, oldShim int) bool {
	return pid == oldPID || pid == oldShim || shim == oldPID || shim == oldShim
}

func assertFreshSignals(t *testing.T, signals []recordedSignal, pid int) {
	t.Helper()
	if len(signals) == 0 {
		t.Fatal("Stop sent no signal to the fresh run")
	}
	for i := 0; i < len(signals); i++ {
		if signals[i].pid != -pid {
			t.Fatalf("signals = %+v, want only fresh PID %d", signals, pid)
		}
	}
}

func TestStopWaitingAutomaticShimReplaysStopped(t *testing.T) {
	job := stopJob("always", 1, 0, longJobCommand(t))
	sup, _ := stopSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatal(err)
	}
	running, ok := readJobRecord(sup.recordPath(job.Name))
	if !ok {
		t.Fatal("running record missing")
	}
	cmd := startWaitingShim(t, sup, job.Name)
	heldDuringAppend := resumeAfterStoppedAppend(t, sup, cmd)
	reads := 0
	oldRead := jobRecordReadFile
	jobRecordReadFile = func(path string) ([]byte, error) {
		reads++
		return oldRead(path)
	}
	t.Cleanup(func() { jobRecordReadFile = oldRead })
	if err := sup.Stop(testContext(t), job.Name); err != nil {
		t.Fatalf("Stop(running job with waiting shim) = %v", err)
	}
	if reads == 0 {
		t.Fatal("Stop did not read the running record")
	}
	if !*heldDuringAppend {
		t.Fatal("job.stopped appended without the job lock")
	}
	assertShimStoppedExit(t, cmd)
	if got := eventlogJob(t, sup, job.Name); got.State != string(StateStopped) || got.LastEvent != "job.stopped" {
		t.Fatalf("job after stop = %+v, want stopped", got)
	}
	if fresh, ok := readJobRecord(sup.recordPath(job.Name)); !ok || fresh.PID != running.PID {
		t.Fatalf("record after stop = %+v, valid %t, want original run", fresh, ok)
	}
}

func assertShimStoppedExit(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	var exitErr *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exitErr) || exitErr.ExitCode() != shimExitStopped {
		t.Fatalf("waiting shim Wait = %v, want exit %d", err, shimExitStopped)
	}
}

func startWaitingShim(t *testing.T, sup *Supervisor, name string) *exec.Cmd {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Close(fd); err != nil {
			t.Error(err)
		}
	})
	if _, err := unix.InotifyAddWatch(fd, sup.lockPath(name), unix.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	job, err := sup.job(name)
	if err != nil {
		t.Fatal(err)
	}
	cmd := sup.shimCmdFor(testContext(t), job, true, nil)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			if err := cmd.Process.Kill(); err != nil {
				t.Error(err)
			}
			if err := cmd.Wait(); err == nil {
				t.Error("killed shim unexpectedly succeeded")
			}
		}
	})
	waitShimLockOpen(t, fd, cmd.Process.Pid)
	return cmd
}

func waitShimLockOpen(t *testing.T, fd int, pid int) {
	t.Helper()
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 15000); err != nil || n != 1 || fds[0].Revents&unix.POLLIN == 0 {
		t.Fatalf("shim lock open: poll = %d, %v, events %x", n, err, fds[0].Revents)
	}
	if err := guardedKill(pid, unix.SIGSTOP, unix.Kill); err != nil {
		t.Fatal(err)
	}
	var status unix.WaitStatus
	if got, err := unix.Wait4(pid, &status, unix.WUNTRACED, nil); err != nil || got != pid || !status.Stopped() {
		t.Fatalf("wait for shim suspension = %d, %v, %v", got, status, err)
	}
}

func resumeAfterStoppedAppend(t *testing.T, sup *Supervisor, cmd *exec.Cmd) *bool {
	t.Helper()
	realAppend := appendEvent
	held := false
	appendEvent = func(w *eventlog.Writer, ctx context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		rec, err := realAppend(w, ctx, check)
		if err == nil && rec.Type == "job.stopped" {
			var lockErr error
			held, lockErr = lockHeld(sup.lockPath("daemon"))
			if lockErr != nil {
				t.Errorf("check job lock: %v", lockErr)
			}
			if err = guardedKill(cmd.Process.Pid, unix.SIGCONT, unix.Kill); err != nil {
				t.Errorf("resume waiting shim: %v", err)
			}
		}
		return rec, err
	}
	t.Cleanup(func() { appendEvent = realAppend })
	return &held
}
