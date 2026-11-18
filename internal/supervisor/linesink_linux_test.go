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
	if call == 1 && w.failFirst {
		return 0, errors.New("disk full")
	}
	return w.buf.Write(p)
}

func TestLineSinkHoldsSlotUntilWriteReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		entered := make(chan struct{}, 1)
		w := &blockedLineWriter{release: release, entered: entered}
		sink := newLineSink(w)
		for i := 0; i < 2; i++ {
			pending, err := writeLine(t.Context(), sink, "finding", time.Second)
			if !errors.Is(err, ErrWriteTimedOut) || (i == 0) != (pending != nil) {
				t.Fatalf("write %d = %v, pending = %v; want ErrWriteTimedOut, admitted=%t", i, err, pending, i == 0)
			}
		}
		select {
		case <-entered:
		default:
			t.Fatal("first Write did not enter before timeout")
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

type gatedFindingWriter struct {
	buf      *synchronizedFindingBuffer
	inFlight atomic.Int32
	overlap  atomic.Bool
	first    atomic.Bool
	second   chan struct{}
	once     sync.Once
}

func (w *gatedFindingWriter) Write(p []byte) (int, error) {
	if w.inFlight.Add(1) > 1 {
		w.overlap.Store(true)
		w.once.Do(func() { close(w.second) })
	}
	defer w.inFlight.Add(-1)
	if w.first.CompareAndSwap(false, true) {
		select {
		case <-w.second:
		case <-time.After(200 * time.Millisecond):
		}
	}
	return w.buf.Write(p)
}

func TestLineSinkSerializesConcurrentLines(t *testing.T) {
	var buf synchronizedFindingBuffer
	w := &gatedFindingWriter{buf: &buf, second: make(chan struct{})}
	sink := newLineSink(w)
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
	if w.overlap.Load() {
		t.Fatal("concurrent Write overlap")
	}
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
	select {
	case release <- struct{}{}:
	case err := <-nextDone:
		t.Fatalf("write ended before admission: %v", err)
	}
	if err := <-nextDone; err != nil {
		t.Fatalf("write after release = %v, want nil", err)
	}
}

func TestRecoverableFindingPendingWrite(t *testing.T) {
	t.Run("admitted timeout then late success", testPendingWriteLateSuccess)
	t.Run("admitted timeout then late failure retries", testPendingWriteLateFailure)
	t.Run("slot never acquired retries", testPendingWriteSlotNeverAcquired)
	t.Run("partial write retry stands alone", testPartialWriteRetry)
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
	entered := make(chan struct{}, 1)
	var releaseOnce sync.Once
	releaseWriter := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWriter)
	w := &blockedLineWriter{release: release, entered: entered}
	sup, state := newPendingWriteSupervisor(t, w)
	var episode findingEpisode
	checkFindingEpisode(t, sup, state, &episode)
	awaitWriteEntry(t, entered)
	assertEpisodeInFlight(t, "tick 1", episode, w.calls.Load())
	checkFindingEpisode(t, sup, state, &episode)
	assertEpisodeInFlight(t, "tick 2 (still pending)", episode, w.calls.Load())
	releaseWriter()
	awaitPendingResult(t, &episode)
	checkFindingEpisode(t, sup, state, &episode)
	if !episode.reported || episode.pending != nil || w.calls.Load() != 1 || strings.Count(w.buf.String(), "\n") != 1 {
		t.Fatalf("tick 3 (late success): episode = %+v, calls = %d, buf = %q; want reported once", episode, w.calls.Load(), w.buf.String())
	}
}

func awaitWriteEntry(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("Write did not enter within 10s")
	}
}

func assertEpisodeInFlight(t *testing.T, label string, episode findingEpisode, calls int32) {
	t.Helper()
	if episode.reported || episode.pending == nil || calls != 1 {
		t.Fatalf("%s: episode = %+v, calls = %d; want 1 in-flight write", label, episode, calls)
	}
}

func awaitPendingResult(t *testing.T, episode *findingEpisode) {
	t.Helper()
	select {
	case err := <-episode.pending:
		ready := make(chan error, 1)
		ready <- err
		episode.pending = ready
	case <-time.After(10 * time.Second):
		t.Fatal("pending write did not finish within 10s")
	}
}

func testPendingWriteLateFailure(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var releaseOnce sync.Once
	releaseWriter := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWriter)
	w := &blockedLineWriter{release: release, entered: entered, failFirst: true}
	sup, state := newPendingWriteSupervisor(t, w)
	var episode findingEpisode
	checkFindingEpisode(t, sup, state, &episode)
	awaitWriteEntry(t, entered)
	assertEpisodeInFlight(t, "tick 1", episode, w.calls.Load())
	releaseWriter()
	awaitPendingResult(t, &episode)
	sup.writeTimeout = defaultWriteTimeout
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
	sup.writeTimeout = defaultWriteTimeout
	checkFindingEpisode(t, sup, state, &episode)
	if !episode.reported || w.calls.Load() != 1 || strings.Count(w.buf.String(), "\n") != 1 {
		t.Fatalf("tick 2 (slot free): episode = %+v, calls = %d, buf = %q; want 1 line", episode, w.calls.Load(), w.buf.String())
	}
}

type partialLineWriter struct {
	buf   strings.Builder
	calls int
}

func (w *partialLineWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == 1 {
		n, err := w.buf.Write(p[:8])
		if err != nil {
			return n, err
		}
		return n, errors.New("partial write")
	}
	return w.buf.Write(p)
}

func testPartialWriteRetry(t *testing.T) {
	w := &partialLineWriter{}
	sink := newLineSink(w)
	line := "epitrop supervisor: finding: no job alive: example"
	if _, err := writeLine(testContext(t), sink, line, time.Second); err == nil {
		t.Fatal("first write succeeded")
	}
	for i := 0; i < 2; i++ {
		if _, err := writeLine(testContext(t), sink, line, time.Second); err != nil {
			t.Fatalf("write %d after the partial write = %v", i+2, err)
		}
	}
	want := line[:8] + "\n" + line + "\n" + line + "\n"
	if got := w.buf.String(); got != want {
		t.Fatalf("sink output = %q, want %q (retry on its own line, no blank line after it)", got, want)
	}
}

type terminalPartialWriter struct {
	n   int
	buf strings.Builder
}

func (w *terminalPartialWriter) Write(p []byte) (int, error) {
	n, err := w.buf.Write(p[:w.n])
	if err != nil {
		return n, err
	}
	return n, errors.New("write failed")
}

func TestTerminalFindingPartialLine(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n       int
		partial bool
	}{{"partial", 8, true}, {"empty", 0, false}} {
		t.Run(tc.name, func(t *testing.T) {
			sup := testSupervisor(t, testJob("broken", []string{"/bin/true"}))
			sup.stderr = &terminalPartialWriter{n: tc.n}
			plantExit(t, sup, "broken", intPtr(1), "")
			err := sup.Supervise(testContext(t))
			if !errors.Is(err, ErrNoJobAlive) || !errors.Is(err, ErrFindingLineNotWritten) || errors.Is(err, ErrPartialLine) != tc.partial {
				t.Fatalf("Supervise = %v, partial = %t; want %t", err, errors.Is(err, ErrPartialLine), tc.partial)
			}
		})
	}
}
