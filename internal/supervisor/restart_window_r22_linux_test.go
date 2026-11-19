//go:build linux

package supervisor

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tekmora/epitrop/internal/config"
	"golang.org/x/sys/unix"
)

type timedStopSignal struct {
	recordedSignal
	at time.Time
}

func r22Signaller(t *testing.T, cancel context.CancelFunc) *[]timedStopSignal {
	t.Helper()
	var records []timedStopSignal
	old := processSignaller
	processSignaller = func(pid int, sig unix.Signal) error {
		records = append(records, timedStopSignal{recordedSignal{pid, sig}, time.Now()})
		err := guardedKill(pid, sig, unix.Kill)
		if err == nil && sig == unix.SIGTERM {
			cancel()
		}
		return err
	}
	t.Cleanup(func() { processSignaller = old })
	return &records
}

func r22Stop(t *testing.T, mode string, grace int) ([]timedStopSignal, int, time.Duration) {
	t.Helper()
	job := testJob("r22-"+mode, []string{os.Args[0], "-test.run=TestHelperProcessJob"})
	job.LogPath = t.TempDir() + "/job.log"
	job.Stop = config.JobStopConfig{Signal: "TERM", GraceSeconds: grace}
	sup := testSupervisor(t, job)
	t.Setenv("EPITROP_TEST_JOB", mode)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(func() bool {
		b, err := os.ReadFile(job.LogPath)
		return err == nil && strings.Contains(string(b), mode+" ready")
	}) {
		t.Fatalf("helper %s did not become ready", mode)
	}
	pid := eventlogJob(t, sup, job.Name).PID
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	rec := r22Signaller(t, cancel)
	start := time.Now()
	err := sup.Stop(ctx, job.Name)
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("Stop = %v, want nil", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("context error = %v", ctx.Err())
	}
	if n := countEvents(t, sup.cfg.EventLog.Dir, "job.stopped"); n != 1 {
		t.Errorf("stopped events = %d, want 1", n)
	}
	if held, err := lockHeld(sup.lockPath(job.Name)); err != nil || held {
		t.Errorf("lock held = %v, %v", held, err)
	}
	return *rec, pid, elapsed
}

func TestR22StopCanceledAfterSignalKeepsGrace(t *testing.T) {
	rec, pid, elapsed := r22Stop(t, "term-delay", 2)
	if len(rec) != 1 || rec[0].recordedSignal != (recordedSignal{-pid, unix.SIGTERM}) {
		t.Fatalf("signals = %+v", rec)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("stop took %v, want at least 250ms", elapsed)
	}
}

func TestR22StopCanceledAfterSignalKillsOnlyAfterGrace(t *testing.T) {
	rec, pid, _ := r22Stop(t, "ignore-term", 1)
	if len(rec) != 2 || rec[0].recordedSignal != (recordedSignal{-pid, unix.SIGTERM}) || rec[1].recordedSignal != (recordedSignal{-pid, unix.SIGKILL}) {
		t.Fatalf("signals = %+v", rec)
	}
	if delay := rec[1].at.Sub(rec[0].at); delay < 950*time.Millisecond {
		t.Errorf("kill after %v, want at least 950ms", delay)
	}
}

func TestR22StopCanceledAtCommitSendsNothing(t *testing.T) {
	job := testJob("daemon", longJobCommand(t))
	sup := testSupervisor(t, job)
	if err := sup.Start(testContext(t), job.Name); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	old := waitStopTargetForStop
	waitStopTargetForStop = func(s *Supervisor, c context.Context, j config.JobConfig) (*heldLock, jobSignalTarget, error) {
		lock, target, err := old(s, c, j)
		cancel()
		return lock, target, err
	}
	t.Cleanup(func() { waitStopTargetForStop = old })
	signals := withRecordingSignaller(t)
	err := sup.Stop(ctx, job.Name)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "supervisor: stop daemon: ") {
		t.Fatalf("Stop = %v, want wrapped canceled", err)
	}
	if len(*signals) != 0 {
		t.Fatalf("signals = %v, want none", *signals)
	}
	if n := countEvents(t, sup.cfg.EventLog.Dir, "job.stopped"); n != 0 {
		t.Fatalf("stopped events = %d", n)
	}
	if held, err := lockHeld(sup.lockPath(job.Name)); err != nil || !held {
		t.Fatalf("lock held = %v, %v", held, err)
	}
}

func TestR22StopCommitTimeout(t *testing.T) {
	var zero time.Duration
	for _, grace := range []int{0, -1, config.MaxStopGraceSeconds} {
		job := config.JobConfig{Stop: config.JobStopConfig{GraceSeconds: grace}}
		boundedGrace := grace
		if boundedGrace < 0 {
			boundedGrace = 0
		}
		want := maxStopAttempts*(time.Duration(boundedGrace)*time.Second+time.Duration(maxStatusPolls+maxStopTargetPolls+1)*startPollInterval) + supervisorStopMarkTimeout
		got := stopCommitTimeout(job)
		if got != want {
			t.Errorf("grace %d: timeout = %v, want %v", grace, got, want)
		}
		if grace == 0 {
			zero = got
		}
		if grace == config.MaxStopGraceSeconds && got-zero != maxStopAttempts*300*time.Second {
			t.Errorf("maximum grace adds %v, want %v", got-zero, maxStopAttempts*300*time.Second)
		}
	}
}
