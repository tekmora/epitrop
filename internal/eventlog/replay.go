package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	corereceipt "github.com/golusoris/golusoris/core/crypto/receipt"
	"github.com/golusoris/golusoris/core/retry"
	"github.com/jonboulle/clockwork"
)

// retryStable runs op until it succeeds, fails with an error no retry can fix, or budget
// has passed, trying every lockRetryDelay as a writer tries the lock. budget is the lock
// timeout: an append in flight holds the lock, so a writer that waits longer would itself
// give up on the lock. It returns op's own result and error, never retry's wrapping of them;
// only an ended ctx adds its error.
func retryStable[T any](ctx context.Context, budget time.Duration, op func() (T, error)) (T, error) {
	if budget <= 0 {
		return op()
	}
	policy := retry.Policy{
		Initial:     lockRetryDelay,
		Max:         lockRetryDelay,
		Multiplier:  1,
		MaxAttempts: stableAttempts(budget),
		Retryable:   transientReplayError,
	}
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var res T
	var last error
	err := retry.Do(bounded, func(context.Context) error {
		res, last = op()
		return last
	}, policy, clockwork.NewRealClock())
	var zero T
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, retry.ErrPermanent):
		return res, last
	case ctx.Err() != nil:
		return zero, fmt.Errorf("eventlog: replay: %w", ctx.Err())
	case last != nil:
		return zero, last
	default:
		return zero, fmt.Errorf("eventlog: replay: %w", err)
	}
}

// stableBudget is how long a stable replay waits out an append in flight: the lock
// timeout of limits, or its default when limits leaves it unset.
func stableBudget(limits Limits) time.Duration {
	if limits.LockTimeout > 0 {
		return limits.LockTimeout
	}
	return defaultLockTimeout
}

// ReplayStable is Replay for a log that other processes append to. An append writes its
// record and then HEAD; a replay that lands between the two reports a HEAD mismatch that
// the next try no longer sees, and on Windows a replay that opens a file while an append
// renames it is refused with a sharing violation. It retries only those cases, for at most
// the lock timeout of limits; every other error is returned at once.
func ReplayStable(ctx context.Context, dir string, pubKeyHex string, limits Limits, reducer Reducer) (State, error) {
	return retryStable(ctx, stableBudget(limits), func() (State, error) {
		return Replay(ctx, dir, pubKeyHex, limits, reducer)
	})
}

type replayCheckpointedResult struct {
	state State
	use   CheckpointUse
}

// ReplayCheckpointedStable is ReplayCheckpointed with the same retry helper as ReplayStable.
func ReplayCheckpointedStable(ctx context.Context, dir string, pubKeyHex string, limits Limits, name string, reducer Reducer) (State, CheckpointUse, error) {
	res, err := retryStable(ctx, stableBudget(limits), func() (replayCheckpointedResult, error) {
		st, use, rErr := ReplayCheckpointed(ctx, dir, pubKeyHex, limits, name, reducer)
		return replayCheckpointedResult{state: st, use: use}, rErr
	})
	return res.state, res.use, err
}

// ReplayFoldStable is ReplayFold with the same retry helper as ReplayStable.
func ReplayFoldStable(ctx context.Context, dir string, pubKeyHex string, limits Limits, fold string, reducer Reducer) (State, error) {
	return retryStable(ctx, stableBudget(limits), func() (State, error) {
		return ReplayFold(ctx, dir, pubKeyHex, limits, fold, reducer)
	})
}

func transientReplayError(err error) bool {
	if fileBusy(err) || errors.Is(err, errRecordMoved) || errors.Is(err, errLogCompacted) {
		return true
	}
	text := err.Error()
	if strings.Contains(text, "HEAD mismatch") || strings.Contains(text, "missing HEAD") {
		return true
	}
	return strings.Contains(text, "HEAD.json") && strings.Contains(text, "no such file")
}

func Replay(ctx context.Context, dir string, pubKeyHex string, limits Limits, reducer Reducer) (State, error) {
	if err := readyContext(ctx, "replay"); err != nil {
		return State{}, err
	}
	normalized, err := normalizeLimits(limits)
	if err != nil {
		return State{}, err
	}
	if err = checkReplayInputs(dir, pubKeyHex, reducer); err != nil {
		return State{}, err
	}
	r := replayRun{eventsDir: eventsDir(dir), pubKeyHex: strings.ToLower(pubKeyHex), limits: normalized, reducer: reducer}
	st, _, err := r.run()
	return st, err
}

