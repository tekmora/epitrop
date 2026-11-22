package supervisor

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

type jobOutput struct {
	pipeR   *os.File
	done    chan error
	once    sync.Once
	readErr error
}

func startJobOutput(log *rotatingLog) (*jobOutput, *os.File, error) {
	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("job-shim: pipe job output: %w", err)
	}
	out := &jobOutput{
		pipeR: pipeR,
		done:  make(chan error, 1),
	}
	go func() {
		out.done <- copyJobLines(pipeR, log)
	}()
	return out, pipeW, nil
}

func shimDrainTimeout(graceSeconds int) time.Duration {
	if graceSeconds < 1 {
		return time.Second
	}
	return time.Duration(graceSeconds) * time.Second
}

func (o *jobOutput) drain(timeout time.Duration) error {
	if o == nil {
		return nil
	}
	o.once.Do(func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case o.readErr = <-o.done:
			closeJobPipe(o.pipeR)
		case <-timer.C:
			interruptJobPipe(o.pipeR)
			o.readErr = <-o.done
			closeJobPipe(o.pipeR)
		}
	})
	return o.readErr
}

func (o *jobOutput) abort() {
	if o == nil {
		return
	}
	o.once.Do(func() {
		interruptJobPipe(o.pipeR)
		o.readErr = <-o.done
		closeJobPipe(o.pipeR)
	})
}

func interruptJobPipe(pipeR *os.File) {
	if err := pipeR.SetReadDeadline(time.Now()); err != nil {
		if !errors.Is(err, os.ErrNoDeadline) && !errors.Is(err, os.ErrClosed) {
			fmt.Fprintf(os.Stderr, "Error: set job pipe deadline: %v\n", err)
		}
		closeJobPipe(pipeR)
	}
}

func closeJobPipe(pipeR *os.File) {
	if err := pipeR.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		fmt.Fprintf(os.Stderr, "Error: close job pipe: %v\n", err)
	}
}
