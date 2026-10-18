package eventlog

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrCursorCompacted marks a cursor that a compaction passed: the records after it, up to
// the log's base, are gone, so the log can no longer hand them out one by one.
var ErrCursorCompacted = errors.New("eventlog: cursor below the compacted base")

// ReplayAfter returns the records after seq after, oldest first, each checked as Replay
// checks it: canonical bytes, signature, seq, and the hash link to the record before it.
// The first link is checked against the record at after, from its file or its segment, or
// against the base when after is the base's seq. It reads to HEAD and checks HEAD against
// the last record; a cursor at HEAD, or at 0 before the first append, returns no records.
// It applies at most limits.MaxReplayRecords records and fails past them. A cursor below
// the base of a compacted log fails with ErrCursorCompacted, and a cursor past HEAD fails.
// Like ReplayStable it retries, for at most the lock timeout, a replay that an append or a
// compaction in flight disturbed.
func ReplayAfter(ctx context.Context, dir string, pubKeyHex string, limits Limits, after uint64) ([]Record, error) {
	if err := readyContext(ctx, "replay"); err != nil {
		return nil, err
	}
	normalized, err := normalizeLimits(limits)
	if err != nil {
		return nil, err
	}
	if err = checkReplayInputs(dir, pubKeyHex, TaskReducer); err != nil {
		return nil, err
	}
	if _, err = os.Stat(eventsDir(dir)); errors.Is(err, os.ErrNotExist) && after == 0 {
		return nil, nil
	}
	return retryStable(ctx, stableBudget(limits), func() ([]Record, error) {
		var recs []Record
		r := replayRun{
			eventsDir:  eventsDir(dir),
			pubKeyHex:  strings.ToLower(pubKeyHex),
			limits:     normalized,
			fromCursor: true,
			after:      after,
			reducer: func(st State, rec Record) (State, error) {
				recs = append(recs, rec)
				return st, nil
			},
		}
		if _, _, rErr := r.run(); rErr != nil {
			return nil, rErr
		}
		return recs, nil
	})
}

// startAtCursor starts c at r.after with the hash of the record there, so the record after
// it must link to it.
func (r replayRun) startAtCursor(b Base, hasBase bool, c *cursor) error {
	if hasBase && r.after < b.Seq {
		return fmt.Errorf("%w: cursor at seq %d, base at seq %d", ErrCursorCompacted, r.after, b.Seq)
	}
	if hasBase && r.after == b.Seq {
		c.last, c.prev = b.Seq, b.Hash
		return nil
	}
	if r.after == 0 {
		return nil
	}
	data, err := r.recordAt(r.after)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("eventlog: cursor at seq %d: the log holds no such record", r.after)
	}
	if err != nil {
		return fmt.Errorf("eventlog: cursor at seq %d: %w", r.after, err)
	}
	c.last, c.prev = r.after, hashHex(data)
	return nil
}

// ReplayAfter is ReplayAfter on the log w appends to, with w's key and limits.
func (w *Writer) ReplayAfter(ctx context.Context, after uint64) ([]Record, error) {
	return ReplayAfter(ctx, w.dir, w.signer.PublicKey(), w.limits, after)
}