// ReplayFold is Replay for the reducer a base holds under fold. On a compacted log it starts
// from the fold's state in the base; a base without that state fails with the base's seq.
// Replay without a fold fails on a compacted log, since it has no state to start from.
func ReplayFold(ctx context.Context, dir string, pubKeyHex string, limits Limits, fold string, reducer Reducer) (State, error) {
	if err := readyContext(ctx, "replay"); err != nil {
		return State{}, err
	}
	if !validCheckpointName(fold) {
		return State{}, fmt.Errorf("eventlog: invalid fold name %q", fold)
	}
	normalized, err := normalizeLimits(limits)
	if err != nil {
		return State{}, err
	}
	if err = checkReplayInputs(dir, pubKeyHex, reducer); err != nil {
		return State{}, err
	}
	r := replayRun{eventsDir: eventsDir(dir), pubKeyHex: strings.ToLower(pubKeyHex), limits: normalized, reducer: reducer, fold: fold}
	st, _, err := r.run()
	return st, err
}

// ReplayCheckpointed replays the event log from the newest valid checkpoint of name, or full
// replay if none. name also names the reducer's fold: on a compacted log a replay without a
// checkpoint past the base starts from the fold's state in the base.
func ReplayCheckpointed(ctx context.Context, dir string, pubKeyHex string, limits Limits, name string, reducer Reducer) (State, CheckpointUse, error) {
	if err := readyContext(ctx, "replay"); err != nil {
		return State{}, CheckpointUse{}, err
	}
	if !validCheckpointName(name) {
		return State{}, CheckpointUse{}, fmt.Errorf("eventlog: invalid checkpoint name %q", name)
	}
	normalized, err := normalizeLimits(limits)
	if err != nil {
		return State{}, CheckpointUse{}, err
	}
	if err = checkReplayInputs(dir, pubKeyHex, reducer); err != nil {
		return State{}, CheckpointUse{}, err
	}
	r := replayRun{
		eventsDir:      eventsDir(dir),
		checkpointsDir: filepath.Join(dir, "checkpoints"),
		pubKeyHex:      strings.ToLower(pubKeyHex),
		limits:         normalized,
		reducer:        reducer,
		name:           name,
		fold:           name,
	}
	return r.run()
}

type replayRun struct {
	eventsDir      string
	checkpointsDir string
	pubKeyHex      string
	limits         Limits
	reducer        Reducer
	name           string
	// fold names the reducer's state in the base of a compacted log; "" has none.
	fold string
	// until, when set, ends the replay after that seq, which a segment must end at; the
	// record files and HEAD are not read. Compact replays the folds to the new base so.
	until uint64
	// fromCursor starts the replay after seq after instead of at the base; ReplayAfter.
	fromCursor bool
	after      uint64
	segs       []segment
}

// cursor is where a replay stands: the state so far, the last seq it applied or that its
// checkpoint covers, the hash of that record, and how many records it applied.
type cursor struct {
	state *State
	last  uint64
	prev  string
	count int
}

type checkpointResult struct {
	seq   uint64
	hash  string
	state json.RawMessage
	use   CheckpointUse
}

const maxCheckpointsScan = 16

// run reads the base first: a compaction writes the new base before it removes the segments
// the base covers. A replay that failed while the base moved under it says so, and a stable
// replay tries again.
func (r replayRun) run() (State, CheckpointUse, error) {
	b, hasBase, err := readBase(r.eventsDir, r.pubKeyHex, r.limits)
	if err != nil {
		return State{}, CheckpointUse{}, err
	}
	st, use, err := r.runFrom(b, hasBase)
	if err != nil && r.baseMoved(b.Seq) {
		return State{}, CheckpointUse{}, fmt.Errorf("%w: %w", errLogCompacted, err)
	}
	return st, use, err
}

func (r replayRun) baseMoved(seq uint64) bool {
	b, _, err := readBase(r.eventsDir, r.pubKeyHex, r.limits)
	return err == nil && b.Seq != seq
}

