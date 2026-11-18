//go:build linux

package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/tekmora/epitrop/internal/eventlog"
)

type blockedLineWriter struct {
	entered   chan<- struct{}
	release   <-chan struct{}
	done      chan<- struct{}
	failFirst bool
	calls     atomic.Int32
	buf       synchronizedFindingBuffer
}

func (w *blockedLineWriter) Write(p []byte) (int, error) {
	call := w.calls.Add(1)
	if w.entered != nil {
		select {
		case w.entered <- struct{}{}:
		default:
		}
	}
	if w.release != nil {
		<-w.release
	}
	defer func() {
		if w.done != nil {
			select {
			case w.done <- struct{}{}:
			default:
			}
		}
	}()
	if call == 1 && w.failFirst {
		return 0, errors.New("disk full")
	}
	return w.buf.Write(p)
}

func TestLineSinkHoldsSlotUntilWriteReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		w := &blockedLineWriter{release: release}
		sink := newLineSink(w)
		for i := 0; i < 2; i++ {
			pending, err := writeLine(t.Context(), sink, "finding", time.Second)
			if !errors.Is(err, ErrWriteTimedOut) || (i == 0) != (pending != nil) {
				t.Fatalf("write %d = %v, pending = %v; want ErrWriteTimedOut, admitted=%t", i, err, pending, i == 0)
			}
		}
		if got := w.calls.Load(); got != 1 {
			t.Errorf("Write calls before release = %d, want 1", got)
		}
		// Sending releases just the outstanding write; closing below allows the next write.
		release <- struct{}{}
		synctest.Wait()
		done := make(chan error, 1)
		go func() {
			_, err := writeLine(t.Context(), sink, "after release", time.Second)
			done <- err
		}()
		release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatalf("write after release = %v, want nil", err)
		}
	})
}

