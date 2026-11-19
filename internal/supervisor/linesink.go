package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrWriteTimedOut marks a line write that passed its deadline; the write may still be
// pending on that writer.
var ErrWriteTimedOut = errors.New("supervisor: stderr write timeout")

// ErrPartialLine marks a failed write that left part of its line on the writer.
var ErrPartialLine = errors.New("supervisor: partial line")

// lineSink permits at most one outstanding write, including a write whose caller timed out.
type lineSink struct {
	writer  io.Writer
	slot    chan struct{}
	partial bool
}

var defaultStderrSink = newLineSink(os.Stderr)

func newLineSink(w io.Writer) *lineSink {
	return &lineSink{writer: w, slot: make(chan struct{}, 1)}
}

// errOut resolves once, including for Supervisor literals built without New.
func (s *Supervisor) errOut() *lineSink {
	s.stderrOnce.Do(func() {
		s.stderrSink = defaultStderrSink
		if s.stderr != nil && s.stderr != io.Writer(os.Stderr) {
			s.stderrSink = newLineSink(s.stderr)
		}
	})
	return s.stderrSink
}

// writeLine uses one deadline for admission and output. Only the writer releases its slot:
// an uninterruptible Write can outlive the caller, but cannot spawn more stalled writes.
// When an admitted write times out or its caller's context ends, writeLine returns the
// write's result channel so the episode can track completion across ticks.
func writeLine(ctx context.Context, sink *lineSink, line string, timeout time.Duration) (<-chan error, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("supervisor: stderr write: %w", err)
	}
	if timeout <= 0 {
		timeout = defaultWriteTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case sink.slot <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("supervisor: stderr write: %w", ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("%w after %s", ErrWriteTimedOut, timeout)
	}
	done := make(chan error, 1)
	go sink.writeAdmitted(line, done)
	select {
	case err := <-done:
		return nil, err
	case <-ctx.Done():
		return done, fmt.Errorf("supervisor: stderr write: %w", ctx.Err())
	case <-timer.C:
		return done, fmt.Errorf("%w after %s", ErrWriteTimedOut, timeout)
	}
}

// writeAdmitted owns the slot until the output and partial-line state are complete.
func (sink *lineSink) writeAdmitted(line string, done chan<- error) {
	output := line
	if sink.partial {
		output = "\n" + output
	}
	n, err := fmt.Fprintln(sink.writer, output)
	if n == len(output)+1 {
		sink.partial = false
	} else if n > 0 && err != nil {
		sink.partial = true
	}
	if err != nil && sink.partial {
		err = fmt.Errorf("%w: %w", ErrPartialLine, err)
	}
	<-sink.slot
	done <- err
}
