package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/tekmora/epitrop/internal/config"
)

const (
	logReadBufferBytes = 32 * 1024
	// maxLogReadSteps bounds the pipe read loop above any year-long byte stream.
	maxLogReadSteps  int64 = 1 << 50
	maxStaleLogIndex       = config.MaxLogKeepFiles + 1
)

// rotatingLog writes whole lines to path and rotates path.1 .. path.<keepFiles> before any
// write that would push a non-empty active file past maxBytes.
type rotatingLog struct {
	mu        sync.Mutex
	path      string
	maxBytes  int
	keepFiles int
	file      *os.File
	size      int64
}

func openRotatingLog(path string, rot config.JobLogRotation) (*rotatingLog, error) {
	rot = config.NormalizeLogRotation(rot)
	if rot.MaxBytes < 1 || rot.KeepFiles < 1 {
		return nil, fmt.Errorf("job-shim: invalid log rotation bounds %d/%d", rot.MaxBytes, rot.KeepFiles)
	}
	l := &rotatingLog{
		path:      path,
		maxBytes:  rot.MaxBytes,
		keepFiles: rot.KeepFiles,
	}
	if err := l.reopen(); err != nil {
		return nil, err
	}
	if err := pruneStaleBackups(path, rot.KeepFiles); err != nil {
		return nil, errors.Join(err, l.Close())
	}
	return l, nil
}

func pruneStaleBackups(path string, keepFiles int) error {
	for step := 0; step < maxStaleLogIndex; step++ {
		idx := keepFiles + 1 + step
		if idx > maxStaleLogIndex {
			break
		}
		if err := removeRegularIfExists(rotatedLogPath(path, idx)); err != nil {
			return fmt.Errorf("job-shim: remove stale log %s: %w", path, err)
		}
	}
	return nil
}

func removeRegularIfExists(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	return removeIfExists(path)
}

func (l *rotatingLog) reopen() error {
	f, err := openAppendLog(l.path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		closeErr := f.Close()
		return errors.Join(fmt.Errorf("job-shim: stat log %s: %w", l.path, err), closeErr)
	}
	l.file = f
	l.size = info.Size()
	return nil
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func (l *rotatingLog) touch() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if err := os.Chtimes(l.path, now, now); err != nil {
		return fmt.Errorf("job-shim: touch log %s: %w", l.path, err)
	}
	return nil
}

func (l *rotatingLog) writeLine(line []byte) error {
	if len(line) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.writeLineLocked(line)
}