func TestLineSinkSerializesConcurrentLines(t *testing.T) {
	var buf synchronizedFindingBuffer
	sink := newLineSink(&buf)
	var wg sync.WaitGroup
	var failed atomic.Bool
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for line := 0; line < 50; line++ {
				if _, err := writeLine(t.Context(), sink, fmt.Sprintf("worker %d line %d", worker, line), defaultWriteTimeout); err != nil {
					failed.Store(true)
					t.Errorf("writeLine = %v, want nil", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if !failed.Load() {
		assertConcurrentLines(t, buf.String())
	}
}

func assertConcurrentLines(t *testing.T, text string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("lines = %d, want 400", len(lines))
	}
	seen := make(map[string]int, 400)
	for i := 0; i < len(lines); i++ {
		seen[lines[i]]++
	}
	for worker := 0; worker < 8; worker++ {
		for line := 0; line < 50; line++ {
			want := fmt.Sprintf("worker %d line %d", worker, line)
			if seen[want] != 1 {
				t.Fatalf("line %q count = %d, want 1 complete line", want, seen[want])
			}
		}
	}
}

func TestSupervisorDefaultSinkIsShared(t *testing.T) {
	first, second, explicit := &Supervisor{}, &Supervisor{}, &Supervisor{stderr: os.Stderr}
	if first.errOut() != defaultStderrSink || second.errOut() != defaultStderrSink || explicit.errOut() != defaultStderrSink {
		t.Fatal("os.Stderr sinks differ, want one package-level sink")
	}
}

func TestShimErrorDoesNotRetryDefaultSink(t *testing.T) {
	w := &retryFindingWriter{}
	old := defaultStderrSink
	defaultStderrSink = newLineSink(w)
	defer func() { defaultStderrSink = old }()
	reportShimError(defaultStderrSink, defaultWriteTimeout, "broken", errors.New("wait failed"))
	if w.calls != 1 {
		t.Fatalf("default stderr writes = %d, want 1 without fallback to the same sink", w.calls)
	}
}

func TestSuperviseCanceledDuringFindingWrite(t *testing.T) {
	sup := testSupervisor(t, testJob("broken", []string{"/bin/true"}))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	w := &blockedLineWriter{entered: entered, release: release}
	sup.stderr = w
	plantExit(t, sup, "broken", intPtr(1), "")
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Supervise(ctx) }()
	select {
	case <-entered:
		cancel()
	case err := <-done:
		t.Fatalf("Supervise ended before finding Write: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("Supervise did not reach finding Write within 10s")
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoJobAlive) {
			t.Fatalf("Supervise = %v, want context.Canceled without ErrNoJobAlive", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Supervise did not return promptly on cancel during finding Write")
	}
	assertCanceledWriteHoldsSlotUntilRelease(t, sup.errOut(), w, release)
}

func assertCanceledWriteHoldsSlotUntilRelease(t *testing.T, sink *lineSink, w *blockedLineWriter, release chan struct{}) {
	t.Helper()
	if pending, err := writeLine(testContext(t), sink, "waiting", 20*time.Millisecond); !errors.Is(err, ErrWriteTimedOut) || pending != nil {
		t.Fatalf("write while slot held = %v, pending = %v; want timeout before admission", err, pending)
	}
	if got := w.calls.Load(); got != 1 {
		t.Fatalf("Write calls while slot held = %d, want 1", got)
	}
	nextDone := make(chan error, 1)
	go func() {
		_, err := writeLine(testContext(t), sink, "after release", time.Second)
		nextDone <- err
	}()
	release <- struct{}{}
	release <- struct{}{}
	if err := <-nextDone; err != nil {
		t.Fatalf("write after release = %v, want nil", err)
	}
}

func TestRecoverableFindingPendingWrite(t *testing.T) {
	t.Run("admitted timeout then late success", testPendingWriteLateSuccess)
	t.Run("admitted timeout then late failure retries", testPendingWriteLateFailure)
	t.Run("slot never acquired retries", testPendingWriteSlotNeverAcquired)
}

func newPendingWriteSupervisor(t *testing.T, w *blockedLineWriter) (*Supervisor, eventlog.State) {
	t.Helper()
	job := budgetJob("window", 1, []string{"/bin/false"})
	job.Restart.WindowSeconds = 2
	sup := testSupervisor(t, job)
	sup.clk = clockwork.NewFakeClockAt(time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC))
	sup.stderr = w
	sup.writeTimeout = 20 * time.Millisecond
	plantSpentBudget(t, sup, job.Name, 1, intPtr(1), "")
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return sup, state
}

func testPendingWriteLateSuccess(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{}, 1)
	w := &blockedLineWriter{release: release, done: done}
	sup, state := newPendingWriteSupervisor(t, w)
	var episode findingEpisode
	checkFindingEpisode(t, sup, state, &episode)
	assertEpisodeInFlight(t, "tick 1", episode, w.calls.Load())
	checkFindingEpisode(t, sup, state, &episode)
	assertEpisodeInFlight(t, "tick 2 (still pending)", episode, w.calls.Load())
	close(release)
	<-done
	checkFindingEpisode(t, sup, state, &episode)
	if !episode.reported || episode.pending != nil || w.calls.Load() != 1 || strings.Count(w.buf.String(), "\n") != 1 {
		t.Fatalf("tick 3 (late success): episode = %+v, calls = %d, buf = %q; want reported once", episode, w.calls.Load(), w.buf.String())
	}
}

func assertEpisodeInFlight(t *testing.T, label string, episode findingEpisode, calls int32) {
	t.Helper()
	if episode.reported || episode.pending == nil || calls != 1 {
		t.Fatalf("%s: episode = %+v, calls = %d; want 1 in-flight write", label, episode, calls)
	}
}

func testPendingWriteLateFailure(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{}, 1)
	w := &blockedLineWriter{release: release, done: done, failFirst: true}
	sup, state := newPendingWriteSupervisor(t, w)
	var episode findingEpisode
	checkFindingEpisode(t, sup, state, &episode)
	assertEpisodeInFlight(t, "tick 1", episode, w.calls.Load())
	close(release)
	<-done
	checkFindingEpisode(t, sup, state, &episode)
	if !episode.reported || episode.pending != nil || w.calls.Load() != 2 || strings.Count(w.buf.String(), "\n") != 1 {
		t.Fatalf("tick 2 (retry): episode = %+v, calls = %d, buf = %q; want 2 calls and 1 line", episode, w.calls.Load(), w.buf.String())
	}
}