func (r replayRun) runFrom(b Base, hasBase bool) (State, CheckpointUse, error) {
	entries, err := os.ReadDir(r.eventsDir)
	if err != nil {
		return State{}, CheckpointUse{}, fmt.Errorf("eventlog: read %s: %w", r.eventsDir, err)
	}
	// The segments are listed after the record files: a rotation writes its segment before
	// it removes the files, so a file this listing missed lies in a segment listed here.
	if r.segs, err = listSegments(r.eventsDir, r.limits.MaxReplayRecords, b.Seq); err != nil {
		return State{}, CheckpointUse{}, err
	}
	st := State{Tasks: map[string]Task{}}
	c := cursor{state: &st}
	if err = r.start(b, hasBase, &c); err != nil {
		return State{}, CheckpointUse{}, err
	}
	cp := r.findCheckpoint(c.last)
	if cp.seq > 0 {
		st = State{Tasks: map[string]Task{}}
		if err = json.Unmarshal(cp.state, &st); err != nil {
			return State{}, CheckpointUse{}, fmt.Errorf("eventlog: unmarshal checkpoint state: %w", err)
		}
		c.last, c.prev = cp.seq, cp.hash
	}
	if err = r.applySegments(looseRecords(entries), &c); err != nil {
		return State{}, CheckpointUse{}, err
	}
	if r.until > 0 {
		return st, cp.use, r.checkUntil(c)
	}
	if err = r.applyToHead(entries, &c); err != nil {
		return State{}, CheckpointUse{}, err
	}
	return st, cp.use, nil
}

// applyToHead applies the record files in entries to c and checks that the last of them is
// the record HEAD.json names.
func (r replayRun) applyToHead(entries []os.DirEntry, c *cursor) error {
	if err := r.applyRecords(entries, c); err != nil {
		return err
	}
	headRecord, hasHead, err := r.readHead()
	if err != nil {
		return err
	}
	return checkHeadMatchesTail(headRecord, hasHead, c.last, c.prev)
}

// start places c where the replay begins: at the caller's cursor, at the base of a compacted
// log, or before seq 1.
func (r replayRun) start(b Base, hasBase bool, c *cursor) error {
	if r.fromCursor {
		return r.startAtCursor(b, hasBase, c)
	}
	if hasBase {
		return r.startAtBase(b, c)
	}
	return nil
}

// startAtBase starts c from the fold's state in b.
func (r replayRun) startAtBase(b Base, c *cursor) error {
	if r.fold == "" {
		return fmt.Errorf("eventlog: log compacted through seq %d: replay needs a fold name", b.Seq)
	}
	raw, ok := b.States[r.fold]
	if !ok {
		return fmt.Errorf("eventlog: base at seq %d holds no state for fold %q", b.Seq, r.fold)
	}
	if err := json.Unmarshal(raw, c.state); err != nil {
		return fmt.Errorf("eventlog: base at seq %d: fold %q: %w", b.Seq, r.fold, err)
	}
	c.last, c.prev = b.Seq, b.Hash
	return nil
}

func (r replayRun) checkUntil(c cursor) error {
	if c.last != r.until {
		return fmt.Errorf("eventlog: replay to seq %d ended at seq %d", r.until, c.last)
	}
	return nil
}

// findCheckpoint returns the newest checkpoint of r.name that matches the log. A checkpoint
// at or below floor, the seq of the log's base, names a record the log no longer holds and is
// passed over.
func (r replayRun) findCheckpoint(floor uint64) checkpointResult {
	if r.name == "" || r.checkpointsDir == "" {
		return checkpointResult{}
	}
	cpDir := filepath.Join(r.checkpointsDir, r.name)
	entries, err := os.ReadDir(cpDir)
	if os.IsNotExist(err) {
		return checkpointResult{}
	}
	if err != nil {
		// An unreadable checkpoint directory costs speed, not correctness: the full replay
		// still runs. It is listed so that the slower replay has a named cause.
		return checkpointResult{use: CheckpointUse{Skipped: []string{fmt.Sprintf("read %s: %v", cpDir, err)}}}
	}
	seqs := filterCheckpointSeqs(entries)
	var use CheckpointUse
	for i := 0; i < len(seqs) && i < maxCheckpointsScan; i++ {
		seq := seqs[i]
		cpPath := filepath.Join(cpDir, checkpointFileName(seq))
		if seq <= floor {
			use.Skipped = append(use.Skipped, fmt.Sprintf("%s: at or below the base at seq %d", filepath.Base(cpPath), floor))
			continue
		}
		cp, ok := r.usableCheckpoint(cpPath, &use)
		if !ok {
			continue
		}
		use.Seq = cp.Seq
		return checkpointResult{seq: cp.Seq, hash: cp.Hash, state: cp.State, use: use}
	}
	return checkpointResult{use: use}
}

func filterCheckpointSeqs(entries []os.DirEntry) []uint64 {
	var seqs []uint64
	for i := 0; i < len(entries) && i < maxCheckpointEntries; i++ {
		entry := entries[i]
		if !entry.IsDir() && isEventRecord(entry.Name()) {
			if seq, parseErr := seqFromName(entry.Name()); parseErr == nil {
				seqs = append(seqs, seq)
			}
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] })
	return seqs
}

