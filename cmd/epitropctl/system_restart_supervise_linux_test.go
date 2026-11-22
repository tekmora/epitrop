//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	superviseRunBound      = 50 * time.Second
	superviseStopBound     = 10 * time.Second
	superviseStderrTailCap = 2048
)

type superviseProc struct {
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	outPath string
	errPath string
	done    chan struct{}
	waitErr error
	stopped bool
}

func openSuperviseLogs(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "supervise-out-*")
	if err != nil {
		t.Fatalf("create supervise stdout log: %v", err)
	}
	errFile, err := os.CreateTemp(dir, "supervise-err-*")
	if err != nil {
		if closeErr := outFile.Close(); closeErr != nil {
			t.Logf("close supervise stdout log: %v", closeErr)
		}
		t.Fatalf("create supervise stderr log: %v", err)
	}
	return outFile, errFile
}

func closeSuperviseLogs(t *testing.T, outFile, errFile *os.File) {
	t.Helper()
	if err := errors.Join(outFile.Close(), errFile.Close()); err != nil {
		t.Fatalf("close supervise log fds: %v", err)
	}
}

func startSupervise(t *testing.T, bin, cfgPath string) *superviseProc {
	t.Helper()
	outFile, errFile := openSuperviseLogs(t)
	outPath, errPath := outFile.Name(), errFile.Name()
	ctx, cancel := context.WithTimeout(t.Context(), superviseRunBound)
	cmd := newCLICommand(ctx, bin, "jobs", "supervise", "--config="+cfgPath)
	cmd.Env = isolatedCLIEnv(t)
	cmd.Stdout = outFile
	cmd.Stderr = errFile
	if startErr := cmd.Start(); startErr != nil {
		defer cancel()
		closeSuperviseLogs(t, outFile, errFile)
		t.Fatalf("start jobs supervise: %v", startErr)
	}
	proc := &superviseProc{
		cmd:     cmd,
		cancel:  cancel,
		outPath: outPath,
		errPath: errPath,
		done:    make(chan struct{}),
	}
	go proc.reap()
	t.Cleanup(func() { proc.stop(t) })
	closeSuperviseLogs(t, outFile, errFile)
	return proc
}

// reap waits for the supervise process and records its exit error before closing done.
func (p *superviseProc) reap() {
	p.waitErr = p.cmd.Wait()
	close(p.done)
}

func (p *superviseProc) stop(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	p.stopped = true
	defer p.cancel()
	if killErr := p.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		t.Fatalf("kill supervise pid %d: %v", p.cmd.Process.Pid, killErr)
	}
	select {
	case <-p.done:
	case <-time.After(superviseStopBound):
		t.Fatalf("wait supervise pid %d timed out after %s", p.cmd.Process.Pid, superviseStopBound)
	}
	var exitErr *exec.ExitError
	if p.waitErr != nil && !errors.As(p.waitErr, &exitErr) {
		t.Fatalf("wait supervise pid %d: %v", p.cmd.Process.Pid, p.waitErr)
	}
}

// readSuperviseLogTail returns at most the last superviseStderrTailCap bytes of path.
func readSuperviseLogTail(t *testing.T, path string) string {
	t.Helper()
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "open log root: " + err.Error()
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			t.Errorf("close supervise log root %s: %v", path, closeErr)
		}
	}()
	return readTailFromRoot(t, root, filepath.Base(path))
}

// readTailFromRoot reads up to the last superviseStderrTailCap bytes of name in root.
func readTailFromRoot(t *testing.T, root *os.Root, name string) string {
	t.Helper()
	f, err := root.Open(name)
	if err != nil {
		return "open log " + name + ": " + err.Error()
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil {
			t.Errorf("close supervise log %s: %v", name, closeErr)
		}
	}()
	info, err := f.Stat()
	if err != nil {
		return "stat log " + name + ": " + err.Error()
	}
	size := max(min(info.Size(), int64(superviseStderrTailCap)), int64(0))
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, max(info.Size()-size, int64(0)))
	if err != nil && !errors.Is(err, io.EOF) {
		return "read log " + name + ": " + err.Error()
	}
	return string(buf[:n])
}

// assertSuperviseAlive fails the test if proc has exited, reporting its exit error,
// stdout log path, and up to superviseStderrTailCap bytes from the end of its stderr log.
func assertSuperviseAlive(t *testing.T, proc *superviseProc) {
	t.Helper()
	select {
	case <-proc.done:
		tail := readSuperviseLogTail(t, proc.errPath)
		t.Fatalf("supervise pid %d exited (%v); stdout=%s stderr tail:\n%s",
			proc.cmd.Process.Pid, proc.waitErr, proc.outPath, tail)
	default:
	}
}

func restartSupervise(t *testing.T, prev *superviseProc, bin, cfgPath string) *superviseProc {
	t.Helper()
	prev.stop(t)
	return startSupervise(t, bin, cfgPath)
}
