//go:build linux

package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/eventlog"
)

func TestR15JoinedSkipPreservesCloseError(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	original := appendEvent
	defer func() { appendEvent = original }()
	closeErr := errors.New("close failed")
	const skipCases = 2
	cases := []error{errSkipJobEvent, errors.Join(errSkipJobEvent, closeErr)}
	for i := 0; i < skipCases; i++ {
		err := cases[i]
		appendEvent = func(*eventlog.Writer, context.Context, func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
			return eventlog.Record{}, err
		}
		appended, got := sup.appendJobEventIf(testContext(t), "job.test", "daemon", nil, nil)
		if appended || (i == 0 && got != nil) || (i == 1 && !errors.Is(got, closeErr)) {
			t.Fatalf("append with %v = %t, %v", err, appended, got)
		}
	}
}

func TestR15StopWaitsForRecordlessShim(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	lock, err := holdLock(sup.lockPath("daemon"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	passedOldBound := make(chan struct{})
	oldRead := jobRecordReadFile
	reads := 0
	jobRecordReadFile = func(path string) ([]byte, error) {
		reads++
		if reads == maxStatusPolls+1 {
			close(passedOldBound)
		}
		return oldRead(path)
	}
	defer func() { jobRecordReadFile = oldRead }()
	ctx, cancel := context.WithTimeout(t.Context(), testWaitBound)
	defer cancel()
	go func() { done <- sup.Stop(ctx, "daemon") }()
	select {
	case err = <-done:
		closeLockForShim(lock)
		t.Fatalf("Stop returned before lock release: %v", err)
	case <-passedOldBound:
	case <-ctx.Done():
		closeLockForShim(lock)
		if stopErr := <-done; stopErr == nil {
			t.Error("Stop succeeded after timeout")
		}
		t.Fatal("recordless wait timed out")
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if got := eventlogJob(t, sup, "daemon"); got.LastEvent != "job.stopped" {
		t.Fatalf("job = %+v", got)
	}
	assertRecordlessStopCancellation(t, sup, oldRead)
}

func assertRecordlessStopCancellation(t *testing.T, sup *Supervisor, oldRead func(string) ([]byte, error)) {
	t.Helper()
	lock, err := holdLock(sup.lockPath("daemon"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockForShim(lock)
	short, stop := context.WithCancel(testContext(t))
	defer stop()
	jobRecordReadFile = func(path string) ([]byte, error) { stop(); return oldRead(path) }
	if err = sup.Stop(short, "daemon"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Stop canceled = %v", err)
	}
}

func TestR15MarkedStaleRecordNotAdopted(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	pid := exitedShimPID(t)
	since := "2026-11-18T08:00:00Z"
	appendHistory(t, sup, "job.started", "daemon", map[string]any{"state": "running", "pid": pid + 1, "shim_pid": pid, "since": since})
	appendHistory(t, sup, eventSupervisorStopped, "daemon", map[string]any{"supervisor_pid": 1, "since": since})
	lock := holdRunningRecordForShim(t, sup, "daemon", pid, since)
	record := fmt.Sprintf(`{"pid":%d,"shim_pid":%d,"started_at":%q}`, pid+1, pid, since)
	if err := os.WriteFile(sup.recordPath("daemon"), []byte(record), 0o600); err != nil {
		closeLockForShim(lock)
		t.Fatal(err)
	}
	reconcileOnce(t, sup, "stale marked record")
	if got := eventlogJob(t, sup, "daemon"); !got.SupervisorStopped || countEvents(t, sup.cfg.EventLog.Dir, "job.adopted") != 0 {
		closeLockForShim(lock)
		t.Fatalf("adopted stale record: %+v", got)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, sup, "released stale record")
	assertInterruptedState(t, eventlogJob(t, sup, "daemon"))
}

func TestR15RestartRejectsSupersededRun(t *testing.T) {
	originalStart := automaticShimStart
	starts := 0
	automaticShimStart = func(stage string) {
		if stage == "before" {
			starts++
		}
	}
	t.Cleanup(func() { automaticShimStart = originalStart })
	job := stopJob("on-failure", 2, 0, longJobCommand(t))
	sup, _ := stopSupervisor(t, job)
	appendHistory(t, sup, "job.started", job.Name, map[string]any{"state": "running", "pid": 10, "shim_pid": 11, "since": "a"})
	appendHistory(t, sup, "job.exited", job.Name, map[string]any{"state": "dead", "code": 1})
	st := eventlogJob(t, sup, job.Name)
	appendHistory(t, sup, "job.started", job.Name, map[string]any{"state": "running", "pid": 12, "shim_pid": 13, "since": "b"})
	appendHistory(t, sup, "job.exited", job.Name, map[string]any{"state": "dead", "code": 0})
	if err := sup.restartJob(testContext(t), job, st); err != nil {
		t.Fatal(err)
	}
	if starts != 0 {
		t.Fatalf("superseded restart attempted %d shim starts", starts)
	}
	if got := eventlogJob(t, sup, job.Name); got.Restarts != st.Restarts || got.Since != "b" || got.LastEvent != "job.exited" {
		t.Fatalf("superseded restart = %+v", got)
	}
}

func TestR15MarkedShimMatches(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	live, exited := os.Getpid(), exitedShimPID(t)
	cases := []struct {
		name   string
		job    eventlog.Job
		status JobStatus
		want   bool
	}{
		{"matching unrelated live", eventlog.Job{Since: "a", ShimPID: live}, JobStatus{Since: "a", ShimPID: live}, false},
		{"different since", eventlog.Job{Since: "a", ShimPID: live}, JobStatus{Since: "b", ShimPID: live}, false},
		{"different pid", eventlog.Job{Since: "a", ShimPID: live}, JobStatus{Since: "a", ShimPID: exited}, false},
		{"exited", eventlog.Job{Since: "a", ShimPID: exited}, JobStatus{Since: "a", ShimPID: exited}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sup.markedShimMatches(tc.job, tc.status); got != tc.want {
				t.Fatalf("markedShimMatches = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestR15SameRestartEnd(t *testing.T) {
	one, otherOne, two := 1, 1, 2
	base := eventlog.Job{LastEvent: "job.exited", State: "dead", Since: "a", ShimPID: 12, LastReason: "exit", LastExitCode: &one}
	changed := func(edit func(*eventlog.Job)) eventlog.Job {
		job := base
		edit(&job)
		return job
	}
	noCode := changed(func(j *eventlog.Job) { j.LastExitCode = nil })
	cases := []struct {
		name string
		a, b eventlog.Job
		want bool
	}{
		{"identical", base, base, true},
		{"both nil", noCode, noCode, true},
		{"nil then one", noCode, base, false},
		{"one then nil", base, noCode, false},
		{"distinct equal codes", base, changed(func(j *eventlog.Job) { j.LastExitCode = &otherOne }), true},
		{"different codes", base, changed(func(j *eventlog.Job) { j.LastExitCode = &two }), false},
		{"event", base, changed(func(j *eventlog.Job) { j.LastEvent = "other" }), false},
		{"state", base, changed(func(j *eventlog.Job) { j.State = "other" }), false},
		{"since", base, changed(func(j *eventlog.Job) { j.Since = "b" }), false},
		{"shim pid", base, changed(func(j *eventlog.Job) { j.ShimPID++ }), false},
		{"reason", base, changed(func(j *eventlog.Job) { j.LastReason = "other" }), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameRestartEnd(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameRestartEnd = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestR15ShimStoppedBeforeStart(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	cases := []struct {
		name   string
		ctx    context.Context
		result *shimStartResult
		want   bool
	}{
		{"nil", canceled, nil, false},
		{"stopped", t.Context(), &shimStartResult{done: closed, stopped: true}, true},
		{"not stopped", t.Context(), &shimStartResult{done: closed}, false},
		{"canceled before completion", canceled, &shimStartResult{done: make(chan struct{}), stopped: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shimStoppedBeforeStart(tc.ctx, tc.result); got != tc.want {
				t.Fatalf("shimStoppedBeforeStart = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestR15StopTargetRefusedRunResets(t *testing.T) {
	job := stopJob("always", 1, 0, longJobCommand(t))
	sup, _ := stopSupervisor(t, job)
	pid := exitedShimPID(t)
	body, err := json.Marshal(jobRecord{Name: job.Name, PID: pid, ShimPID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := holdLock(sup.lockPath(job.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	oldRead, reads := jobRecordReadFile, 0
	jobRecordReadFile = func(string) ([]byte, error) {
		reads++
		if reads == 2*maxStatusPolls+50 {
			cancel()
		}
		if reads%2 == 1 {
			return nil, fs.ErrNotExist
		}
		return body, nil
	}
	t.Cleanup(func() { jobRecordReadFile = oldRead })
	gotLock, _, err := sup.waitStopTarget(ctx, job)
	if gotLock != nil {
		if closeErr := gotLock.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, context.Canceled) || reads <= 2*maxStatusPolls {
		t.Fatalf("waitStopTarget = %v after %d reads, want cancellation after alternating polls", err, reads)
	}
}

func TestR15StoppedWaiterSurvivesReplacement(t *testing.T) {
	sup, _ := stopSupervisor(t, stopJob("always", 1, 0, longJobCommand(t)))
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `exit "$1"`, "sh", strconv.Itoa(shimExitStopped))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	sup.recordSpawnedShim("daemon", pid)
	done := sup.spawnedShim["daemon"].done
	result := &shimStartResult{done: make(chan struct{})}
	sup.shimWaiters.Add(1)
	go sup.collectShimFor(cmd, "daemon", result)
	select {
	case <-done:
	case <-time.After(testWaitBound):
		t.Fatal("collector timed out")
	}
	finished := sup.finishedShims("daemon")
	sup.recordSpawnedShimFor("daemon", pid+1, 0, finished)
	if err := sup.waitStartedFor(testContext(t), "daemon", eventlog.Job{}, pid, true, result); !errors.Is(err, ErrStoppedBeforeStart) {
		t.Fatalf("waitStarted = %v", err)
	}
}

func TestR15RestartAcceptsUnchangedRun(t *testing.T) {
	job := stopJob("on-failure", 2, 1, longJobCommand(t))
	sup, _ := stopSupervisor(t, job)
	appendHistory(t, sup, "job.started", job.Name, map[string]any{"state": "running", "pid": 10, "shim_pid": 11, "since": "a"})
	appendHistory(t, sup, "job.exited", job.Name, map[string]any{"state": "dead", "code": 1})
	st := eventlogJob(t, sup, job.Name)
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	original := appendEvent
	appendEvent = func(w *eventlog.Writer, c context.Context, check func(context.Context) (eventlog.Event, error)) (eventlog.Record, error) {
		rec, err := original(w, c, check)
		if err == nil && rec.Type == "job.restarted" {
			cancel()
		}
		return rec, err
	}
	defer func() { appendEvent = original }()
	if err := sup.restartJob(ctx, job, st); !errors.Is(err, context.Canceled) {
		t.Fatalf("restartJob = %v, want cancellation after append", err)
	}
	if got := eventlogJob(t, sup, job.Name); got.LastEvent != "job.restarted" || got.Restarts != st.Restarts+1 {
		t.Fatalf("unchanged restart = %+v", got)
	}
}