func testPendingWriteSlotNeverAcquired(t *testing.T) {
	w := &blockedLineWriter{}
	sup, state := newPendingWriteSupervisor(t, w)
	sup.errOut().slot <- struct{}{}
	var episode findingEpisode
	checkFindingEpisode(t, sup, state, &episode)
	if episode.reported || episode.pending != nil || w.calls.Load() != 0 {
		t.Fatalf("tick 1 (no slot): episode = %+v, calls = %d; want un-admitted timeout", episode, w.calls.Load())
	}
	<-sup.errOut().slot
	checkFindingEpisode(t, sup, state, &episode)
	if !episode.reported || w.calls.Load() != 1 || strings.Count(w.buf.String(), "\n") != 1 {
		t.Fatalf("tick 2 (slot free): episode = %+v, calls = %d, buf = %q; want 1 line", episode, w.calls.Load(), w.buf.String())
	}
}

func plantRunningOnce(t *testing.T, sup *Supervisor, name, since string) eventlog.State {
	t.Helper()
	cfg := shimConfig{Name: name, RecordPath: sup.recordPath(name), LogPath: sup.cfg.Jobs[0].LogPath}
	if err := writeRecord(cfg, 4242, since); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"state": string(StateRunning), "pid": 4242, "shim_pid": 4241, "since": since, "sandbox": "off"}
	if err := sup.appendJobEvent(testContext(t), "job.started", name, payload); err != nil {
		t.Fatal(err)
	}
	state, err := sup.replayState(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestNoJobAliveLostOnce(t *testing.T) {
	t.Run("lock free once and spent always is terminal finding", testLostOnceFinding)
	t.Run("held lock once stays alive", testHeldLockOnceAlive)
	t.Run("new start before confirm replay cancels finding", testLostOnceNewStartBeforeConfirm)
}

func testLostOnceFinding(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, budgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	sup.afterTick = cancel
	if err := sup.Supervise(ctx); !errors.Is(err, ErrNoJobAlive) {
		t.Fatalf("Supervise = %v, want ErrNoJobAlive", err)
	}
	want := "epitrop supervisor: finding: no job alive: manual dead (exit lost, restarts 0/0 in 0s), broken dead (exit 1, restarts 1/1 in 0s); supervise exits 3\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

func testHeldLockOnceAlive(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, budgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	lock, err := holdLock(sup.lockPath("manual"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeLockForShim(lock)
	episode := findingEpisode{reported: true}
	if err = sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" || episode.reported {
		t.Fatalf("checkNoJobAlive = %v, stderr = %q, reported = %t; want alive once job", err, stderr.String(), episode.reported)
	}
}

func testLostOnceNewStartBeforeConfirm(t *testing.T) {
	once := testJob("manual", []string{"/bin/true"})
	once.Schedule = "once"
	sup := testSupervisor(t, once, budgetJob("broken", 1, []string{"/bin/true"}))
	stderr := captureSupervisorStderr(sup)
	plantSpentBudget(t, sup, "broken", 1, intPtr(1), "")
	state := plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:00Z")
	sup.beforeConfirmReplay = func() {
		plantRunningOnce(t, sup, "manual", "2026-11-18T08:00:01Z")
	}
	episode := findingEpisode{reported: true, pending: make(chan error)}
	if err := sup.checkNoJobAlive(testContext(t), state, &episode); err != nil || stderr.String() != "" || episode.reported || episode.pending != nil {
		t.Fatalf("checkNoJobAlive = %v, stderr = %q, episode = %+v; want no finding and cleared episode", err, stderr.String(), episode)
	}
}
