// Package sessionlog holds the bounded file walk and line scan that the
// local session-log sources (codexlocal, claudelocal) share. It reads only:
// it never writes to a session directory. Every loop has a scalar bound and
// every walk and scan checks its context (HISS-02).
//
// Ported from cordanaLLM/praetor tribunus/internal/sources/sessionlog at 5d4acfb (EUPL-1.2).
package sessionlog

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Limits bounds one scan of one file.
type Limits struct {
	// MaxFileBytes refuses a file larger than this.
	MaxFileBytes int64
	// MaxLines refuses a file with more lines than this.
	MaxLines int
	// MaxLineBytes is the longest line handed to the visitor; a longer line
	// is skipped whole and counted, never truncated.
	MaxLineBytes int
}

// WalkJSONL calls visit for every *.jsonl regular file under root, at most
// maxFiles directory entries in all. A missing root returns found=false and
// no error: that is a skip, not a failure. visit's error stops the walk.
func WalkJSONL(ctx context.Context, root string, maxFiles int, visit func(path string, info fs.FileInfo) error) (found bool, err error) {
	walk := &jsonlWalk{ctx: ctx, root: root, maxFiles: maxFiles, visit: visit}
	if _, statErr := os.Stat(root); errors.Is(statErr, fs.ErrNotExist) {
		return false, nil
	}
	if walkErr := filepath.WalkDir(root, walk.consider); walkErr != nil {
		return walk.found, fmt.Errorf("walk %s: %w", root, walkErr)
	}
	return walk.found, nil
}

type jsonlWalk struct {
	ctx      context.Context
	root     string
	maxFiles int
	scanned  int
	found    bool
	visit    func(path string, info fs.FileInfo) error
}

func (w *jsonlWalk) consider(p string, d fs.DirEntry, walkErr error) error {
	if walkErr != nil {
		if errors.Is(walkErr, fs.ErrNotExist) {
			return nil
		}
		return walkErr
	}
	if ctxErr := w.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if d.IsDir() {
		return nil
	}
	w.scanned++
	if w.scanned > w.maxFiles {
		return fmt.Errorf("%s exceeds %d files scanned", w.root, w.maxFiles)
	}
	if filepath.Ext(p) != ".jsonl" || !d.Type().IsRegular() {
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return err
	}
	w.found = true
	return w.visit(p, info)
}

// ScanLines hands each line of path (without its newline) to visit, within
// limits. It returns how many overlong lines it skipped. The slice passed to
// visit is only valid during the call.
func ScanLines(ctx context.Context, path string, limits Limits, visit func(line []byte)) (skipped int, err error) {
	// #nosec G304 -- path comes from WalkJSONL under a caller-supplied root.
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > limits.MaxFileBytes {
		return 0, fmt.Errorf("%s exceeds %d bytes", path, limits.MaxFileBytes)
	}
	scan := &lineScan{ctx: ctx, limits: limits, visit: visit,
		reader: bufio.NewReaderSize(io.LimitReader(f, limits.MaxFileBytes+1), limits.MaxLineBytes)}
	if err := scan.run(); err != nil {
		return scan.skipped, fmt.Errorf("scan %s: %w", path, err)
	}
	return scan.skipped, nil
}

type lineScan struct {
	ctx        context.Context
	limits     Limits
	reader     *bufio.Reader
	visit      func(line []byte)
	lines      int
	skipped    int
	discarding bool
}

// run reads slices until EOF. Each ReadSlice consumes at least one byte, so
// the slice count is bounded by the file's byte cap plus the line count.
func (s *lineScan) run() error {
	maxReads := s.limits.MaxFileBytes + int64(s.limits.MaxLines) + 1
	for reads := int64(0); reads < maxReads; reads++ {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		chunk, err := s.reader.ReadSlice('\n')
		done, stepErr := s.step(chunk, err)
		if stepErr != nil || done {
			return stepErr
		}
	}
	return fmt.Errorf("exceeds %d reads", maxReads)
}

// step handles one ReadSlice result; done is true at EOF.
func (s *lineScan) step(chunk []byte, err error) (done bool, stepErr error) {
	if errors.Is(err, bufio.ErrBufferFull) {
		s.discarding = true
		return false, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return true, err
	}
	// An overlong last line without a newline ends at EOF with an empty chunk.
	if len(chunk) > 0 || s.discarding {
		if lineErr := s.endLine(chunk); lineErr != nil {
			return true, lineErr
		}
	}
	return errors.Is(err, io.EOF), nil
}

// endLine finishes one line: an overlong one is counted and dropped.
func (s *lineScan) endLine(chunk []byte) error {
	s.lines++
	if s.lines > s.limits.MaxLines {
		return fmt.Errorf("exceeds %d lines", s.limits.MaxLines)
	}
	if s.discarding {
		s.discarding = false
		s.skipped++
		return nil
	}
	line := chunk
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	if len(line) > 0 {
		s.visit(line)
	}
	return nil
}