// usableCheckpoint reads the checkpoint at cpPath and reports whether it is valid and matches
// the log; use records why it is not, keeping the first divergence.
func (r replayRun) usableCheckpoint(cpPath string, use *CheckpointUse) (Checkpoint, bool) {
	cp, ok := r.checkCheckpointCandidate(cpPath, use)
	if !ok {
		return Checkpoint{}, false
	}
	if div, diverged := r.checkCheckpointAgainstLog(cp, cpPath); diverged {
		if use.Diverged == nil {
			use.Diverged = div
		}
		return Checkpoint{}, false
	}
	return cp, true
}

func (r replayRun) checkCheckpointCandidate(cpPath string, use *CheckpointUse) (Checkpoint, bool) {
	data, err := readFileBounded(cpPath, r.limits.MaxRecordBytes)
	if err != nil {
		use.Skipped = append(use.Skipped, fmt.Sprintf("%s: %v", filepath.Base(cpPath), err))
		return Checkpoint{}, false
	}
	cp, err := decodeCheckpoint(cpPath, data, r.pubKeyHex)
	if err != nil {
		use.Skipped = append(use.Skipped, fmt.Sprintf("%s: %v", filepath.Base(cpPath), err))
		return Checkpoint{}, false
	}
	if cp.Name != r.name {
		use.Skipped = append(use.Skipped, fmt.Sprintf("%s: name %q does not match %q", filepath.Base(cpPath), cp.Name, r.name))
		return Checkpoint{}, false
	}
	return cp, true
}

func (r replayRun) divergenceFor(cp Checkpoint, cpPath string, gotHash string) *Divergence {
	h, _, err := r.readHead()
	if err != nil {
		h = head{}
	}
	return &Divergence{
		Checkpoint: DivergedCheckpoint{Name: cp.Name, Seq: cp.Seq, Hash: cp.Hash},
		Log:        DivergedLog{Seq: h.Seq, Hash: h.Hash, HashAtCheckpointSeq: gotHash},
		File:       cpPath,
	}
}

func (r replayRun) checkCheckpointAgainstLog(cp Checkpoint, cpPath string) (*Divergence, bool) {
	recData, err := r.recordAt(cp.Seq)
	if err != nil {
		return r.divergenceFor(cp, cpPath, ""), true
	}
	gotHash := hashHex(recData)
	if gotHash != cp.Hash {
		return r.divergenceFor(cp, cpPath, gotHash), true
	}
	return nil, false
}

// recordAt returns the bytes of record seq, from its file or from the segment holding it.
func (r replayRun) recordAt(seq uint64) ([]byte, error) {
	data, err := readFileBounded(eventPath(r.eventsDir, seq), r.limits.MaxRecordBytes)
	if !errors.Is(err, os.ErrNotExist) {
		return data, err
	}
	return segmentRecord(r.eventsDir, r.segs, seq, r.limits.MaxRecordBytes)
}

