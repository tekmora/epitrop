package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrWriteTimedOut marks a line write that passed its deadline; the write may still be
// pending on that writer.
var ErrWriteTimedOut = errors.New("supervisor: stderr write timeout")

// lineSink permits at most one outstanding write, including a write whose caller timed out.
type lineSink struct {
	writer io.Writer
	slot   chan struct{}
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
func writeLine(sink *lineSink, line string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultWriteTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case sink.slot <- struct{}{}:
	case <-timer.C:
		return fmt.Errorf("%w after %s", ErrWriteTimedOut, timeout)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fmt.Fprintln(sink.writer, line)
		<-sink.slot
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("%w after %s", ErrWriteTimedOut, timeout)
	}
}