func (l *rotatingLog) writeLineLocked(line []byte) error {
	if len(line) <= l.maxBytes {
		return l.writePieceLocked(line)
	}
	pieces := (len(line) + l.maxBytes - 1) / l.maxBytes
	for i := 0; i < pieces; i++ {
		start := i * l.maxBytes
		end := min(start+l.maxBytes, len(line))
		if err := l.writePieceLocked(line[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (l *rotatingLog) writePieceLocked(piece []byte) error {
	if l.file == nil {
		if err := l.reopen(); err != nil {
			return err
		}
	}
	if l.size > 0 && l.size+int64(len(piece)) > int64(l.maxBytes) {
		if err := l.rotateLocked(); err != nil {
			return err
		}
	}
	n, err := l.file.Write(piece)
	l.size += int64(n)
	if err != nil {
		return fmt.Errorf("job-shim: write log %s: %w", l.path, err)
	}
	return nil
}

func (l *rotatingLog) rotateLocked() error {
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		if err != nil {
			return fmt.Errorf("job-shim: close log %s: %w", l.path, err)
		}
	}
	gap, err := firstMissingBackup(l.path, l.keepFiles)
	if err != nil {
		return err
	}
	shiftCount := gap - 1
	for step := 0; step < shiftCount; step++ {
		idx := shiftCount - step
		if err := renameIfExists(rotatedLogPath(l.path, idx), rotatedLogPath(l.path, idx+1)); err != nil {
			return fmt.Errorf("job-shim: rotate log %s: %w", l.path, err)
		}
	}
	if err := renameIfExists(l.path, rotatedLogPath(l.path, 1)); err != nil {
		return fmt.Errorf("job-shim: rotate log %s: %w", l.path, err)
	}
	if err := removeIfExists(rotatedLogPath(l.path, l.keepFiles+1)); err != nil {
		return fmt.Errorf("job-shim: remove oldest log %s: %w", l.path, err)
	}
	return l.reopen()
}

func firstMissingBackup(path string, keepFiles int) (int, error) {
	for step := 0; step < keepFiles; step++ {
		idx := step + 1
		if _, err := os.Lstat(rotatedLogPath(path, idx)); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return idx, nil
			}
			return 0, fmt.Errorf("job-shim: stat rotated log %s: %w", path, err)
		}
	}
	return keepFiles + 1, nil
}

func rotatedLogPath(path string, index int) string {
	return path + "." + strconv.Itoa(index)
}

func renameIfExists(src, dst string) error {
	err := os.Rename(src, dst)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// rotatingLineWriter adapts rotatingLog to io.Writer for Epitrop's own job-log lines,
// appending a trailing newline when missing so buffered job bytes never interleave.
type rotatingLineWriter struct {
	log *rotatingLog
}

func (l *rotatingLog) lineWriter() io.Writer {
	return rotatingLineWriter{log: l}
}

func (w rotatingLineWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	rest := p
	limit := len(p)
	for step := 0; step < limit && len(rest) > 0; step++ {
		nl := bytes.IndexByte(rest, '\n')
		if nl >= 0 {
			if err := w.log.writeLineLocked(rest[:nl+1]); err != nil {
				return 0, err
			}
			rest = rest[nl+1:]
			continue
		}
		line := make([]byte, len(rest)+1)
		copy(line, rest)
		line[len(rest)] = '\n'
		if err := w.log.writeLineLocked(line); err != nil {
			return 0, err
		}
		rest = nil
	}
	return len(p), nil
}

// lineSplitter buffers partial job lines up to cap bytes (max_bytes) and forwards whole
// lines, or max_bytes pieces of an overlong line, to log.
type lineSplitter struct {
	log     *rotatingLog
	pending []byte
	cap     int
}

func newLineSplitter(log *rotatingLog) *lineSplitter {
	return &lineSplitter{log: log, cap: log.maxBytes}
}

func (s *lineSplitter) writeChunk(chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	var firstErr error
	wrote := false
	limit := len(chunk)
	for step := 0; step < limit && len(chunk) > 0; step++ {
		take, didWrite, err := s.consumeStep(chunk)
		wrote = wrote || didWrite
		recordFirstErr(&firstErr, err)
		chunk = chunk[take:]
	}
	if !wrote && len(s.pending) > 0 {
		recordFirstErr(&firstErr, s.log.touch())
	}
	return firstErr
}

func (s *lineSplitter) consumeStep(chunk []byte) (int, bool, error) {
	room := s.cap - len(s.pending)
	window := chunk
	if len(window) > room {
		window = window[:room]
	}
	if nl := bytes.IndexByte(window, '\n'); nl >= 0 {
		take := nl + 1
		if len(s.pending) == 0 {
			return take, true, s.log.writeLine(window[:take])
		}
		s.pending = appendBounded(s.pending, window[:take], s.cap)
		err := s.log.writeLine(s.pending)
		s.pending = s.pending[:0]
		return take, true, err
	}
	s.pending = appendBounded(s.pending, window, s.cap)
	if len(s.pending) < s.cap {
		return len(window), false, nil
	}
	err := s.log.writeLine(s.pending)
	s.pending = s.pending[:0]
	return len(window), true, err
}

func (s *lineSplitter) flush() error {
	if len(s.pending) == 0 {
		return nil
	}
	err := s.log.writeLine(s.pending)
	s.pending = s.pending[:0]
	return err
}

func appendBounded(dst, src []byte, maxCap int) []byte {
	need := len(dst) + len(src)
	if need <= cap(dst) {
		return append(dst, src...)
	}
	next := max(cap(dst)*2, 256, need)
	if next > maxCap {
		next = maxCap
	}
	grown := make([]byte, len(dst), next)
	copy(grown, dst)
	return append(grown, src...)
}

func copyJobLines(r io.Reader, log *rotatingLog) error {
	splitter := newLineSplitter(log)
	buf := make([]byte, logReadBufferBytes)
	var firstWriteErr error
	for step := int64(0); step < maxLogReadSteps; step++ {
		n, readErr := r.Read(buf)
		if n > 0 {
			recordFirstErr(&firstWriteErr, splitter.writeChunk(buf[:n]))
		}
		if readErr != nil {
			return finishJobRead(splitter, firstWriteErr, readErr)
		}
	}
	recordFirstErr(&firstWriteErr, splitter.flush())
	return firstWriteErr
}

func finishJobRead(splitter *lineSplitter, firstWriteErr, readErr error) error {
	recordFirstErr(&firstWriteErr, splitter.flush())
	if benignPipeEnd(readErr) {
		return firstWriteErr
	}
	return errors.Join(firstWriteErr, fmt.Errorf("job-shim: read job output: %w", readErr))
}

func benignPipeEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || errors.Is(err, os.ErrDeadlineExceeded)
}

func recordFirstErr(dst *error, err error) {
	if err != nil && *dst == nil {
		*dst = err
	}
}