// applySegments applies the records of the segments that lie after c. Each record is
// also compared with a file of the same seq that an interrupted rotation left behind.
func (r replayRun) applySegments(loose map[uint64]string, c *cursor) error {
	for _, seg := range r.segs {
		if seg.last <= c.last {
			continue
		}
		if r.until > 0 && seg.first > r.until {
			break
		}
		err := readSegment(r.eventsDir, seg, r.limits.MaxRecordBytes, func(seq uint64, data []byte) error {
			if err := sameAsLoose(r.eventsDir, loose, seq, data, seg, r.limits.MaxRecordBytes); err != nil {
				return err
			}
			if seq <= c.last {
				return nil
			}
			return r.apply(c, seg.name, seq, data)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// applyRecords applies the record files that lie after c.
func (r replayRun) applyRecords(entries []os.DirEntry, c *cursor) error {
	for _, entry := range entries {
		if !isEventRecord(entry.Name()) {
			continue
		}
		seq, err := seqFromName(entry.Name())
		if err != nil {
			return err
		}
		if seq <= c.last {
			continue
		}
		if seq != c.last+1 {
			return fmt.Errorf("eventlog: gap at seq %d file %s", c.last+1, entry.Name())
		}
		data, err := readFileBounded(filepath.Join(r.eventsDir, entry.Name()), r.limits.MaxRecordBytes)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: seq %d file %s", errRecordMoved, seq, entry.Name())
		}
		if err != nil {
			return err
		}
		if err = r.apply(c, entry.Name(), seq, data); err != nil {
			return err
		}
	}
	return nil
}

// apply verifies data as record seq after c and runs the reducer on it. name is the record
// file or the segment it came from, for the error.
func (r replayRun) apply(c *cursor, name string, seq uint64, data []byte) error {
	c.count++
	if c.count > r.limits.MaxReplayRecords {
		return fmt.Errorf("eventlog: max replay records %d exceeded", r.limits.MaxReplayRecords)
	}
	if seq != c.last+1 {
		return fmt.Errorf("eventlog: gap at seq %d file %s", c.last+1, name)
	}
	if len(data) > r.limits.MaxRecordBytes {
		return fmt.Errorf("%w: seq %d file %s is %d bytes", ErrRecordTooLarge, seq, name, len(data))
	}
	rec, err := decodeRecord(filepath.Join(r.eventsDir, name), data, r.pubKeyHex, seq, c.prev)
	if err != nil {
		return fmt.Errorf("eventlog: seq %d file %s: %w", seq, name, err)
	}
	next, err := r.reducer(*c.state, rec)
	if err != nil {
		return fmt.Errorf("eventlog: seq %d file %s: %w", seq, name, err)
	}
	*c.state, c.last, c.prev = next, seq, hashHex(data)
	return nil
}

func decodeRecord(path string, data []byte, pub string, expected uint64, prev string) (Record, error) {
	if err := verifyCanonical(path, data); err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, fmt.Errorf("eventlog: decode %s: %w", path, err)
	}
	if err := checkRecordLinks(rec, expected, prev, path); err != nil {
		return Record{}, err
	}
	unsigned, err := canonicalJSON(recordUnsigned(rec))
	if err != nil {
		return Record{}, err
	}
	if err = ensureExpectedPublicKey(rec.Receipt.PublicKey, pub, fmt.Sprintf("seq %d file %s", rec.Seq, path)); err != nil {
		return Record{}, err
	}
	if err = corereceipt.VerifyOutput(rec.Receipt, unsigned); err != nil {
		return Record{}, fmt.Errorf("eventlog: seq %d file %s: verify receipt: %w", rec.Seq, path, err)
	}
	return rec, nil
}

func checkRecordLinks(rec Record, expected uint64, prev string, path string) error {
	if rec.Seq != expected {
		return fmt.Errorf("eventlog: seq %d file %s: record seq %d", expected, path, rec.Seq)
	}
	wantPrev := prev
	if expected == 1 {
		wantPrev = zeroHash
	}
	if rec.Prev != wantPrev {
		return fmt.Errorf("eventlog: seq %d file %s: prev mismatch", expected, path)
	}
	if rec.Receipt == nil {
		return fmt.Errorf("eventlog: seq %d file %s: missing receipt", expected, path)
	}
	return nil
}

func (r replayRun) readHead() (head, bool, error) {
	path := filepath.Join(r.eventsDir, "HEAD.json")
	data, err := readFileBounded(path, r.limits.MaxRecordBytes)
	if errors.Is(err, os.ErrNotExist) {
		return head{}, false, nil
	}
	if err != nil {
		return head{}, false, err
	}
	var h head
	if err = decodeHead(path, data, r.pubKeyHex, &h); err != nil {
		return head{}, false, err
	}
	return h, true, nil
}

func checkHeadMatchesTail(h head, hasHead bool, last uint64, lastHash string) error {
	if !hasHead && last == 0 {
		return nil
	}
	if !hasHead {
		return fmt.Errorf("eventlog: missing HEAD with last seq %d", last)
	}
	if h.Seq != last {
		return fmt.Errorf("eventlog: HEAD mismatch: head seq %d, last seq %d", h.Seq, last)
	}
	if h.Hash != lastHash {
		return fmt.Errorf("eventlog: HEAD mismatch: seq %d hash mismatch", last)
	}
	return nil
}

func checkReplayInputs(dir string, pub string, reducer Reducer) error {
	if dir == "" {
		return fmt.Errorf("eventlog: dir is required")
	}
	if err := checkPublicKeyHex(pub); err != nil {
		return err
	}
	if reducer == nil {
		return fmt.Errorf("eventlog: reducer is required")
	}
	return nil
}

func seqFromName(name string) (uint64, error) {
	seq, err := strconv.ParseUint(strings.TrimSuffix(name, ".json"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("eventlog: parse seq from %s: %w", name, err)
	}
	return seq, nil
}

// stableAttempts bounds retryStable's tries over budget: one every lockRetryDelay, as a
// writer tries the lock, plus the first and the last.
func stableAttempts(budget time.Duration) int {
	if budget <= 0 {
		return 1
	}
	return int(budget/lockRetryDelay) + 2
}
