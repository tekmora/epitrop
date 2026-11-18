//go:build linux

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type blockedLineWriter struct {
	release <-chan struct{}
	calls   atomic.Int32
}

func (w *blockedLineWriter) Write(p []byte) (int, error) {
	w.calls.Add(1)
	<-w.release
	return len(p), nil
}

func TestLineSinkHoldsSlotUntilWriteReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		w := &blockedLineWriter{release: release}
		sink := newLineSink(w)
		for i := 0; i < 2; i++ {
			if err := writeLine(sink, "finding", time.Second); !errors.Is(err, ErrWriteTimedOut) {
				t.Fatalf("write %d = %v, want ErrWriteTimedOut", i, err)
			}
		}
		if got := w.calls.Load(); got != 1 {
			t.Errorf("Write calls before release = %d, want 1", got)
		}
		// Sending releases just the outstanding write; closing below allows the next write.
		release <- struct{}{}
		synctest.Wait()
		done := make(chan error, 1)
		go func() { done <- writeLine(sink, "after release", time.Second) }()
		release <- struct{}{}
		if err := <-done; err != nil {
			t.Fatalf("write after release = %v, want nil", err)
		}
	})
}

func TestLineSinkSerializesConcurrentLines(t *testing.T) {
	var buf bytes.Buffer
	sink := newLineSink(&buf)
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for line := 0; line < 50; line++ {
				if err := writeLine(sink, fmt.Sprintf("worker %d line %d", worker, line), defaultWriteTimeout); err != nil {
					t.Errorf("writeLine = %v, want nil", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	assertConcurrentLines(t, buf.String())
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
